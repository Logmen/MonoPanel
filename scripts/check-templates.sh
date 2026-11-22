#!/usr/bin/env bash
# Feed the rendered configuration (the golden files the panel actually writes)
# to the real nginx and Apache binaries, so a broken template is caught here
# rather than on a customer's host.
#
# Usage: scripts/check-templates.sh [golden-dir]
# Needs: nginx, and Debian's apache2 for the Apache check (skipped when absent).
set -euo pipefail

golden=${1:-internal/render/testdata}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

command -v nginx >/dev/null || { echo "nginx not installed"; exit 1; }

# The panel installs nginx from nginx.org; "http2 on;" (1.25.1+) and QUIC are
# generated unconditionally, so an older distro nginx cannot validate them.
version=$(nginx -v 2>&1 | sed -E 's#.*/([0-9]+\.[0-9]+\.[0-9]+).*#\1#')
if [ "$(printf '%s\n1.25.1\n' "$version" | sort -V | head -1)" != "1.25.1" ]; then
	echo "nginx $version is older than 1.25.1: the panel installs nginx.org packages, which support 'http2 on;'" >&2
	exit 1
fi
echo "nginx $version"

# A minimal tree that mirrors what the panel creates on a host.
mkdir -p "$work"/{conf,snippets,sites,logs,docroot,certs,run/php,backend}
cp templates/nginx/snippets/*.conf "$work/snippets/"
# The backend secret of alex's Apache, in the one form the agent writes
# (confcheck.NginxApacheBackendConf, ApacheBackendConf).
secret=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
printf 'proxy_set_header X-MonoPanel-Backend "%s";\n' "$secret" > "$work/backend/alex.conf"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=example.com \
	-keyout "$work/certs/privkey.pem" -out "$work/certs/fullchain.pem" >/dev/null 2>&1
: > "$work/run/php/example.com.sock"

# The site blocks are generated for one http context; build it around them.
write_main() {
	cat > "$work/conf/nginx.conf" <<EOF
worker_processes 1;
pid $work/nginx.pid;
error_log $work/logs/error.log;
events { worker_connections 64; }
http {
    include $(nginx_mime_path);
    log_format main '\$remote_addr \$request';
    access_log $work/logs/access.log main;
    client_body_temp_path $work/tmp;
    proxy_temp_path $work/tmp-proxy;
    fastcgi_temp_path $work/tmp-fcgi;
    uwsgi_temp_path $work/tmp-uwsgi;
    scgi_temp_path $work/tmp-scgi;
    map \$http_upgrade \$connection_upgrade { default upgrade; '' close; }
    include $work/sites/site.conf;
}
EOF
}

nginx_mime_path() {
	for p in /etc/nginx/mime.types /usr/local/nginx/conf/mime.types /opt/homebrew/etc/nginx/mime.types; do
		[ -f "$p" ] && { echo "$p"; return; }
	done
	echo /dev/null
}

fail=0
shopt -s nullglob
for f in "$golden"/nginx-site-*.golden; do
	name=$(basename "$f" .conf.golden)
	# Rewrite the golden paths onto the throwaway tree: the directives are what
	# matters here, not where the panel would have put the files.
	sed -e "s#/var/lib/monopanel/certs/example.com/fullchain.pem#$work/certs/fullchain.pem#" \
		-e "s#/var/lib/monopanel/certs/example.com/privkey.pem#$work/certs/privkey.pem#" \
		-e "s#/run/monopanel/php/example.com.sock#$work/run/php/example.com.sock#" \
		-e "s#/var/www/alex/data/www/example.com#$work/docroot#" \
		-e "s#/var/log/monopanel/sites/alex#$work/logs#" \
		-e "s#include monopanel/snippets/#include $work/snippets/#" \
		-e "s#include monopanel/apache/alex.conf;#include $work/backend/alex.conf;#" \
		-e "s#include monopanel/sites/example.com.d/\*.conf;##" \
		-e "s#listen 203.0.113.10:80#listen 127.0.0.1:18080#g" \
		-e "s#listen 203.0.113.10:443#listen 127.0.0.1:18443#g" \
		-e "s#listen \[2001:db8::10\]:80#listen [::1]:18080#g" \
		-e "s#listen \[2001:db8::10\]:443#listen [::1]:18443#g" \
		"$f" > "$work/sites/site.conf"
	write_main
	if out=$(nginx -t -c "$work/conf/nginx.conf" -p "$work" -e "$work/logs/startup.log" 2>&1); then
		echo "ok   $name"
	else
		echo "FAIL $name"
		echo "$out" | sed 's/^/     /'
		fail=1
	fi
done

# Apache: the whole configuration of an account's Apache (Debian-family
# hosts only) — its httpd.conf with the site's virtual host included, as
# monopanel-apache@<login> starts it. apache2ctl/apachectl are wrappers that
# create /var/run/apache2; the binary takes the same -t and runs
# unprivileged. The paths move onto the throwaway tree, the account onto the
# user running the check (User must name an existing account).
apachectl=$(command -v apache2 || command -v httpd || command -v apache2ctl || command -v apachectl || true)
main="$golden/apache-account.conf.golden"
vhost="$golden/apache-site.conf.golden"
if [ -n "$apachectl" ] && [ -f "$main" ] && [ -f "$vhost" ] && [ -d /usr/lib/apache2/modules ]; then
	mkdir -p "$work/apache/sites/example.com.d" "$work/apache/run"
	apache_paths() {
		sed -e "s#/var/www/alex/data/www/example.com#$work/docroot#" \
			-e "s#/var/log/monopanel/sites/alex#$work/logs#" \
			-e "s#/run/monopanel-apache/alex#$work/apache/run#" \
			-e "s#/etc/apache2/monopanel/accounts/alex/sites#$work/apache/sites#" \
			-e "s#/etc/apache2/monopanel/accounts/alex/backend.conf#$work/apache/backend.conf#" \
			-e "s#^User alex\$#User $(id -un)#" \
			-e "s#^Group alex\$#Group $(id -gn)#" \
			"$1"
	}
	apache_paths "$main" > "$work/apache/httpd.conf"
	# backend.conf in the one form the agent writes, with the secret above
	# (confcheck.ApacheBackendConf; TestApacheBackendGolden keeps them equal).
	cp "$golden/apache-backend.conf.golden" "$work/apache/backend.conf"
	apache_paths "$vhost" > "$work/apache/sites/example.com.conf"
	if out=$($apachectl -t -f "$work/apache/httpd.conf" 2>&1); then
		echo "ok   apache-account (with apache-site)"
	else
		echo "FAIL apache-account (with apache-site)"
		echo "$out" | sed 's/^/     /'
		fail=1
	fi
else
	echo "skip apache-account (no Debian apache2)"
fi

exit $fail
