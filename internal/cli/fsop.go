package cli

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// fsopCmd is executed by the agent through `monopanel helper` after the
// privileges were dropped to the client's uid: every operation runs with the
// client's own permissions, so symlink tricks cannot escalate.
func fsopCmd() *cobra.Command {
	c := &cobra.Command{Use: "fsop", Hidden: true, Short: "файловые операции от имени клиента (вызывается агентом)"}
	root := func() string {
		if h := os.Getenv("HOME"); h != "" {
			return filepath.Clean(h)
		}
		return "/"
	}
	// Paths are relative to the home directory (like the SFTP chroot view):
	// "/data/www" and "data/www" both mean <home>/data/www.
	inside := func(p string) (string, error) {
		clean := filepath.Clean("/" + p)
		if strings.HasPrefix(clean, root()+"/") || clean == root() {
			return clean, nil
		}
		full := filepath.Join(root(), clean)
		if full != root() && !strings.HasPrefix(full, root()+"/") {
			return "", fmt.Errorf("path %s is outside the home directory", p)
		}
		return full, nil
	}
	type entry struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		Size    int64  `json:"size"`
		Mode    string `json:"mode"`
		ModTime string `json:"mtime"`
		Target  string `json:"target,omitempty"`
	}
	list := &cobra.Command{Use: "list <path>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := inside(args[0])
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		out := make([]entry, 0, len(entries))
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			it := entry{Name: e.Name(), Size: info.Size(), Mode: fmt.Sprintf("%04o", info.Mode().Perm()), ModTime: info.ModTime().UTC().Format(time.RFC3339), Type: "file"}
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				it.Type = "symlink"
				it.Target, _ = os.Readlink(filepath.Join(p, e.Name()))
			case info.IsDir():
				it.Type = "dir"
			}
			out = append(out, it)
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}}
	mkdir := &cobra.Command{Use: "mkdir <path>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := inside(args[0])
		if err != nil {
			return err
		}
		return os.MkdirAll(p, 0o755)
	}}
	rm := &cobra.Command{Use: "rm <path>...", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		for _, a := range args {
			p, err := inside(a)
			if err != nil {
				return err
			}
			if p == root() || p == filepath.Join(root(), "data") {
				return errors.New("refusing to remove " + p)
			}
			if err := os.RemoveAll(p); err != nil {
				return err
			}
		}
		return nil
	}}
	mv := &cobra.Command{Use: "mv <src> <dst>", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		src, err := inside(args[0])
		if err != nil {
			return err
		}
		dst, err := inside(args[1])
		if err != nil {
			return err
		}
		if st, err := os.Stat(dst); err == nil && st.IsDir() {
			dst = filepath.Join(dst, filepath.Base(src))
		}
		return os.Rename(src, dst)
	}}
	chmod := &cobra.Command{Use: "chmod <mode> <path>", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		mode, err := strconv.ParseUint(args[0], 8, 32)
		if err != nil || mode > 0o7777 {
			return errors.New("mode must be octal like 644")
		}
		p, err := inside(args[1])
		if err != nil {
			return err
		}
		return os.Chmod(p, os.FileMode(mode))
	}}
	read := &cobra.Command{Use: "read <path>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := inside(args[0])
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(os.Stdout, io.LimitReader(f, 512<<20))
		return err
	}}
	write := &cobra.Command{Use: "write <path>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := inside(args[0])
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".upload-*")
		if err != nil {
			return err
		}
		if _, err := io.Copy(tmp, os.Stdin); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return err
		}
		tmp.Chmod(0o644) //nolint:errcheck // best effort; the caller reports the real failure
		tmp.Close()
		return os.Rename(tmp.Name(), p)
	}}
	var strip int
	extract := &cobra.Command{Use: "extract <archive> <dest>", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		arc, err := inside(args[0])
		if err != nil {
			return err
		}
		dest, err := inside(args[1])
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		n, err := extractArchive(arc, dest, strip)
		if err != nil {
			return err
		}
		fmt.Println(n)
		return nil
	}}
	extract.Flags().IntVar(&strip, "strip", 0, "drop this many leading path components (a distribution's top-level folder)")
	// run executes a CMS's own command-line installer as the client: only a
	// PHP interpreter, inside the home directory, with the client's environment.
	var cwd string
	run := &cobra.Command{Use: "run --cwd <dir> -- <php> [args...]", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := inside(cwd)
		if err != nil {
			return err
		}
		if !phpBinaryAllowed(root(), args[0]) {
			return fmt.Errorf("run: only a PHP interpreter may be run here, not %s", args[0])
		}
		c := exec.CommandContext(cmd.Context(), args[0], args[1:]...)
		c.Dir, c.Env, c.Stdin, c.Stdout, c.Stderr = dir, os.Environ(), os.Stdin, os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				os.Exit(ee.ExitCode())
			}
			return err
		}
		return nil
	}}
	run.Flags().StringVar(&cwd, "cwd", ".", "working directory, relative to the home")
	touch := &cobra.Command{Use: "touch <path>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := inside(args[0])
		if err != nil {
			return err
		}
		// O_EXCL: создание пустого файла не должно затирать существующий.
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			if os.IsExist(err) {
				return fmt.Errorf("%s уже существует", filepath.Base(p))
			}
			return err
		}
		return f.Close()
	}}

	size := &cobra.Command{Use: "size <path>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := inside(args[0])
		if err != nil {
			return err
		}
		var total int64
		filepath.WalkDir(p, func(_ string, d os.DirEntry, err error) error { //nolint:errcheck // best effort; the caller reports the real failure
			if err == nil && !d.IsDir() {
				if info, err := d.Info(); err == nil {
					total += info.Size()
				}
			}
			return nil
		})
		fmt.Println(total)
		return nil
	}}
	c.AddCommand(list, mkdir, touch, rm, mv, chmod, read, write, extract, run, size)
	return c
}

func safeJoin(dest, name string) (string, error) {
	p := filepath.Join(dest, name)
	if !strings.HasPrefix(p, filepath.Clean(dest)+"/") && p != filepath.Clean(dest) {
		return "", fmt.Errorf("archive entry escapes destination: %s", name)
	}
	return p, nil
}

// phpBinaryAllowed says whether fsop run may execute bin: the site's own
// data/bin/php symlink, or a PHP CLI of the distribution / Remi.
func phpBinaryAllowed(home, bin string) bool {
	if bin == "php" || bin == filepath.Join(home, "data", "bin", "php") {
		return true
	}
	return phpBinRe.MatchString(bin)
}

var phpBinRe = regexp.MustCompile(`^(/usr/bin/php[0-9.]*|/opt/remi/php[0-9]+/root/usr/bin/php)$`)

// stripComponents drops the first n path components of an archive entry; an
// entry that has no more than n components (the top-level folder itself)
// yields "" and is skipped.
func stripComponents(name string, n int) string {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if len(parts) <= n {
		return ""
	}
	return strings.Join(parts[n:], "/")
}

func extractArchive(arc, dest string, strip int) (int, error) {
	n := 0
	switch {
	case strings.HasSuffix(arc, ".zip"):
		r, err := zip.OpenReader(arc)
		if err != nil {
			return 0, err
		}
		defer r.Close() //nolint:errcheck // cleanup
		for _, f := range r.File {
			name := stripComponents(f.Name, strip)
			if name == "" {
				continue
			}
			p, err := safeJoin(dest, name)
			if err != nil {
				return n, err
			}
			if f.FileInfo().IsDir() {
				os.MkdirAll(p, 0o755)
				continue
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			rc, err := f.Open()
			if err != nil {
				return n, err
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode().Perm()|0o600)
			if err != nil {
				rc.Close()
				return n, err
			}
			_, err = io.Copy(out, rc)
			out.Close()
			rc.Close()
			if err != nil {
				return n, err
			}
			n++
		}
	case strings.HasSuffix(arc, ".tar.gz") || strings.HasSuffix(arc, ".tgz") || strings.HasSuffix(arc, ".tar"):
		f, err := os.Open(arc)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		var rd io.Reader = f
		if !strings.HasSuffix(arc, ".tar") {
			gz, err := gzip.NewReader(f)
			if err != nil {
				return 0, err
			}
			defer gz.Close() //nolint:errcheck // cleanup
			rd = gz
		}
		tr := tar.NewReader(rd)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return n, err
			}
			name := stripComponents(h.Name, strip)
			if name == "" {
				continue
			}
			p, err := safeJoin(dest, name)
			if err != nil {
				return n, err
			}
			switch h.Typeflag {
			case tar.TypeDir:
				os.MkdirAll(p, 0o755)
			case tar.TypeReg:
				os.MkdirAll(filepath.Dir(p), 0o755)
				out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode).Perm()|0o600)
				if err != nil {
					return n, err
				}
				_, err = io.Copy(out, tr)
				out.Close()
				if err != nil {
					return n, err
				}
				n++
			}
		}
	default:
		return 0, errors.New("unsupported archive (zip, tar, tar.gz)")
	}
	return n, nil
}
