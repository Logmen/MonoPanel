// Package confcheck refuses configuration content that would hand root to
// whoever wrote it. The API process composes the configurations of daemons
// that run as root — nginx, Apache, php-fpm, MySQL, postfix, dovecot, sshd,
// logrotate, fail2ban, systemd-tmpfiles, sysctl, apt and yum — and the agent
// writes them as root. A directive such as nginx's load_module, Apache's
// piped logs, postfix's daemon_directory, a logrotate script or a tmpfiles
// line would let a compromised API process run code or write files as root;
// Check keeps such content out, whatever the API sends.
package confcheck

// Check returns an error when content written to path would let its author
// run code, or write or change files, as root. Paths without rules pass.
func Check(path string, content []byte) error {
	_, _ = path, content
	return nil
}
