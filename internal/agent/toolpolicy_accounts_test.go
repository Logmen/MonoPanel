package agent

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
)

// The calls of internal/api to /v1/user/ensure pass; the ones that would
// make the API a sudoer, a docker member or the owner of a system account
// are refused.
func TestEnsureUserPolicy(t *testing.T) {
	for _, rel := range []string{debian13, alma9} {
		s := policyServer(t, rel)
		withAccounts(t, standardAccounts(s)...)
		web := s.profile.Web()
		nologin := s.profile.NologinShell()
		home := func(login string) string { return filepath.Join(s.cfg.WWWRoot, login) }
		sphinx := map[string]string{debian13: "sphinxsearch", alma9: "sphinx"}[rel]
		sphinxData := map[string]string{debian13: "/var/lib/sphinxsearch/data", alma9: "/var/lib/sphinx"}[rel]
		for _, req := range []EnsureUnixUserRequest{
			// ops_users.go provisionUser: a new SFTP client, a new shell client
			{Login: "carol", Home: home("carol"), Shell: nologin, UpdateShell: true, CreateHome: true, Groups: []string{sftpGroup}, RemoveGroups: []string{s.cfg.CLIGroup}, Comment: "MonoPanel user"},
			{Login: "dave", Home: home("dave"), Shell: "/bin/bash", UpdateShell: true, CreateHome: true, Groups: []string{s.cfg.CLIGroup}, RemoveGroups: []string{sftpGroup}, Comment: "MonoPanel user"},
			// the same for existing clients (a site fix, a migration)
			{Login: "alex", Home: home("alex"), Shell: "/bin/bash", UpdateShell: true, CreateHome: true, Groups: []string{s.cfg.CLIGroup}, RemoveGroups: []string{sftpGroup}, Comment: "MonoPanel user"},
			// refreshCLIGroup
			{Login: "bob", Groups: []string{s.cfg.CLIGroup}},
			// stack.go: nginx's user joins the web group; apache.go: the
			// distribution's Apache account leaves it
			{Login: web.NginxUser, System: true, Groups: []string{s.cfg.WebGroup}},
			{Login: web.ApacheUser, System: true, RemoveGroups: []string{s.cfg.WebGroup}},
			// ops_mail_install.go
			{Login: "vmail", System: true, Home: mailStore, PrimaryGroup: "vmail", Comment: "MonoPanel virtual mail"},
			{Login: "postfix", System: true, Groups: []string{"opendkim"}},
			// mail_policy.go ensureMailPolicyAccount: the milter's own user
			{Login: "monopanel-mail", System: true, PrimaryGroup: "monopanel-mail", Comment: "MonoPanel mail policy"},
			// ops_tools.go installSphinx3Binaries (and the packaged user)
			{Login: sphinx, System: true, Home: sphinxData, Shell: nologin},
		} {
			req := req
			if err := s.checkEnsureUser(&req); err != nil {
				t.Errorf("%s: %+v refused: %v", rel, req, err)
			}
		}
		for name, req := range map[string]EnsureUnixUserRequest{
			"sudo":                   {Login: "alex", Groups: []string{"sudo"}},
			"wheel":                  {Login: "alex", Groups: []string{"wheel"}},
			"docker":                 {Login: "carol", Home: home("carol"), Groups: []string{"docker"}},
			"root group":             {Login: "carol", Home: home("carol"), Groups: []string{"root"}},
			"shadow":                 {Login: "alex", Groups: []string{"shadow"}},
			"disk":                   {Login: "alex", Groups: []string{"disk"}},
			"adm":                    {Login: "alex", Groups: []string{"adm"}},
			"journal":                {Login: "alex", Groups: []string{"systemd-journal"}},
			"service group":          {Login: "alex", Groups: []string{s.cfg.ServiceGroup}},
			"web group for client":   {Login: "alex", Groups: []string{s.cfg.WebGroup}},
			"opendkim for client":    {Login: "alex", Groups: []string{"opendkim"}},
			"vmail for client":       {Login: "alex", Groups: []string{"vmail"}},
			"mail policy for client": {Login: "alex", Groups: []string{"monopanel-mail"}},
			"mail policy for nginx":  {Login: web.NginxUser, System: true, Groups: []string{"monopanel-mail"}},
			// The policy's socket is postfix's own: postfix needs no group of it.
			"mail policy for postfix": {Login: "postfix", System: true, Groups: []string{"monopanel-mail"}},
			"mail policy user shell":  {Login: "monopanel-mail", System: true, Shell: "/bin/bash"},
			"mail policy user home":   {Login: "monopanel-mail", System: true, Home: "/var/lib/monopanel"},
			"mail policy user group":  {Login: "monopanel-mail", System: true, Groups: []string{"postfix"}},
			"mail policy primary":     {Login: "monopanel-mail", System: true, PrimaryGroup: "monopanel"},
			"service user in policy":  {Login: "monopanel", Groups: []string{"monopanel-mail"}},
			"primary docker":          {Login: "carol", Home: home("carol"), PrimaryGroup: "docker"},
			"primary group client":    {Login: "carol", Home: home("carol"), PrimaryGroup: "users"},
			"new client in /home":     {Login: "carol", Home: "/home/carol", CreateHome: true},
			"new client in /root":     {Login: "carol", Home: "/root", CreateHome: true},
			"new client deep":         {Login: "carol", Home: home("x") + "/carol", CreateHome: true},
			"new client no home":      {Login: "carol", Groups: []string{s.cfg.CLIGroup}},
			"new client unclean":      {Login: "carol", Home: home("carol") + "/../carol"},
			"new client in a home":    {Login: "carol", Home: home("alex"), CreateHome: true},
			"new client elsewhere":    {Login: "carol", Home: home("html"), CreateHome: true},
			"client shell sh":         {Login: "alex", Shell: "/bin/sh", UpdateShell: true},
			"client odd shell":        {Login: "alex", Shell: "/usr/bin/sudo", UpdateShell: true},
			"remove from sudo":        {Login: "alex", RemoveGroups: []string{"sudo"}},
			"take over admin":         {Login: "admin", Groups: []string{s.cfg.CLIGroup}},
			"take over root":          {Login: "root", Groups: []string{s.cfg.CLIGroup}},
			"take over www-data":      {Login: "www-data", Shell: "/bin/bash", UpdateShell: true},
			"take over system uid":    {Login: "daemonish", Groups: []string{s.cfg.CLIGroup}},
			"take over docker user":   {Login: "sneaky", Groups: []string{s.cfg.CLIGroup}},
			"take over root group":    {Login: "rootgroup", Groups: []string{s.cfg.CLIGroup}},
			"service user":            {Login: "monopanel", Groups: []string{s.cfg.CLIGroup}},
			"login of a group":        {Login: "docker", Home: home("docker"), CreateHome: true},
			"system root":             {Login: "root", System: true, Groups: []string{s.cfg.WebGroup}},
			"system new":              {Login: "toor", System: true, Shell: nologin},
			"system shell":            {Login: "vmail", System: true, Home: mailStore, Shell: "/bin/bash"},
			"system update shell":     {Login: web.NginxUser, System: true, Shell: nologin, UpdateShell: true},
			"system home":             {Login: "vmail", System: true, Home: "/root"},
			"system create home":      {Login: "vmail", System: true, Home: mailStore, CreateHome: true},
			"system primary":          {Login: "vmail", System: true, PrimaryGroup: "root"},
			"system other group":      {Login: web.NginxUser, System: true, Groups: []string{"opendkim"}},
			"system remove":           {Login: "postfix", System: true, RemoveGroups: []string{"opendkim"}},
			"apache user joins web":   {Login: web.ApacheUser, System: true, Groups: []string{s.cfg.WebGroup}},
			"apache user joins its":   {Login: web.ApacheUser, System: true, Groups: []string{web.ApacheUser}},
			"apache user leaves cli":  {Login: web.ApacheUser, System: true, RemoveGroups: []string{s.cfg.CLIGroup}},
			"apache user leaves two":  {Login: web.ApacheUser, System: true, RemoveGroups: []string{s.cfg.WebGroup, "sudo"}},
			"apache user shell":       {Login: web.ApacheUser, System: true, RemoveGroups: []string{s.cfg.WebGroup}, Shell: nologin},
			"apache user home":        {Login: web.ApacheUser, System: true, RemoveGroups: []string{s.cfg.WebGroup}, Home: "/var/www"},
			"apache user made":        {Login: web.ApacheUser, System: true},
			"comment colon":           {Login: "carol", Home: home("carol"), Comment: "x:0:0"},
			"comment newline":         {Login: "carol", Home: home("carol"), Comment: "x\nroot::0:0::/:/bin/sh"},
		} {
			req := req
			err := s.checkEnsureUser(&req)
			var ae *Error
			if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
				t.Errorf("%s: %s must be refused with 403, got %v", rel, name, err)
			}
		}
		for _, req := range []EnsureUnixUserRequest{{Login: "../root"}, {Login: "alex", Groups: []string{"a b"}}, {Login: "alex", PrimaryGroup: "-g"}} {
			req := req
			var ae *Error
			if err := s.checkEnsureUser(&req); !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
				t.Errorf("%s: %+v: want 400, got %v", rel, req, err)
			}
		}
	}
}

// Passwords, shadow hashes, removal, runas and crontabs are for the panel's
// own accounts only.
func TestAccountRPCsRefuseOthersThanClients(t *testing.T) {
	ctx := context.Background()
	s := policyServer(t, debian13)
	withAccounts(t, standardAccounts(s)...)
	for _, login := range []string{"alex", "bob"} {
		if _, err := s.panelAccount(login); err != nil {
			t.Errorf("%s is a client: %v", login, err)
		}
	}
	others := []string{"root", "admin", "sneaky", "daemonish", "rootgroup", "www-data", "nginx", "postfix", "vmail", "monopanel"}
	for _, login := range others {
		if _, err := s.panelAccount(login); err == nil {
			t.Errorf("%s must not count as a client", login)
		}
		if _, err := s.setUnixPassword(ctx, &SetUnixPasswordRequest{Login: login, Password: "correct horse battery"}); err == nil {
			t.Errorf("password of %s must be refused", login)
		}
		if _, err := s.setUnixPassword(ctx, &SetUnixPasswordRequest{Login: login, Password: "$6$salt$hash", Encrypted: true}); err == nil {
			t.Errorf("hash of %s must be refused", login)
		}
		if _, err := s.unixShadow(ctx, &UnixShadowRequest{Login: login}); err == nil {
			t.Errorf("shadow of %s must be refused", login)
		}
		if _, err := s.runAsUser(ctx, &RunAsUserRequest{Login: login, Args: []string{"list", "/"}}); err == nil {
			t.Errorf("runas %s must be refused", login)
		}
		if _, err := s.removeUnixUser(ctx, &RemoveUnixUserRequest{Login: login, RemoveHome: true, HomeUnder: s.cfg.WWWRoot}); err == nil {
			t.Errorf("removing %s must be refused", login)
		}
	}
	if _, err := s.setUnixPassword(ctx, &SetUnixPasswordRequest{Login: "nobody-here", Password: "correct horse battery"}); err == nil {
		t.Error("a missing account must be refused")
	}
	// runas: only the file operations of the panel
	for _, args := range [][]string{{"--help"}, {"helper"}, {"agent"}, {"setup"}, {"../x"}} {
		if _, err := s.runAsUser(ctx, &RunAsUserRequest{Login: "alex", Args: args}); err == nil {
			t.Errorf("runas fsop %q must be refused", args)
		}
	}
	// removal: the web root's own directories, and the account's own home
	for name, req := range map[string]RemoveUnixUserRequest{
		"home under /":        {Login: "gone", RemoveHome: true, HomeUnder: "/", Home: "/etc"},
		"home outside":        {Login: "gone", RemoveHome: true, HomeUnder: s.cfg.WWWRoot, Home: "/etc"},
		"the web root":        {Login: "gone", RemoveHome: true, HomeUnder: s.cfg.WWWRoot, Home: s.cfg.WWWRoot},
		"another's home":      {Login: "gone", RemoveHome: true, HomeUnder: s.cfg.WWWRoot, Home: filepath.Join(s.cfg.WWWRoot, "bob")},
		"deeper":              {Login: "gone", RemoveHome: true, HomeUnder: s.cfg.WWWRoot, Home: filepath.Join(s.cfg.WWWRoot, "gone", "data")},
		"client's other home": {Login: "alex", RemoveHome: true, HomeUnder: s.cfg.WWWRoot, Home: filepath.Join(s.cfg.WWWRoot, "bob")},
	} {
		req := req
		if _, err := s.removeUnixUser(ctx, &req); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

// /v1/group/ensure makes the panel's groups and no other.
func TestEnsureGroupPolicy(t *testing.T) {
	ctx := context.Background()
	s := policyServer(t, debian13)
	for _, g := range []string{"wheel", "sudo", "admins", "docker", "lpadmin"} {
		var ae *Error
		if _, err := s.ensureGroup(ctx, &EnsureGroupRequest{Name: g, System: true}); !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
			t.Errorf("%s: want 403, got %v", g, err)
		}
	}
	var ae *Error
	if _, err := s.ensureGroup(ctx, &EnsureGroupRequest{Name: "Bad Name"}); !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
		t.Errorf("bad name: %v", err)
	}
}
