package agent

import (
	"strings"

	"monopanel/internal/agent/confcheck"
	"monopanel/internal/osprofile"
)

// panelValidators are the configuration checks the API runs through
// config/apply and paths/remove, argv for argv: nginx -t of the OS layout
// (osprofile WebLayout: every site, stack, mail and doctor call), php-fpm -t
// of each branch's layout (Sury's php-fpmX.Y, Remi's
// /opt/remi/phpXY/root/usr/sbin/php-fpm), sshd -t (ops_users.go), the
// MySQL/Percona check (ops_db.go; mariadbd is the same server under its own
// name), logrotate on the panel's site rotation (logrotate.go),
// fail2ban-client -t (fail2ban.go), postfix check and doveconf -n
// (ops_mail.go). The check of an account's Apache has a login in it
// (apacheInstanceCheck).
func (s *Server) panelValidators() [][]string {
	web := s.profile.Web()
	out := [][]string{
		web.NginxCheckArgv,
		{"/usr/sbin/sshd", "-t"},
		{"/usr/sbin/mysqld", "--validate-config", "--user=mysql"},
		{"/usr/sbin/mariadbd", "--validate-config", "--user=mysql"},
		{"/usr/sbin/logrotate", "--debug", "/etc/logrotate.d/monopanel-sites"},
		{"/usr/bin/fail2ban-client", "-t"},
		{"/usr/sbin/postfix", "check"},
		{"/usr/bin/doveconf", "-n"},
	}
	for _, v := range osprofile.PHPVersions(s.profile) {
		if l := osprofile.PHP(s.profile, v.Version); l != nil {
			out = append(out, l.FPMCheckArgv)
		}
	}
	return append(out, extraValidators...)
}

// extraValidators are checks tests add (true and false); nothing in the
// agent sets it.
var extraValidators [][]string

// validatorAllowed decides whether argv may run as a configuration check
// (config/apply and paths/remove run them as root). The API names the
// validator, so only the checks the panel uses pass, binary and arguments
// exactly: any other argv would be a root shell for whoever controls the API
// process (nginx -c of a file of its own, php -r, sshd -f … -D).
func (s *Server) validatorAllowed(argv []string) error {
	if len(argv) == 0 {
		return notAllowed("an empty validator", "a validator is one of the panel's configuration checks")
	}
	if s.apacheInstanceCheck(argv) {
		return nil
	}
	for _, v := range s.panelValidators() {
		if len(v) > 0 && equalArgs(argv, v) {
			return nil
		}
	}
	return notAllowed("the validator "+strings.Join(quoteArgs(argv), " "), "it is not one of the panel's configuration checks")
}

// apacheInstanceCheck tells the check of an account's Apache (sites in
// Apache mode, Debian): apache2 -t -f on the httpd.conf of the account's
// directory, in exactly that shape. apache2 parses the file as root and
// loads the modules it names; the file is one config/apply wrote after
// confcheck read it as that account's (its modules from the allow-list), and
// a login without one only makes apache2 fail.
func (s *Server) apacheInstanceCheck(argv []string) bool {
	if s.profile == nil || s.profile.Family() != osprofile.FamilyDebian {
		return false
	}
	login, ok := confcheck.ApacheInstanceCheckLogin(argv)
	return ok && nameRe.MatchString(login)
}
