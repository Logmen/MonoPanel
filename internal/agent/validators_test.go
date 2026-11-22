package agent

import (
	"testing"
)

// The checks the API runs pass, argv for argv; anything else is a root
// shell and is refused.
func TestValidatorAllowed(t *testing.T) {
	deb := policyServer(t, debian13)
	el := policyServer(t, alma9)
	for _, c := range []struct {
		s    *Server
		argv []string
	}{
		{deb, []string{"/usr/sbin/nginx", "-t", "-q"}},
		{el, []string{"/usr/sbin/nginx", "-t", "-q"}},
		// an account's Apache (Debian): apache2 -t on its own httpd.conf
		{deb, []string{"/usr/sbin/apache2", "-t", "-f", "/etc/apache2/monopanel/accounts/alex/httpd.conf"}},
		{deb, []string{"/usr/sbin/apache2", "-t", "-f", "/etc/apache2/monopanel/accounts/shop_2/httpd.conf"}},
		// the shared apache2.service, which an unfinished move from 0.8.19
		// keeps serving the sites that did not move (Debian)
		{deb, []string{"/usr/sbin/apache2ctl", "-t"}},
		{deb, []string{"/usr/sbin/php-fpm8.4", "-t", "-y", "/etc/php/8.4/fpm/php-fpm.conf"}},
		{deb, []string{"/usr/sbin/php-fpm5.6", "-t", "-y", "/etc/php/5.6/fpm/php-fpm.conf"}},
		{el, []string{"/opt/remi/php83/root/usr/sbin/php-fpm", "-t", "-y", "/etc/opt/remi/php83/php-fpm.conf"}},
		{deb, []string{"/usr/sbin/sshd", "-t"}},
		{deb, []string{"/usr/sbin/mysqld", "--validate-config", "--user=mysql"}},
		{el, []string{"/usr/sbin/mariadbd", "--validate-config", "--user=mysql"}},
		{deb, []string{"/usr/sbin/logrotate", "--debug", "/etc/logrotate.d/monopanel-sites"}},
		{deb, []string{"/usr/bin/fail2ban-client", "-t"}},
		{deb, []string{"/usr/sbin/postfix", "check"}},
		{el, []string{"/usr/bin/doveconf", "-n"}},
	} {
		if err := c.s.validatorAllowed(c.argv); err != nil {
			t.Errorf("%q refused: %v", c.argv, err)
		}
	}
	for _, argv := range [][]string{
		{},
		{"/bin/sh", "-c", "id"},
		{"/usr/bin/python3", "-c", "import os"},
		{"/usr/sbin/nginx", "-t"},
		{"/usr/sbin/nginx", "-t", "-q", "-c", "/var/lib/monopanel/evil.conf"},
		{"/usr/sbin/nginx", "-t", "-q", "-g", "load_module /tmp/x.so;"},
		{"/usr/sbin/nginx", "-c", "/tmp/x"},
		{"/usr/local/sbin/nginx", "-t", "-q"},
		{"/usr/sbin/apachectl", "-t"},
		{"/usr/sbin/apache2ctl", "-t", "-f", "/tmp/httpd.conf"},
		{"/usr/sbin/apache2ctl", "-t", "-D", "X"},
		{"/usr/sbin/apache2ctl", "-k", "start", "-C", "LoadModule x /tmp/x.so"},
		{"/usr/sbin/apache2", "-t", "-f", "/tmp/httpd.conf"},
		{"/usr/sbin/apache2", "-t", "-f", "/etc/apache2/monopanel/accounts/root/httpd.conf"},
		{"/usr/sbin/apache2", "-t", "-f", "/etc/apache2/monopanel/accounts/alex/sites/example.com.conf"},
		{"/usr/sbin/apache2", "-t", "-f", "/etc/apache2/monopanel/accounts/../../../tmp/httpd.conf"},
		{"/usr/sbin/apache2", "-t", "-f", "/etc/apache2/monopanel/accounts/alex/httpd.conf", "-C", "LoadModule x /tmp/x.so"},
		{"/usr/sbin/apache2", "-f", "/etc/apache2/monopanel/accounts/alex/httpd.conf", "-DFOREGROUND"},
		{"/usr/sbin/apache2", "-t"},
		{"/usr/sbin/php-fpm8.4", "-t", "-y", "/tmp/php-fpm.conf"},
		{"/usr/sbin/php-fpm8.4", "-t", "-y", "/etc/php/8.4/fpm/php-fpm.conf", "-d", "extension=/tmp/x.so"},
		{"/usr/bin/php8.4", "-r", "system('id');"},
		{"/usr/sbin/sshd", "-t", "-f", "/tmp/sshd_config"},
		{"/usr/sbin/sshd", "-D", "-o", "PermitRootLogin=yes"},
		{"/usr/sbin/mysqld", "--validate-config", "--user=root"},
		{"/usr/sbin/mysqld", "--validate-config", "--user=mysql", "--plugin-load=x=/tmp/x.so"},
		{"/usr/sbin/logrotate", "/etc/logrotate.d/monopanel-sites"},
		{"/usr/sbin/logrotate", "--debug", "/tmp/evil"},
		{"/usr/bin/fail2ban-client", "-c", "/tmp", "-t"},
		{"/usr/sbin/postfix", "-c", "/tmp", "check"},
		{"/usr/bin/doveconf", "-c", "/tmp/dovecot.conf", "-n"},
		{"usr/sbin/nginx", "-t", "-q"},
	} {
		if err := deb.validatorAllowed(argv); err == nil {
			t.Errorf("%q must be refused", argv)
		}
	}
	// EL has no Apache mode, and no shared Apache of the panel's.
	if err := el.validatorAllowed([]string{"/usr/sbin/apache2", "-t", "-f", "/etc/apache2/monopanel/accounts/alex/httpd.conf"}); err == nil {
		t.Error("the check of an account's Apache passed on EL")
	}
	if err := el.validatorAllowed([]string{"/usr/sbin/apache2ctl", "-t"}); err == nil {
		t.Error("the check of the shared Apache passed on EL")
	}
}
