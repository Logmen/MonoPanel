package agent

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"monopanel/internal/osprofile"
)

// sftpGroup holds the SFTP-only clients (internal/api/ops_users.go uses the
// same name); the sshd drop-in chroots its members.
const sftpGroup = "monopanel-sftp"

// mailStore is the virtual mail store, vmail's (internal/api/ops_mail.go:
// mailBase and vmailUser).
const (
	mailStore = "/var/mail/monopanel"
	mailUser  = "vmail"
)

// privilegedGroups are groups whose members are root, or as good as root:
// sudo rights, the docker or lxd socket, raw disks, /etc/shadow, the journal,
// kernel memory, the TLS and SSH host keys (ssl-cert, EL's ssh_keys), the
// panel's own service group (the agent socket, the panel's data). No panel account is in one, and the agent puts no account
// into one.
var privilegedGroups = map[string]bool{
	"root": true, "sudo": true, "wheel": true, "admin": true, "adm": true, "staff": true,
	"docker": true, "lxd": true, "lxc": true, "incus": true, "incus-admin": true,
	"libvirt": true, "libvirtd": true, "kvm": true, "shadow": true, "disk": true,
	"systemd-journal": true, "sys": true, "kmem": true, "bin": true, "daemon": true,
	"operator": true, "ssl-cert": true, "ssh_keys": true,
}

// The account database, replaced by tests: the panel's accounts are real
// unix users, and tests cannot make those.
var (
	lookupAccount = user.Lookup
	accountGroups = func(u *user.User) ([]string, error) {
		ids, err := u.GroupIds()
		if err != nil {
			return nil, err
		}
		ids = append(ids, u.Gid)
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			if id == "0" {
				out = append(out, "root")
				continue
			}
			if g, err := user.LookupGroupId(id); err == nil {
				out = append(out, g.Name)
			}
		}
		return out, nil
	}
	loginUIDMin = uidMin
)

func (s *Server) privilegedGroup(name string) bool {
	return privilegedGroups[name] || name == s.cfg.ServiceGroup
}

// errNoAccount is a login without a unix account.
var errNoAccount = errors.New("no such account")

// panelAccount returns the unix account of one of the panel's clients: an
// ordinary account (uid ≥ UID_MIN) with its home below the web root, not
// root, not the panel's service user, and in no privileged group. Every RPC
// that acts for an account or as one — its password, its shadow hash, its
// removal, its crontab, runas — asks this, so the server's own users (an
// administrator in sudo or wheel, a system daemon) stay out of reach.
func (s *Server) panelAccount(login string) (*user.User, error) {
	if !nameRe.MatchString(login) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid login"}
	}
	u, err := lookupAccount(login)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return nil, errNoAccount
		}
		return nil, err
	}
	if err := s.checkPanelAccount(u); err != nil {
		return nil, err
	}
	return u, nil
}

// checkPanelAccount explains why u is not a panel account, if it is not.
func (s *Server) checkPanelAccount(u *user.User) error {
	refuse := func(why string) error { return notAllowed("the account "+u.Username, why) }
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return refuse("its uid " + u.Uid + " is not a number")
	}
	gid, _ := strconv.Atoi(u.Gid)
	switch {
	case u.Username == "root" || uid == 0 || gid == 0:
		return refuse("it is root's")
	case u.Username == s.cfg.ServiceUser:
		return refuse("it is the panel's own service user")
	case uid < loginUIDMin():
		return refuse(fmt.Sprintf("uid %d is a system account's (below UID_MIN %d)", uid, loginUIDMin()))
	case !below(filepath.Clean(u.HomeDir), s.cfg.WWWRoot):
		return refuse(fmt.Sprintf("its home %s is not below %s", u.HomeDir, s.cfg.WWWRoot))
	}
	groups, err := accountGroups(u)
	if err != nil {
		return refuse("its groups cannot be read: " + err.Error())
	}
	for _, g := range groups {
		if s.privilegedGroup(g) {
			return refuse("it is a member of the privileged group " + g)
		}
	}
	return nil
}

// systemLogins are the system users the panel adds to groups or creates:
// nginx's user joins the web group, postfix joins opendkim (the milter
// socket), vmail owns the mail store, Sphinx 3 gets a user of its own; the
// distribution's Apache account only leaves the web group
// (checkApacheUserLeaves).
func (s *Server) systemLogins() map[string]bool {
	web := s.profile.Web()
	out := map[string]bool{mailUser: true, "postfix": true}
	for _, l := range []string{web.NginxUser, web.ApacheUser, osprofile.Sphinx(s.profile).User} {
		if l != "" {
			out[l] = true
		}
	}
	return out
}

// checkEnsureUser vets /v1/user/ensure. The calls the panel makes:
//
//   - a client (internal/api/ops_users.go provisionUser): home <www_root>/<login>,
//     shell nologin or /bin/bash, the SFTP or the CLI group, the other one
//     removed; refreshCLIGroup adds an existing client to the CLI group;
//   - nginx's user joins the web group (stack.go), postfix joins opendkim
//     (ops_mail_install.go), vmail is made with its own primary group and the
//     mail store as home, Sphinx 3's user with its data directory
//     (ops_tools.go); the distribution's Apache account leaves the web group
//     (apache.go, which 0.8.19's installApache had it join).
//
// Anything else would let the API make itself a sudoer, an account with a
// login shell and root's groups, or take over one of the system's users.
func (s *Server) checkEnsureUser(req *EnsureUnixUserRequest) error {
	if !nameRe.MatchString(req.Login) {
		return &Error{Status: http.StatusBadRequest, Message: "invalid login"}
	}
	for _, g := range append(append(append([]string{}, req.Groups...), req.RemoveGroups...), req.PrimaryGroup) {
		if g != "" && !nameRe.MatchString(g) {
			return &Error{Status: http.StatusBadRequest, Message: "invalid group name: " + g}
		}
	}
	if strings.ContainsAny(req.Comment, ":\n\r\x00") || len(req.Comment) > 128 {
		return notAllowed(fmt.Sprintf("the comment %q", req.Comment), "a passwd comment has no ':' or control characters and at most 128 bytes")
	}
	for _, g := range append(append([]string{}, req.Groups...), req.PrimaryGroup) {
		if g != "" && s.privilegedGroup(g) {
			return notAllowed("the group "+g+" for "+req.Login, "it is a privileged group")
		}
	}
	existing, err := lookupAccount(req.Login)
	if err != nil {
		var unknown user.UnknownUserError
		if !errors.As(err, &unknown) {
			return err
		}
		existing = nil
	}
	if req.System {
		return s.checkEnsureSystemUser(req, existing)
	}
	return s.checkEnsureClient(req, existing)
}

func (s *Server) checkEnsureSystemUser(req *EnsureUnixUserRequest, existing *user.User) error {
	login := req.Login
	if !s.systemLogins()[login] {
		return notAllowed("the system account "+login, "the panel manages only the web servers' users, postfix, vmail and the Sphinx user")
	}
	web := s.profile.Web()
	if login == web.ApacheUser {
		return s.checkApacheUserLeaves(req, existing)
	}
	if req.Shell != "" && req.Shell != s.profile.NologinShell() {
		return notAllowed("the shell "+req.Shell+" for the system account "+login, "system accounts get "+s.profile.NologinShell())
	}
	if req.UpdateShell || req.CreateHome || len(req.RemoveGroups) > 0 {
		return notAllowed("changing the shell, home or groups removal of the system account "+login, "the panel only creates it and adds groups")
	}
	if req.PrimaryGroup != "" && req.PrimaryGroup != login {
		return notAllowed("the primary group "+req.PrimaryGroup+" for "+login, "a system account gets its own group")
	}
	if req.Home != "" {
		want := map[string]string{mailUser: mailStore, osprofile.Sphinx(s.profile).User: osprofile.Sphinx(s.profile).DataDir}[login]
		if filepath.Clean(req.Home) != want || want == "" {
			return notAllowed("the home "+req.Home+" for "+login, "it is not the directory the panel keeps for it")
		}
	}
	for _, g := range req.Groups {
		ok := g == login ||
			(g == s.cfg.WebGroup && login == web.NginxUser) ||
			(g == "opendkim" && login == "postfix")
		if !ok {
			return notAllowed("the group "+g+" for "+login, "the panel adds only nginx's user to "+s.cfg.WebGroup+" and postfix to opendkim")
		}
	}
	if existing != nil {
		if uid, _ := strconv.Atoi(existing.Uid); uid == 0 {
			return notAllowed("the account "+login, "it is root's")
		}
	}
	return nil
}

// checkApacheUserLeaves allows the one change the panel makes to the
// distribution's Apache account (www-data, apache): it leaves the web group.
// Up to 0.8.19 the shared Apache ran as that account, and the web group let
// its children read every site and connect to every php-fpm pool; the sites
// run on their accounts' own Apaches now, and the account joins nothing and
// is never made.
func (s *Server) checkApacheUserLeaves(req *EnsureUnixUserRequest, existing *user.User) error {
	login := req.Login
	switch {
	case existing == nil:
		return notAllowed("making the account "+login, "the panel takes the distribution's Apache account out of "+s.cfg.WebGroup+" and makes no such account")
	case len(req.Groups) > 0:
		return notAllowed("the groups "+strings.Join(req.Groups, ", ")+" for "+login, "the distribution's Apache serves none of the panel's sites: its account joins no group of the panel")
	case len(req.RemoveGroups) != 1 || req.RemoveGroups[0] != s.cfg.WebGroup:
		return notAllowed("removing "+login+" from "+strings.Join(req.RemoveGroups, ", "), "the panel only takes it out of "+s.cfg.WebGroup)
	case req.UpdateShell || req.CreateHome || req.Shell != "" || req.Home != "" || req.PrimaryGroup != "" || req.Comment != "":
		return notAllowed("changing the account "+login, "the panel only takes it out of "+s.cfg.WebGroup)
	}
	if uid, _ := strconv.Atoi(existing.Uid); uid == 0 {
		return notAllowed("the account "+login, "it is root's")
	}
	return nil
}

func (s *Server) checkEnsureClient(req *EnsureUnixUserRequest, existing *user.User) error {
	login := req.Login
	if s.privilegedGroup(login) {
		// useradd -U would make (or join) a group of that name
		return notAllowed("the account "+login, "it is the name of a privileged group")
	}
	if existing != nil {
		if err := s.checkPanelAccount(existing); err != nil {
			return err
		}
	} else {
		// A new client lives in <www_root>/<login> (internal/api/ops_users.go
		// provisionUser): that is what makes it one of the panel's accounts
		// for every later call. Another directory of the web root would be
		// another client's home, or something of the administrator's there,
		// which useradd -m takes as it finds it.
		if req.Home != filepath.Join(s.cfg.WWWRoot, login) {
			return notAllowed(fmt.Sprintf("the home %q for the new account %s", req.Home, login), "a client's home is "+filepath.Join(s.cfg.WWWRoot, login))
		}
	}
	switch req.Shell {
	case "", s.profile.NologinShell(), "/bin/bash":
	default:
		return notAllowed("the shell "+req.Shell+" for "+login, "clients get /bin/bash or "+s.profile.NologinShell())
	}
	if req.PrimaryGroup != "" {
		return notAllowed("the primary group "+req.PrimaryGroup+" for "+login, "a client gets a group of its own")
	}
	for _, g := range req.Groups {
		if g != s.cfg.CLIGroup && g != sftpGroup && g != login {
			return notAllowed("the group "+g+" for "+login, "clients join only "+s.cfg.CLIGroup+" or "+sftpGroup)
		}
	}
	for _, g := range req.RemoveGroups {
		if g != s.cfg.CLIGroup && g != sftpGroup {
			return notAllowed("removing "+login+" from "+g, "the panel only moves clients between "+s.cfg.CLIGroup+" and "+sftpGroup)
		}
	}
	return nil
}

// fsopCommands are the subcommands of monopanel fsop the API runs as a client
// (internal/api/ops_files.go, cms.go, ops_mail_webmail.go,
// migrate_foreign.go, and tail for the logs of a site that has not moved
// yet, metrics.go).
var fsopCommands = map[string]bool{
	"list": true, "mkdir": true, "rm": true, "mv": true, "chmod": true, "read": true,
	"tail": true, "write": true, "extract": true, "run": true, "touch": true, "size": true,
}

// hardlinksProtected tells whether the kernel stops users from linking files
// they do not own (fs.protected_hardlinks, on by default everywhere): only
// then is a file with several names in an account's tree surely the
// account's own, and not /etc/shadow under a second name. Tests replace it.
var hardlinksProtected = func() bool {
	b, err := os.ReadFile("/proc/sys/fs/protected_hardlinks")
	return err == nil && strings.TrimSpace(string(b)) == "1"
}
