package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/store"
)

// usersLine is the line of dovecot's users file for an address.
func usersLine(f *siteFixture, address string) string {
	f.t.Helper()
	users, _ := f.agent.File(dovecotUsers)
	for _, l := range strings.Split(users, "\n") {
		if strings.HasPrefix(l, address+":") {
			return l
		}
	}
	f.t.Fatalf("%s is not in the users file:\n%s", address, users)
	return ""
}

// A quota of 0 is no limit in the syntax of either dovecot — 2.3 reads a zero
// limit of the * rule as none, 2.4 takes "unlimited" — and an omitted quota is
// 1024 MB, on create and on edit.
func TestMailboxQuotaZeroIsUnlimited(t *testing.T) {
	for _, c := range []struct {
		name                string
		versions            map[string]string
		zero, mb512, mb1024 string
	}{
		{"dovecot 2.3", nil, "userdb_quota_rule=*:storage=0", "userdb_quota_rule=*:storage=512M", "userdb_quota_rule=*:storage=1024M"},
		{"dovecot 2.4", map[string]string{"dovecot-core": "1:2.4.1+dfsg1-6ubuntu1"}, "userdb_quota_storage_size=unlimited", "userdb_quota_storage_size=512M", "userdb_quota_storage_size=1024M"},
	} {
		f := newMailFixtureWith(t, c.versions)
		f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "example.com", "user": "alex"}, http.StatusCreated, nil)
		var box struct {
			Mailbox *store.Mailbox `json:"mailbox"`
		}
		f.call(http.MethodPost, "/mail/mailboxes", map[string]any{"address": "zero@example.com", "quota_mb": 0}, http.StatusCreated, &box)
		if box.Mailbox.QuotaMB != 0 {
			t.Fatalf("%s: quota 0 stored as %d", c.name, box.Mailbox.QuotaMB)
		}
		f.call(http.MethodPost, "/mail/mailboxes", map[string]any{"address": "default@example.com"}, http.StatusCreated, &box)
		if box.Mailbox.QuotaMB != 1024 {
			t.Fatalf("%s: an omitted quota stored as %d", c.name, box.Mailbox.QuotaMB)
		}
		check := func(address, want string) {
			t.Helper()
			if line := usersLine(f, address); !strings.HasSuffix(line, "::::::"+want) {
				t.Errorf("%s: %s: %q, want the quota field %q", c.name, address, line, want)
			}
		}
		check("zero@example.com", c.zero)
		check("default@example.com", c.mb1024)

		f.call(http.MethodPatch, "/mail/mailboxes/zero@example.com", map[string]any{"quota_mb": 512}, http.StatusOK, nil)
		check("zero@example.com", c.mb512)
		f.call(http.MethodPatch, "/mail/mailboxes/zero@example.com", map[string]any{"quota_mb": 0}, http.StatusOK, nil)
		check("zero@example.com", c.zero)
		if users, _ := f.agent.File(dovecotUsers); strings.Contains(users, "storage=0M") || strings.Contains(users, "size=0M") {
			t.Errorf("%s: a zero size limit in the users file:\n%s", c.name, users)
		}
	}
}

// Every name that goes into the mail maps is checked once more when they are
// written. A value that came into the store another way — an import from a
// hostile source, an edited database — must not add a line of its own: the
// apply stops, names the entry and writes nothing.
func TestMailMapsRefuseForgedEntries(t *testing.T) {
	f := newMailFixture(t)
	f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "example.com", "user": "alex"}, http.StatusCreated, nil)
	f.call(http.MethodPost, "/mail/mailboxes", map[string]any{"address": "ivan@example.com"}, http.StatusCreated, nil)
	domain, err := f.db.GetMailDomain(f.ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	maps := []string{dovecotUsers, postfixMapsDir + "/mailboxes", postfixMapsDir + "/aliases", postfixMapsDir + "/senders", postfixMapsDir + "/domains", dkimDir + "/KeyTable", dkimDir + "/SigningTable", dkimDir + "/TrustedHosts"}
	before := map[string]string{}
	for _, p := range maps {
		before[p], _ = f.agent.File(p)
	}
	apply := func(name, want string) {
		t.Helper()
		var ref struct {
			JobID int64 `json:"job_id"`
		}
		f.call(http.MethodPost, "/mail/apply", nil, http.StatusAccepted, &ref)
		job := f.waitJob(ref.JobID)
		if job.Status != store.JobFailed || !strings.Contains(job.Error, want) || !strings.Contains(job.Error, "nothing was written") {
			t.Errorf("%s: %s %q, want an error naming %q", name, job.Status, job.Error, want)
		}
		for _, p := range maps {
			if now, _ := f.agent.File(p); now != before[p] {
				t.Errorf("%s: %s was rewritten:\n%s", name, p, now)
			}
		}
	}
	forged := "x\nvictim@example.net:{PLAIN}secret::::::"

	box := func(local, hash string) func() {
		b := &store.Mailbox{DomainID: domain.ID, LocalPart: local, Address: local + "@example.com", PasswordHash: hash, QuotaMB: 0, Active: true}
		if err := f.db.CreateMailbox(f.ctx, b); err != nil {
			t.Fatal(err)
		}
		return func() { f.db.DeleteMailbox(f.ctx, b.ID) } //nolint:errcheck // test cleanup
	}
	undo := box("anna", "{BLF-CRYPT}"+forged)
	apply("a line break in a password hash", `mailbox "anna@example.com" has a password hash with a colon or a control character`)
	undo()
	undo = box("anna", "{PLAIN}x:0:0::/root")
	apply("a colon in a password hash", `mailbox "anna@example.com" has a password hash with a colon`)
	undo()
	undo = box("../../etc", "{BLF-CRYPT}$2a$11$abc")
	apply("a path in the local part", `mailbox "../../etc@example.com"`)
	undo()
	undo = box("anna victim", "{BLF-CRYPT}$2a$11$abc")
	apply("a blank in an address", `mailbox "anna victim@example.com"`)
	undo()
	// The address is what dovecot signs in with and finds the maildir by:
	// a mailbox of this domain must not carry another domain's address.
	other := &store.Mailbox{DomainID: domain.ID, LocalPart: "ceo", Address: "ceo@victim.example.net", PasswordHash: "{BLF-CRYPT}$2a$11$abc", Active: true}
	if err := f.db.CreateMailbox(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	apply("another domain's address", `mailbox "ceo@victim.example.net" (local part "ceo") of mail domain "example.com" is not the local part at the domain`)
	f.db.DeleteMailbox(f.ctx, other.ID) //nolint:errcheck // test cleanup

	alias := func(source, dest string) func() {
		a := &store.MailAlias{DomainID: domain.ID, Source: source, Destination: dest, Active: true}
		if err := f.db.CreateMailAlias(f.ctx, a); err != nil {
			t.Fatal(err)
		}
		return func() { f.db.DeleteMailAlias(f.ctx, a.ID) } //nolint:errcheck // test cleanup
	}
	undo = alias("info", "ivan@example.com outsider@example.net")
	apply("a blank between recipients", `the recipient "ivan@example.com outsider@example.net" of alias "info"`)
	undo()
	undo = alias("info\n@example.org", "ivan@example.com")
	apply("a line break in the alias", `alias "info\n@example.org" of mail domain "example.com"`)
	undo()

	// A line break adds a line; "*" is a wildcard in opendkim's
	// SigningTable, where "*@*" would sign every tenant's mail with this key.
	for _, name := range []string{"evil.example\nexample.net", "*", "*.example.net"} {
		if err := f.db.CreateMailDomain(f.ctx, &store.MailDomain{UserID: domain.UserID, Name: name, Active: true}); err != nil {
			t.Fatal(err)
		}
		apply(fmt.Sprintf("the domain %q", name), fmt.Sprintf("mail domain %q is not a domain name", name))
		evil, _ := f.db.GetMailDomain(f.ctx, name)
		f.db.DeleteMailDomain(f.ctx, evil.ID) //nolint:errcheck // test cleanup
	}

	selector := domain.DKIMSelector
	domain.DKIMSelector = "../../../etc/cron.d/x"
	if err := f.db.UpdateMailDomain(f.ctx, domain); err != nil {
		t.Fatal(err)
	}
	apply("a path in the DKIM selector", `the DKIM selector "../../../etc/cron.d/x" of mail domain "example.com"`)
	domain.DKIMSelector = selector
	f.db.UpdateMailDomain(f.ctx, domain) //nolint:errcheck // test cleanup

	c := f.s.loadMailConfig(f.ctx)
	host := c.Hostname
	c.Hostname = "mail.example.com\n0.0.0.0/0"
	f.s.saveMailConfig(f.ctx, c) //nolint:errcheck // test setup
	apply("a line break in the hostname", `the mail server hostname "mail.example.com\n0.0.0.0/0"`)
	c.Hostname = host
	f.s.saveMailConfig(f.ctx, c) //nolint:errcheck // test cleanup

	// With the forged entries gone, the maps are written again.
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/mail/apply", nil, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("a clean apply: %s %s", job.Status, job.Error)
	}
	if users, _ := f.agent.File(dovecotUsers); strings.Contains(users, "victim") || !strings.Contains(users, "ivan@example.com:{BLF-CRYPT}") {
		t.Fatalf("users file:\n%s", users)
	}
}

// A mailbox that arrived without a password (a migration whose source could
// not hand it over) is no forged entry: it keeps receiving mail with an empty
// password field, which no password matches, and the maps of everybody else
// are written as usual.
func TestMailMapsKeepAMailboxWithoutAPassword(t *testing.T) {
	f := newMailFixture(t)
	f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "example.com", "user": "alex"}, http.StatusCreated, nil)
	domain, err := f.db.GetMailDomain(f.ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateMailbox(f.ctx, &store.Mailbox{DomainID: domain.ID, LocalPart: "anna", Address: "anna@example.com", QuotaMB: 1024, Active: true}); err != nil {
		t.Fatal(err)
	}
	f.call(http.MethodPost, "/mail/mailboxes", map[string]any{"address": "ivan@example.com"}, http.StatusCreated, nil)
	if line := usersLine(f, "anna@example.com"); line != "anna@example.com:::::::userdb_quota_rule=*:storage=1024M" {
		t.Fatalf("the mailbox without a password: %q", line)
	}
	if line := usersLine(f, "ivan@example.com"); !strings.HasPrefix(line, "ivan@example.com:{BLF-CRYPT}$2") {
		t.Fatalf("a mailbox added next to it: %q", line)
	}
	if boxes, _ := f.agent.File(postfixMapsDir + "/mailboxes"); !strings.Contains(boxes, "\nanna@example.com example.com/anna/\n") {
		t.Fatalf("postfix no longer delivers to it:\n%s", boxes)
	}
}

// Deleting a domain or purging a mailbox removes directories named after
// them: a name that came in another way must not lead into another tenant's
// directory. The entry goes, its files stay.
func TestMailDeleteRemovesOnlyItsOwnDirectories(t *testing.T) {
	f := newMailFixture(t)
	f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "example.com", "user": "alex"}, http.StatusCreated, nil)
	domain, err := f.db.GetMailDomain(f.ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	removed := func() []string {
		t.Helper()
		out := []string{}
		for _, p := range f.agent.Removed() {
			if strings.HasPrefix(p, mailBase+"/") {
				out = append(out, p)
			}
		}
		return out
	}

	// The address is a good one, the local part leads elsewhere.
	if err := f.db.CreateMailbox(f.ctx, &store.Mailbox{DomainID: domain.ID, LocalPart: "../victim.example.net/ceo", Address: "ceo@example.com", PasswordHash: "{BLF-CRYPT}$2a$11$abc", Active: true}); err != nil {
		t.Fatal(err)
	}
	f.call(http.MethodDelete, "/mail/mailboxes/ceo@example.com?purge=true", nil, http.StatusNoContent, nil)
	if got := removed(); len(got) != 0 {
		t.Fatalf("a forged local part was purged: %v", got)
	}
	if _, err := f.db.GetMailbox(f.ctx, "ceo@example.com"); err == nil {
		t.Fatal("the forged mailbox is still there")
	}
	f.call(http.MethodPost, "/mail/mailboxes", map[string]any{"address": "ivan@example.com"}, http.StatusCreated, nil)
	f.call(http.MethodDelete, "/mail/mailboxes/ivan@example.com?purge=true", nil, http.StatusNoContent, nil)
	if got := removed(); len(got) != 1 || got[0] != mailBase+"/example.com/ivan" {
		t.Fatalf("purging a mailbox: %v", got)
	}

	if err := f.db.CreateMailDomain(f.ctx, &store.MailDomain{UserID: domain.UserID, Name: "..", Active: false}); err != nil {
		t.Fatal(err)
	}
	f.call(http.MethodDelete, "/mail/domains/..", nil, http.StatusNoContent, nil)
	if got := removed(); len(got) != 1 {
		t.Fatalf("a forged domain's directories were removed: %v", got)
	}
	f.call(http.MethodDelete, "/mail/domains/example.com", nil, http.StatusNoContent, nil)
	if got := removed(); len(got) != 2 || got[1] != mailBase+"/example.com" {
		t.Fatalf("deleting a domain: %v", got)
	}
}

// The API itself lets none of these values in: the mail server's name is a
// host name, a domain is one DNS allows, a mailbox's display name is one line,
// and a password bcrypt cannot take is the request's fault.
func TestMailInputIsChecked(t *testing.T) {
	f := newMailFixture(t)
	for name, body := range map[string]map[string]any{
		"a line break in the hostname": {"hostname": "mail.example.com\nmynetworks = 0.0.0.0/0"},
		"a blank in the hostname":      {"hostname": "mail.example.com x"},
		"a bare name":                  {"hostname": "mail"},
		"a blocklist with a blank":     {"rbl": []string{"zen.spamhaus.org x"}},
	} {
		if code, detail := fwCall(f, http.MethodPut, "/mail/settings", body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, code, detail)
		}
	}
	if c := f.s.loadMailConfig(f.ctx); c.Hostname != "mail.example.com" {
		t.Fatalf("hostname changed: %q", c.Hostname)
	}
	if code, detail := fwCall(f, http.MethodPost, "/mail/install", map[string]any{"hostname": "mail.example.com\nmynetworks = 0.0.0.0/0"}); code != http.StatusUnprocessableEntity {
		t.Errorf("install with a line break in the hostname: %d %s", code, detail)
	}
	f.call(http.MethodPut, "/mail/settings", map[string]any{"hostname": "Mail.Example.COM."}, http.StatusOK, nil)
	if c := f.s.loadMailConfig(f.ctx); c.Hostname != "mail.example.com" {
		t.Fatalf("hostname not normalised: %q", c.Hostname)
	}

	long := strings.Repeat("a", 64) + ".example.com"
	if code, detail := fwCall(f, http.MethodPost, "/mail/domains", map[string]any{"name": long, "user": "alex"}); code != http.StatusUnprocessableEntity {
		t.Errorf("a label longer than DNS allows: %d %s", code, detail)
	}
	f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "example.com", "user": "alex"}, http.StatusCreated, nil)
	if code, detail := fwCall(f, http.MethodPost, "/mail/aliases", map[string]any{"address": "info@example.com", "destinations": []string{"ivan@" + long}}); code != http.StatusUnprocessableEntity {
		t.Errorf("a recipient domain longer than DNS allows: %d %s", code, detail)
	}
	if code, detail := fwCall(f, http.MethodPost, "/mail/mailboxes", map[string]any{"address": "ivan@example.com", "name": "Ivan\x1b[2J"}); code != http.StatusUnprocessableEntity {
		t.Errorf("a control character in the name: %d %s", code, detail)
	}
	if code, detail := fwCall(f, http.MethodPost, "/mail/mailboxes", map[string]any{"address": "ivan@example.com", "password": strings.Repeat("п", 37)}); code != http.StatusUnprocessableEntity || !strings.Contains(detail, "72 bytes") {
		t.Errorf("a password over 72 bytes: %d %s", code, detail)
	}
	f.call(http.MethodPost, "/mail/mailboxes", map[string]any{"address": "ivan@example.com", "name": "Иван Петров", "password": strings.Repeat("п", 36)}, http.StatusCreated, nil)
	if code, detail := fwCall(f, http.MethodPatch, "/mail/mailboxes/ivan@example.com", map[string]any{"name": "Ivan\nPetrov"}); code != http.StatusUnprocessableEntity {
		t.Errorf("a line break in the name: %d %s", code, detail)
	}
	if code, detail := fwCall(f, http.MethodPatch, "/mail/mailboxes/ivan@example.com", map[string]any{"password": strings.Repeat("x", 73)}); code != http.StatusUnprocessableEntity || !strings.Contains(detail, "72 bytes") {
		t.Errorf("a new password over 72 bytes: %d %s", code, detail)
	}
}
