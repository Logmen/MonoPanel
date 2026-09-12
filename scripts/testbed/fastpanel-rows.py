# Rows a FASTPANEL 2 writes for an account with two sites, a database, a
# certificate, a cron task and a mailbox — shaped like a live 1.11 panel.
# Run by scripts/testbed/sources.sh seed fastpanel (@ZONE@ and @CERT@ are
# substituted there); the panel itself is licence-locked on the testbed.
import sqlite3, datetime, subprocess, os
subprocess.run(["openssl","req","-x509","-newkey","rsa:2048","-nodes","-keyout","/tmp/app-self.key","-out","/tmp/app-self.crt","-days","30","-subj","/CN=app.@ZONE@"], check=True, capture_output=True)
c = sqlite3.connect("/usr/local/fastpanel2/app/db/fastpanel2.db")
cur = c.cursor()
now = datetime.datetime.utcnow().strftime("%Y-%m-%d %H:%M:%S")
if cur.execute("SELECT COUNT(*) FROM panel_account WHERE username='shop'").fetchone()[0]:
    raise SystemExit("shop already there")
cur.execute("INSERT INTO panel_account (username, home_dir, ssh_access, owner_id, roles, php_version_cli, status, quota_value, created_at) VALUES (?,?,?,?,?,?,?,?,?)", ("shop", "/var/www/shop/data", 1, 1, '["ROLE_USER"]', 82, "active", 0, now))
owner = cur.lastrowid
cols = "(domain, idn_domain, index_dir, static_sub_directory, charset, gzip, status, auto_sub_domains, admin_email, https_redirect, http2, http3, hsts, http_auth, owner_id, manual_changes, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
cur.execute("INSERT INTO site " + cols, ("wp.@ZONE@", "wp.@ZONE@", "/var/www/shop/data/www/wp.@ZONE@", "", "utf-8", 1, "active", 0, "admin@@ZONE@", 1, 1, 0, 0, 0, owner, 0, now))
wp = cur.lastrowid
cur.execute("INSERT INTO site " + cols, ("app.@ZONE@", "app.@ZONE@", "/var/www/shop/data/www/app.@ZONE@/public", "", "utf-8", 1, "active", 0, "admin@@ZONE@", 0, 0, 0, 0, 0, owner, 1, now))
app = cur.lastrowid
cur.execute("INSERT INTO virtualhost_aliases (name, idn_name, site_id) VALUES (?,?,?)", ("alias.@ZONE@", "alias.@ZONE@", app))
cur.execute("INSERT INTO virtualhost_aliases (name, idn_name, site_id) VALUES (?,?,?)", ("www.wp.@ZONE@", "www.wp.@ZONE@", wp))
for sid, port, short in ((wp, 3025, "wp"), (app, 3026, "app")):
    cur.execute("INSERT INTO website_backends (frontend_id, main, service_name, location, listen_type, addr, port, proto, type, handler, handler_version, app_type, process_type, work_dir) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)", (sid, 1, "shop", "", 0, "127.0.0.1", port, "http", "php", "php_fpm", "82", "", "simple", ""))
    front = open("/etc/nginx/fastpanel2-sites/shop/%s.@ZONE@.conf" % short).read()
    cur.execute("INSERT INTO virtualhost_configuration (virtualhost_id, frontend, backend, phpini, php_parameters) VALUES (?,?,?,?,?)", (sid, front, "", "", None))
cur.execute("INSERT INTO db (name, server_id, engine, charset, enabled, site_id, owner_id, size, dump, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)", ("shop_wp", 1, "", "utf8mb4", 1, wp, owner, 0, 0, now))
dbid = cur.lastrowid
cur.execute("INSERT INTO database_user (owner_id, server_id, login, crypted_password, allow_remote_connection, created_at) VALUES (?,?,?,?,?,?)", (owner, 1, "shop_wp", "encrypted-in-fastpanel", 0, now))
uid = cur.lastrowid
cur.execute("INSERT INTO datbases_users (user_id, database_id) VALUES (?,?)", (uid, dbid))
cur.execute("INSERT INTO certificate (owner_id, name, wildcard, type, common_name, alternative_names, virtualhost_id, enabled, created_at, expired_at) VALUES (?,?,?,?,?,?,?,?,?,?)", (owner, "@CERT@", 0, "letsencrypt", "wp.@ZONE@", "", wp, 1, now, "2026-12-11 00:00:00+00:00"))
cur.execute("UPDATE site SET certificate_id=? WHERE id=?", (cur.lastrowid, wp))
cur.execute("INSERT INTO certificate (owner_id, name, wildcard, type, common_name, alternative_names, private_key, certificate, chain, virtualhost_id, enabled, created_at, expired_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)", (owner, "app.@ZONE@_self_02", 0, "exists", "app.@ZONE@", "", open("/tmp/app-self.key").read(), open("/tmp/app-self.crt").read(), "", app, 1, now, "2026-10-12 00:00:00+00:00"))
cur.execute("UPDATE site SET certificate_id=? WHERE id=?", (cur.lastrowid, app))
cur.execute("INSERT INTO task (minute, hour, day_of_month, month, day_of_week, command, enabled, owner_id, vhost_id, commentss, createdAt) VALUES (?,?,?,?,?,?,?,?,?,?,?)", ("*/10", "*", "*", "*", "*", "/var/www/shop/data/bin/php /var/www/shop/data/www/wp.@ZONE@/wp-cron.php", 1, owner, wp, "", now))
cur.execute("INSERT INTO email_domain (owner_id, site_id, name, dkim, enabled, created_at) VALUES (?,?,?,?,?,?)", (owner, wp, "wp.@ZONE@", 0, 1, now))
dom = cur.lastrowid
cur.execute("INSERT INTO mailboxes (owner_id, domain_id, enabled, login, address, size, quota, encoded_password, created) VALUES (?,?,?,?,?,?,?,?,?)", (owner, dom, 1, "info", "info@wp.@ZONE@", 0, 0, "{SHA512-CRYPT}$6$test", now))
c.commit()
os.remove("/tmp/app-self.key"); os.remove("/tmp/app-self.crt")
print("rows ok", owner, wp, app)
