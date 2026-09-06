#!/usr/bin/env bash
# Feed the rendered configuration (the golden files the panel actually writes)
# to the real nginx and Apache binaries, so a broken template is caught here
# rather than on a customer's host.
#
# Usage: scripts/check-templates.sh [golden-dir]
# Needs: nginx, and apache2ctl/httpd for the vhost check (skipped when absent).
set -euo pipefail

golden=${1:-internal/render/testdata}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

command -v nginx >/dev/null || { echo "nginx not installed"; exit 1; }

# A minimal tree that mirrors what the panel creates on a host.
mkdir -p "$work"/{conf,snippets,sites,logs,docroot,certs,run/php}
cp templates/nginx/snippets/*.conf "$work/snippets/"
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
		-e "s#/var/www/alex/data/logs#$work/logs#" \
		-e "s#include monopanel/snippets/#include $work/snippets/#" \
		-e "s#include monopanel/sites/example.com.d/\*.conf;##" \
		-e "s#listen 203.0.113.10#listen 127.0.0.1#g" \
		"$f" > "$work/sites/site.conf"
	write_main
	if out=$(nginx -t -c "$work/conf/nginx.conf" -p "$work" 2>&1); then
		echo "ok   $name"
	else
		echo "FAIL $name"
		echo "$out" | sed 's/^/     /'
		fail=1
	fi
done

# Apache vhost: the panel only generates it on Debian-family hosts.
apachectl=$(command -v apache2ctl || command -v apachectl || true)
vhost="$golden/apache-site.conf.golden"
if [ -n "$apachectl" ] && [ -f "$vhost" ]; then
	mkdir -p "$work/apache/sites" "$work/apache/include"
	sed -e "s#/var/www/alex/data/www/example.com#$work/docroot#" \
		-e "s#/var/www/alex/data/logs#$work/logs#" \
		-e "s#/etc/apache2/monopanel/sites/example.com.d#$work/apache/include#" \
		"$vhost" > "$work/apache/sites/site.conf"
	modules=$(dirname "$(dirname "$apachectl")")/lib/apache2/modules
	[ -d "$modules" ] || modules=/usr/lib/apache2/modules
	cat > "$work/apache/httpd.conf" <<EOF
ServerName localhost
PidFile $work/apache/httpd.pid
ErrorLog $work/logs/apache-error.log
EOF
	# Some of these are compiled in depending on the build; only load what is missing.
	for m in mpm_event:mod_mpm_event authz_core:mod_authz_core dir:mod_dir mime:mod_mime \
		proxy:mod_proxy proxy_fcgi:mod_proxy_fcgi remoteip:mod_remoteip log_config:mod_log_config unixd:mod_unixd; do
		name=${m%%:*}; file=${m##*:}
		[ -f "$modules/$file.so" ] || continue
		printf '<IfModule !%s_module>\nLoadModule %s_module %s/%s.so\n</IfModule>\n' "$name" "$name" "$modules" "$file" >> "$work/apache/httpd.conf"
	done
	cat >> "$work/apache/httpd.conf" <<EOF
Listen 127.0.0.1:18080
Include $work/apache/sites/site.conf
EOF
	if out=$($apachectl -t -f "$work/apache/httpd.conf" -d "$work/apache" 2>&1); then
		echo "ok   apache-site"
	else
		echo "FAIL apache-site"
		echo "$out" | sed 's/^/     /'
		fail=1
	fi
else
	echo "skip apache-site (no apachectl)"
fi

exit $fail
