package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"monopanel/internal/osprofile"
)

// The shared Apache of 0.8.19 and older took every site's custom directives
// from <domain>.d/*.conf next to its virtual host (confcheck.ApacheLegacySites).
// The upgrade carries them over to the site's directory of its account's own
// Apache, through config/apply, where they are checked as that Apache's
// directives. They are the administrators' files, root's — on a host with a
// root umask of 077 readable by root alone — so the agent reads them for the
// API, and nothing else: the regular files of one site's directory that
// Apache's IncludeOptional *.conf took, or the names in the directory (the
// upgrade removes what it carried and keeps the rest). The way to the
// directory is walked as the agent walks the system's directories, and from
// the directory itself down no symlink is followed at all (openLegacyDir); a
// file is looked at before it is opened (a device or a FIFO is never opened),
// then opened without following a symlink and must still be that regular
// file, with no other name. Whatever stands there in another form fails the
// read, by name: a site moved without a directive the shared Apache read — an
// access rule among them — would lose it without a word, while the shared
// Apache keeps serving a site that has not moved. Each file's permission bits
// come along: one only root may read stays so where it is carried to.

const (
	// apacheLegacyFileMax bounds one file of directives, apacheLegacyTotalMax
	// a site's together: the distributions' whole apache2.conf is 7 KB.
	apacheLegacyFileMax  = 1 << 20
	apacheLegacyTotalMax = 4 << 20
)

// apacheLegacyDomainRe is a site's name as the panel stores it (lower case,
// labels of letters, digits and hyphens): one component of a path.
var apacheLegacyDomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

func (s *Server) apacheLegacy(_ context.Context, req *ApacheLegacyRequest) (*ApacheLegacyResponse, error) {
	if req.Domain != "" && (len(req.Domain) > 253 || !apacheLegacyDomainRe.MatchString(req.Domain)) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid domain: " + req.Domain}
	}
	// Only Debian's and Ubuntu's panel ran sites in the shared Apache.
	if s.profile.Family() != osprofile.FamilyDebian {
		return &ApacheLegacyResponse{}, nil
	}
	return ReadApacheLegacy(s.apacheLegacySites, req.Domain)
}

// ReadApacheLegacy reads the shared Apache's directory dir (absolute,
// openLegacyDir): with a domain, the regular *.conf files of dir/<domain>.d;
// without one, the names in dir. A missing directory has nothing. The
// agent's test double reads with it as well.
func ReadApacheLegacy(dir, domain string) (*ApacheLegacyResponse, error) {
	top, err := openLegacyDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return &ApacheLegacyResponse{}, nil
	}
	if err != nil {
		return nil, legacyErr(err)
	}
	defer unix.Close(top) //nolint:errcheck // a directory opened for reading
	if domain == "" {
		names, err := fdNames(top, dir)
		if err != nil {
			return nil, err
		}
		return &ApacheLegacyResponse{Names: names}, nil
	}
	sub := filepath.Join(dir, domain+".d")
	fd, err := unix.Openat(top, domain+".d", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return &ApacheLegacyResponse{}, nil
	case errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR):
		return nil, &Error{Status: http.StatusForbidden, Message: sub + " is a symlink or not a directory: the agent reads the custom directives only from the directory itself"}
	case err != nil:
		return nil, &Error{Message: "open " + sub, Output: err.Error()}
	}
	defer unix.Close(fd) //nolint:errcheck // a directory opened for reading
	names, err := fdNames(fd, sub)
	if err != nil {
		return nil, err
	}
	resp := &ApacheLegacyResponse{}
	total := 0
	for _, name := range names {
		// What IncludeOptional <domain>.d/*.conf took: Apache's wildcard
		// matches no leading dot.
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".conf") {
			continue
		}
		p := filepath.Join(sub, name)
		data, mode, err := readLegacyFileAt(fd, name)
		if err != nil {
			return nil, &Error{Status: http.StatusForbidden, Message: p + ": " + err.Error()}
		}
		if total += len(data); total > apacheLegacyTotalMax {
			return nil, &Error{Status: http.StatusForbidden, Message: fmt.Sprintf("%s: the custom directives there are larger than %d bytes together", sub, apacheLegacyTotalMax)}
		}
		resp.Files = append(resp.Files, ApacheLegacyFile{Name: name, Content: string(data), Mode: mode})
	}
	return resp, nil
}

// openLegacyDir opens the shared Apache's directory p: the way to its parent
// as the agent walks the system's directories (openDirNoFollow: a symlink of
// root's in a directory of root's — an /etc/apache2 an administrator keeps
// elsewhere — is followed, as for every file the agent writes in /etc, the
// accounts' Apaches' among them), and p itself, and everything below it,
// without following any symlink, root's or not (openDirBelow). Nothing the
// API asks for makes a symlink on the way there — config/apply writes
// regular files, enabling a unit links in /etc/systemd alone, the agent
// makes other symlinks only in the panel's trees (homes, what it unpacks
// there), and of the a2* tools it runs only those that remove one — so one
// of root's there is the administrator's. Refusing it would fail every apply
// of a site in Apache mode on such a host — each looks for directives to
// carry — while the directory itself and what is in it are what root reads
// for the API, and they are never anything else than what stands there.
func openLegacyDir(p string) (int, error) {
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) || p == "/" {
		return -1, fmt.Errorf("%s is not a directory of the shared Apache", p)
	}
	return openDirBelow(filepath.Dir(p), p, false, 0)
}

// legacyErr turns a refusal on the way into the agent's answer.
func legacyErr(err error) error {
	if errors.Is(err, errSymlinkInPath) || errors.Is(err, errSymlinkComponent) {
		return &Error{Status: http.StatusForbidden, Message: err.Error()}
	}
	return &Error{Message: "the shared Apache's directory of 0.8.19", Output: err.Error()}
}

// fdNames are the names in the opened directory fd (p names it), sorted.
func fdNames(fd int, p string) ([]string, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, &Error{Message: "read " + p, Output: err.Error()}
	}
	f := os.NewFile(uintptr(dup), p)
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, &Error{Message: "read " + p, Output: err.Error()}
	}
	sort.Strings(names)
	return names, nil
}

// readLegacyFileAt reads the regular file name of the directory dir: looked
// at first without following a symlink, so that nothing else is ever opened,
// then opened without following one and without blocking, and still the same
// file with one name and at most apacheLegacyFileMax bytes. It tells the
// file's permission bits as well.
func readLegacyFileAt(dir int, name string) ([]byte, uint32, error) {
	var before unix.Stat_t
	if err := unix.Fstatat(dir, name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, 0, err
	}
	if err := legacyPlain(&before); err != nil {
		return nil, 0, err
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, 0, errors.New("a symlink: the agent does not follow it; put the file itself there")
	}
	if err != nil {
		return nil, 0, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, 0, err
	}
	if st.Dev != before.Dev || st.Ino != before.Ino {
		return nil, 0, errors.New("it was replaced while the agent read it")
	}
	if err := legacyPlain(&st); err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(io.LimitReader(f, apacheLegacyFileMax+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > apacheLegacyFileMax {
		return nil, 0, fmt.Errorf("larger than %d bytes", apacheLegacyFileMax)
	}
	return data, st.Mode & 0o777, nil
}

// legacyPlain refuses what the agent does not read as a file of directives.
func legacyPlain(st *unix.Stat_t) error {
	switch {
	case st.Mode&unix.S_IFMT == unix.S_IFLNK:
		return errors.New("a symlink: the agent does not follow it; put the file itself there")
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		return errors.New("not a regular file")
	case st.Nlink > 1:
		return errors.New("it has other names (a hard link): the agent reads a file with one name only")
	case st.Size > apacheLegacyFileMax:
		return fmt.Errorf("larger than %d bytes", apacheLegacyFileMax)
	}
	return nil
}
