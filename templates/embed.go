// Package templates embeds the configuration templates shipped with the
// panel. Files can be overridden by copies under /etc/monopanel/templates.
package templates

import "embed"

// FS holds nginx/, apache/, php-fpm/, php/, site/, mail/ and systemd/ templates.
//
//go:embed nginx apache php-fpm php site mysql nftables fail2ban cron systemd mail memcached sphinx
var FS embed.FS
