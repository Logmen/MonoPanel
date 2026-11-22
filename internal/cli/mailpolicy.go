package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"monopanel/internal/buildinfo"
	"monopanel/internal/mailpolicy"
	"monopanel/internal/osprofile"
)

// mailPolicyCmd runs the mail policy (monopanel-mailpolicy.service) as its
// own system user: it reads no config.yaml — that one is root:monopanel and
// none of its business — only the policy and pickup's header table. It
// listens on the socket systemd passes it (the socket unit of the OS
// family), never on one it binds itself but with --listen.
func mailPolicyCmd() *cobra.Command {
	var listen, policy string
	var probe, exists bool
	c := &cobra.Command{
		Use:    "mail-policy",
		Short:  T("почтовая политика postfix: ни один аккаунт не шлёт от доменов другого (monopanel-mailpolicy.service)", "postfix's mail policy: no account sends as another account's domain (monopanel-mailpolicy.service)"),
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if exists {
				// The package scripts ask whether the binary that stays
				// has the policy (postremove.sh).
				return nil
			}
			l, err := mailPolicyListener(listen)
			if err != nil {
				return err
			}
			if probe {
				if err := mailpolicy.Probe(cmd.Context(), l, 3*time.Second); err != nil {
					return fmt.Errorf("%s %s: %w", l.Network, l.Address, err)
				}
				fmt.Println("ok")
				return nil
			}
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			var ln net.Listener
			if listen != "" {
				ln, err = mailpolicy.Listen(l)
			} else {
				ln, err = mailpolicy.FromSystemd()
			}
			if err != nil {
				return err
			}
			srv := &mailpolicy.Server{Loader: mailpolicy.NewLoader(policy, mailpolicy.HeaderChecksFile, logger), Log: logger}
			logger.Info("mail policy listening", "network", ln.Addr().Network(), "address", ln.Addr().String(), "version", buildinfo.Version)
			srv.Loader.Current()
			return srv.Serve(cmd.Context(), ln)
		},
	}
	c.Flags().StringVar(&listen, "listen", "", T("слушать самой: unix:<путь> или tcp:<адрес>:<порт> (по умолчанию — сокет, который передаёт systemd; для --probe — адрес ОС: сокет на Debian, 127.0.0.1:8890 на EL)", "listen by itself: unix:<path> or tcp:<address>:<port> (default: the socket systemd passes; for --probe the OS's address: the socket on Debian, 127.0.0.1:8890 on EL)"))
	c.Flags().StringVar(&policy, "policy", mailpolicy.PolicyFile, T("файл политики", "the policy file"))
	c.Flags().BoolVar(&probe, "probe", false, T("только проверить, что политика отвечает по протоколу milter", "only check that the policy answers the milter protocol"))
	c.Flags().BoolVar(&exists, "exists", false, T("ничего не делать: успех значит, что в этом бинарнике есть политика (для скриптов пакета)", "do nothing: success says this binary has the policy (for the package scripts)"))
	_ = c.Flags().MarkHidden("exists")
	return c
}

// mailPolicyListener is where the policy listens: --listen, or what the OS
// family takes (mailpolicy.ListenerFor).
func mailPolicyListener(spec string) (mailpolicy.Listener, error) {
	if spec == "" {
		profile, _, err := osprofile.DetectOrGeneric()
		if err != nil {
			return mailpolicy.Listener{}, err
		}
		return mailpolicy.ListenerFor(profile.Family() == osprofile.FamilyDebian), nil
	}
	network, address, ok := strings.Cut(spec, ":")
	if !ok || address == "" || (network != "unix" && network != "tcp") {
		return mailpolicy.Listener{}, fmt.Errorf("--listen %q: unix:<path> or tcp:<address>:<port>", spec)
	}
	postfix := "unix:" + address
	if network == "tcp" {
		postfix = "inet:" + address
	}
	return mailpolicy.Listener{Network: network, Address: address, Postfix: postfix}, nil
}

// mailGuardCmd loads the mail guard of EL (monopanel-mailguard.service, as
// root): the nftables table that lets only root and postfix connect to the
// policy's loopback port and to opendkim's (mailpolicy.GuardRuleset).
// --remove takes it away.
func mailGuardCmd() *cobra.Command {
	var remove bool
	c := &cobra.Command{
		Use:    "mail-guard",
		Short:  T("защита портов milter на EL: к политике и opendkim подключаются только root и postfix (monopanel-mailguard.service)", "the milter ports' guard of EL: only root and postfix connect to the policy and opendkim (monopanel-mailguard.service)"),
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			nft, err := exec.LookPath("nft")
			if err != nil {
				nft = "/usr/sbin/nft"
			}
			if remove {
				out, err := exec.CommandContext(cmd.Context(), nft, "delete", "table", "inet", mailpolicy.GuardTable).CombinedOutput()
				if err != nil && !strings.Contains(string(out), "No such file or directory") {
					return fmt.Errorf("nft delete table inet %s: %w: %s", mailpolicy.GuardTable, err, strings.TrimSpace(string(out)))
				}
				return nil
			}
			pf, err := user.Lookup("postfix")
			if err != nil {
				return fmt.Errorf("postfix's user: %w", err)
			}
			uid, err := strconv.Atoi(pf.Uid)
			if err != nil || uid <= 0 {
				return errors.New("postfix's user has no uid of its own: " + pf.Uid)
			}
			run := exec.CommandContext(cmd.Context(), nft, "-f", "-")
			run.Stdin = strings.NewReader(mailpolicy.GuardRuleset(uid))
			if out, err := run.CombinedOutput(); err != nil {
				return fmt.Errorf("nft -f: %w: %s", err, strings.TrimSpace(string(out)))
			}
			fmt.Printf("mail guard: only root and postfix (uid %d) connect to %s and 127.0.0.1:%d\n", uid, mailpolicy.TCPAddress, mailpolicy.DKIMPort)
			return nil
		},
	}
	c.Flags().BoolVar(&remove, "remove", false, T("снять защиту", "take the guard away"))
	return c
}
