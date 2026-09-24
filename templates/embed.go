// Package templates embeds the configuration templates shipped with the
// panel. Files can be overridden by copies under /etc/monopanel/templates.
package templates

import "embed"

// FS holds nginx/, apache/, php-fpm/, php/, site/, mail/, systemd/, logrotate/ and the other templates.
//
//go:embed nginx apache php-fpm php site mysql nftables fail2ban cron systemd mail memcached sphinx logrotate
var FS embed.FS
