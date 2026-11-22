package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// untarOpts are the parts of GNU tar's command line the panel uses.
type untarOpts struct {
	gzip bool
	// strip drops that many leading components of every name
	// (--strip-components).
	strip int
	// members, when given, are the only names extracted, with everything
	// below them; they are matched before stripping, as GNU tar does.
	members []string
}

// untar extracts the tar stream r below the directory destFd (dest names it
// in messages) the way GNU tar -x --no-same-owner does for root, with what
// GNU tar cannot be told: every directory on the way is opened without
// following a symlink (a symlink in place of a directory fails the member,
// one in place of a file or a directory member is replaced); device nodes and
// FIFOs are left out; no set-user-ID or set-group-ID bit survives on a file;
// a hard link points only at a file this archive made; the destination
// itself keeps its owner and mode. Everything made is root's, as with
// --no-same-owner: the API hands the tree to its owner afterwards (/v1/chown).
// It returns tar's exit status: 0, or 2 after errors.
func untar(ctx context.Context, r io.Reader, destFd int, dest string, o untarOpts, out io.Writer) int {
	x := &extractor{ctx: ctx, root: destFd, dest: dest, o: o, out: out, regular: map[string]bool{}, matched: make([]bool, len(o.members))}
	defer x.dirs.closeAll()
	code := x.run(r)
	x.finishDirs()
	for i, m := range o.members {
		if !x.matched[i] {
			x.errorf("%s: Not found in archive", m)
		}
	}
	if x.skipped > 0 {
		fmt.Fprintf(out, "tar: %d device nodes or FIFOs left out\n", x.skipped)
	}
	if x.errs > 0 && code == 0 {
		fmt.Fprintln(out, "tar: Exiting with failure status due to previous errors")
		code = 2
	}
	return code
}

type dirMeta struct {
	rel   string
	mode  uint32
	mtime time.Time
}

type extractor struct {
	ctx  context.Context
	root int
	dest string
	o    untarOpts
	out  io.Writer

	regular map[string]bool // files made by this archive: hard link targets
	created []dirMeta       // directory members, their mode and time set at the end
	matched []bool
	dirs    dirCache
	errs    int
	skipped int
}

func (x *extractor) errorf(format string, a ...any) {
	x.errs++
	if x.errs <= 100 {
		fmt.Fprintf(x.out, "tar: "+format+"\n", a...)
	}
}

func (x *extractor) run(r io.Reader) int {
	src := io.Reader(ctxReader{ctx: x.ctx, r: r})
	if x.o.gzip {
		zr, err := gzip.NewReader(src)
		if err != nil {
			x.errorf("gzip: %v", err)
			return 2
		}
		defer zr.Close() //nolint:errcheck // a reader: nothing to flush
		src = zr
	}
	tr := tar.NewReader(src)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return 0
		}
		if err != nil {
			x.errorf("%v", err)
			return 2
		}
		if err := x.ctx.Err(); err != nil {
			x.errorf("%v", err)
			return 2
		}
		x.member(h, tr)
	}
}

// splitName turns a member name into its components: no leading "/" or "./",
// and nil for a name with ".." in it (GNU tar refuses those as well).
func splitName(name string) ([]string, bool) {
	var parts []string
	for _, p := range strings.Split(name, "/") {
		switch p {
		case "", ".":
		case "..":
			return nil, false
		default:
			parts = append(parts, p)
		}
	}
	return parts, true
}

func (x *extractor) member(h *tar.Header, r io.Reader) {
	parts, ok := splitName(h.Name)
	if !ok {
		x.errorf("%s: Member name contains '..'", h.Name)
		return
	}
	if len(x.o.members) > 0 {
		full, hit := strings.Join(parts, "/"), false
		for i, m := range x.o.members {
			if full == m || strings.HasPrefix(full, m+"/") {
				x.matched[i], hit = true, true
			}
		}
		if !hit {
			return
		}
	}
	if len(parts) <= x.o.strip {
		return // the destination itself, or a name --strip-components drops
	}
	rel := strings.Join(parts[x.o.strip:], "/")
	switch h.Typeflag {
	case tar.TypeDir:
		x.mkdir(rel, h)
	case tar.TypeReg, '\x00', tar.TypeGNUSparse, tar.TypeCont:
		x.file(rel, h, r)
	case tar.TypeSymlink:
		x.symlink(rel, h)
	case tar.TypeLink:
		x.hardlink(rel, h)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		x.skipped++
	case tar.TypeXGlobalHeader:
	default:
		x.errorf("%s: Unknown file type '%c', left out", rel, h.Typeflag)
	}
}

// parent opens the directory a member goes into; missing directories on the
// way are made root's, 0755, as GNU tar makes them.
func (x *extractor) parent(rel string) (int, string, error) {
	dir, base := path.Split(rel)
	dir = strings.TrimSuffix(dir, "/")
	fd, err := x.dirs.open(x.root, x.dest, dir)
	return fd, base, err
}

// clear removes what is in the way of a new entry: anything but a directory,
// which is reported (a directory is never replaced).
func (x *extractor) clear(dir int, base, rel string) bool {
	var st unix.Stat_t
	if err := unix.Fstatat(dir, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return errors.Is(err, unix.ENOENT)
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		x.errorf("%s: Cannot replace a directory", rel)
		return false
	}
	if err := unix.Unlinkat(dir, base, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		x.errorf("%s: Cannot unlink: %v", rel, err)
		return false
	}
	delete(x.regular, rel)
	return true
}

func (x *extractor) mkdir(rel string, h *tar.Header) {
	dir, base, err := x.parent(rel)
	if err != nil {
		x.errorf("%s: %v", rel, err)
		return
	}
	var st unix.Stat_t
	err = unix.Fstatat(dir, base, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case err == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR:
	case err == nil:
		// a file or a symlink where the archive has a directory: GNU tar
		// replaces it, and so does this (the symlink itself, never its target)
		if err := unix.Unlinkat(dir, base, 0); err != nil {
			x.errorf("%s: Cannot unlink: %v", rel, err)
			return
		}
		delete(x.regular, rel)
		fallthrough
	default:
		if err := unix.Mkdirat(dir, base, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			x.errorf("%s: Cannot mkdir: %v", rel, err)
			return
		}
	}
	x.created = append(x.created, dirMeta{rel: rel, mode: uint32(h.Mode) & 0o3777, mtime: h.ModTime})
}

func (x *extractor) file(rel string, h *tar.Header, r io.Reader) {
	dir, base, err := x.parent(rel)
	if err != nil {
		x.errorf("%s: %v", rel, err)
		return
	}
	flags := unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(dir, base, flags, 0o600)
	if errors.Is(err, unix.EEXIST) {
		if !x.clear(dir, base, rel) {
			return
		}
		fd, err = unix.Openat(dir, base, flags, 0o600)
	}
	if err != nil {
		x.errorf("%s: Cannot open: %v", rel, err)
		return
	}
	f := os.NewFile(uintptr(fd), rel)
	_, err = io.Copy(f, r)
	if err == nil {
		// the permission bits only: no set-user-ID, set-group-ID or sticky bit
		err = f.Chmod(os.FileMode(uint32(h.Mode) & 0o777))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		x.errorf("%s: %v", rel, err)
		return
	}
	setMtime(dir, base, h.ModTime)
	x.regular[rel] = true
}

func (x *extractor) symlink(rel string, h *tar.Header) {
	dir, base, err := x.parent(rel)
	if err != nil {
		x.errorf("%s: %v", rel, err)
		return
	}
	err = unix.Symlinkat(h.Linkname, dir, base)
	if errors.Is(err, unix.EEXIST) {
		if !x.clear(dir, base, rel) {
			return
		}
		err = unix.Symlinkat(h.Linkname, dir, base)
	}
	if err != nil {
		x.errorf("%s: Cannot create symlink to %q: %v", rel, h.Linkname, err)
		return
	}
	delete(x.regular, rel)
	setMtime(dir, base, h.ModTime)
}

func (x *extractor) hardlink(rel string, h *tar.Header) {
	parts, ok := splitName(h.Linkname)
	if !ok || len(parts) <= x.o.strip {
		x.errorf("%s: Cannot hard link to %q", rel, h.Linkname)
		return
	}
	target := strings.Join(parts[x.o.strip:], "/")
	if !x.regular[target] {
		x.errorf("%s: Cannot hard link to %s: not a file of this archive", rel, target)
		return
	}
	tdir, tbase, err := x.parent(target)
	if err != nil {
		x.errorf("%s: %v", rel, err)
		return
	}
	dir, base, err := x.parent(rel)
	if err != nil {
		x.errorf("%s: %v", rel, err)
		return
	}
	// flags 0: a symlink found at the target is linked itself, not followed
	err = unix.Linkat(tdir, tbase, dir, base, 0)
	if errors.Is(err, unix.EEXIST) {
		if !x.clear(dir, base, rel) {
			return
		}
		err = unix.Linkat(tdir, tbase, dir, base, 0)
	}
	if err != nil {
		x.errorf("%s: Cannot hard link to %s: %v", rel, target, err)
		return
	}
	x.regular[rel] = true
}

// finishDirs gives the directory members their mode and time, the deepest
// first (a time set on a parent would change again as its children appear).
func (x *extractor) finishDirs() {
	sort.SliceStable(x.created, func(i, j int) bool {
		return strings.Count(x.created[i].rel, "/") > strings.Count(x.created[j].rel, "/")
	})
	for _, d := range x.created {
		fd, err := openDirAt(x.root, x.dest, d.rel, false, 0)
		if err != nil {
			x.errorf("%s: %v", d.rel, err)
			continue
		}
		if err := unix.Fchmod(fd, d.mode); err != nil {
			x.errorf("%s: Cannot change mode: %v", d.rel, err)
		}
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		if dir, base, err := x.parent(d.rel); err == nil {
			setMtime(dir, base, d.mtime)
		}
	}
}

// setMtime sets the modification time of name in dir, not following a
// symlink; a time that does not stick is not worth an error.
func setMtime(dir int, name string, t time.Time) {
	if t.IsZero() {
		return
	}
	ts := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, unix.NsecToTimespec(t.UnixNano())}
	unix.UtimesNanoAt(dir, name, ts, unix.AT_SYMLINK_NOFOLLOW) //nolint:errcheck // see above
}

// dirCache keeps the directories an extraction opened lately: members come
// grouped by directory, and walking from the destination for each of them
// would cost a syscall per level and member.
type dirCache struct {
	fds   map[string]int
	order []string
}

const dirCacheSize = 64

func (c *dirCache) open(root int, base, rel string) (int, error) {
	if rel == "" || rel == "." {
		return root, nil
	}
	if fd, ok := c.fds[rel]; ok {
		return fd, nil
	}
	fd, err := openDirAt(root, base, rel, true, 0o755)
	if err != nil {
		return -1, err
	}
	if c.fds == nil {
		c.fds = map[string]int{}
	}
	if len(c.order) >= dirCacheSize {
		old := c.order[0]
		c.order = c.order[1:]
		unix.Close(c.fds[old]) //nolint:errcheck // a directory opened for reading
		delete(c.fds, old)
	}
	c.fds[rel] = fd
	c.order = append(c.order, rel)
	return fd, nil
}

func (c *dirCache) closeAll() {
	for _, fd := range c.fds {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
	}
	c.fds, c.order = nil, nil
}

// ctxReader stops a copy when the request is gone.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
