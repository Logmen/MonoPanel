package agent

import (
	"strings"

	"monopanel/internal/osprofile"
)

// panelValidators are the configuration checks the API runs through
// config/apply and paths/remove, argv for argv: nginx -t and the Apache check
// of the OS layout (osprofile WebLayout: every site, stack, mail and doctor
// call), php-fpm -t of each branch's layout (Sury's php-fpmX.Y, Remi's
// /opt/remi/phpXY/root/usr/sbin/php-fpm), sshd -t (ops_users.go), the
// MySQL/Percona check (ops_db.go; mariadbd is the same server under its own
// name), logrotate on the panel's site rotation (logrotate.go),
// fail2ban-client -t (fail2ban.go), postfix check and doveconf -n
// (ops_mail.go).
func (s *Server) panelValidators() [][]string {
	web := s.profile.Web()
	out := [][]string{
		web.NginxCheckArgv,
		web.ApacheCheckArgv,
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
	for _, v := range s.panelValidators() {
		if len(v) > 0 && equalArgs(argv, v) {
			return nil
		}
	}
	return notAllowed("the validator "+strings.Join(quoteArgs(argv), " "), "it is not one of the panel's configuration checks")
}
