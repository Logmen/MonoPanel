package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"monopanel/internal/agent"
)

// Versions are the ones an Ubuntu 24.04 testbed server reported, so the
// pages show what a real one would.
var packageVersions = map[string]string{
	"nginx":                 "1.30.5-1~noble",
	"apache2":               "2.4.58-1ubuntu8.15",
	"percona-server-server": "8.4.11-11-1.noble",
	"percona-server-client": "8.4.11-11-1.noble",
	"percona-release":       "1.0-31.generic",
	"mysql-server":          "8.4.6-1ubuntu24.04",
	"mysql-client":          "8.4.6-1ubuntu24.04",
	"postfix":               "3.8.6-1ubuntu0.1",
	"postfix-mysql":         "3.8.6-1ubuntu0.1",
	"dovecot-core":          "1:2.3.21+dfsg1-2ubuntu6.5",
	"dovecot-imapd":         "1:2.3.21+dfsg1-2ubuntu6.5",
	"dovecot-pop3d":         "1:2.3.21+dfsg1-2ubuntu6.5",
	"dovecot-lmtpd":         "1:2.3.21+dfsg1-2ubuntu6.5",
	"dovecot-sieve":         "1:2.3.21+dfsg1-2ubuntu6.5",
	"dovecot-managesieved":  "1:2.3.21+dfsg1-2ubuntu6.5",
	"opendkim":              "2.11.0~beta2-9build4",
	"opendkim-tools":        "2.11.0~beta2-9build4",
	"fail2ban":              "1.0.2-3ubuntu0.1",
	"valkey-server":         "7.2.13+dfsg1-0ubuntu0.1",
	"memcached":             "1.6.24-1ubuntu0.3",
	"jpegoptim":             "1.5.5-1build1",
	"git":                   "1:2.43.0-1ubuntu7.3",
	"restic":                "0.16.4-1ubuntu0.1",
	"nftables":              "1.0.9-1ubuntu0.1",
	"sphinxsearch":          "2.2.11-8build1",
	"acl":                   "2.3.2-1build1.1",
	"quota":                 "4.06-1build5",
	"cron":                  "3.0pl1-184ubuntu2",
	"logrotate":             "3.21.0-2build1",
	"unzip":                 "6.0-28ubuntu4.1",
	"openssh-server":        "1:9.6p1-3ubuntu13.14",
	"ca-certificates":       "20240203",
	"curl":                  "8.5.0-2ubuntu10.6",
}

// phpVersions are the ondrej/php builds for noble.
var phpVersions = map[string]string{
	"5.6": "5.6.40-103", "7.0": "7.0.33-91", "7.1": "7.1.33-79", "7.2": "7.2.34-65", "7.3": "7.3.33-34",
	"7.4": "1:7.4.33-30", "8.0": "1:8.0.30-24", "8.1": "8.1.34-8", "8.2": "8.2.34-1", "8.3": "8.3.33-1",
	"8.4": "8.4.26-1", "8.5": "8.5.11-1",
}

// peclVersions are the PECL extensions packaged separately.
var peclVersions = map[string]string{
	"apcu": "5.1.28-2", "igbinary": "3.2.16-6", "imagick": "3.8.1-1", "memcache": "1:4.0.5.2-1",
	"memcached": "3.4.0-1", "msgpack": "1:3.0.0-2", "redis": "6.3.0-2", "xdebug": "3.4.5-1",
}

// phpPackageModules are the modules a php<v>-<package> brings; a package not
// listed here brings the module of its own name.
var phpPackageModules = map[string][]string{
	"common":  {"calendar", "ctype", "exif", "ffi", "fileinfo", "ftp", "gettext", "iconv", "pdo", "phar", "posix", "shmop", "sockets", "sysvmsg", "sysvsem", "sysvshm", "tokenizer"},
	"mysql":   {"mysqli", "mysqlnd", "pdo_mysql"},
	"xml":     {"dom", "simplexml", "xml", "xmlreader", "xmlwriter", "xsl"},
	"sqlite3": {"pdo_sqlite", "sqlite3"},
	"fpm":     nil, "cli": nil, "dev": nil,
}

var modulePriority = map[string]string{"mysqlnd": "10", "opcache": "10", "pdo": "10", "xml": "15", "memcached": "25", "redis": "25"}

var phpPkgRe = regexp.MustCompile(`^php(\d\.\d)-([a-z0-9_]+)$`)

// candidate is the version a repository offers for a package.
func candidate(name string) (string, bool) {
	if v, ok := packageVersions[name]; ok {
		return v, true
	}
	if m := phpPkgRe.FindStringSubmatch(name); m != nil {
		base, ok := phpVersions[m[1]]
		if !ok {
			return "", false
		}
		if v, ok := peclVersions[m[2]]; ok {
			base = v
		}
		return base + "+ubuntu24.04.1+deb.sury.org+1", true
	}
	return "", false
}

// unitsOf are the services a package brings.
func unitsOf(pkg string) []string {
	switch pkg {
	case "nginx":
		return []string{"nginx.service"}
	case "apache2":
		return []string{"apache2.service"}
	case "percona-server-server", "mysql-server":
		return []string{"mysql.service"}
	case "postfix":
		return []string{"postfix.service"}
	case "dovecot-core":
		return []string{"dovecot.service"}
	case "opendkim":
		return []string{"opendkim.service"}
	case "fail2ban":
		return []string{"fail2ban.service"}
	case "valkey-server":
		return []string{"valkey-server.service"}
	case "memcached":
		return []string{"memcached.service"}
	case "sphinxsearch":
		return []string{"sphinxsearch.service"}
	}
	if m := phpPkgRe.FindStringSubmatch(pkg); m != nil && m[2] == "fpm" {
		return []string{"php" + m[1] + "-fpm.service"}
	}
	return nil
}

func (a *Agent) pkg(_ context.Context, req *agent.PkgRequest) (*agent.PkgResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch req.Action {
	case "update-index":
		return &agent.PkgResponse{Output: "Hit:1 http://archive.ubuntu.com/ubuntu noble InRelease\nReading package lists... Done\n"}, nil
	case "query":
		out := &agent.PkgResponse{Installed: map[string]string{}}
		for _, p := range req.Packages {
			if v, ok := a.st.Packages[p]; ok {
				out.Installed[p] = v
			}
		}
		return out, nil
	case "available":
		out := &agent.PkgResponse{Available: map[string]string{}}
		for _, p := range req.Packages {
			if v, ok := candidate(p); ok {
				out.Available[p] = v
			}
		}
		return out, nil
	case "install":
		var b strings.Builder
		out := &agent.PkgResponse{Installed: map[string]string{}}
		for _, p := range req.Packages {
			v, ok := candidate(p)
			if !ok {
				if strings.Contains(p, "/") || strings.HasPrefix(p, "http") {
					continue // a release package by URL: its repository is "already there"
				}
				v = "1.0-1ubuntu1"
			}
			a.installPackage(p, v)
			out.Installed[p] = v
			fmt.Fprintf(&b, "Setting up %s (%s) ...\n", p, v)
		}
		out.Output = b.String()
		return out, nil
	case "remove":
		for _, p := range req.Packages {
			delete(a.st.Packages, p)
			for _, u := range unitsOf(p) {
				delete(a.st.Units, u)
			}
			if m := phpPkgRe.FindStringSubmatch(p); m != nil {
				if m[2] == "fpm" || m[2] == "common" {
					for name := range a.st.Files {
						if strings.HasPrefix(name, "/etc/php/"+m[1]+"/") || name == "/etc/php/"+m[1] {
							delete(a.st.Files, name)
						}
					}
				}
				for _, mod := range modulesOf(m[2]) {
					a.phpModule(m[1], mod, false, false)
				}
			}
		}
		return &agent.PkgResponse{Output: "Removing " + strings.Join(req.Packages, " ") + " ...\n"}, nil
	}
	return nil, &agent.Error{Status: http.StatusBadRequest, Message: "unknown pkg action " + req.Action}
}

func modulesOf(pkg string) []string {
	if mods, ok := phpPackageModules[pkg]; ok {
		return mods
	}
	return []string{pkg}
}

// installPackage records a package and what it brings along; the caller
// holds mu.
func (a *Agent) installPackage(p, version string) {
	a.st.Packages[p] = version
	for _, u := range unitsOf(p) {
		st := a.unit(u)
		st.Enabled = true
		st.start()
	}
	if m := phpPkgRe.FindStringSubmatch(p); m != nil {
		ver := m[1]
		for _, dir := range []string{"mods-available", "fpm/conf.d", "cli/conf.d", "fpm/pool.d"} {
			a.mkdirs("/etc/php/" + ver + "/" + dir)
		}
		for _, mod := range modulesOf(m[2]) {
			a.phpModule(ver, mod, true, true)
		}
	}
}

// phpModule makes a module exist (mods-available) and switches it on or off
// for php-fpm and the CLI; the caller holds mu.
func (a *Agent) phpModule(ver, mod string, exists, enabled bool) {
	avail := "/etc/php/" + ver + "/mods-available/" + mod + ".ini"
	prio := modulePriority[mod]
	if prio == "" {
		prio = "20"
	}
	if exists {
		a.put(avail, fmt.Sprintf("; configuration for php %s module\n; priority=%s\nextension=%s.so\n", mod, prio, mod), 0o644)
	} else if !enabled {
		delete(a.st.Files, avail)
	}
	for _, sapi := range []string{"fpm", "cli"} {
		link := "/etc/php/" + ver + "/" + sapi + "/conf.d/" + prio + "-" + mod + ".ini"
		if enabled {
			a.put(link, "", 0o777)
		} else {
			delete(a.st.Files, link)
		}
	}
}

type resticSnapshot struct {
	ID    string   `json:"id"`
	Time  string   `json:"time"`
	Paths []string `json:"paths"`
	Tags  []string `json:"tags"`
	Files int64    `json:"files"`
	Bytes int64    `json:"bytes"`
}

// tool answers the allow-listed administrative tools with what the real ones
// would print for this pretend server.
func (a *Agent) tool(_ context.Context, req *agent.ToolRequest) (*agent.ToolResponse, error) {
	okOut := func(s string) (*agent.ToolResponse, error) { return &agent.ToolResponse{Output: s}, nil }
	args := strings.Join(req.Args, " ")
	switch req.Name {
	case "nginx":
		if strings.Contains(args, "-v") && !strings.Contains(args, "-t") {
			return okOut("nginx version: nginx/1.30.5\n")
		}
		return okOut("nginx: the configuration file /etc/nginx/nginx.conf syntax is ok\nnginx: configuration file /etc/nginx/nginx.conf test is successful\n")
	case "apachectl":
		if strings.Contains(args, "-v") {
			return okOut("Server version: Apache/2.4.58 (Ubuntu)\n")
		}
		return okOut("Syntax OK\n")
	case "php":
		return okOut("PHP 8.4.26 (cli) (built: Sep 11 2026 07:58:12) (NTS)\n")
	case "phpenmod", "phpdismod":
		if len(req.Args) >= 3 && req.Args[0] == "-v" {
			a.mu.Lock()
			a.phpModule(req.Args[1], req.Args[2], true, req.Name == "phpenmod")
			a.mu.Unlock()
		}
		return okOut("")
	case "mysql":
		return a.mysql(req)
	case "mysqldump":
		if req.OutputFile != "" && a.real(req.OutputFile) {
			_ = os.WriteFile(req.OutputFile, []byte("-- MySQL dump 10.13  Distrib 8.4.11-11\n"), 0o600)
		}
		return okOut("")
	case "percona-release":
		return okOut("* Enabling the Percona Server 8.4 repository\n* Enabling the Percona Tools repository\n")
	case "nft":
		return a.nft(req.Args)
	case "fail2ban-client":
		return okOut(fail2banStatus(req.Args))
	case "journalctl":
		return okOut(journal(req.Args))
	case "sshd":
		return okOut("port 22\npermitrootlogin prohibit-password\npasswordauthentication yes\npubkeyauthentication yes\nsubsystem sftp internal-sftp\n")
	case "du":
		dir := ""
		if len(req.Args) > 0 {
			dir = req.Args[len(req.Args)-1]
		}
		return okOut(fmt.Sprintf("%d\t%s\n", dirSize(dir), dir))
	case "crontab":
		return a.crontab(req)
	case "restic":
		return a.restic(req.Args)
	case "postconf":
		return okOut("mail_version = 3.8.6\n")
	case "doveconf":
		return okOut("2.3.21 (47349e2482)\n")
	case "postqueue":
		return okOut("")
	case "doveadm":
		return okOut("Quota name Type    Value Limit %\nUser quota STORAGE 1824 2097152 0\n")
	case "systemctl":
		return okOut("")
	}
	return okOut("")
}

// mysql answers the statements whose output the panel reads and remembers
// the databases it creates, so their sizes can be shown.
func (a *Agent) mysql(req *agent.ToolRequest) (*agent.ToolResponse, error) {
	sql := req.Stdin + " " + strings.Join(req.Args, " ")
	upper := strings.ToUpper(sql)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range createDBRe.FindAllStringSubmatch(sql, -1) {
		a.st.MySQL[m[1]] = true
	}
	for _, m := range dropDBRe.FindAllStringSubmatch(sql, -1) {
		delete(a.st.MySQL, m[1])
	}
	switch {
	case strings.Contains(upper, "SELECT VERSION()"):
		return &agent.ToolResponse{Output: "8.4.11-11\n"}, nil
	case strings.Contains(upper, "INFORMATION_SCHEMA.TABLES GROUP BY TABLE_SCHEMA"):
		names := make([]string, 0, len(a.st.MySQL))
		for n := range a.st.MySQL {
			names = append(names, n)
		}
		sort.Strings(names)
		var b strings.Builder
		for _, n := range names {
			fmt.Fprintf(&b, "%s\t%d\n", n, dbSize(n))
		}
		return &agent.ToolResponse{Output: b.String()}, nil
	}
	return &agent.ToolResponse{}, nil
}

var (
	createDBRe = regexp.MustCompile("(?i)CREATE DATABASE (?:IF NOT EXISTS )?`?([A-Za-z0-9_]+)`?")
	dropDBRe   = regexp.MustCompile("(?i)DROP DATABASE (?:IF EXISTS )?`?([A-Za-z0-9_]+)`?")
)

// dbSize is a stable made-up size: a WordPress database is a few megabytes,
// a fresh one is empty.
func dbSize(name string) int64 {
	switch {
	case strings.HasSuffix(name, "_wordpress"):
		return 7360 << 10
	case strings.HasSuffix(name, "_opencart"):
		return 3400 << 10
	case strings.HasSuffix(name, "_roundcube"):
		return 512 << 10
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	return int64(h.Sum32()%64) << 16
}

func dirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})
	if total == 0 {
		total = 4096
	}
	return total
}

func (a *Agent) nft(args []string) (*agent.ToolResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	joined := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(joined, "-c "):
		return &agent.ToolResponse{}, nil
	case strings.HasPrefix(joined, "-f "):
		a.st.Firewall = true
		return &agent.ToolResponse{}, nil
	case strings.HasPrefix(joined, "delete table"):
		a.st.Firewall = false
		return &agent.ToolResponse{}, nil
	case strings.HasPrefix(joined, "list table"):
		if !a.st.Firewall {
			return &agent.ToolResponse{ExitCode: 1, Output: "Error: No such file or directory\n"}, nil
		}
		return &agent.ToolResponse{Output: "table inet monopanel {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t}\n}\n"}, nil
	}
	return &agent.ToolResponse{}, nil
}

var jails = []string{"monopanel", "nginx-botsearch", "nginx-http-auth", "sshd"}

func fail2banStatus(args []string) string {
	if len(args) == 1 && args[0] == "status" {
		return fmt.Sprintf("Status\n|- Number of jail:\t%d\n`- Jail list:\t%s\n", len(jails), strings.Join(jails, ", "))
	}
	if len(args) == 2 && args[0] == "status" {
		failed, total, banned := 0, 0, ""
		switch args[1] {
		case "sshd":
			failed, total, banned = 3, 41, "198.51.100.23"
		case "nginx-botsearch":
			failed, total = 1, 12
		}
		count := 0
		if banned != "" {
			count = 1
		}
		return fmt.Sprintf("Status for the jail: %s\n|- Filter\n|  |- Currently failed:\t%d\n|  |- Total failed:\t%d\n|  `- File list:\t/var/log/auth.log\n`- Actions\n   |- Currently banned:\t%d\n   |- Total banned:\t%d\n   `- Banned IP list:\t%s\n", args[1], failed, total, count, count+total/10, banned)
	}
	return ""
}

// journal makes a few plausible lines for a unit's log.
func journal(args []string) string {
	unit := "system"
	for i, a := range args {
		if a == "-u" && i+1 < len(args) {
			unit = args[i+1]
		}
	}
	name := strings.TrimSuffix(unit, ".service")
	now := time.Now().Add(-40 * time.Minute)
	lines := []string{"Started " + unit + ".", name + " is running", "Reloading " + unit + "...", "Reloaded " + unit + "."}
	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%s %s %s[%d]: %s\n", now.Add(time.Duration(i*9)*time.Minute).Format("2006-01-02T15:04:05-0700"), Hostname, name, 1200+i, l)
	}
	return b.String()
}

func (a *Agent) crontab(req *agent.ToolRequest) (*agent.ToolResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	login := ""
	remove, list := false, false
	for i, arg := range req.Args {
		switch arg {
		case "-u":
			if i+1 < len(req.Args) {
				login = req.Args[i+1]
			}
		case "-r":
			remove = true
		case "-l":
			list = true
		}
	}
	switch {
	case remove:
		delete(a.st.Crontabs, login)
	case list:
		if tab, ok := a.st.Crontabs[login]; ok {
			return &agent.ToolResponse{Output: tab}, nil
		}
		return &agent.ToolResponse{ExitCode: 1, Output: "no crontab for " + login + "\n"}, nil
	default:
		a.st.Crontabs[login] = req.Stdin
	}
	return &agent.ToolResponse{}, nil
}

// restic keeps the snapshots of the pretend repository; a backup takes the
// real size of the site files.
func (a *Agent) restic(args []string) (*agent.ToolResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(args) == 0 {
		return &agent.ToolResponse{}, nil
	}
	switch args[0] {
	case "snapshots":
		type snap struct {
			ID       string   `json:"id"`
			ShortID  string   `json:"short_id"`
			Time     string   `json:"time"`
			Hostname string   `json:"hostname"`
			Paths    []string `json:"paths"`
			Tags     []string `json:"tags"`
		}
		list := make([]snap, 0, len(a.st.Restic))
		for _, s := range a.st.Restic {
			list = append(list, snap{ID: s.ID, ShortID: s.ID[:8], Time: s.Time, Hostname: Hostname, Paths: s.Paths, Tags: s.Tags})
		}
		b, _ := json.Marshal(list)
		return &agent.ToolResponse{Output: string(b) + "\n"}, nil
	case "backup":
		var paths, tags []string
		for i := 1; i < len(args); i++ {
			switch {
			case args[i] == "--tag" && i+1 < len(args):
				tags = append(tags, args[i+1])
				i++
			case strings.HasPrefix(args[i], "--"):
			default:
				paths = append(paths, args[i])
			}
		}
		var files, size int64
		for _, p := range paths {
			_ = filepath.Walk(p, func(_ string, fi os.FileInfo, err error) error {
				if err == nil && !fi.IsDir() {
					files++
					size += fi.Size()
				}
				return nil
			})
		}
		now := time.Now().UTC()
		sum := sha256.Sum256([]byte(now.String()))
		s := resticSnapshot{ID: hex.EncodeToString(sum[:]), Time: now.Format(time.RFC3339Nano), Paths: paths, Tags: tags, Files: files + 7200, Bytes: size + 140<<20}
		a.st.Restic = append(a.st.Restic, s)
		line, _ := json.Marshal(map[string]any{"message_type": "summary", "snapshot_id": s.ID, "total_files_processed": s.Files, "total_bytes_processed": s.Bytes, "data_added": s.Bytes / 9})
		return &agent.ToolResponse{Output: string(line) + "\n"}, nil
	}
	return &agent.ToolResponse{}, nil
}

// BaseSystem is what the pretend server has before the panel installs
// anything: an Ubuntu with the panel itself running.
func (a *Agent) BaseSystem() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range []string{"acl", "quota", "cron", "logrotate", "unzip", "openssh-server", "ca-certificates", "curl", "nftables"} {
		a.installPackage(p, packageVersions[p])
	}
	for _, u := range []string{"monopanel-agent.service", "monopanel-api.service", "cron.service", "ssh.service", "systemd-journald.service"} {
		st := a.unit(u)
		st.Enabled = true
		st.start()
	}
	for _, d := range []string{"/etc/nginx", "/etc/systemd/system", "/var/log/nginx", "/etc/logrotate.d"} {
		a.mkdirs(d)
	}
	a.put(path.Join("/etc", "hostname"), Hostname+"\n", 0o644)
	// What an app service may run: the page checks that the binary exists.
	for _, bin := range []string{"/bin/sh", "/bin/bash", "/usr/bin/bash", "/usr/bin/env", "/usr/bin/python3", "/usr/bin/node", "/usr/bin/npm", "/usr/bin/git"} {
		a.put(bin, "", 0o755)
	}
}
