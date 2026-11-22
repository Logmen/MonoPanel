package agent

import (
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"monopanel/internal/osprofile"
)

var (
	tarNewerRe    = regexp.MustCompile(`^--newer-mtime=[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}$`)
	tarStripRe    = regexp.MustCompile(`^--strip-components=([0-9])$`)
	downloadTarRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.(tar\.gz|tgz|tar)$`)
)

// planTar: an account's home or mail domain archived for the other server of
// a migration (/v1/stream/out), and the archives the panel unpacks: a
// migration's home or mail, a CMS, Sphinx 3, Roundcube.
func (s *Server) planTar(c *toolCall) (*toolPlan, error) {
	if c.mode == modeStreamOut {
		return s.planTarCreate(c)
	}
	return s.planTarExtract(c)
}

// planTarCreate: tar --create --file - --directory <dir> --sparse
// --warning=no-file-changed --warning=no-file-removed [--exclude=…]…
// [--newer-mtime=…] . (internal/api/ops_migrate.go). tar runs as root and
// reads everything: the directory must be a home or a mail domain, and it is
// handed to tar as a descriptor the agent opened without following a symlink.
// --to-command, --checkpoint-action, -I, --rsh-command, --info-script, -f to
// a file, -T, -P and the like are not among the options.
func (s *Server) planTarCreate(c *toolCall) (*toolPlan, error) {
	a := c.args
	head := []string{"--create", "--file", "-", "--directory", "", "--sparse", "--warning=no-file-changed", "--warning=no-file-removed"}
	if len(a) < len(head)+1 || a[len(a)-1] != "." {
		return nil, refuseArgs("tar", a, "the panel archives with tar --create --file - --directory <dir> --sparse … .")
	}
	for i, h := range head {
		if i != 4 && a[i] != h {
			return nil, refuseArgs("tar", a, "the panel archives with tar --create --file - --directory <dir> --sparse … .")
		}
	}
	newer := false
	for _, x := range a[len(head) : len(a)-1] {
		switch {
		case strings.HasPrefix(x, "--exclude=") && len(x) > len("--exclude="):
		case tarNewerRe.MatchString(x) && !newer:
			newer = true
		default:
			return nil, notAllowed("the tar option "+x, "only --exclude= and --newer-mtime= follow")
		}
	}
	dir := a[4]
	if !cleanAbs(dir) || !(below(dir, s.cfg.WWWRoot) || below(dir, mailStore)) {
		return nil, notAllowed("archiving "+dir, "the panel archives a home or a mail domain")
	}
	root := s.cfg.WWWRoot
	if below(dir, mailStore) {
		root = mailStore
	}
	fd, err := openDirBelow(root, dir, false, 0)
	if err != nil {
		return nil, notAllowed("archiving "+dir, err.Error())
	}
	args := append([]string{}, a...)
	args[4] = "/proc/self/fd/3"
	return &toolPlan{args: args, files: []*os.File{fdFile(fd, dir)}}, nil
}

// planTarExtract: the extractions of the panel —
//
//	-xzf <data_dir>/downloads/<roundcube>.tar.gz -C <site> --strip-components=1 --no-same-owner   (/v1/tool, ops_mail_webmail.go)
//	-xzf - -C <sphinx install dir> --strip-components=1 --no-same-owner                         (ops_tools.go)
//	-xzf - | -xf - -C <docroot> --no-same-owner --warning=no-timestamp [--strip-components=N] [member…]  (cms.go)
//	--extract --file - --directory <home or mail domain> --no-same-owner --warning=no-timestamp (ops_migrate_import.go)
//
// GNU tar as root restores set-user-ID bits, makes device nodes and follows
// a symlink it finds on the way — one an account left in its home, or one the
// archive itself made — and the archive is the API's to choose. So the agent
// does the extraction itself (untar), into a directory it opened without
// following a symlink.
func (s *Server) planTarExtract(c *toolCall) (*toolPlan, error) {
	a := c.args
	var archive, dest string
	haveArchive, haveDest, extract, gz := false, false, false, false
	strip := 0
	var members []string
	for i := 0; i < len(a); i++ {
		x := a[i]
		next := func() (string, bool) {
			if i+1 >= len(a) {
				return "", false
			}
			i++
			return a[i], true
		}
		var ok bool
		switch {
		case x == "-xzf" || x == "-xf":
			if extract || haveArchive {
				return nil, refuseArgs("tar", a, "one archive, extracted once")
			}
			extract, gz = true, x == "-xzf"
			if archive, ok = next(); !ok {
				return nil, refuseArgs("tar", a, "-f needs the archive")
			}
			haveArchive = true
		case x == "--extract":
			if extract {
				return nil, refuseArgs("tar", a, "one archive, extracted once")
			}
			extract = true
		case x == "--file":
			if haveArchive {
				return nil, refuseArgs("tar", a, "one archive")
			}
			if archive, ok = next(); !ok {
				return nil, refuseArgs("tar", a, "--file needs the archive")
			}
			haveArchive = true
		case x == "-C" || x == "--directory":
			if haveDest {
				return nil, refuseArgs("tar", a, "one destination")
			}
			if dest, ok = next(); !ok {
				return nil, refuseArgs("tar", a, x+" needs the destination")
			}
			haveDest = true
		case tarStripRe.MatchString(x):
			strip, _ = strconv.Atoi(tarStripRe.FindStringSubmatch(x)[1])
		case x == "--no-same-owner" || x == "--warning=no-timestamp":
		case strings.HasPrefix(x, "-"):
			return nil, notAllowed("the tar option "+x, "the panel never passes it")
		default:
			m := path.Clean(x)
			if x == "" || path.IsAbs(m) || m == "." || m == ".." || strings.HasPrefix(m, "../") || m != x {
				return nil, notAllowed("the tar member "+strconv.Quote(x), "a relative name inside the archive")
			}
			members = append(members, m)
		}
	}
	if !extract || !haveArchive || !haveDest {
		return nil, refuseArgs("tar", a, "an extraction names the archive (-f) and the destination (-C)")
	}
	sphinx := osprofile.Sphinx(s.profile).InstallDir
	var root string
	switch {
	case !cleanAbs(dest):
	case below(dest, s.cfg.WWWRoot):
		root = s.cfg.WWWRoot
	case below(dest, mailStore):
		root = mailStore
	case sphinx != "" && dest == sphinx:
		root = sphinx
	}
	if root == "" {
		return nil, notAllowed("extracting into "+dest, "the panel unpacks into the accounts' homes, the mail store and Sphinx's directory")
	}
	plan := &toolPlan{}
	var src *os.File
	switch {
	case c.mode == modeStreamIn && archive == "-":
	case c.mode == modeTool && archive != "-":
		f, err := s.openDownload(archive)
		if err != nil {
			return nil, err
		}
		src = f
		plan.closers = append(plan.closers, f)
	default:
		return nil, notAllowed("tar -f "+archive+" through "+c.mode.String(), "a stream comes on stdin, a file from "+s.cfg.DownloadsDir())
	}
	dfd, err := openDirBelow(root, dest, false, 0)
	if err != nil {
		plan.close()
		return nil, notAllowed("extracting into "+dest, err.Error())
	}
	plan.closers = append(plan.closers, fdFile(dfd, dest))
	if root == s.cfg.WWWRoot {
		// The directories an archive brings get its modes, and a symlink of
		// its making may stand where a directory is missing: never on the way
		// to a shared folder's mount point, for as long as the extraction
		// runs (sharemount.go). The destination itself keeps its own.
		g, release, err := s.holdShareGuard()
		if err != nil {
			plan.close()
			return nil, err
		}
		plan.closers = append(plan.closers, closerFunc(release))
		if m, ok := g.below(dest); ok {
			plan.close()
			return nil, g.refuse("extracting an archive into "+dest, m)
		}
	}
	opts := untarOpts{gzip: gz, strip: strip, members: members}
	plan.native = func(ctx context.Context, stdin io.Reader, out io.Writer) int {
		in := stdin
		if src != nil {
			in = src
		}
		return untar(ctx, in, dfd, dest, opts, out)
	}
	return plan, nil
}

// openDownload opens an archive the API downloaded, a plain file of
// <data_dir>/downloads reached without following a symlink: the directory is
// the API's.
func (s *Server) openDownload(p string) (*os.File, error) {
	if !cleanAbs(p) || filepath.Dir(p) != s.cfg.DownloadsDir() || !downloadTarRe.MatchString(filepath.Base(p)) {
		return nil, notAllowed("the archive "+p, "archives are read from "+s.cfg.DownloadsDir())
	}
	dfd, err := openDirBelow(s.cfg.DataDir, filepath.Dir(p), false, 0)
	if err != nil {
		return nil, notAllowed("the archive "+p, err.Error())
	}
	defer unix.Close(dfd) //nolint:errcheck // a directory opened for reading
	fd, err := unix.Openat(dfd, filepath.Base(p), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, notAllowed("the archive "+p, err.Error())
	}
	f := os.NewFile(uintptr(fd), p)
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, notAllowed("the archive "+p, "not a regular file")
	}
	return f, nil
}
