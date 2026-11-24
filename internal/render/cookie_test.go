package render

import (
	"regexp"
	"strings"
	"testing"
)

// nginxMap is the map block named by its result variable in conf: the
// regex entries in their order and their values (nginx tries them in the
// order they stand, the first match wins) and the default.
func nginxMap(t *testing.T, conf, result string) (def string, keys []*regexp.Regexp, vals []string) {
	t.Helper()
	start := strings.Index(conf, "map $http_cookie "+result+" {")
	if start < 0 {
		t.Fatalf("no map $http_cookie %s in:\n%s", result, conf)
	}
	body := conf[start:]
	body = body[strings.Index(body, "{")+1 : strings.Index(body, "\n}")]
	entry := regexp.MustCompile(`^\s*"~([^"]*)"\s+"([^"]*)";$`)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "default "):
			def = strings.TrimSuffix(strings.TrimPrefix(line, "default "), ";")
		default:
			m := entry.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("map entry not of the form \"~regex\" \"value\": %s", line)
			}
			keys = append(keys, regexp.MustCompile(m[1]))
			vals = append(vals, m[2])
		}
	}
	return def, keys, vals
}

// applyMap does what nginx does with the map for a Cookie header: the
// first regex that matches gives the value, its named captures filled in.
func applyMap(def string, keys []*regexp.Regexp, vals []string, cookie string) string {
	for i, re := range keys {
		m := re.FindStringSubmatch(cookie)
		if m == nil {
			continue
		}
		v := vals[i]
		for j, name := range re.SubexpNames() {
			if name != "" {
				v = strings.ReplaceAll(v, "$"+name, m[j])
			}
		}
		return v
	}
	if def == "$http_cookie" {
		return cookie
	}
	return def
}

// The panel's session cookie reaches every port of the panel's host:
// phpMyAdmin's and the webmail's server blocks hand PHP the Cookie header
// without it, in every location that runs PHP.
func TestPortBlocksKeepThePanelCookieFromPHP(t *testing.T) {
	r := New("")
	pma, err := r.Render("phpmyadmin/nginx.conf.tmpl", PhpMyAdmin{Port: 2083, ServerName: "panel.example.com", CertPath: "/c.pem", KeyPath: "/k.pem",
		Docroot: "/opt/monopanel/phpmyadmin/5.2.3", Socket: "/run/monopanel/php/monopanel-phpmyadmin.sock"})
	if err != nil {
		t.Fatal(err)
	}
	wm, err := r.Render("mail/webmail.conf.tmpl", Webmail{IP: "127.0.0.1", Port: 2096, ServerName: "panel.example.com", CertPath: "/c.pem", KeyPath: "/k.pem",
		Docroot: "/var/www/alex/data/www/example.com", LogDir: "/var/log/monopanel/sites/alex", Socket: "/run/monopanel/php/example.com.sock", ClientMaxBodySize: "64m"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, conf, variable string }{
		{"phpmyadmin", pma, "$monopanel_pma_cookie"},
		{"webmail", wm, "$monopanel_webmail_cookie"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Every PHP location sets HTTP_COOKIE after the snippet (nginx
			// then leaves the client's header out).
			passes := strings.Count(c.conf, "fastcgi_pass ")
			want := "include monopanel/snippets/fastcgi.conf;\n        fastcgi_param HTTP_COOKIE " + c.variable + " if_not_empty;"
			if passes == 0 || strings.Count(c.conf, want) != passes {
				t.Fatalf("%d fastcgi_pass, but %d times %q:\n%s", passes, strings.Count(c.conf, want), want, c.conf)
			}
			def, keys, vals := nginxMap(t, c.conf, c.variable)
			for _, tc := range []struct{ in, out string }{
				{"", ""},
				{"roundcube_sessid=abc; pma_lang=en", "roundcube_sessid=abc; pma_lang=en"},
				{"mp_session=SECRET", ""},
				{"mp_session=SECRET; other=1", "; other=1"},
				{"other=1; mp_session=SECRET", "other=1"},
				{"a=1; mp_session=SECRET; b=2", "a=1; b=2"},
				{"a=1;mp_session=SECRET;b=2", "a=1;b=2"},
				{"xmp_session=1; a=2", "xmp_session=1; a=2"},
				{"mp_sessionx=1", "mp_sessionx=1"},
				// A decoy of the page's own, with a longer path, comes first:
				// no cookie at all rather than the real one behind it.
				{"mp_session=DECOY; a=1; mp_session=SECRET", ""},
				{"a=1; mp_session=DECOY; mp_session=SECRET", ""},
			} {
				got := applyMap(def, keys, vals, tc.in)
				if got != tc.out {
					t.Errorf("Cookie %q → PHP gets %q, want %q", tc.in, got, tc.out)
				}
				if strings.Contains(got, "SECRET") {
					t.Errorf("Cookie %q: the panel's session reaches PHP: %q", tc.in, got)
				}
			}
		})
	}
}
