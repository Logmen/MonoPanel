package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// An in-place restore has restic restore into a directory of the agent's
// and moves what came out into place itself (restoreInPlace, restoreMerge).
// That directory has to lie on the file system the restore goes to: on
// another one every file is copied instead of renamed, and the whole
// selection is first written where it does not belong — a home of 60 GB
// staged on a root file system of 20, next to MySQL and the mail queue,
// fills it. So what goes into the web root is staged in the web root's own
// directory of the agent's, the restore area, next to .shares; the panel's
// configuration, certificates, ACME account and dumps, which are small and
// lie on the system's file systems, stay staged in RestoreDir.
//
// The restore area is root's and 0700, and so is every staging directory in
// it: nobody else enters it. The agent's calls that change, archive, measure
// or back up what lies below the web root refuse it (inRestoreArea): the API
// cannot swap a staged directory for a symlink while restic writes into it,
// nor slip a file past sanitize into what the merge moves into place. A
// server backup leaves it out (--exclude), and the merge never moves a
// snapshot's restore area into the live one.

// restoreAreaName is the restore area's name in the web root: a login never
// begins with a dot, so no home is called that.
const restoreAreaName = ".restore"

// stagingNameRe is the name of a staging directory, in the restore area and
// in RestoreDir alike.
var stagingNameRe = regexp.MustCompile(`^\.in-place-[0-9a-f]{16}$`)

// restoreArea is the web root's restore area.
func (s *Server) restoreArea() string {
	return filepath.Join(filepath.Clean(s.cfg.WWWRoot), restoreAreaName)
}

// inRestoreArea tells the restore area and what lies below it.
func (s *Server) inRestoreArea(p string) bool {
	return atOrBelow(filepath.Clean(p), s.restoreArea())
}

// restoreAreaErr is why a call may not touch p in the restore area.
func (s *Server) restoreAreaErr(p string) error {
	return fmt.Errorf("%s is the agent's own: %s stages in-place restores", p, s.restoreArea())
}

// restoreBase is the directory of the agent's that holds target, a restore's
// target: RestoreDir (a restore aside, the staging of what lies outside the
// web root) or the restore area; "" for none.
func (s *Server) restoreBase(target string) string {
	for _, b := range []string{RestoreDir, s.restoreRoot, s.restoreArea()} {
		if b != "" && below(target, b) {
			return b
		}
	}
	return ""
}

// openRestoreArea opens the restore area, made root's and 0700 when it is
// missing. The web root it lies in must be one nobody but root may write to,
// as setup makes it, and the area a directory of root's that nobody else may
// write to: anything else under the name — a symlink, a directory somebody
// else owns or may write to, one handed to an account before the agent
// refused it — is refused, not taken over. A directory of root's alone there
// is taken for the area whoever made it (only an administrator can have:
// logins never begin with a dot); what it holds besides the stagings stays,
// though a server backup leaves the area out.
func (s *Server) openRestoreArea() (int, error) {
	root := filepath.Clean(s.cfg.WWWRoot)
	rfd, err := openDirTrusted(root)
	if err != nil {
		return -1, err
	}
	defer unix.Close(rfd) //nolint:errcheck // a directory opened for reading
	t := s.trust()
	var st unix.Stat_t
	if err := unix.Fstat(rfd, &st); err != nil {
		return -1, &os.PathError{Op: "stat", Path: root, Err: err}
	}
	if !t.aloneOrSticky(&st) {
		return -1, fmt.Errorf("the web root %s is not root's alone (uid %d, mode %#o): an in-place restore cannot be staged in it", root, st.Uid, st.Mode&0o7777)
	}
	area := s.restoreArea()
	err = unix.Mkdirat(rfd, restoreAreaName, 0o700)
	made := err == nil
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, &os.PathError{Op: "mkdir", Path: area, Err: err}
	}
	fd, err := unix.Openat(rfd, restoreAreaName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return -1, fmt.Errorf("%w: %s is no directory", errSymlinkComponent, area)
	}
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: area, Err: err}
	}
	if made {
		// a default ACL of the web root would have given the new
		// directory entries of its own: the area is root's alone
		for _, acl := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
			if err := unix.Fremovexattr(fd, acl); err != nil && !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) {
				unix.Close(fd) //nolint:errcheck // a directory opened for reading
				return -1, &os.PathError{Op: "removexattr", Path: area, Err: err}
			}
		}
		if err := unix.Fchmod(fd, 0o700); err != nil {
			unix.Close(fd) //nolint:errcheck // a directory opened for reading
			return -1, &os.PathError{Op: "chmod", Path: area, Err: err}
		}
	}
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		return -1, &os.PathError{Op: "stat", Path: area, Err: err}
	}
	if !t.rootOwned(st.Uid) || st.Mode&0o022 != 0 || hasAccessACL(fd) {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		return -1, fmt.Errorf("%s belongs to uid %d with mode %#o: it is not the agent's alone, and an in-place restore is staged there; remove it", area, st.Uid, st.Mode&0o7777)
	}
	return fd, nil
}

// stageNeed is what a staging directory takes: bytes and inodes.
type stageNeed struct {
	bytes, inodes uint64
}

// stagingBlock is the block a file's size is rounded up to, and what a
// directory takes: what the file systems a panel runs on use.
const stagingBlock = 4096

// add counts a node of the snapshot's listing (restic ls --json). The sums
// saturate: the sizes are the snapshot's, which the API composes, and a sum
// that wrapped around would make two files of 8 EB look like a few bytes.
func (n *stageNeed) add(typ string, size uint64) {
	n.inodes = satAdd(n.inodes, 1)
	switch typ {
	case "file":
		if r := size % stagingBlock; r != 0 {
			size = satAdd(size, stagingBlock-r)
		}
		n.bytes = satAdd(n.bytes, size)
	case "dir":
		n.bytes = satAdd(n.bytes, stagingBlock)
	}
}

// plus adds what another staging takes.
func (n *stageNeed) plus(o stageNeed) {
	n.bytes, n.inodes = satAdd(n.bytes, o.bytes), satAdd(n.inodes, o.inodes)
}

// satAdd is a+b, or the largest uint64 where that does not fit.
func satAdd(a, b uint64) uint64 {
	if s := a + b; s >= a {
		return s
	}
	return math.MaxUint64
}

// satMul is a*b, or the largest uint64 where that does not fit.
func satMul(a, b uint64) uint64 {
	if a != 0 && b > math.MaxUint64/a {
		return math.MaxUint64
	}
	return a * b
}

// stagingSlack and stagingInodeSlack are kept free beyond what a staging
// takes: what the count leaves out (the directories on the way, the file
// system's own metadata) and room for what shares the file system — MySQL,
// the journal, the mail queue — while the restore runs.
const (
	stagingSlack      = 64 << 20
	stagingInodeSlack = 1024
)

// stagingStatfs is fstatfs; tests stand in a file system of their own.
var stagingStatfs = unix.Fstatfs

// inPlaceStage is one restic run of an in-place restore: the includes that
// go to one file system, staged in a directory of base.
type inPlaceStage struct {
	base     string
	includes []string
	need     stageNeed
	// dir is the staging directory once made, fd that directory opened.
	dir string
	fd  int
}

// inPlaceStages sorts the includes of an in-place restore by where they are
// staged: what goes into the web root in the restore area, the rest in
// RestoreDir. A stage without includes is left out.
func (s *Server) inPlaceStages(includes []string) []*inPlaceStage {
	www := &inPlaceStage{base: s.restoreArea(), fd: -1}
	agent := &inPlaceStage{base: s.restoreRoot, fd: -1}
	for _, inc := range includes {
		if r, ok := s.inPlaceRoot(inc); ok && r.kind == inPlaceHomes {
			www.includes = append(www.includes, inc)
		} else {
			agent.includes = append(agent.includes, inc)
		}
	}
	var out []*inPlaceStage
	for _, st := range []*inPlaceStage{www, agent} {
		if len(st.includes) > 0 {
			out = append(out, st)
		}
	}
	return out
}

// covers tells whether the snapshot's path p is restored by this stage.
func (st *inPlaceStage) covers(p string) bool {
	return slices.ContainsFunc(st.includes, func(inc string) bool { return atOrBelow(p, inc) })
}

// make makes the staging directory: a fresh one, root's and 0700, in base —
// reached without following a symlink and refused when somebody else may
// write to it.
func (st *inPlaceStage) make(s *Server) error {
	var bfd int
	var err error
	if st.base == s.restoreArea() {
		bfd, err = s.openRestoreArea()
	} else {
		bfd, err = ensureRootDirAs(st.base, 0o700, s.rootUID)
	}
	if err != nil {
		return err
	}
	defer unix.Close(bfd) //nolint:errcheck // a directory opened for reading
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	name := ".in-place-" + hex.EncodeToString(b)
	dir := filepath.Join(st.base, name)
	if err := unix.Mkdirat(bfd, name, 0o700); err != nil {
		return &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	st.dir = dir
	fd, err := unix.Openat(bfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: dir, Err: err}
	}
	st.fd = fd
	// The staging inherits nothing: a default ACL the area or RestoreDir
	// got after the agent made them (an administrator's setfacl -R -d over
	// the web root) would give every entry restic makes here entries of
	// its own, which the merge carries into the homes — onto the live
	// directories too, whose ACLs it takes from the staged ones.
	for _, acl := range []string{"system.posix_acl_default", "system.posix_acl_access"} {
		if err := unix.Fremovexattr(fd, acl); err != nil && !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) {
			return &os.PathError{Op: "removexattr", Path: dir, Err: err}
		}
	}
	return nil
}

// checkStagesSpace refuses an in-place restore whose stagings have no room
// before restic writes anything. The stages on one file system share its
// room — on a host with a single file system the restore area and RestoreDir
// hold their parts at the same time, until the merge —, so each is checked
// against what all of them stage there.
func checkStagesSpace(stages []*inPlaceStage) error {
	devs := make([]uint64, len(stages))
	for i, st := range stages {
		var fst unix.Stat_t
		if err := unix.Fstat(st.fd, &fst); err != nil {
			return &os.PathError{Op: "stat", Path: st.dir, Err: err}
		}
		devs[i] = fst.Dev
	}
	for i, st := range stages {
		var need stageNeed
		var what []string
		for j, o := range stages {
			if devs[j] == devs[i] {
				need.plus(o.need)
				what = append(what, o.includes...)
			}
		}
		if err := checkRoom(st.fd, st.base, need, "restoring "+strings.Join(what, ", ")+" in place, staged before it is moved into place,", "free some space, or restore aside and copy what is needed"); err != nil {
			return err
		}
	}
	return nil
}

// checkRoom refuses need on the file system of fd, which where names: the
// blocks free for the non-root (root's reserve stays) and the free inodes
// must hold it with the slack on top. what says what takes the room, advice
// what to do instead.
func checkRoom(fd int, where string, need stageNeed, what, advice string) error {
	var fs unix.Statfs_t
	if err := stagingStatfs(fd, &fs); err != nil {
		return &os.PathError{Op: "statfs", Path: where, Err: err}
	}
	unit := uint64(fs.Frsize)
	if unit == 0 {
		unit = uint64(fs.Bsize)
	}
	free := satMul(fs.Bavail, unit)
	want := satAdd(satAdd(need.bytes, need.bytes/100), stagingSlack)
	if free < want {
		return &Error{Status: http.StatusInsufficientStorage, Message: fmt.Sprintf("%s takes about %s on the file system of %s, and %s are free there: %s",
			what, sizeOf(want), where, sizeOf(free), advice)}
	}
	if fs.Files > 0 && fs.Ffree < satAdd(need.inodes, stagingInodeSlack) {
		return &Error{Status: http.StatusInsufficientStorage, Message: fmt.Sprintf("%s makes about %d files on the file system of %s, and it has %d free inodes: %s",
			what, need.inodes, where, fs.Ffree, advice)}
	}
	return nil
}

// remove takes the staging directory away with what was not moved.
func (st *inPlaceStage) remove() {
	if st.fd >= 0 {
		unix.Close(st.fd) //nolint:errcheck // a directory opened for reading
		st.fd = -1
	}
	if st.dir != "" {
		// the directories above are root's and 0700, and RemoveAll follows
		// no symlink
		os.RemoveAll(st.dir) //nolint:errcheck // what is left goes at the next start
	}
}

// staleStaging lists the staging directories an earlier agent left behind:
// restarted while restic or the merge ran (an upgrade, an OOM kill), it never
// came to remove them, and nothing else does — root's alone, they hold the
// disk where nobody sees them. No restore runs yet as the agent starts, so
// every one there is stale. A restore area that is not root's alone is left
// as it is: whatever it holds is not the agent's to remove.
func (s *Server) staleStaging() []string {
	var out []string
	list := func(base string, fd int) {
		defer unix.Close(fd) //nolint:errcheck // a directory opened for reading
		names, err := readDirNames(fd)
		if err != nil {
			return
		}
		for _, n := range names {
			if stagingNameRe.MatchString(n) {
				out = append(out, filepath.Join(base, n))
			}
		}
	}
	t := s.trust()
	if fd, err := openDirTrusted(s.restoreRoot); err == nil {
		list(s.restoreRoot, fd)
	}
	rfd, err := openDirTrusted(filepath.Clean(s.cfg.WWWRoot))
	if err != nil {
		return out
	}
	defer unix.Close(rfd) //nolint:errcheck // a directory opened for reading
	var st unix.Stat_t
	if unix.Fstat(rfd, &st) != nil || !t.aloneOrSticky(&st) {
		return out
	}
	fd, err := unix.Openat(rfd, restoreAreaName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return out
	}
	if unix.Fstat(fd, &st) != nil || !t.rootOwned(st.Uid) || st.Mode&0o022 != 0 || hasAccessACL(fd) {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		s.log.Warn("the restore area of the web root is not the agent's alone: what it holds stays", "dir", s.restoreArea())
		return out
	}
	list(s.restoreArea(), fd)
	return out
}

// removeStaging removes what staleStaging found.
func (s *Server) removeStaging(dirs []string) {
	for _, d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			s.log.Warn("what an interrupted in-place restore staged was not removed", "dir", d, "err", err)
			continue
		}
		s.log.Info("removed what an interrupted in-place restore staged", "dir", d)
	}
}

// sizeOf is a byte count as people read it.
func sizeOf(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	f, i := float64(b)/unit, 0
	for ; f >= unit && i < 4; i++ {
		f /= unit
	}
	return fmt.Sprintf("%.1f %s", f, []string{"KB", "MB", "GB", "TB", "PB"}[i])
}
