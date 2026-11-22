package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func filesCmd() *cobra.Command {
	c := &cobra.Command{Use: "files", Short: T("файлы клиентов (выполняются от имени клиента через helper)", "clients' files (operations run as the client through the helper)")}
	var user string
	c.PersistentFlags().StringVar(&user, "user", "", T("владелец файлов (обязателен для администратора)", "owner of the files (required for an administrator)"))
	ls := &cobra.Command{Use: "ls [path]", Short: T("список каталога (пути относительно домашнего каталога)", "list a directory (paths are relative to the home directory)"), Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		p := "/"
		if len(args) == 1 {
			p = args[0]
		}
		l, err := cl.FilesList(cmd.Context(), user, p)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(l)
		}
		rows := make([][]string, 0, len(l.Entries))
		for _, e := range l.Entries {
			name := e.Name
			if e.Type == "dir" {
				name += "/"
			} else if e.Type == "symlink" {
				name += " -> " + e.Target
			}
			mtime := e.ModTime
			if len(mtime) > 16 {
				mtime = mtime[:16]
			}
			rows = append(rows, []string{e.Mode, humanBytes(uint64(e.Size)), mtime, name})
		}
		fmt.Fprintf(os.Stderr, "%s:%s (%s)\n", l.User, l.Path, l.Home)
		table([]string{"MODE", "SIZE", "MTIME", "NAME"}, rows)
		return nil
	}}
	op := func(name, short string, nargs cobra.PositionalArgs, build func(args []string) apitypes.FileOpRequest) *cobra.Command {
		return &cobra.Command{Use: name, Short: short, Args: nargs, RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			req := build(args)
			req.User = user
			res, err := cl.FilesOp(cmd.Context(), req)
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(res)
			}
			if res.Value != 0 || req.Op == "size" {
				fmt.Println(res.Value)
			}
			return nil
		}}
	}
	mkdir := op("mkdir <path>", T("создать каталог", "create a directory"), cobra.ExactArgs(1), func(a []string) apitypes.FileOpRequest { return apitypes.FileOpRequest{Op: "mkdir", Path: a[0]} })
	var force bool
	touch := op("touch <path>", T("создать пустой файл (если файл уже есть — ошибка; --force его опустошает)", "create an empty file (an existing one is an error; --force empties it)"), cobra.ExactArgs(1), func(a []string) apitypes.FileOpRequest {
		return apitypes.FileOpRequest{Op: "touch", Path: a[0], Force: force}
	})
	touch.Flags().BoolVar(&force, "force", false, T("опустошить существующий файл", "empty an existing file"))
	rm := op("rm <path>...", T("удалить файлы или каталоги", "delete files or directories"), cobra.MinimumNArgs(1), func(a []string) apitypes.FileOpRequest {
		return apitypes.FileOpRequest{Op: "rm", Path: a[0], Paths: a[1:]}
	})
	mv := op("mv <src> <dst>", T("переместить/переименовать", "move or rename"), cobra.ExactArgs(2), func(a []string) apitypes.FileOpRequest {
		return apitypes.FileOpRequest{Op: "mv", Path: a[0], Dest: a[1]}
	})
	chmod := op("chmod <mode> <path>", T("права, например 644", "change permissions, for example 644"), cobra.ExactArgs(2), func(a []string) apitypes.FileOpRequest {
		return apitypes.FileOpRequest{Op: "chmod", Mode: a[0], Path: a[1]}
	})
	extract := op("extract <archive> <dest>", T("распаковать zip / tar.gz", "extract a zip / tar.gz"), cobra.ExactArgs(2), func(a []string) apitypes.FileOpRequest {
		return apitypes.FileOpRequest{Op: "extract", Path: a[0], Dest: a[1]}
	})
	size := op("size <path>", T("размер каталога в байтах", "directory size in bytes"), cobra.ExactArgs(1), func(a []string) apitypes.FileOpRequest { return apitypes.FileOpRequest{Op: "size", Path: a[0]} })
	var getForce bool
	get := &cobra.Command{Use: "get <remote> [local]", Short: T("скачать файл (local по умолчанию — имя файла, '-' = stdout)", "download a file (local defaults to the file name, '-' = stdout)"), Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		data, err := cl.FileDownload(cmd.Context(), user, args[0])
		if err != nil {
			return err
		}
		local := args[0][strings.LastIndex(args[0], "/")+1:]
		if len(args) == 2 {
			local = args[1]
		}
		if local == "-" {
			_, err = os.Stdout.Write(data)
			return err
		}
		// The name may be the account's choice (.bashrc, .ssh/…): an existing
		// local file is replaced only when asked.
		flag := os.O_WRONLY | os.O_CREATE | os.O_EXCL
		if getForce {
			flag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		}
		f, err := os.OpenFile(local, flag, 0o644)
		if errors.Is(err, fs.ErrExist) {
			return &exitError{code: 2, msg: fmt.Sprintf(T("%s уже есть: укажите другое имя или --force", "%s already exists: name another file or use --force"), local)}
		}
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}}
	get.Flags().BoolVar(&getForce, "force", false, T("перезаписать существующий локальный файл", "overwrite an existing local file"))
	put := &cobra.Command{Use: "put <local> <remote>", Short: T("загрузить файл ('-' = stdin; пустой локальный файл опустошает удалённый)", "upload a file ('-' = stdin; an empty local file empties the remote one)"), Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		var data []byte
		if args[0] == "-" {
			// A closed stdin (the TUI, a script) must not empty the file.
			if data, err = io.ReadAll(stdin); err == nil && len(data) == 0 {
				return &exitError{code: 2, msg: T("на stdin пусто; опустошить файл: mp files touch --force", "nothing on stdin; to empty a file: mp files touch --force")}
			}
		} else {
			data, err = os.ReadFile(args[0])
		}
		if err != nil {
			return err
		}
		var res *apitypes.FileOpResult
		if len(data) == 0 {
			// The API takes no empty upload; an empty file is a forced touch,
			// as the web editor saves an emptied file.
			res, err = cl.FilesOp(cmd.Context(), apitypes.FileOpRequest{User: user, Op: "touch", Path: args[1], Force: true})
		} else {
			res, err = cl.FileUpload(cmd.Context(), user, args[1], data)
		}
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Printf(T("загружено %s байт → %s\n", "uploaded %s bytes → %s\n"), strconv.Itoa(len(data)), args[1])
		return nil
	}}
	c.AddCommand(ls, mkdir, touch, rm, mv, chmod, extract, size, get, put)
	return c
}
