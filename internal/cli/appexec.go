package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// appExecCmd starts an app service's command with the variables of the
// account's own environment file. systemd would read an EnvironmentFile= as
// root before it drops to the account, following any symlink the account put
// there — to /etc/shadow, or to another account's .env — and its complaints
// about the lines quote them in the account's journal. The unit runs this as
// the account instead (User= already applied), so the file is read with the
// account's own permissions, and then execs the command in place.
func appExecCmd() *cobra.Command {
	var envFile string
	c := &cobra.Command{
		Use:    "app-exec --env-file <file> -- <command> [args…]",
		Short:  T("служебная: запуск app-сервиса с его env-файлом", "internal: start an app service with its env file"),
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			env := os.Environ()
			if envFile != "" {
				vars, err := readEnvFile(envFile)
				if err != nil {
					return err
				}
				env = append(env, vars...) // later entries win, as the file did over Environment=
			}
			if !strings.HasPrefix(args[0], "/") {
				return fmt.Errorf("the command must be an absolute path: %s", args[0])
			}
			return syscall.Exec(args[0], args, env)
		},
	}
	c.Flags().StringVar(&envFile, "env-file", "", T("файл переменных KEY=VALUE", "file of KEY=VALUE variables"))
	return c
}

// readEnvFile reads an environment file the way systemd reads
// EnvironmentFile=: KEY=VALUE lines, # and ; comments, values in single
// quotes taken as they are, in double quotes with \" \\ \$ \` escapes;
// a line without "=" is skipped.
func readEnvFile(p string) ([]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			continue
		}
		v = strings.TrimSpace(v)
		switch {
		case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
			v = v[1 : len(v)-1]
		case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
			v = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`, "\\`", "`").Replace(v[1 : len(v)-1])
		}
		out = append(out, k+"="+v)
	}
	return out, sc.Err()
}
