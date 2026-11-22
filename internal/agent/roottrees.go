package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/agent/confcheck"
	"monopanel/internal/osprofile"
)

// Root's daemons act by name on two trees that lie in directories the API's
// user may write to. In <run_dir>/php the php-fpm masters make the pools'
// sockets: they remove what is at a socket's path, bind it, then chown it
// and set its ACL by that path. In <log_dir>/sites nginx, Apache and the
// php-fpm masters open the site logs, creating and appending, following
// symlinks — nginx -t and php-fpm -t as well. The run and the log directory
// are root:<service group> 1771: while php/ or sites/ is missing, the API
// could make it itself, and in a directory of its own it could slip a
// symlink under a socket's or a log's name between root's steps, for a
// chown, an ACL or an append as root of any file (/etc/shadow,
// /etc/ld.so.preload). Once a tree is root's, the sticky bit keeps the API
// from renaming or removing it, and only root adds entries to it.
//
// So both are root's before the API runs, and the agent drives those
// daemons only while they are:
//
//   - systemd-tmpfiles makes php/ at boot (the package's and mp setup's
//     tmpfiles file), and the agent makes both at its start. There it also
//     takes over one it did not make (openTreeDir) and clears the tree of
//     what root does not put there (sweepPHPSocketDir, sweepSiteLogTree):
//     whoever could write to it could have put anything there — up to
//     0.8.19 the API could make either one, the log directory being its own
//     and the run directory without the sticky bit. One entry it cannot
//     clear stops nothing else (sweepErrs);
//   - confcheck lets the servers log only in sites/ and right in an
//     account's directory of it: deeper down, a directory the start could
//     not move out (a full disk) would be someone else's;
//   - before it starts, restarts or reloads php-fpm, nginx or Apache, before
//     it runs their configuration checks, and before it installs or removes
//     packages, whose scripts do the same, it checks that the trees are
//     root's alone — and the site log tree cleared since the start, an
//     account's directory taken over, a link removed: what the php-fpm
//     masters find left in php/ they unlink before they bind — and refuses
//     otherwise (vetRootDaemonTrees, vetPackageScripts).

// phpSocketDirMode is the mode of <run_dir>/php: root writes, the nginx
// workers and the accounts' Apaches pass through to the sockets.
const phpSocketDirMode = 0o755

// quarantinePrefix starts the name of the directory, root's alone, that
// takes what the agent moved out of a tree it cleared.
const quarantinePrefix = "untrusted-"

// errMovedAway marks a directory its old owner renamed away while the agent
// took it over.
var errMovedAway = errors.New("renamed away while being taken over")

// treeDir is how a directory of the trees is owned: by root (rootUID, the
// test's own user in tests), with gid (-1 keeps the group) and mode.
type treeDir struct {
	rootUID, gid int
	mode         uint32
}

// rootsAlone tells a directory of root's that nobody else may write to.
// With an access ACL the group bits are its mask: without a write bit
// there, no named user or group may write either.
func rootsAlone(st *unix.Stat_t, rootUID int) bool {
	return int(st.Uid) == rootUID && st.Mode&0o022 == 0
}

// openTreeDir opens the directory name of parent without following a
// symlink — making it when it is missing and create is set — and checks
// that it is root's alone. One that is not, or something else under the
// name, is refused, unless takeOver (the agent's start): a symlink or a file
// then goes itself, never what it points to, and the directory is made
// anew; a directory someone else may write to is taken over (takeOverDir).
// A directory root made just now, or keeps at the start, gets d's group and
// mode. took tells that what stood there was not root's alone, and so
// neither is what it holds.
func openTreeDir(parent int, parentPath, name string, d treeDir, create, takeOver bool) (fd int, made, took bool, err error) {
	p := filepath.Join(parentPath, name)
	for range 3 {
		made = false
		fd, err = unix.Openat(parent, name, dirFlags, 0)
		if errors.Is(err, unix.ENOENT) && create {
			switch merr := unix.Mkdirat(parent, name, d.mode); {
			case merr == nil:
				made = true
			case !errors.Is(merr, unix.EEXIST):
				return -1, false, took, &os.PathError{Op: "mkdir", Path: p, Err: merr}
			}
			fd, err = unix.Openat(parent, name, dirFlags, 0)
		}
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			if !takeOver {
				return -1, false, false, fmt.Errorf("%w: refusing %s", errSymlinkInPath, p)
			}
			// Gone meanwhile, or a directory by now: looked at again.
			if uerr := unix.Unlinkat(parent, name, 0); uerr != nil && !errors.Is(uerr, unix.ENOENT) && !errors.Is(uerr, unix.EISDIR) {
				return -1, false, took, &os.PathError{Op: "remove", Path: p, Err: uerr}
			}
			took = true
			continue
		}
		if err != nil {
			return -1, false, took, &os.PathError{Op: "open", Path: p, Err: err}
		}
		var st unix.Stat_t
		if err = unix.Fstat(fd, &st); err != nil {
			unix.Close(fd) //nolint:errcheck // a directory opened for reading
			return -1, false, took, &os.PathError{Op: "stat", Path: p, Err: err}
		}
		switch {
		case int(st.Uid) == d.rootUID && (made || takeOver && rootsAlone(&st, d.rootUID)):
			err = fixTreeDir(fd, p, &st, d)
		case rootsAlone(&st, d.rootUID):
		case !takeOver:
			err = fmt.Errorf("%w: %s (owner uid %d, mode %#o)", errNotRootsAlone, p, st.Uid, st.Mode&0o7777)
		default:
			took = true
			if err = takeOverDir(parent, fd, name, p, d); errors.Is(err, errMovedAway) {
				unix.Close(fd) //nolint:errcheck // a directory opened for reading
				continue
			}
		}
		if err != nil {
			unix.Close(fd) //nolint:errcheck // a directory opened for reading
			return -1, false, took, err
		}
		return fd, made, took, nil
	}
	return -1, false, took, fmt.Errorf("%w: %s kept changing while the agent took it over", errNotRootsAlone, p)
}

// fixTreeDir gives a directory of root's alone d's group and mode.
func fixTreeDir(fd int, p string, st *unix.Stat_t, d treeDir) error {
	if d.gid >= 0 && int(st.Gid) != d.gid {
		if err := unix.Fchown(fd, -1, d.gid); err != nil {
			return &os.PathError{Op: "chown", Path: p, Err: err}
		}
	}
	if st.Mode&0o7777 != d.mode {
		if err := unix.Fchmod(fd, d.mode); err != nil {
			return &os.PathError{Op: "chmod", Path: p, Err: err}
		}
	}
	return nil
}

// takeOverDir gives the directory fd, the entry name of parent, to root
// with d's group and mode and without an ACL, through the descriptor. The
// owner goes first: from then on the old one can neither change the mode
// back nor, once it is set, add anything. Until the chown the old owner
// could have renamed the directory away and put something else under the
// name: errMovedAway, and what was taken over lies elsewhere then, root's.
func takeOverDir(parent, fd int, name, p string, d treeDir) error {
	gid := d.gid
	if gid < 0 {
		gid = os.Getegid()
	}
	if err := unix.Fchown(fd, d.rootUID, gid); err != nil {
		return &os.PathError{Op: "chown", Path: p, Err: err}
	}
	if _, err := dropACLs(fd, p); err != nil {
		return err
	}
	if err := unix.Fchmod(fd, d.mode); err != nil {
		return &os.PathError{Op: "chmod", Path: p, Err: err}
	}
	var st, now unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &os.PathError{Op: "stat", Path: p, Err: err}
	}
	if err := unix.Fstatat(parent, name, &now, unix.AT_SYMLINK_NOFOLLOW); err != nil || now.Dev != st.Dev || now.Ino != st.Ino {
		return fmt.Errorf("%w: %s", errMovedAway, p)
	}
	return nil
}

// dropACLs removes the access and the default ACL of the directory fd (p)
// and tells whether it had one. A default ACL would hand whoever it names
// every socket or log made in the directory later.
func dropACLs(fd int, p string) (bool, error) {
	had := false
	for _, attr := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		switch err := unix.Fremovexattr(fd, attr); {
		case err == nil:
			had = true
		case !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.EOPNOTSUPP):
			return had, &os.PathError{Op: "remove the ACL of", Path: p, Err: err}
		}
	}
	return had, nil
}

// entryNames lists the directory dir through a descriptor of its own, opened
// now: the caller's stays where it is for the *at calls, and some file
// systems (btrfs) list through an open directory only what it held when it
// was opened.
func entryNames(dir int, p string) ([]string, error) {
	fd, err := unix.Openat(dir, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, &os.PathError{Op: "list", Path: p, Err: err}
	}
	return names, nil
}

// quarantine takes the directories the agent finds in a tree it clears: an
// untrusted-<time>-<random> directory of root's alone (0700), made when
// first needed next to the tree, on its file system — a rename moves a
// directory in one step, without descending into it or following anything
// in it. No daemon of the panel opens anything there: confcheck keeps their
// sockets and logs inside the trees. The administrator finds there what
// someone else made.
type quarantine struct {
	parent     int
	parentPath string
	rootUID    int
	fd         int
	path       string
	n          int
	// err is why it could not be made (a full disk): not tried again for
	// every entry that needs it.
	err error
}

func newQuarantine(parent int, parentPath string, rootUID int) *quarantine {
	return &quarantine{parent: parent, parentPath: parentPath, rootUID: rootUID, fd: -1}
}

func (q *quarantine) close() {
	if q.fd >= 0 {
		unix.Close(q.fd) //nolint:errcheck // a directory opened for reading
		q.fd = -1
	}
}

// take moves the entry name of dir (p) into the quarantine and returns where
// it went, "" when it is gone.
func (q *quarantine) take(dir int, name, p string) (string, error) {
	if q.fd < 0 {
		if q.err == nil {
			q.err = q.make()
		}
		if q.err != nil {
			return "", fmt.Errorf("%s stays: no quarantine to move it to: %w", p, q.err)
		}
	}
	q.n++
	base := name
	if len(base) > 200 {
		base = base[:200]
	}
	to := fmt.Sprintf("%d-%s", q.n, base)
	switch err := unix.Renameat(dir, name, q.fd, to); {
	case errors.Is(err, unix.ENOENT):
		return "", nil
	case err != nil:
		return "", &os.PathError{Op: "move to the quarantine", Path: p, Err: err}
	}
	return filepath.Join(q.path, to), nil
}

func (q *quarantine) make() error {
	// A name nobody can make ahead of the agent in the directory the API's
	// user may write to.
	var name, p string
	for i := 0; ; i++ {
		var rnd [4]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return err
		}
		name = quarantinePrefix + time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(rnd[:])
		p = filepath.Join(q.parentPath, name)
		err := unix.Mkdirat(q.parent, name, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EEXIST) || i == 9 {
			return &os.PathError{Op: "mkdir", Path: p, Err: err}
		}
	}
	fd, err := unix.Openat(q.parent, name, dirFlags, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: p, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || int(st.Uid) != q.rootUID || st.Mode&0o077 != 0 {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		return fmt.Errorf("%w: %s is not the directory the agent just made", errNotRootsAlone, p)
	}
	q.fd, q.path = fd, p
	return nil
}

// treeSweep is what clearing a tree did.
type treeSweep struct {
	took    []string // directories taken over from someone else
	removed []string // entries removed themselves: symlinks, FIFOs, files that do not belong
	moved   []string // directories moved to the quarantine, "from -> to"
}

func (r *treeSweep) empty() bool { return len(r.took)+len(r.removed)+len(r.moved) == 0 }

// remove removes the entry name of dir (p) itself, never what it points to.
func (r *treeSweep) remove(dir int, name, p string) error {
	switch err := unix.Unlinkat(dir, name, 0); {
	case err == nil:
		r.removed = append(r.removed, p)
	case !errors.Is(err, unix.ENOENT):
		return &os.PathError{Op: "remove", Path: p, Err: err}
	}
	return nil
}

// move moves the directory name of dir (p) to the quarantine.
func (r *treeSweep) move(q *quarantine, dir int, name, p string) error {
	to, err := q.take(dir, name, p)
	if err != nil || to == "" {
		return err
	}
	r.moved = append(r.moved, p+" -> "+to)
	return nil
}

// sweepErrs is what a sweep could not clear. It goes on past an entry that
// fails: one directory it cannot move must not leave the rest of the tree —
// other accounts' directories not yet taken over, links not yet removed — as
// its old owner made it.
type sweepErrs struct {
	first []string
	n     int
}

func (e *sweepErrs) add(err error) {
	if err == nil {
		return
	}
	if e.n++; len(e.first) < 5 {
		e.first = append(e.first, err.Error())
	}
}

func (e *sweepErrs) err() error {
	switch {
	case e.n == 0:
		return nil
	case e.n > len(e.first):
		return fmt.Errorf("%s; and %d more", strings.Join(e.first, "; "), e.n-len(e.first))
	}
	return errors.New(strings.Join(e.first, "; "))
}

// entryAt is the stat of the entry name of dir, not following a symlink;
// ok is false when it is gone.
func entryAt(dir int, name, p string) (st unix.Stat_t, ok bool, err error) {
	switch err = unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); {
	case err == nil:
		return st, true, nil
	case errors.Is(err, unix.ENOENT):
		return st, false, nil
	}
	return st, false, &os.PathError{Op: "stat", Path: p, Err: err}
}

// sweepPHPSocketDir clears <run_dir>/php, opened and root's alone, of what
// the php-fpm masters do not make there: they make sockets and nothing
// else. A socket stays — the pools' are in use, and a master removes
// whatever is at its socket's path before it binds it —, but for one of
// the service user's (serviceUID, -1 for none): a pool's socket is its
// account's or root's, one of the API's would take a site's requests. A
// directory goes to the quarantine, anything else (a symlink, a file, a
// FIFO) is removed.
func sweepPHPSocketDir(dir int, dirPath string, serviceUID int, q *quarantine, r *treeSweep) error {
	names, err := entryNames(dir, dirPath)
	if err != nil {
		return err
	}
	var errs sweepErrs
	for _, n := range names {
		p := filepath.Join(dirPath, n)
		st, ok, err := entryAt(dir, n, p)
		switch {
		case err != nil:
		case !ok:
		case st.Mode&unix.S_IFMT == unix.S_IFSOCK:
			if serviceUID >= 0 && int(st.Uid) == serviceUID {
				err = r.remove(dir, n, p)
			}
		case st.Mode&unix.S_IFMT == unix.S_IFDIR:
			err = r.move(q, dir, n, p)
		default:
			err = r.remove(dir, n, p)
		}
		errs.add(err)
	}
	return errs.err()
}

// sweepSiteLogTree clears <log_dir>/sites, opened and root's alone, of what
// root does not put there. sites/ holds directories, one per account,
// root's alone (root:<login> 0751): one that is not is taken over, anything
// else but a directory is removed (sweepAccountLogs goes on in each).
func sweepSiteLogTree(sites int, sitesPath string, rootUID, rootGID int, q *quarantine, r *treeSweep) error {
	names, err := entryNames(sites, sitesPath)
	if err != nil {
		return err
	}
	var errs sweepErrs
	for _, n := range names {
		p := filepath.Join(sitesPath, n)
		st, ok, err := entryAt(sites, n, p)
		if err != nil || !ok {
			errs.add(err)
			continue
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			errs.add(r.remove(sites, n, p))
			continue
		}
		// The account's group reads its logs; a directory of no panel
		// account gets root's.
		acct := unixUser{uid: -1, gid: rootGID}
		if u, err := lookupUser(n); err == nil && nameRe.MatchString(n) && u.uid >= uidMin() {
			acct = u
		}
		fd, _, took, err := openTreeDir(sites, sitesPath, n, treeDir{rootUID: rootUID, gid: acct.gid, mode: siteLogsLoginMode}, false, true)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs.add(err)
			}
			continue
		}
		if took {
			r.took = append(r.took, p)
		}
		errs.add(sweepAccountLogs(fd, p, acct.uid, q, r))
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
	}
	return errs.err()
}

// sweepAccountLogs clears an account's directory of the site log tree,
// opened and root's alone. It holds the logs and their rotations, plain
// files: a directory goes to the quarantine, anything else but a plain file
// is removed, and so is a file with a second name unless it is the
// account's own PHP error log (owner, -1 for none), which PHP opens as the
// account and the account may link elsewhere. A second name of any other
// file would turn the servers' appends — and nginx's chown to its worker
// user when it reopens a log — into those of that file.
func sweepAccountLogs(dir int, dirPath string, owner int, q *quarantine, r *treeSweep) error {
	names, err := entryNames(dir, dirPath)
	if err != nil {
		return err
	}
	var errs sweepErrs
	for _, n := range names {
		p := filepath.Join(dirPath, n)
		st, ok, err := entryAt(dir, n, p)
		switch {
		case err != nil:
		case !ok:
		case st.Mode&unix.S_IFMT == unix.S_IFREG && (st.Nlink == 1 || owner >= 0 && int(st.Uid) == owner && accountWritesLog(n)):
		case st.Mode&unix.S_IFMT == unix.S_IFDIR:
			err = r.move(q, dir, n, p)
		default:
			err = r.remove(dir, n, p)
		}
		errs.add(err)
	}
	return errs.err()
}

// preparePHPSocketDir makes <run_dir>/php root's alone at the agent's start,
// before the API runs: made when it is missing, taken over when someone else
// made it, and cleared of what the php-fpm masters do not put there.
func (s *Server) preparePHPSocketDir(ctx context.Context) error {
	run, err := walkDir("/", filepath.Clean(s.cfg.RunDir), walkOpts{trust: s.trust(), follow: followAll, uid: -1, gid: -1})
	if err != nil {
		return err
	}
	defer run.close()
	dir := filepath.Join(run.resolved, "php")
	fd, made, took, err := openTreeDir(run.fd, run.resolved, "php", treeDir{rootUID: s.rootUID, gid: os.Getegid(), mode: phpSocketDirMode}, true, true)
	if err != nil {
		return err
	}
	defer unix.Close(fd) //nolint:errcheck // a directory opened for reading
	if made {
		s.restorecon(ctx, dir)
	}
	// postinstall runs systemd-tmpfiles before it restarts the agent: its
	// d line gives an API-made php/ to root, owner and mode, and leaves the
	// ACL. php/ is made anew at every boot, with none.
	had, err := dropACLs(fd, dir)
	if err != nil {
		return err
	}
	r := &treeSweep{}
	if took || had {
		r.took = append(r.took, dir)
	}
	q := newQuarantine(run.fd, run.resolved, s.rootUID)
	defer q.close()
	serviceUID, lerr := lookupUID(s.cfg.ServiceUser)
	if lerr != nil || serviceUID == s.rootUID {
		serviceUID = -1
	}
	err = sweepPHPSocketDir(fd, dir, serviceUID, q, r)
	s.logSweep("php-fpm socket directory", dir, q, r)
	if err != nil {
		return fmt.Errorf("%s is root's alone now, but clearing it of what php-fpm does not make there failed: %w; remove what is no socket there by hand", dir, err)
	}
	return nil
}

// prepareSiteLogTree is PrepareLogDir at the agent's start, before the API
// runs, and goes further: a sites/ that is not root's alone is taken over
// rather than refused, and the tree is cleared of what root does not put
// there (sweepSiteLogTree) — the API's user could have made either up to
// 0.8.19, when the log directory was its own. What came of it is kept for
// siteLogTreeCleared.
func (s *Server) prepareSiteLogTree(ctx context.Context) error {
	s.siteLogTreeMu.Lock()
	defer s.siteLogTreeMu.Unlock()
	s.siteLogTreeErr = s.takeOverSiteLogTree(ctx)
	return s.siteLogTreeErr
}

// siteLogTreeCleared is nil once the agent's start has taken the site log
// tree over and cleared it, and tries again while it has not. Clearing may
// fail halfway and leave sites/ root's alone with what the old owner made
// still inside — an account's directory it could not take over, a link it
// could not remove, a directory it could not move out: the quarantine is not
// made on a full disk, and the log directory is the API's to fill.
// checkSiteLogRoot looks at sites/ alone.
func (s *Server) siteLogTreeCleared(ctx context.Context) error {
	s.siteLogTreeMu.Lock()
	defer s.siteLogTreeMu.Unlock()
	if s.siteLogTreeErr != nil {
		s.siteLogTreeErr = s.takeOverSiteLogTree(ctx)
	}
	return s.siteLogTreeErr
}

// takeOverSiteLogTree takes the site log tree over and clears it
// (prepareSiteLogTree).
func (s *Server) takeOverSiteLogTree(ctx context.Context) error {
	logDir := filepath.Clean(s.cfg.LogDir)
	gid := s.serviceGID()
	if err := makeLogDir(logDir, s.cfg.ServiceGroup, gid); err != nil {
		return err
	}
	ids := siteLogIDs{rootUID: s.rootUID, rootGID: os.Getegid(), serviceGID: gid, takeOver: os.Geteuid() == 0}
	top, err := openLogDir(logDir, ids, &SiteLogsResponse{})
	if err != nil {
		return err
	}
	defer unix.Close(top) //nolint:errcheck // a directory opened for reading
	// Its old owner, the service user, could rename sites/ under the
	// agent's hands while the directory is still its own.
	if err := rootsSticky(top, logDir, ids.rootUID); err != nil {
		return err
	}
	sitesPath := filepath.Join(logDir, "sites")
	sites, made, took, err := openTreeDir(top, logDir, "sites", treeDir{rootUID: ids.rootUID, gid: ids.rootGID, mode: siteLogsMode}, true, true)
	if err != nil {
		return err
	}
	defer unix.Close(sites) //nolint:errcheck // a directory opened for reading
	if made {
		s.restorecon(ctx, sitesPath)
	}
	r := &treeSweep{}
	if took {
		r.took = append(r.took, sitesPath)
	}
	q := newQuarantine(top, logDir, ids.rootUID)
	defer q.close()
	err = sweepSiteLogTree(sites, sitesPath, ids.rootUID, ids.rootGID, q, r)
	s.logSweep("site log tree", sitesPath, q, r)
	if err != nil {
		return fmt.Errorf("%s is root's alone now, but clearing it of what root does not put there failed: %w; remove what is no plain log file there by hand", sitesPath, err)
	}
	return nil
}

// logSweep reports what clearing a tree did, if anything: someone besides
// root wrote to it.
func (s *Server) logSweep(what, dir string, q *quarantine, r *treeSweep) {
	if r.empty() {
		return
	}
	s.log.Warn(what+": someone besides root has written there; the agent took over what was not root's alone and cleared out what root does not put there",
		"dir", dir, "taken_over", r.took, "removed", firstOf(r.removed), "quarantined", firstOf(r.moved), "quarantine", q.path)
}

// firstOf is a list for a log line: its first 20 entries and how many more.
func firstOf(list []string) []string {
	const n = 20
	if len(list) <= n {
		return list
	}
	return append(append([]string{}, list[:n]...), fmt.Sprintf("… and %d more", len(list)-n))
}

// rootDaemonTrees tells which of the trees the root master of unit acts on
// by name: php-fpm on both (its pools' sockets, their slow and access
// logs), nginx and Apache — the distribution's or an account's — on the
// site logs.
func rootDaemonTrees(unit string) (php, logs bool) {
	if _, ok := confcheck.ApacheInstanceLoginOf(unit); ok {
		return false, true
	}
	name, ok := strings.CutSuffix(unit, ".service")
	if !ok {
		if dot := strings.LastIndexByte(unit, '.'); dot >= 0 && actionableKinds[unit[dot+1:]] {
			return false, false // a socket, a timer: no daemon of these
		}
		// php8.4-fpm, a service to systemctl
		name = unit
	}
	switch {
	case strings.Contains(name, "php") && strings.Contains(name, "fpm"):
		return true, true
	case name == "nginx" || strings.HasPrefix(name, "nginx-") || strings.HasPrefix(name, "nginx@"),
		name == "apache2" || name == "httpd" || strings.HasPrefix(name, "apache2@") || strings.HasPrefix(name, "httpd@"):
		return false, true
	}
	return false, false
}

// validatorOpensSiteLogs tells the configuration checks that open the site
// logs as root: nginx -t and php-fpm -t open every log their configuration
// names (php-fpm -t binds no socket), and the apache2 -t of an account's
// Apache and of the shared one, which open none, are counted with them.
func (s *Server) validatorOpensSiteLogs(argv []string) bool {
	if s.profile == nil {
		return false
	}
	if s.apacheInstanceCheck(argv) || equalArgs(argv, s.profile.Web().NginxCheckArgv) || equalArgs(argv, confcheck.ApacheSharedCheck()) {
		return true
	}
	for _, v := range osprofile.PHPVersions(s.profile) {
		if l := osprofile.PHP(s.profile, v.Version); l != nil && equalArgs(argv, l.FPMCheckArgv) {
			return true
		}
	}
	return false
}

// vetRootDaemonTrees refuses to start, restart or reload units whose root
// masters act on the trees by name, and to run configuration checks that
// open the site logs, while a tree they use is not root's alone; a missing
// one is made, root's.
func (s *Server) vetRootDaemonTrees(ctx context.Context, units []string, validators [][]string) error {
	var phpBy, logsBy string
	for _, u := range units {
		php, logs := rootDaemonTrees(fullUnit(u))
		if php && phpBy == "" {
			phpBy = "to start, restart or reload " + fullUnit(u)
		}
		if logs && logsBy == "" {
			logsBy = "to start, restart or reload " + fullUnit(u)
		}
	}
	for _, argv := range validators {
		if logsBy == "" && s.validatorOpensSiteLogs(argv) {
			logsBy = "to run " + strings.Join(quoteArgs(argv), " ")
		}
	}
	return s.vetTrees(ctx, phpBy, logsBy)
}

// vetPackageScripts is vetRootDaemonTrees for the package manager: what
// installs or removes packages also starts, restarts and reloads php-fpm,
// nginx and Apache — their packages' maintainer scripts and triggers (any
// php8.4-* package restarts php8.4-fpm), and needrestart after any package
// that changed a library they use.
func (s *Server) vetPackageScripts(ctx context.Context, action string) error {
	by := "to " + action + " packages (their scripts start, restart and reload php-fpm, nginx and Apache)"
	return s.vetTrees(ctx, by, by)
}

// vetTrees refuses what phpBy and logsBy say, when not empty, while the
// tree it needs is not root's alone (or, for the site logs, not cleared
// since the agent's start).
func (s *Server) vetTrees(ctx context.Context, phpBy, logsBy string) error {
	if phpBy != "" {
		if err := s.checkPHPSocketDir(ctx); err != nil {
			return &Error{Status: http.StatusForbidden, Message: fmt.Sprintf("refusing %s: %v; the php-fpm master makes the pools' sockets in %s and hands them over by name, so whoever may write there could have it chown any file. The agent takes the directory over at its start: restart monopanel-agent", phpBy, err, filepath.Join(s.cfg.RunDir, "php"))}
		}
	}
	if logsBy != "" {
		sites := filepath.Join(s.cfg.LogDir, "sites")
		if err := s.siteLogTreeCleared(ctx); err != nil {
			return &Error{Status: http.StatusForbidden, Message: fmt.Sprintf("refusing %s: %v; nginx, Apache and php-fpm open the site logs in %s as root, by name, so what its old owner left there could have them append to any file. The agent tries to clear the tree again before each such action", logsBy, err, sites)}
		}
		if err := s.checkSiteLogRoot(ctx); err != nil {
			return &Error{Status: http.StatusForbidden, Message: fmt.Sprintf("refusing %s: %v; nginx, Apache and php-fpm open the site logs in %s as root, by name, so whoever may write there could have them append to any file. The agent takes the tree over at its start and clears out what root did not put there: restart monopanel-agent", logsBy, err, sites)}
		}
	}
	return nil
}

// checkPHPSocketDir checks that <run_dir>/php is root's alone, in the run
// directory reached through directories root may go through (as the agent
// reaches it at its start); a missing one is made.
func (s *Server) checkPHPSocketDir(ctx context.Context) error {
	run, err := walkDir("/", filepath.Clean(s.cfg.RunDir), walkOpts{trust: s.trust(), follow: followAll, uid: -1, gid: -1})
	if err != nil {
		return err
	}
	defer run.close()
	return s.checkTreeDir(ctx, run.fd, run.resolved, "php", phpSocketDirMode)
}

// checkSiteLogRoot checks that <log_dir>/sites is root's alone, in a log
// directory of root's with the sticky bit, reached as sitelogs/ensure
// reaches it (a setting of root's, below /var/log); a missing one is made.
func (s *Server) checkSiteLogRoot(ctx context.Context) error {
	logDir := filepath.Clean(s.cfg.LogDir)
	top, err := unix.Open(logDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: logDir, Err: err}
	}
	defer unix.Close(top) //nolint:errcheck // a directory opened for reading
	if err := rootsSticky(top, logDir, s.rootUID); err != nil {
		return err
	}
	return s.checkTreeDir(ctx, top, logDir, "sites", siteLogsMode)
}

// checkTreeDir checks that the directory name of parent is root's alone,
// making it when it is missing.
func (s *Server) checkTreeDir(ctx context.Context, parent int, parentPath, name string, mode uint32) error {
	fd, made, _, err := openTreeDir(parent, parentPath, name, treeDir{rootUID: s.rootUID, gid: -1, mode: mode}, true, false)
	if err != nil {
		return err
	}
	unix.Close(fd) //nolint:errcheck // a directory opened for reading
	if made {
		s.restorecon(ctx, filepath.Join(parentPath, name))
	}
	return nil
}

// rootsSticky checks that the directory fd (p) is root's and that nobody
// else may rename or remove root's entries in it: nobody else may write to
// it, or it carries the sticky bit (the run and the log directory, 1771).
func rootsSticky(fd int, p string, rootUID int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &os.PathError{Op: "stat", Path: p, Err: err}
	}
	if int(st.Uid) != rootUID || st.Mode&0o022 != 0 && st.Mode&unix.S_ISVTX == 0 {
		return fmt.Errorf("%w: %s (owner uid %d, mode %#o)", errNotRootsAlone, p, st.Uid, st.Mode&0o7777)
	}
	return nil
}
