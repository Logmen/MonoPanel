package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func filesCmd() *cobra.Command {
	c := &cobra.Command{Use: "files", Short: "файлы клиентов (выполняются от имени клиента через helper)"}
	var user string
	c.PersistentFlags().StringVar(&user, "user", "", "владелец файлов (обязателен для администратора)")
	ls := &cobra.Command{Use: "ls [path]", Short: "список каталога (пути относительно домашнего каталога)", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
			rows = append(rows, []string{e.Mode, humanBytes(uint64(e.Size)), e.ModTime[:16], name})
		}
		fmt.Fprintf(os.Stderr, "%s:%s (%s)\n", l.User, l.Path, l.Home)
		table([]string{"MODE", "SIZE", "MTIME", "NAME"}, rows)
		return nil
	}}
	op := func(name, short string, argc int, build func(args []string) apitypes.FileOpRequest) *cobra.Command {
		return &cobra.Command{Use: name, Short: short, Args: cobra.ExactArgs(argc), RunE: func(cmd *cobra.Command, args []string) error {
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
			if res.Value != 0 {
				fmt.Println(res.Value)
			}
			return nil
		}}
	}
	mkdir := op("mkdir <path>", "создать каталог", 1, func(a []string) apitypes.FileOpRequest { return apitypes.FileOpRequest{Op: "mkdir", Path: a[0]} })
	rm := op("rm <path>", "удалить файл или каталог", 1, func(a []string) apitypes.FileOpRequest { return apitypes.FileOpRequest{Op: "rm", Path: a[0]} })
	mv := op("mv <src> <dst>", "переместить/переименовать", 2, func(a []string) apitypes.FileOpRequest {
		return apitypes.FileOpRequest{Op: "mv", Path: a[0], Dest: a[1]}
	})
	chmod := op("chmod <mode> <path>", "права, например 644", 2, func(a []string) apitypes.FileOpRequest {
		return apitypes.FileOpRequest{Op: "chmod", Mode: a[0], Path: a[1]}
	})
	extract := op("extract <archive> <dest>", "распаковать zip / tar.gz", 2, func(a []string) apitypes.FileOpRequest {
		return apitypes.FileOpRequest{Op: "extract", Path: a[0], Dest: a[1]}
	})
	size := op("size <path>", "размер каталога в байтах", 1, func(a []string) apitypes.FileOpRequest { return apitypes.FileOpRequest{Op: "size", Path: a[0]} })
	get := &cobra.Command{Use: "get <remote> [local]", Short: "скачать файл (local по умолчанию — имя файла, '-' = stdout)", Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
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
		return os.WriteFile(local, data, 0o644)
	}}
	put := &cobra.Command{Use: "put <local> <remote>", Short: "загрузить файл", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(args[0])
		if err != nil {
			return err
		}
		res, err := cl.FileUpload(cmd.Context(), user, args[1], data)
		if err != nil {
			return err
		}
		fmt.Printf("загружено %s байт → %s\n", strconv.FormatInt(res.Value, 10), args[1])
		return nil
	}}
	c.AddCommand(ls, mkdir, rm, mv, chmod, extract, size, get, put)
	return c
}
