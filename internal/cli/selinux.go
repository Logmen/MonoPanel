package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

// selinuxCmd shows and switches the host's SELinux mode. Permissive is
// offered with its price spelled out and a confirmation; enforcing needs none.
func selinuxCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{Use: "selinux [enforcing|permissive]", Short: T("режим SELinux: показать или переключить (с предупреждением)", "SELinux mode: show or switch it (with a warning)"), Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		st, err := cl.SELinux(cmd.Context())
		if err != nil {
			return err
		}
		if len(args) == 0 {
			if g.json {
				return printJSON(st)
			}
			printSELinux(st)
			return nil
		}
		mode := args[0]
		if mode != "enforcing" && mode != "permissive" {
			return errors.New(T("режим: enforcing или permissive", "mode: enforcing or permissive"))
		}
		if !st.Supported {
			return errors.New(T("SELinux отсутствует на этом сервере", "SELinux is not present on this server"))
		}
		if mode == "permissive" && !yes {
			fmt.Fprintln(os.Stderr, T("Внимание. ", "Warning: ")+st.Warning)
			if g.json {
				return errors.New(T("подтвердите флагом --yes", "confirm with --yes"))
			}
			fmt.Fprint(os.Stderr, T("Перевести SELinux в permissive? [y/N] ", "Switch SELinux to permissive? [y/N] "))
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if l := strings.ToLower(strings.TrimSpace(line)); l != "y" && l != "yes" && l != T("д", "y") && l != T("да", "yes") {
				return errors.New(T("отменено", "cancelled"))
			}
		}
		st, err = cl.SetSELinux(cmd.Context(), mode)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(st)
		}
		printSELinux(st)
		return nil
	}}
	c.Flags().BoolVar(&yes, "yes", false, T("не спрашивать подтверждения у permissive", "do not ask for confirmation when switching to permissive"))
	return c
}

func printSELinux(st *apitypes.SELinuxStatus) {
	if !st.Supported {
		fmt.Println(T("SELinux: отсутствует на этом сервере", "SELinux: not present on this server"))
		return
	}
	line := "SELinux: " + st.Mode
	if st.Configured != "" && st.Configured != st.Mode {
		line += fmt.Sprintf(T(" (после перезагрузки: %s)", " (after reboot: %s)"), st.Configured)
	}
	fmt.Println(line)
	if st.Mode != "enforcing" || (st.Configured != "" && st.Configured != "enforcing") {
		fmt.Println(T("Внимание: ", "Warning: ") + st.Warning)
	}
}
