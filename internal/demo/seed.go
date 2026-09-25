package demo

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"monopanel/internal/apitypes"
	"monopanel/internal/client"
	"monopanel/internal/store"
)

// seed fills the demo the way scripts/screenshots/seed.sh fills the server
// the documentation's screenshots come from, through the same API. What
// needs the internet (the CMS installers) is laid out by hand instead.
func seed(ctx context.Context, cl *client.Client, db *store.DB, o Options, log *slog.Logger) error {
	s := &seeder{ctx: ctx, cl: cl, log: log}
	yes := true

	s.step("stack")
	s.job(cl.StackInstall(ctx, "nginx"))
	s.job(cl.StackInstall(ctx, "percona"))
	s.job(cl.PHPInstall(ctx, "8.4"))
	s.job(cl.PHPInstall(ctx, "7.4"))
	s.must(cl.SetPHPExtension(ctx, "8.4", "redis", true))
	s.job(cl.StackInstall(ctx, "apache"))
	s.job(cl.StackInstall(ctx, "fail2ban"))
	s.job(cl.StackInstall(ctx, "memcached"))
	s.job(cl.StackInstall(ctx, "valkey"))
	s.job(cl.StackInstall(ctx, "git"))

	s.step("accounts")
	s.userJob(cl.CreateUser(ctx, apitypes.CreateUserRequest{Login: "alex", Email: "alex@example.com", Password: s.password(), Shell: true}))
	s.userJob(cl.CreateUser(ctx, apitypes.CreateUserRequest{Login: "shop", Email: "shop@example.com", Password: s.password()}))
	s.userJob(cl.CreateUser(ctx, apitypes.CreateUserRequest{Login: "maria", Email: "maria@example.org", Password: s.password()}))

	s.step("sites")
	s.siteJob(cl.CreateSite(ctx, apitypes.SiteRequest{Domain: "example.com", User: "alex", WWW: true, Preset: "wordpress", SSL: "none", IP: SiteIP}))
	s.siteJob(cl.CreateSite(ctx, apitypes.SiteRequest{Domain: "shop.example.com", User: "shop", Preset: "opencart", SSL: "none", IP: SiteIP}))
	s.siteJob(cl.CreateSite(ctx, apitypes.SiteRequest{Domain: "old.example.com", User: "alex", PHPVersion: "7.4", Mode: "apache", SSL: "none", IP: SiteIP}))
	s.must(cl.CreateApp(ctx, "maria", apitypes.AppRequest{Name: "api", Command: "/usr/bin/python3 -m http.server 3000", Description: "app.example.org backend", Enabled: &yes}))
	s.siteJob(cl.CreateSite(ctx, apitypes.SiteRequest{Domain: "app.example.org", User: "maria", Mode: "proxy", Backend: "http://127.0.0.1:3000", SSL: "none", IP: SiteIP}))

	s.step("valkey")
	s.must(cl.ValkeyPut(ctx, "alex", "cache", 128))
	s.must(cl.ValkeyPut(ctx, "alex", "sessions", 64))
	s.siteJob(cl.UpdateSite(ctx, "example.com", apitypes.SiteUpdateRequest{SessionStore: "valkey"}))

	s.step("databases")
	s.must(cl.CreateDatabase(ctx, apitypes.DatabaseRequest{Name: "wordpress", User: "alex"}))
	s.must(cl.CreateDatabase(ctx, apitypes.DatabaseRequest{Name: "opencart", User: "shop"}))
	s.must(cl.CreateDatabase(ctx, apitypes.DatabaseRequest{Name: "analytics", User: "alex"}))
	s.must(cl.CreateDatabase(ctx, apitypes.DatabaseRequest{Name: "crm", User: "shop"}))

	s.step("cron")
	s.must(cl.CronAdd(ctx, "alex", apitypes.CronRequest{Schedule: "*/5 * * * *", Command: "php ~/data/www/example.com/wp-cron.php", Comment: "WordPress cron"}))
	s.must(cl.CronAdd(ctx, "shop", apitypes.CronRequest{Schedule: "@daily", Command: "find ~/data/tmp -type f -mtime +7 -delete", Comment: "tmp cleanup"}))

	s.step("mail")
	s.job(cl.MailInstall(ctx, apitypes.MailInstallRequest{Hostname: "mail.example.com"}))
	s.must(cl.CreateMailDomain(ctx, apitypes.MailDomainRequest{Name: "example.com", User: "alex"}))
	s.must(cl.CreateMailbox(ctx, apitypes.MailboxRequest{Address: "ivan@example.com", Name: "Ivan Petrov", QuotaMB: 2048, Password: s.password()}))
	s.must(cl.CreateMailbox(ctx, apitypes.MailboxRequest{Address: "info@example.com", Name: "Example", QuotaMB: 1024, Password: s.password()}))
	s.must(cl.CreateMailAlias(ctx, apitypes.MailAliasRequest{Address: "sales@example.com", Destinations: []string{"ivan@example.com", "info@example.com"}}))

	s.step("firewall")
	s.must(cl.FirewallAction(ctx, "enable"))
	s.must(cl.FirewallRuleAdd(ctx, apitypes.FirewallRuleRequest{Kind: "allow", Proto: "tcp", Port: "3306", Source: "10.10.0.0/24", Comment: "MySQL for the office"}))
	s.must(cl.FirewallBan(ctx, "ban", "203.0.113.45"))

	s.step("files")
	if s.err == nil {
		s.err = s.files(db, o)
	}

	s.step("backups")
	s.must(cl.BackupTargetCreate(ctx, apitypes.BackupTargetRequest{Name: "local", Type: "local", Repository: "/var/backups/monopanel", Schedule: "daily"}))
	s.job(cl.BackupRun(ctx, "local", "server"))
	return s.err
}

// seeder stops at the first error and says at which step it happened.
type seeder struct {
	ctx  context.Context
	cl   *client.Client
	log  *slog.Logger
	name string
	err  error
}

func (s *seeder) step(name string) {
	if s.err == nil {
		s.name = name
		s.log.Info("demo seed", "step", name)
	}
}

func (s *seeder) must(_ any, err error) {
	if s.err == nil && err != nil {
		s.err = fmt.Errorf("seed %s: %w", s.name, err)
	}
}

func (s *seeder) wait(id int64) {
	if s.err != nil || id == 0 {
		return
	}
	job, err := s.cl.WaitJob(s.ctx, id, nil)
	switch {
	case err != nil:
		s.err = fmt.Errorf("seed %s: job #%d: %w", s.name, id, err)
	case job.Status != store.JobDone:
		s.err = fmt.Errorf("seed %s: job #%d %s: %s", s.name, id, job.Type, job.Error)
	}
}

func (s *seeder) job(ref *apitypes.JobRef, err error) {
	s.must(nil, err)
	if err == nil {
		s.wait(ref.JobID)
	}
}

func (s *seeder) userJob(u *apitypes.UserWithJob, err error) {
	s.must(nil, err)
	if err == nil {
		s.wait(u.JobID)
	}
}

func (s *seeder) siteJob(site *apitypes.SiteWithJob, err error) {
	s.must(nil, err)
	if err == nil {
		s.wait(site.JobID)
	}
}

// password is a throwaway one: nobody signs in as these accounts.
func (s *seeder) password() string {
	const letters = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	for i := range b {
		b[i] = letters[rand.IntN(len(letters))]
	}
	return string(b)
}

// files lays out what the CMS installers would have put there, plus a few
// days of site logs, and records the CMSs on the sites.
func (s *seeder) files(db *store.DB, o Options) error {
	installed := time.Now().Add(-9 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for _, c := range []struct{ domain, login, cms, version, database string }{
		{"example.com", "alex", "wordpress", "7.1.2", "alex_wordpress"},
		{"shop.example.com", "shop", "opencart", "4.1.0.4", "shop_opencart"},
	} {
		site, err := db.GetSiteByDomain(s.ctx, c.domain)
		if err != nil {
			return err
		}
		site.CMS, site.CMSVersion, site.CMSAt, site.CMSDatabase = c.cms, c.version, installed, c.database
		if err := db.UpdateSite(s.ctx, site); err != nil {
			return err
		}
		root := filepath.Join(o.WWW, c.login, "data", "www", c.domain)
		_ = os.Remove(filepath.Join(root, "index.html"))
		tree := wordpressTree
		if c.cms == "opencart" {
			tree = opencartTree
		}
		for name, content := range tree(c.domain) {
			if err := writeFile(filepath.Join(root, name), content); err != nil {
				return err
			}
		}
	}
	for _, l := range []struct{ domain, login string }{
		{"example.com", "alex"}, {"shop.example.com", "shop"}, {"old.example.com", "alex"}, {"app.example.org", "maria"},
	} {
		logs := filepath.Join(o.WWW, l.login, "data", "logs")
		if err := writeFile(filepath.Join(logs, l.domain+".access.log"), accessLog(l.domain)); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(logs, l.domain+".error.log"), errorLog(l.domain)); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(name, content string) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	return os.WriteFile(name, []byte(content), 0o644)
}

func wordpressTree(domain string) map[string]string {
	php := func(what string) string { return "<?php\n/**\n * " + what + "\n *\n * @package WordPress\n */\n" }
	tree := map[string]string{
		"index.php":               php("Front to the WordPress application. This file doesn't do anything, but loads wp-blog-header.php which does and tells WordPress to load the theme.") + "\ndefine( 'WP_USE_THEMES', true );\n\nrequire __DIR__ . '/wp-blog-header.php';\n",
		"wp-blog-header.php":      php("Loads the WordPress environment and template.") + "\nif ( ! isset( $wp_did_header ) ) {\n\t$wp_did_header = true;\n\trequire_once __DIR__ . '/wp-load.php';\n\twp();\n\trequire_once ABSPATH . WPINC . '/template-loader.php';\n}\n",
		"wp-load.php":             php("Bootstrap file for setting the ABSPATH constant and loading the wp-config.php file."),
		"wp-login.php":            php("WordPress User Page. Handles authentication, registering, resetting passwords, forgot password, and other user handling."),
		"wp-cron.php":             php("A pseudo-cron daemon for scheduling WordPress tasks.\n *\n * WP-Cron is triggered when the site receives a visit. In the scenario\n * where a site may not receive enough visits to execute scheduled tasks\n * in a timely manner, this file can be called directly or via a server\n * CRON daemon for X number of times.") + "\nignore_user_abort( true );\n\nif ( ! headers_sent() ) {\n\theader( 'Expires: Wed, 11 Jan 1984 05:00:00 GMT' );\n\theader( 'Cache-Control: no-cache, must-revalidate, max-age=0' );\n}\n",
		"wp-settings.php":         php("Used to set up and fix common variables and include the WordPress procedural and class library."),
		"wp-comments-post.php":    php("Handles Comment Post to WordPress and prevents duplicate comment posting."),
		"wp-links-opml.php":       php("Outputs the OPML XML format for getting the links defined in the link administration."),
		"wp-mail.php":             php("Gets the email message from the user's mailbox to add as a WordPress post."),
		"wp-signup.php":           php("WordPress Signup Page. Handles the user registration of new sites in a multisite network."),
		"wp-trackback.php":        php("Handle Trackbacks and Pingbacks Sent to WordPress."),
		"wp-activate.php":         php("Confirms that the activation key that is sent in an email after a user signs up for a new site matches the key for that user and then displays confirmation."),
		"xmlrpc.php":              php("XML-RPC protocol support for WordPress."),
		"wp-config-sample.php":    php("The base configuration for WordPress"),
		"wp-config.php":           "<?php\n/** The name of the database for WordPress */\ndefine( 'DB_NAME', 'alex_wordpress' );\n\n/** Database username */\ndefine( 'DB_USER', 'alex_wordpress' );\n\n/** Database password */\ndefine( 'DB_PASSWORD', '••••••••' );\n\n/** Database hostname */\ndefine( 'DB_HOST', 'localhost' );\n\ndefine( 'DB_CHARSET', 'utf8mb4' );\ndefine( 'DB_COLLATE', '' );\n\n/** Sessions and the object cache live in the account's Valkey. */\ndefine( 'WP_REDIS_SCHEME', 'unix' );\ndefine( 'WP_REDIS_PATH', '/run/monopanel-valkey/alex-cache/valkey.sock' );\n\n$table_prefix = 'wp_';\n\ndefine( 'WP_DEBUG', false );\n\nif ( ! defined( 'ABSPATH' ) ) {\n\tdefine( 'ABSPATH', __DIR__ . '/' );\n}\n\nrequire_once ABSPATH . 'wp-settings.php';\n",
		"readme.html":             "<!DOCTYPE html>\n<html lang=\"en\">\n<head><meta charset=\"utf-8\"><title>WordPress &#8250; ReadMe</title></head>\n<body>\n<h1 id=\"logo\">WordPress</h1>\n<p>Semantic Personal Publishing Platform</p>\n</body>\n</html>\n",
		"license.txt":             "WordPress - Web publishing software\n\nCopyright 2011-2026 by the contributors\n\nThis program is free software; you can redistribute it and/or modify\nit under the terms of the GNU General Public License as published by\nthe Free Software Foundation; either version 2 of the License, or\n(at your option) any later version.\n",
		"wp-admin/index.php":      php("Dashboard Administration Screen"),
		"wp-admin/admin.php":      php("WordPress Administration Bootstrap"),
		"wp-includes/version.php": "<?php\n/**\n * WordPress Version\n */\n$wp_version = '7.1.2';\n$wp_db_version = 60717;\n$required_php_version = '7.4';\n$required_mysql_version = '5.5.5';\n",
		"wp-includes/load.php":    php("These functions are needed to load WordPress."),
		"wp-content/index.php":    "<?php\n// Silence is golden.\n",
		"wp-content/themes/twentytwentyfive/style.css":     "/*\nTheme Name: Twenty Twenty-Five\nAuthor: the WordPress team\nVersion: 1.3\nRequires PHP: 7.2\nText Domain: twentytwentyfive\n*/\n",
		"wp-content/themes/twentytwentyfive/functions.php": php("Twenty Twenty-Five functions and definitions."),
		"wp-content/plugins/index.php":                     "<?php\n// Silence is golden.\n",
		"wp-content/uploads/2026/09/.keep":                 "",
		".htaccess":                                        "# BEGIN WordPress\n# nginx serves this site; the rules live in the panel's preset.\n# END WordPress\n",
	}
	return tree
}

func opencartTree(domain string) map[string]string {
	return map[string]string{
		"index.php":              "<?php\n// Version\ndefine('VERSION', '4.1.0.4');\n\n// Configuration\nif (is_file('config.php')) {\n\trequire_once('config.php');\n}\n\n// Startup\nrequire_once(DIR_SYSTEM . 'startup.php');\n\n// Framework\nrequire_once(DIR_SYSTEM . 'framework.php');\n",
		"config.php":             "<?php\n// APPLICATION\ndefine('APPLICATION', 'Catalog');\n\n// HTTP\ndefine('HTTP_SERVER', 'http://" + domain + "/');\n\n// DB\ndefine('DB_DRIVER', 'mysqli');\ndefine('DB_HOSTNAME', 'localhost');\ndefine('DB_USERNAME', 'shop_opencart');\ndefine('DB_PASSWORD', '••••••••');\ndefine('DB_DATABASE', 'shop_opencart');\ndefine('DB_PREFIX', 'oc_');\n",
		"admin/index.php":        "<?php\n// Version\ndefine('VERSION', '4.1.0.4');\n",
		"admin/config.php":       "<?php\n// APPLICATION\ndefine('APPLICATION', 'Admin');\n",
		"catalog/index.html":     "",
		"system/startup.php":     "<?php\n// Error Reporting\nerror_reporting(E_ALL);\n",
		"image/catalog/logo.png": "",
		"robots.txt":             "User-agent: *\nDisallow: /admin/\n",
	}
}

var logPaths = []string{"/", "/", "/", "/blog/", "/about/", "/contact/", "/wp-content/themes/twentytwentyfive/style.css", "/favicon.ico", "/wp-json/wp/v2/posts?per_page=5", "/feed/", "/sitemap.xml", "/robots.txt"}
var logAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 15_6) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/19.0 Safari/605.1.15",
	"Mozilla/5.0 (iPhone; CPU iPhone OS 19_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148",
	"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
	"Mozilla/5.0 (compatible; YandexBot/3.0; +http://yandex.com/bots)",
}

// accessLog is two days of nginx's "main" format with $host and timings.
func accessLog(domain string) string {
	r := rand.New(rand.NewPCG(uint64(len(domain)), 7))
	var b strings.Builder
	start := time.Now().Add(-48 * time.Hour)
	for t := start; t.Before(time.Now()); t = t.Add(time.Duration(40+r.IntN(900)) * time.Second) {
		p := logPaths[r.IntN(len(logPaths))]
		status, size := 200, 5000+r.IntN(60000)
		switch {
		case r.IntN(40) == 0:
			p, status, size = "/wp-login.php", 404, 153
		case r.IntN(25) == 0:
			status, size = 304, 0
		}
		fmt.Fprintf(&b, "198.51.100.%d - - [%s] \"GET %s HTTP/2.0\" %d %d \"-\" \"%s\" %s %.3f %.3f\n",
			1+r.IntN(250), t.Format("02/Jan/2006:15:04:05 -0700"), p, status, size, logAgents[r.IntN(len(logAgents))], domain, 0.01+r.Float64()*0.2, 0.008+r.Float64()*0.18)
	}
	return b.String()
}

func errorLog(domain string) string {
	t := time.Now().Add(-30 * time.Hour)
	return fmt.Sprintf("%s [error] 2481#2481: *1937 open() \"/var/www/robots.txt\" failed (2: No such file or directory), client: 198.51.100.77, server: %s, request: \"GET /robots.txt HTTP/2.0\", host: \"%s\"\n",
		t.Format("2006/01/02 15:04:05"), domain, domain)
}
