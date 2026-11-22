package confcheck

import (
	"strings"
	"testing"

	"monopanel/internal/mailpolicy"
)

const (
	mainCf   = "/etc/postfix/main.cf"
	masterCf = "/etc/postfix/master.cf"
	dovecot  = "/etc/dovecot/dovecot.conf"
	dcUsers  = "/etc/dovecot/monopanel/users"
	dcConfD  = "/etc/dovecot/conf.d/90-x.conf"
)

func TestPostfixRefusals(t *testing.T) {
	runRefusals(t, []refusal{
		{"daemon directory", mainCf, "myhostname = mail.example.com\ndaemon_directory = /var/lib/monopanel/bin\n", []string{"daemon_directory", "line 2"}},
		{"command directory", mainCf, "command_directory = /tmp\n", []string{"command_directory"}},
		{"shlib directory", mainCf, "shlib_directory = /tmp\n", []string{"shlib_directory"}},
		{"meta directory", mainCf, "meta_directory = /var/lib/monopanel\n", []string{"meta_directory"}},
		{"queue directory", mainCf, "queue_directory = /etc\n", []string{"queue_directory"}},
		{"data directory", mainCf, "data_directory = /etc/cron.d\n", []string{"data_directory"}},
		{"sendmail path", mainCf, "sendmail_path = /tmp/sendmail\n", []string{"sendmail_path"}},
		{"newaliases path", mainCf, "newaliases_path = /tmp/x\n", []string{"newaliases_path"}},
		{"mail owner root", mainCf, "mail_owner = root\n", []string{"mail_owner"}},
		{"setgid group", mainCf, "setgid_group = root\n", []string{"setgid_group"}},
		{"default privs", mainCf, "default_privs = root\n", []string{"default_privs"}},
		{"import environment", mainCf, "import_environment = MAIL_CONFIG LD_PRELOAD=/tmp/x.so\n", []string{"import_environment"}},
		{"export environment", mainCf, "export_environment = LD_PRELOAD=/tmp/x.so\n", []string{"export_environment"}},
		{"mailbox command", mainCf, "mailbox_command = /bin/sh -c 'id > /tmp/x'\n", []string{"mailbox_command"}},
		{"home mailbox", mainCf, "home_mailbox = .ssh/authorized_keys\n", []string{"home_mailbox"}},
		{"forward path", mainCf, "forward_path = /var/lib/monopanel/fwd/$user\n", []string{"forward_path"}},
		{"command shell", mainCf, "local_command_shell = /tmp/sh\n", []string{"local_command_shell"}},
		{"debugger", mainCf, "debugger_command = /tmp/x\n", []string{"debugger_command"}},
		{"multi instance", mainCf, "multi_instance_wrapper = /tmp/x\n", []string{"multi_instance_wrapper"}},
		{"maillog into cron", mainCf, "maillog_file = /etc/cron.d/postfix\n", []string{"maillog_file"}},
		{"maillog below the panel's dir", mainCf, "maillog_file = /var/log/monopanel/mail.log\n", []string{"maillog_file"}},
		{"maillog prefixes", mainCf, "maillog_file_prefixes = /\n", []string{"maillog_file_prefixes"}},
		{"maillog compressor", mainCf, "maillog_file_compressor = /tmp/x\n", []string{"maillog_file_compressor"}},
		{"aliases elsewhere", mainCf, "alias_maps = hash:/var/lib/monopanel/aliases\n", []string{"alias_maps"}},
		{"alias database elsewhere", mainCf, "alias_database = hash:/tmp/aliases\n", []string{"alias_database"}},
		{"virtual uid root", mainCf, "virtual_uid_maps = static:0\n", []string{"virtual_uid_maps"}},
		{"virtual uid of a sudoer", mainCf, "virtual_uid_maps = static:1000\n", []string{"virtual_uid_maps", "vmail"}},
		{"virtual uid map", mainCf, "virtual_uid_maps = hash:/etc/postfix/monopanel/uids\n", []string{"virtual_uid_maps"}},
		{"virtual gid root", mainCf, "virtual_gid_maps = static:0\n", []string{"virtual_gid_maps"}},
		{"virtual base home", mainCf, "virtual_mailbox_base = /home/ubuntu\n", []string{"virtual_mailbox_base"}},
		{"virtual minimum uid", mainCf, "virtual_minimum_uid = 0\n", []string{"virtual_minimum_uid"}},
		{"session cache elsewhere", mainCf, "smtpd_tls_session_cache_database = btree:/etc/cron.d/x\n", []string{"smtpd_tls_session_cache_database"}},
		{"config directory", mainCf, "config_directory = /tmp\n", []string{"config_directory"}},
		{"in a continuation", mainCf, "smtpd_recipient_restrictions =\n    permit_mynetworks\nmailbox_command = /tmp/x\n", []string{"mailbox_command", "line 3"}},
		{"not a parameter", mainCf, "this is not postfix\n", nil},
		{"master program", masterCf, "evil unix - n n - - /tmp/evil\n", []string{"/tmp/evil"}},
		{"master shell", masterCf, "evil unix - n n - - sh\n", []string{"\"sh\""}},
		{"privileged smtpd", masterCf, "smtp inet n n n - - smtpd\n", []string{"unpriv"}},
		{"pipe as root", masterCf, "evil unix - n n - - pipe flags=R user=root argv=/bin/sh -c id\n", []string{"user=root"}},
		{"pipe without user", masterCf, "evil unix - n n - - pipe argv=/bin/sh -c id\n", []string{"user="}},
		{"pipe as the sudoer", masterCf, "evil unix - n n - - pipe user=ubuntu argv=/bin/sh -c id\n", []string{"user=ubuntu"}},
		{"pipe as a sudo system account", masterCf, "evil unix - n n - - pipe user=sysop argv=/bin/sh -c id\n", []string{"user=sysop", "sudo"}},
		{"pipe group root", masterCf, "evil unix - n n - - pipe user=vmail:root argv=/bin/id\n", []string{"user=vmail:root"}},
		{"pipe as the API", masterCf, "evil unix - n n - - spawn user=monopanel argv=/bin/id\n", []string{"user=monopanel"}},
		{"debugger flag", masterCf, "smtp inet n - n - - smtpd -D\n", []string{"-D"}},
		{"option daemon directory", masterCf, "smtp inet n - n - - smtpd -o daemon_directory=/tmp\n", []string{"daemon_directory"}},
		{"option group", masterCf, "smtp inet n - n - - smtpd\n  -o { import_environment = LD_PRELOAD=/tmp/x.so }\n", []string{"import_environment"}},
		{"option continued", masterCf, "smtp inet n - n - - smtpd\n  -o syslog_name=x\n  -o mailbox_command=/tmp/x\n", []string{"mailbox_command"}},
		{"short master line", masterCf, "smtp inet n - n\n", nil},
		{"dynamicmaps", "/etc/postfix/dynamicmaps.cf", "evil /tmp/evil.so dict_evil_open\n", []string{"dynamicmaps.cf"}},
		{"postfix-files", "/etc/postfix/postfix-files", "$queue_directory:d:root:root:755\n", []string{"postfix-files"}},
		{"sasl", "/etc/postfix/sasl/smtpd.conf", "pwcheck_method: saslauthd\n", []string{"smtpd.conf"}},
		{"maps subdirectory", "/etc/postfix/monopanel/sub/x", "x\n", []string{"monopanel/sub/x"}},
		// Postfix 3.9 initialises OpenSSL with tls_config_file as root; an
		// OpenSSL configuration loads engines and providers of any path.
		{"openssl configuration", mainCf, "tls_config_file = /etc/postfix/monopanel/evil.cnf\n", []string{"tls_config_file"}},
		{"openssl program", mainCf, "openssl_path = /var/www/alex/openssl\n", []string{"openssl_path"}},
		{"sasl configuration path", mainCf, "cyrus_sasl_config_path = /var/www/alex/sasl\n", []string{"cyrus_sasl_config_path"}},
		// Daemons open their tables as root and cidr quotes the lines it
		// cannot parse into the mail log.
		{"table of root's secrets", mainCf, "smtpd_client_restrictions = check_client_access cidr:/etc/shadow\n", []string{"cidr:/etc/shadow"}},
		{"table behind proxy", mainCf, "virtual_alias_maps = proxy:hash:/var/lib/monopanel/x\n", []string{"hash:/var/lib/monopanel/x"}},
		{"table inside unionmap", mainCf, "virtual_alias_maps = unionmap:{hash:/etc/postfix/monopanel/aliases, texthash:/root/x}\n", []string{"texthash:/root/x"}},
		{"table walks up", mainCf, "header_checks = regexp:/etc/postfix/../shadow\n", []string{"regexp:/etc/postfix/../shadow"}},
		{"client library table", mainCf, "virtual_alias_maps = proxy:mysql:/etc/postfix/monopanel/x.cf\n", []string{"mysql"}},
		{"table in a master option", masterCf, "smtp inet n - n - - smtpd -o smtpd_client_restrictions=check_client_access,cidr:/etc/shadow\n", []string{"cidr:/etc/shadow"}},
		// A program running as one of these is a step to root.
		{"pipe as the web server", masterCf, "x unix - n n - - pipe user=www-data argv=/bin/sh -c id\n", []string{"user=www-data"}},
		{"pipe as mysql", masterCf, "x unix - n n - - pipe user=mysql argv=/bin/sh -c id\n", []string{"user=mysql"}},
		{"pipe with the web server's group", masterCf, "x unix - n n - - pipe user=vmail:nginx argv=/bin/id\n", []string{"vmail:nginx"}},
		{"spawn as opendkim", masterCf, "x unix - n n - 0 spawn user=opendkim argv=/bin/id\n", []string{"user=opendkim"}},
		{"vertical tab continuation", mainCf, "myhostname = x\n\vdaemon_directory = /tmp\n", []string{"'\\v'"}},
	})
	// The panel's tables and caches, ${data_directory} included.
	pass(t, mainCf, "alias_maps = hash:/etc/aliases\nvirtual_alias_maps = proxy:hash:/etc/postfix/monopanel/aliases\nsmtpd_tls_session_cache_database = btree:${data_directory}/smtpd_scache\npostscreen_cache_map = btree:$data_directory/postscreen_cache\nsmtpd_milters = local:/run/opendkim/opendkim.sock, inet:127.0.0.1:8891\nlocal_recipient_maps = proxy:unix:passwd.byname $alias_maps\ntls_config_file = default\n")
	pass(t, masterCf, "x unix - n n - - pipe flags=DRhu user=vmail:vmail argv=/usr/lib/dovecot/dovecot-lda -f ${sender} -d ${recipient}\n")
}

const dcHead = "first_valid_uid = 990\nlast_valid_uid = 990\n"

func TestDovecotRefusals(t *testing.T) {
	runRefusals(t, []refusal{
		{"executable", dovecot, dcHead + "service imap {\n  executable = /tmp/evil\n}\n", []string{"executable", "line 4"}},
		{"import environment", dovecot, dcHead + "import_environment = LD_PRELOAD=/tmp/x.so\n", []string{"import_environment"}},
		{"plugin dir", dovecot, dcHead + "mail_plugin_dir = /tmp\n", []string{"mail_plugin_dir"}},
		{"base dir", dovecot, dcHead + "base_dir = /var/lib/monopanel/run\n", []string{"base_dir"}},
		{"dsync command", dovecot, dcHead + "dsync_remote_cmd = /bin/sh -c id\n", []string{"dsync_remote_cmd"}},
		{"doveadm password", dovecot, dcHead + "doveadm_password = secret\n", []string{"doveadm_password"}},
		{"service as root", dovecot, dcHead + "service imap-login {\n  user = root\n}\n", []string{"user = root"}},
		{"service shadow group", dovecot, dcHead + "service auth {\n  extra_groups = shadow\n}\n", []string{"shadow"}},
		{"listener path", dovecot, dcHead + "service auth {\n  unix_listener /etc/cron.d/x {\n    mode = 0600\n  }\n}\n", []string{"/etc/cron.d/x"}},
		{"listener for everybody", dovecot, dcHead + "service auth {\n  unix_listener auth-userdb {\n    mode = 0666\n  }\n}\n", []string{"mode = 0666"}},
		{"listener for the API", dovecot, dcHead + "service doveadm {\n  unix_listener doveadm-server {\n    user = monopanel\n  }\n}\n", []string{"user = monopanel"}},
		{"checkpassword", dovecot, dcHead + "passdb {\n  driver = checkpassword\n  args = /tmp/x\n}\n", []string{"checkpassword"}},
		{"lua passdb 2.4", dovecot, dcHead + "passdb lua {\n  lua_file = /tmp/x.lua\n}\n", []string{"lua"}},
		{"static userdb", dovecot, dcHead + "userdb {\n  driver = static\n  args = uid=1000 gid=1000 home=/home/ubuntu\n}\n", nil},
		{"userdb uid root", dovecot, dcHead + "userdb {\n  driver = passwd-file\n  args = /etc/dovecot/monopanel/users\n  default_fields = uid=0 gid=990 home=/var/mail/monopanel/%d/%n\n}\n", []string{"uid=0"}},
		{"userdb home elsewhere", dovecot, dcHead + "userdb {\n  driver = passwd-file\n  args = /etc/dovecot/monopanel/users\n  default_fields = uid=990 gid=990 home=/home/ubuntu\n}\n", []string{"home=/home/ubuntu"}},
		{"userdb fields 2.4", dovecot, dcHead + "userdb passwd-file {\n  passwd_file_path = /etc/dovecot/monopanel/users\n  fields {\n    uid = 1000\n  }\n}\n", []string{"uid"}},
		{"userdb extra field", dovecot, dcHead + "userdb {\n  driver = passwd-file\n  args = /etc/dovecot/monopanel/users\n  override_fields = first_valid_uid=0\n}\n", []string{"first_valid_uid=0"}},
		{"passwd-file elsewhere", dovecot, dcHead + "passdb {\n  driver = passwd-file\n  args = scheme=BLF-CRYPT /var/lib/monopanel/users\n}\n", []string{"/var/lib/monopanel/users"}},
		{"mbox in the home", dovecot, dcHead + "mail_location = mbox:~/.ssh:INBOX=~/.ssh/authorized_keys\n", []string{"mail_location"}},
		{"maildir elsewhere", dovecot, dcHead + "mail_location = maildir:/home/%u/Maildir\n", []string{"mail_location"}},
		{"mbox driver", dovecot, dcHead + "mail_driver = mbox\n", []string{"mail_driver"}},
		{"mail path elsewhere", dovecot, dcHead + "mail_path = /root/%{user}\n", []string{"mail_path"}},
		{"mail uid root", dovecot, dcHead + "mail_uid = 0\n", []string{"mail_uid"}},
		{"mail uid of a sudoer", dovecot, dcHead + "mail_uid = 1000\n", []string{"mail_uid", "vmail"}},
		{"first valid uid zero", dovecot, "first_valid_uid = 0\nlast_valid_uid = 990\n", []string{"first_valid_uid"}},
		{"no pinned uids", dovecot, "protocols = imap\n", []string{"first_valid_uid"}},
		{"pinned in a section", dovecot, "first_valid_uid = 990\nprotocol imap {\n  last_valid_uid = 990\n}\n", []string{"last_valid_uid"}},
		{"pinned in conf.d", dcConfD, "first_valid_uid = 0\n", []string{"first_valid_uid"}},
		{"log path cron", dovecot, dcHead + "log_path = /etc/cron.d/dovecot\n", []string{"log_path"}},
		{"log path below the panel's dir", dovecot, dcHead + "info_log_path = /var/log/monopanel/dovecot.log\n", []string{"info_log_path"}},
		{"include shadow", dovecot, dcHead + "!include /etc/shadow\n", []string{"/etc/shadow"}},
		{"include the users file", dovecot, dcHead + "!include_try monopanel/users\n", []string{"monopanel/users"}},
		{"include dotdot", dovecot, dcHead + "!include ../x.conf\n", []string{".."}},
		// dovecot globs the path with GLOB_BRACE: each of these reads
		// /var/lib/monopanel/x.conf, the API's own file, as root's
		// configuration.
		{"include through braces", dovecot, dcHead + "!include /etc/dovecot/{..,x}/{..,x}/var/lib/monopanel/x.conf\n", []string{"{..,x}", "glob"}},
		{"include through escaped dots", dovecot, dcHead + "!include_try /etc/dovecot/\\.\\./\\.\\./var/lib/monopanel/x.conf\n", []string{"glob"}},
		{"include through quoted escapes", dovecot, dcHead + "!include \"/etc/dovecot/\\\\.\\\\./\\\\.\\\\./var/lib/monopanel/x.conf\"\n", []string{"glob"}},
		{"include through a wildcard directory", dovecot, dcHead + "!include conf.d/.*/.*/.*/var/lib/monopanel/x.conf\n", []string{".*", "glob"}},
		{"privileged group", dovecot, dcHead + "mail_privileged_group = shadow\n", []string{"shadow"}},
		{"internal user root", dovecot, dcHead + "default_internal_user = root\n", []string{"default_internal_user"}},
		{"value from a file", dovecot, dcHead + "mail_location = </var/lib/monopanel/x\n", []string{"<file"}},
		{"crypto device", dovecot, dcHead + "ssl_crypto_device = pkcs11\n", []string{"ssl_crypto_device"}},
		{"continued key", dovecot, dcHead + "exe\\\ncutable = /tmp/x\n", nil},
		{"unclosed", dovecot, dcHead + "service imap {\n", nil},
		{"users uid", dcUsers, "a@example.com:{BLF-CRYPT}x:0:0::/root::\n", []string{"field 3"}},
		{"users home", dcUsers, "a@example.com:{BLF-CRYPT}x::::/home/ubuntu:::\n", []string{"field 6"}},
		{"users shell", dcUsers, "a@example.com:{BLF-CRYPT}x:::::/bin/sh::\n", []string{"field 7"}},
		{"users extra uid", dcUsers, "a@example.com:{BLF-CRYPT}x::::::userdb_uid=0\n", []string{"userdb_uid=0"}},
		{"users extra mail", dcUsers, "a@example.com:{BLF-CRYPT}x::::::userdb_mail=mbox:/root\n", []string{"userdb_mail"}},
		{"users name", dcUsers, "../../a:{BLF-CRYPT}x::::::\n", nil},
		{"other file", "/etc/dovecot/dovecot-sql", "driver = sqlite\n", []string{"dovecot-sql"}},
		// The passwd-file is the panel's users file and no other: another
		// file's extra fields (userdb_mail_plugin_dir, userdb_system_groups_user)
		// would go unread here.
		{"passwd-file of another name", dovecot, dcHead + "userdb {\n  driver = passwd-file\n  args = /etc/dovecot/evil.conf\n}\n", []string{"/etc/dovecot/evil.conf"}},
		{"passwd-file path 2.4", dovecot, dcHead + "userdb passwd-file {\n  passwd_file_path = /etc/dovecot/conf.d/x.conf\n}\n", []string{"passwd_file_path"}},
		{"passwd-file path at the top", dovecot, dcHead + "userdb_passwd_file_path = /etc/dovecot/x.ext\n", []string{"/etc/dovecot/x.ext"}},
		// dovecot 2.4 spells the settings of a filter with its prefix too, and
		// writes lists as sections.
		{"prefixed driver", dovecot, dcHead + "passdb_driver = checkpassword\n", []string{"checkpassword"}},
		{"prefixed driver in its passdb", dovecot, dcHead + "passdb x {\n  passdb_driver = lua\n}\n", []string{"lua"}},
		{"prefixed service user", dovecot, dcHead + "service imap-login {\n  service_user = root\n}\n", []string{"service_user = root"}},
		{"service user at the top", dovecot, dcHead + "service_user = root\n", []string{"root"}},
		{"prefixed executable", dovecot, dcHead + "service x {\n  service_executable = /bin/sh\n}\n", []string{"service_executable"}},
		{"prefixed listener mode", dovecot, dcHead + "service auth {\n  unix_listener auth-userdb {\n    unix_listener_mode = 0666\n  }\n}\n", []string{"0666"}},
		{"environment as a list", dovecot, dcHead + "import_environment {\n  LD_PRELOAD = /var/www/alex/evil.so\n}\n", []string{"import_environment"}},
		{"extra groups as a list", dovecot, dcHead + "service auth {\n  extra_groups {\n    docker = yes\n  }\n}\n", []string{"docker"}},
		{"access groups as a list", dovecot, dcHead + "mail_access_groups {\n  shadow = yes\n}\n", []string{"shadow"}},
		{"userdb fields at the top", dovecot, dcHead + "userdb_fields {\n  uid = 0\n}\n", []string{"uid"}},
		{"setting by its path", dovecot, dcHead + "service/auth/executable = /bin/sh\n", []string{"service/auth/executable"}},
		{"group of settings", dovecot, dcHead + "group @svc {\n  user = root\n}\n", []string{"groups of settings"}},
		{"group of settings with a name", dovecot, dcHead + "group @svc x {\n  user = root\n}\n", nil},
		{"group reference", dovecot, dcHead + "service auth {\n  @svc = x\n}\n", []string{"@svc"}},
		// str_unescape drops every backslash: the check sees what dovecot does.
		{"escaped dots in a quoted path", dovecot, dcHead + "mail_location = \"maildir:/var/mail/monopanel/\\.\\./\\.\\./etc\"\n", []string{"mail_location"}},
	})
	// A wildcard in the file name stays: it matches no "..".
	pass(t, dovecot, dcHead+"!include_try conf.d/*.conf\n!include /etc/dovecot/monopanel/local.conf\n")
}

func TestOpenDKIMRefusals(t *testing.T) {
	const conf = "/etc/opendkim.conf"
	const ok = "UserID opendkim\n"
	runRefusals(t, []refusal{
		{"user root", conf, "UserID root\n", []string{"UserID root"}},
		{"no user", conf, "Mode sv\n", []string{"UserID"}},
		{"socket elsewhere", conf, ok + "Socket local:/etc/cron.d/x\n", []string{"Socket"}},
		{"pid elsewhere", conf, ok + "PidFile /etc/passwd\n", []string{"PidFile"}},
		{"lua dataset", conf, ok + "KeyTable lua:/tmp/x.lua\n", []string{"lua:"}},
		{"policy script", conf, ok + "SetupPolicyScript /tmp/x.lua\n", []string{"SetupPolicyScript"}},
		{"report command", conf, ok + "ReportCommand /tmp/x\n", []string{"ReportCommand"}},
		{"defaults daemon options", dkimDefaults, "DAEMON_OPTS=\"-x /tmp/x.conf\"\n", []string{"DAEMON_OPTS"}},
		{"defaults user", dkimDefaults, "USER=root\n", []string{"USER=root"}},
		{"defaults command substitution", dkimDefaults, "SOCKET=$(touch /tmp/x)\n", []string{"SOCKET"}},
		{"defaults backticks", dkimDefaults, "RUNDIR=`id`\n", []string{"RUNDIR"}},
		{"defaults two commands", dkimDefaults, "RUNDIR=/run/opendkim; touch /tmp/x\n", nil},
		{"defaults socket elsewhere", dkimDefaults, "SOCKET=local:/etc/x.sock\n", []string{"SOCKET"}},
		{"defaults bare command", dkimDefaults, "touch /tmp/x\n", nil},
		// config_load reads lines with fgets into about a kilobyte: the rest
		// of a longer line is a line of its own.
		{"long line", conf, ok + "Syslog yes " + strings.Repeat(" ", 1100) + "UserID root\n", []string{"line 2", "bytes"}},
		{"include", conf, ok + "Include /var/www/alex/opendkim.conf\n", []string{"Include"}},
		{"user after a comment sign", conf, "UserID root # opendkim\n", []string{"UserID root"}},
	})
	// opendkim cuts every line at its first "#".
	pass(t, conf, "UserID opendkim # the package's account\nSocket local:/run/opendkim/opendkim.sock\n")
}

// The mail policy's file: the JSON the API writes, root's, of the milter's
// group, 0640 — nothing else, and no other owner or mode.
func TestMailPolicyFile(t *testing.T) {
	good := `{"version":1,"hostname":"mail.example.com","delivery_user":"vmail","accounts":{"alice":{"domains":["example.com"]}},"mailboxes":{"info@example.com":"alice"}}`
	if err := Check(mailpolicy.PolicyFile, []byte(good)); err != nil {
		t.Fatalf("the API's policy: %v", err)
	}
	for name, content := range map[string]string{
		"not json":       "accounts = alice\n",
		"unknown field":  `{"version":1,"accounts":{},"mailboxes":{},"exec":"/bin/sh"}`,
		"bad domain":     `{"version":1,"accounts":{"alice":{"domains":["*"]}},"mailboxes":{}}`,
		"bad login":      `{"version":1,"accounts":{"../root":{"domains":[]}},"mailboxes":{}}`,
		"orphan mailbox": `{"version":1,"accounts":{},"mailboxes":{"a@b.example":"bob"}}`,
		"two values":     good + good,
	} {
		if err := Check(mailpolicy.PolicyFile, []byte(content)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := CheckOwnership(mailpolicy.PolicyFile, "root", mailpolicy.User, 0o640); err != nil {
		t.Errorf("root:%s 0640: %v", mailpolicy.User, err)
	}
	if err := CheckOwnership(mailpolicy.PolicyFile, "", mailpolicy.User, 0o640); err != nil {
		t.Errorf("(root):%s 0640: %v", mailpolicy.User, err)
	}
	for _, o := range []struct {
		owner, group string
		mode         uint32
	}{
		{mailpolicy.User, mailpolicy.User, 0o640},
		{"root", "root", 0o644},
		{"root", mailpolicy.User, 0o644},
		{"root", mailpolicy.User, 0o660},
		{"root", mailpolicy.User, 0o600},
		{"root", "monopanel", 0o640},
		{"root", "", 0o640},
	} {
		if err := CheckOwnership(mailpolicy.PolicyFile, o.owner, o.group, o.mode); err == nil {
			t.Errorf("%s:%s %04o accepted", o.owner, o.group, o.mode)
		}
	}
	// The group is the policy's file's alone.
	if err := CheckOwnership("/etc/postfix/monopanel/mailboxes", "root", mailpolicy.User, 0o640); err == nil {
		t.Error("another map of the milter's group accepted")
	}
	// pickup's header table: exactly the panel's.
	if err := Check(mailpolicy.HeaderChecksFile, []byte(mailpolicy.HeaderChecksContent)); err != nil {
		t.Errorf("the panel's header table: %v", err)
	}
	for _, content := range []string{"", "/^Received:/ IGNORE\n", mailpolicy.HeaderChecksContent + "/^X:/ IGNORE\n"} {
		if err := Check(mailpolicy.HeaderChecksFile, []byte(content)); err == nil {
			t.Errorf("header table %q accepted", content)
		}
	}
}
