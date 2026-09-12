package cli

import (
	"bufio"
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
	c := &cobra.Command{Use: "selinux [enforcing|permissive]", Short: "режим SELinux: показать или переключить (с предупреждением)", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
			return fmt.Errorf("режим: enforcing или permissive")
		}
		if !st.Supported {
			return fmt.Errorf("SELinux отсутствует на этом сервере")
		}
		if mode == "permissive" && !yes {
			fmt.Fprintln(os.Stderr, "Внимание. "+st.Warning)
			if g.json {
				return fmt.Errorf("подтвердите флагом --yes")
			}
			fmt.Fprint(os.Stderr, "Перевести SELinux в permissive? [y/N] ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if l := strings.ToLower(strings.TrimSpace(line)); l != "y" && l != "yes" && l != "д" && l != "да" {
				return fmt.Errorf("отменено")
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
	c.Flags().BoolVar(&yes, "yes", false, "не спрашивать подтверждения у permissive")
	return c
}

func printSELinux(st *apitypes.SELinuxStatus) {
	if !st.Supported {
		fmt.Println("SELinux: отсутствует на этом сервере")
		return
	}
	line := "SELinux: " + st.Mode
	if st.Configured != "" && st.Configured != st.Mode {
		line += " (после перезагрузки: " + st.Configured + ")"
	}
	fmt.Println(line)
	if st.Mode != "enforcing" || (st.Configured != "" && st.Configured != "enforcing") {
		fmt.Println("Внимание: " + st.Warning)
	}
}
