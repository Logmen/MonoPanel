package api

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// The files of an account arrive as a tar the source made, and tar -x runs as
// root here (it has to: the home belongs to root for an SFTP-only account, and
// the ownership is fixed afterwards). GNU tar already refuses names with ..
// and defuses symlinks that lead out, but as root it also creates what an
// account's files never are: a device node — a copy of /dev/mem or of the
// disk under an innocent name — or a FIFO, and it keeps setuid bits on files
// owned by root until the chown. The ownership fix would then hand the device
// to the account. migrateTar re-writes the stream member by member before it
// reaches tar and keeps only regular files, directories, symlinks and hard
// links inside the tree, with plain permission bits.
//
// The mail store needs more: every Maildir there belongs to vmail, and dovecot
// follows a symlink wherever it leads, so a mailbox that is a link to another
// domain's Maildir would hand that mail to whoever knows the new mailbox's
// password. GNU tar only delays such links, it still makes them. There
// (confined) a symlink must stay below its own directory: a relative target
// without "..", like the .dovecot.sieve → sieve/… link managesieve keeps.

// migrateTarHoleBudget is how many zero bytes the holes of sparse files may
// expand to in one archive. Go's tar writer cannot write a sparse member, so a
// hole arrives as zeros on the disk: without a limit a few kilobytes of the
// source's archive could claim terabytes here. An account that really has
// that much in holes (a disk image) has its files refused with the reason.
// A variable, so a test need not write gigabytes.
var migrateTarHoleBudget int64 = 4 << 30

// migrateTarStats says what the filter left out.
type migrateTarStats struct {
	dropped int
	first   string // the first member left out, for the job log
	// confined keeps symlinks below their own directory (the mail store).
	confined bool
	// holes counts the zeros sparse members expanded to so far.
	holes int64
	// err is why the filter stopped early; final once done is closed.
	err error
}

// migrateTar starts filtering src; the returned reader is the clean stream.
// done is closed once the filter is finished with src and the stats are final.
func migrateTar(src io.Reader, confined bool) (out io.ReadCloser, stats *migrateTarStats, done <-chan struct{}) {
	pr, pw := io.Pipe()
	st := &migrateTarStats{confined: confined}
	fin := make(chan struct{})
	go func() {
		defer close(fin)
		st.err = st.copy(src, pw)
		pw.CloseWithError(st.err) //nolint:errcheck // a pipe's CloseWithError never fails
	}()
	return pr, st, fin
}

func (st *migrateTarStats) drop(name, why string) {
	if st.dropped == 0 {
		st.first = shown(name) + " (" + why + ")"
	}
	st.dropped++
}

// countingReader counts what the tar reader takes from the source: the bytes
// a member's data really occupies in the archive.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (st *migrateTarStats) copy(src io.Reader, dst io.Writer) error {
	in := &countingReader{r: src}
	tr, tw := tar.NewReader(in), tar.NewWriter(dst)
	links := map[string]bool{} // symlinks seen so far: nothing goes through them
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return tw.Close()
		}
		if errors.Is(err, tar.ErrInsecurePath) && h != nil {
			// Go says so itself with GODEBUG=tarinsecurepath=0.
			st.drop(h.Name, "a path outside the tree")
			continue
		}
		if err != nil {
			return fmt.Errorf("the source's archive: %w", err)
		}
		name, ok := tarMemberPath(h.Name)
		if !ok {
			st.drop(h.Name, "a path outside the tree")
			continue
		}
		if under := tarUnderLink(name, links); under != "" {
			st.drop(h.Name, "inside the symlink "+under)
			continue
		}
		out := &tar.Header{Name: h.Name, ModTime: h.ModTime, Mode: h.Mode & 0o777}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeGNUSparse:
			// A sparse file comes expanded: its holes arrive as zeros.
			out.Typeflag, out.Size = tar.TypeReg, h.Size
		case tar.TypeDir:
			out.Typeflag = tar.TypeDir
		case tar.TypeSymlink:
			if st.confined && !tarConfinedLink(h.Linkname) {
				st.drop(h.Name, "a symlink to "+shown(h.Linkname)+", outside its own directory")
				continue
			}
			// tar -x runs as root, and the files arrive in more than one
			// pass (the copy, then what changed meanwhile): a link left by
			// one pass that leads out of the tree (data/x -> /etc) would
			// have the next one write /etc/cron.d/… through it. Only links
			// that stay inside the tree are made.
			if !st.confined && !tarTreeLink(name, h.Linkname) {
				st.drop(h.Name, "a symlink to "+shown(h.Linkname)+", outside the tree")
				links[name] = true // what was meant to go through it stays out too
				continue
			}
			out.Typeflag, out.Linkname = tar.TypeSymlink, h.Linkname
			links[name] = true
		case tar.TypeLink:
			target, ok := tarMemberPath(h.Linkname)
			if !ok || tarUnderLink(target, links) != "" {
				st.drop(h.Name, "a hard link to "+shown(h.Linkname))
				continue
			}
			out.Typeflag, out.Linkname = tar.TypeLink, h.Linkname
		default:
			st.drop(h.Name, fmt.Sprintf("member type %q is not a file, a directory or a link", h.Typeflag))
			continue
		}
		if err := tw.WriteHeader(out); err != nil {
			return err
		}
		if out.Typeflag == tar.TypeReg {
			if err := st.copyData(tw, tr, in, h.Name); err != nil {
				return err
			}
		}
	}
}

// copyData copies the data of one member. What it writes beyond what it read
// from the archive is a sparse file's holes, which count against the budget.
func (st *migrateTarStats) copyData(tw io.Writer, tr io.Reader, in *countingReader, name string) error {
	start := in.n
	var written int64
	buf := make([]byte, 64<<10)
	for {
		n, err := tr.Read(buf)
		if n > 0 {
			if _, werr := tw.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			// Written beyond what the archive held is holes coming out as zeros.
			if st.holes+written-(in.n-start) > migrateTarHoleBudget {
				return fmt.Errorf("the source's archive: its sparse files would arrive as more than %d MiB of zeros (%s); move such files by hand", migrateTarHoleBudget>>20, shown(name))
			}
		}
		if errors.Is(err, io.EOF) {
			st.holes += written - (in.n - start)
			return nil
		}
		if err != nil {
			return fmt.Errorf("the source's archive: %w", err)
		}
	}
}

// tarMemberPath cleans a member name; one that is absolute or climbs out of
// the tree is not a member of it.
func tarMemberPath(name string) (string, bool) {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
		return "", false
	}
	p := path.Clean(name)
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", false
		}
	}
	return p, true
}

// tarConfinedLink says whether a symlink target stays below the link's own
// directory: relative and without "..". A chain of such links stays there
// too.
func tarConfinedLink(target string) bool {
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsRune(target, 0) {
		return false
	}
	for _, part := range strings.Split(target, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// tarTreeLink says whether a symlink at name (a cleaned member path) leads to
// a place inside the tree: a relative target that does not climb above the
// tree's top. An absolute one names a path of the source's file system.
func tarTreeLink(name, target string) bool {
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsRune(target, 0) {
		return false
	}
	p := path.Clean(path.Join(path.Dir(name), target))
	return p != ".." && !strings.HasPrefix(p, "../")
}

// tarUnderLink names the symlink a path would be extracted through, if any.
func tarUnderLink(p string, links map[string]bool) string {
	for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if links[dir] {
			return dir
		}
	}
	return ""
}
