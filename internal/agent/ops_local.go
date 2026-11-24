package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"monopanel/internal/agent/confcheck"
	"monopanel/internal/osprofile"
	"monopanel/internal/phpmyadmin"
)

// EnsureDir creates one directory with mode (special bits included) and
// ownership, or fixes them up (idempotent). It is mp setup's, which runs as
// root before the agent exists and also fixes up the API's own data
// directory: the walk follows no symlink root did not make, but it does not
// refuse a directory the service user owns (dirTrust.strict off).
func EnsureDir(d DirSpec) (created bool, err error) {
	created, _, err = ensureDirAt(d, setupTrust(), followAll)
	return created, err
}

// ensureDirAt makes d.Path, or gives it the mode and the owner, through its
// parent opened by walkDir; the levels made on the way get the owner too: a
// root-owned parent keeps the service user from managing what lives under it
// (the ACME webroot's .well-known was made that way and broke certificates).
// An existing directory owned by someone else than root or the requested
// owner is taken over only from a parent nobody but root may write to: in
// any other one its owner may have made it, with whatever inside, for root
// to hand on. follow is how many leading components may be root's symlinks
// (walkOpts.follow); d.Path itself never is one.
func ensureDirAt(d DirSpec, t *dirTrust, follow int) (created bool, made []string, err error) {
	p := filepath.Clean(d.Path)
	if !filepath.IsAbs(p) {
		return false, nil, errors.New("path must be absolute")
	}
	if p == "/" {
		return false, nil, errors.New("refusing to change /")
	}
	mode := d.Mode & 0o7777
	if mode == 0 {
		mode = 0o755
	}
	uid, gid := -1, -1
	if d.Owner != "" {
		if uid, err = lookupUID(d.Owner); err != nil {
			return false, nil, err
		}
	}
	if d.Group != "" {
		if gid, err = lookupGID(d.Group); err != nil {
			return false, nil, err
		}
	}
	parent, err := walkDir("/", filepath.Dir(p), walkOpts{trust: t, follow: follow, create: true, mode: mode & 0o777, uid: uid, gid: gid})
	if err != nil {
		return false, nil, err
	}
	defer parent.close()
	made = parent.made
	name := filepath.Base(p)
	here := filepath.Join(parent.resolved, name)
	switch err := unix.Mkdirat(parent.fd, name, mode&0o777); {
	case err == nil:
		created = true
		made = append(made, here)
	case !errors.Is(err, unix.EEXIST):
		return false, made, &os.PathError{Op: "mkdir", Path: p, Err: err}
	}
	fd, err := unix.Openat(parent.fd, name, dirFlags, 0)
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return false, made, errors.New("exists and is not a directory: " + p)
	}
	if err != nil {
		return created, made, &os.PathError{Op: "open", Path: p, Err: err}
	}
	defer unix.Close(fd) //nolint:errcheck // a directory opened for reading
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return created, made, &os.PathError{Op: "stat", Path: p, Err: err}
	}
	if !created && !t.rootOwned(st.Uid) && (uid < 0 || int(st.Uid) != uid) {
		var pst unix.Stat_t
		if err := unix.Fstat(parent.fd, &pst); err != nil {
			return false, made, &os.PathError{Op: "stat", Path: parent.resolved, Err: err}
		}
		if !t.rootOwned(pst.Uid) || pst.Mode&0o022 != 0 {
			return false, made, fmt.Errorf("%w: %s belongs to uid %d and %s may be written to by others: the agent does not take over a directory someone else may have made there; remove it or give it to root by hand", errUntrusted, p, st.Uid, parent.resolved)
		}
	}
	if st.Mode&0o7777 != mode {
		if err := unix.Fchmod(fd, mode); err != nil {
			return created, made, &os.PathError{Op: "chmod", Path: p, Err: err}
		}
	}
	if (uid >= 0 && int(st.Uid) != uid) || (gid >= 0 && int(st.Gid) != gid) {
		if err := unix.Fchown(fd, uid, gid); err != nil {
			return created, made, &os.PathError{Op: "chown", Path: p, Err: err}
		}
	}
	return created, made, nil
}

// errSymlinkInPath marks a path below the web root that leads through a
// symlink or a file where a directory should be.
var errSymlinkInPath = errors.New("a symlink or a file in the path")

// dirIDs resolves the mode and the numeric owner and group of a DirSpec (-1:
// leave as is).
func dirIDs(d DirSpec) (mode os.FileMode, uid, gid int, err error) {
	mode = os.FileMode(d.Mode) & os.ModePerm
	if mode == 0 {
		mode = 0o755
	}
	uid, gid = -1, -1
	if d.Owner != "" {
		if uid, err = lookupUID(d.Owner); err != nil {
			return 0, -1, -1, err
		}
	}
	if d.Group != "" {
		if gid, err = lookupGID(d.Group); err != nil {
			return 0, -1, -1, err
		}
	}
	return mode, uid, gid, nil
}

// ensureHomeDir is EnsureDir for a directory below root (the web root, whose
// subdirectories the clients own). It walks every component below root
// through the directory opened before it, never following a symlink
// (O_NOFOLLOW), makes the missing levels there and changes mode and owner
// through the opened directory: a client who swaps a component for a symlink
// at any moment can only make the call fail, never redirect it.
func ensureHomeDir(root string, d DirSpec) (created bool, err error) {
	return ensureHomeDirGuarded(root, d, nil)
}

// ensureHomeDirGuarded is ensureHomeDir that leaves the way to the shared
// folders' mount points root's (shareGuard.dirChange): a level it makes or the
// directory it changes there is never given to anybody else or opened to
// others' writes.
func ensureHomeDirGuarded(root string, d DirSpec, g *shareGuard) (created bool, err error) {
	mode, uid, gid, err := dirIDs(d)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(root, d.Path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return false, fmt.Errorf("%s is not below %s", d.Path, root)
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, &os.PathError{Op: "open", Path: root, Err: err}
	}
	defer func() { unix.Close(fd) }() //nolint:errcheck // a directory opened for reading
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	parts := strings.Split(rel, "/")
	cur := root
	for i, name := range parts {
		cur = filepath.Join(cur, name)
		made := false
		next, err := unix.Openat(fd, name, flags, 0)
		if errors.Is(err, unix.ENOENT) {
			// a level made here gets the owner and the mode as well
			if err := g.dirChange(cur, nil, false, uid, gid, uint32(mode)); err != nil {
				return created, err
			}
			if err := unix.Mkdirat(fd, name, uint32(mode)); err != nil && !errors.Is(err, unix.EEXIST) {
				return created, &os.PathError{Op: "mkdir", Path: cur, Err: err}
			}
			made = true
			next, err = unix.Openat(fd, name, flags, 0)
		}
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return created, fmt.Errorf("%w: refusing %s", errSymlinkInPath, cur)
		}
		if err != nil {
			return created, &os.PathError{Op: "open", Path: cur, Err: err}
		}
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		fd = next
		last := i == len(parts)-1
		if made && last {
			created = true
		}
		// The levels made on the way get the owner too, as EnsureDir gives
		// them (a root-owned parent keeps the client out of its own tree).
		if made && !last && (uid >= 0 || gid >= 0) {
			if err := unix.Fchown(fd, uid, gid); err != nil {
				return created, &os.PathError{Op: "chown", Path: cur, Err: err}
			}
		}
	}
	if g != nil {
		// A shared folder's mount point with the folder mounted on it opens
		// as the folder: the mode and owner meant for the root-owned
		// directory below would land on the site's folder (the API asks for
		// the mount point again whenever it applies the share).
		if g.isMount(filepath.Clean(d.Path)) && mountedOn(fd) {
			return created, nil
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return created, &os.PathError{Op: "stat", Path: d.Path, Err: err}
		}
		if err := g.dirChange(filepath.Clean(d.Path), &st, hasAccessACL(fd), uid, gid, uint32(mode)); err != nil {
			return created, err
		}
	}
	if err := unix.Fchmod(fd, uint32(mode)); err != nil {
		return created, &os.PathError{Op: "chmod", Path: d.Path, Err: err}
	}
	if uid >= 0 || gid >= 0 {
		if err := unix.Fchown(fd, uid, gid); err != nil {
			return created, &os.PathError{Op: "chown", Path: d.Path, Err: err}
		}
	}
	return created, nil
}

func (s *Server) dirAllowed(p string) bool {
	// A directory made in place of config.yaml or the secret key, or a
	// change of mode or owner of the directory that holds them, is a write
	// to them as well; the restore area is the agent's (restore_staging.go).
	if s.coversProtected(p) || s.inRootTree(p) || s.inRestoreArea(p) {
		return false
	}
	if s.pathAllowed(strings.TrimSuffix(p, "/") + "/") {
		return true
	}
	// The web root (the accounts' homes), the run directory (php-fpm's
	// sockets in /run/monopanel/php) and that of the accounts' Apaches:
	// apache2 -t refuses a DefaultRuntimeDir or a Mutex directory that does
	// not exist, and systemd makes it only when the unit starts (root's,
	// like everything the agent makes outside the web root). The log
	// directory is not here: its site-log tree has its own call
	// (/v1/sitelogs/ensure).
	for _, root := range []string{filepath.Clean(s.cfg.WWWRoot), filepath.Clean(s.cfg.RunDir), confcheck.ApacheInstanceRunRoot} {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

// dirOwners are who may own what dirs/ensure makes outside the web root
// besides root: the mail store is vmail's, the DKIM keys' directory
// root:opendkim, MySQL's slow log directory on EL mysql's, the search
// daemon's data and log directories its user's. A directory of a root
// daemon's configuration handed to anyone else would let them change what
// lives there without the agent's checks.
func (s *Server) dirOwners() []struct{ dir, owner, group string } {
	out := []struct{ dir, owner, group string }{
		{"/var/mail/monopanel", "vmail", "vmail"},
		{"/etc/opendkim/keys", "", "opendkim"},
		{"/var/log/mysql", "mysql", "mysql"},
		// phpMyAdmin's sessions, temporary files, tickets and sign-out
		// marks are its pool user's (ops_phpmyadmin.go).
		{phpmyadmin.StateDir, phpmyadmin.User, phpmyadmin.User},
		{phpmyadmin.RunDir, phpmyadmin.User, phpmyadmin.User},
	}
	if s.profile != nil {
		sx := osprofile.Sphinx(s.profile)
		out = append(out, struct{ dir, owner, group string }{sx.DataDir, sx.User, sx.User}, struct{ dir, owner, group string }{sx.LogDir, sx.User, sx.User})
	}
	return out
}

// checkDirSpec refuses what dirs/ensure must not give a directory outside
// the web root: a mode that lets group or others write to it or carries a
// special bit, an owner or a group besides root and those of dirOwners.
func (s *Server) checkDirSpec(d DirSpec) error {
	if d.Mode&^0o777 != 0 || d.Mode&0o022 != 0 {
		return fmt.Errorf("mode %#o of %s is not allowed: a directory the agent makes outside the web root is never writable by group or others and carries no special bit", d.Mode, d.Path)
	}
	owner := d.Owner == "" || d.Owner == "root"
	group := d.Group == "" || d.Group == "root"
	for _, o := range s.dirOwners() {
		if o.dir != "" && (d.Path == o.dir || strings.HasPrefix(d.Path, o.dir+"/")) {
			owner = owner || (o.owner != "" && d.Owner == o.owner)
			group = group || d.Group == o.group
		}
	}
	if !owner {
		return fmt.Errorf("owner %q of %s is not allowed: directories the agent makes there are root's", d.Owner, d.Path)
	}
	if !group {
		return fmt.Errorf("group %q of %s is not allowed: directories the agent makes there are root's", d.Group, d.Path)
	}
	return nil
}

func (s *Server) ensureDirs(ctx context.Context, req *EnsureDirsRequest) (*EnsureDirsResponse, error) {
	resp := &EnsureDirsResponse{Created: []string{}}
	root := filepath.Clean(s.cfg.WWWRoot)
	// Below the web root the way to a shared folder's mount point stays
	// root's (sharemount.go): a home holding one is never handed to its
	// account.
	var guard *shareGuard
	for _, d := range req.Dirs {
		if guard == nil && strings.HasPrefix(filepath.Clean(d.Path), root+"/") {
			g, release, err := s.holdShareGuard()
			if err != nil {
				return nil, err
			}
			defer release()
			guard = g
		}
	}
	for _, d := range req.Dirs {
		p := filepath.Clean(d.Path)
		if !filepath.IsAbs(p) || !s.dirAllowed(p) {
			return nil, &Error{Status: http.StatusForbidden, Message: "directory not allowed: " + p}
		}
		d.Path = p
		var created bool
		var err error
		if strings.HasPrefix(p, root+"/") {
			// Clients own the directories below the web root: one of them
			// swapped for a symlink would turn this root mkdir, chmod and
			// chown into one of /etc/systemd/system or /root.
			created, err = ensureHomeDirGuarded(root, d, guard)
			if errors.Is(err, errSymlinkInPath) {
				return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
			}
			var ae *Error
			if errors.As(err, &ae) {
				return nil, ae
			}
		} else {
			if err := s.checkDirSpec(d); err != nil {
				return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
			}
			created, _, err = ensureDirAt(d, s.trust(), s.managedDepth(p, s.cfg.RunDir, s.cfg.WWWRoot))
			if errors.Is(err, errUntrusted) || errors.Is(err, errSymlinkInPath) {
				return nil, &Error{Status: http.StatusForbidden, Message: "directory " + p + ": " + err.Error()}
			}
		}
		if err != nil {
			return nil, &Error{Message: "ensure dir " + p, Output: err.Error()}
		}
		if created {
			resp.Created = append(resp.Created, p)
		}
	}
	s.restorecon(ctx, resp.Created...)
	return resp, nil
}

// EnsureDirOwner gives path, a file or a directory, an owner and a group by
// name (mp setup, for config.yaml, the secret key and the panel's
// certificate). It goes through the directories like EnsureDir and changes
// the entry itself, never what a symlink there points to, and refuses a
// file with a second name: in the API's own directory that could be a hard
// link to another file of the system.
func EnsureDirOwner(path, owner, group string) (bool, error) {
	uid, gid := -1, -1
	var err error
	if owner != "" {
		if uid, err = lookupUID(owner); err != nil {
			return false, err
		}
	}
	if group != "" {
		if gid, err = lookupGID(group); err != nil {
			return false, err
		}
	}
	if uid < 0 && gid < 0 {
		return false, nil
	}
	p := filepath.Clean(path)
	parent, err := walkDir("/", filepath.Dir(p), walkOpts{trust: setupTrust(), follow: followAll, uid: -1, gid: -1})
	if err != nil {
		return false, err
	}
	defer parent.close()
	name := filepath.Base(p)
	var st unix.Stat_t
	if err := unix.Fstatat(parent.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, &os.PathError{Op: "stat", Path: p, Err: err}
	}
	if st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink > 1 && (uid < 0 || int(st.Uid) != uid) {
		return false, fmt.Errorf("%s has %d names: refusing to change the owner of a file linked elsewhere", p, st.Nlink)
	}
	if err := unix.Fchownat(parent.fd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, &os.PathError{Op: "chown", Path: p, Err: err}
	}
	return true, nil
}
