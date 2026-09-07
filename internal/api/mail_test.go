package api

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"monopanel/internal/store"
)

// mailFixture is a panel with the mail stack installed against the fake agent.
// A certificate for the mail host is stored first: without it the install job
// would go looking for one in real DNS.
func newMailFixture(t *testing.T) *siteFixture {
	t.Helper()
	f := newSiteFixture(t)
	// За фейковым агентом никто портов не занимает: считаем, что слушает наш
	// же postfix — его установка узнаёт по баннеру с именем хоста.
	f.s.SetPortProbe(func(int, bool) (bool, string) { return true, "220 mail.example.com ESMTP" })
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "key.pem")
	if _, err := EnsureSelfSigned(cert, key, []string{"mail.example.com"}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(60 * 24 * time.Hour)
	if err := f.db.UpsertCertificate(f.ctx, &store.Certificate{
		Name: "mail.example.com", Names: []string{"mail.example.com"}, Kind: store.CertKindACME,
		Status: store.CertValid, NotAfter: &until, CertPath: cert, KeyPath: key,
	}); err != nil {
		t.Fatal(err)
	}
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/mail/install", map[string]any{"hostname": "mail.example.com"}, http.StatusAccepted, &out)
	if job := f.waitJob(out.JobID); job.Status != store.JobDone {
		t.Fatalf("установка почты: %s %s", job.Status, job.Error)
	}
	return f
}

func TestMailInstallRendersConfiguration(t *testing.T) {
	f := newMailFixture(t)

	main, ok := f.agent.File("/etc/postfix/main.cf")
	if !ok {
		t.Fatalf("main.cf не записан; файлы: %v", f.agent.Files())
	}
	for _, want := range []string{
		"myhostname = mail.example.com",
		"virtual_transport = lmtp:unix:private/dovecot-lmtp",
		"virtual_mailbox_maps = hash:/etc/postfix/monopanel/mailboxes",
		"smtpd_sasl_type = dovecot",
		"smtpd_tls_cert_file = ",
		"smtpd_milters = local:/run/opendkim/opendkim.sock",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("main.cf без %q", want)
		}
	}
	// Проверка «отправитель равен логину» нужна только на портах отправки,
	// поэтому она в master.cf, а не в main.cf.
	master, _ := f.agent.File("/etc/postfix/master.cf")
	for _, want := range []string{"smtp         inet", "submission   inet", "smtpd_tls_wrappermode=yes", "submission-header-cleanup", "reject_sender_login_mismatch"} {
		if !strings.Contains(master, want) {
			t.Errorf("master.cf без %q", want)
		}
	}
	dove, ok := f.agent.File("/etc/dovecot/dovecot.conf")
	if !ok {
		t.Fatal("dovecot.conf не записан")
	}
	for _, want := range []string{
		"mail_location = maildir:/var/mail/monopanel/%d/%n",
		"scheme=BLF-CRYPT username_format=%u /etc/dovecot/monopanel/users",
		"unix_listener /var/spool/postfix/private/dovecot-lmtp",
		"ssl = required",
		"port = 993",
		"quota = maildir:User quota",
	} {
		if !strings.Contains(dove, want) {
			t.Errorf("dovecot.conf без %q", want)
		}
	}
	for _, unit := range []string{"postfix.service", "dovecot.service", "opendkim.service"} {
		if f.agent.UnitAction(unit) == "" {
			t.Errorf("%s не перезапускался", unit)
		}
	}
	postmapped := map[string]bool{}
	for _, tool := range f.agent.Tools() {
		if tool.Name == "postmap" && len(tool.Args) == 1 {
			postmapped[filepath.Base(tool.Args[0])] = true
		}
	}
	for _, m := range []string{"domains", "mailboxes", "aliases", "senders"} {
		if !postmapped[m] {
			t.Errorf("карта %s не собрана postmap; вызовы: %v", m, f.agent.Tools())
		}
	}
}

// Порт 25 может держать чужой демон (на dev-хосте это приёмник mail-tester).
// Установка не должна ронять postfix об чужой сокет: она снимает приём почты
// снаружи и говорит об этом.
func TestMailInstallYieldsPort25ToAnotherDaemon(t *testing.T) {
	f := newSiteFixture(t)
	f.s.SetPortProbe(func(port int, _ bool) (bool, string) {
		if port == 25 {
			return true, "220 mail-test.example.org MailTester"
		}
		return true, "220 mail.example.com ESMTP"
	})
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/mail/install", map[string]any{"hostname": "mail.example.com"}, http.StatusAccepted, &out)
	if job := f.waitJob(out.JobID); job.Status != store.JobDone {
		t.Fatalf("установка почты: %s %s", job.Status, job.Error)
	}
	master, _ := f.agent.File("/etc/postfix/master.cf")
	if strings.Contains(master, "smtp         inet") {
		t.Error("postfix всё равно пытается занять 25 порт")
	}
	if !strings.Contains(master, "submission   inet") {
		t.Error("отправка через 587 должна остаться")
	}
	var st struct {
		Warnings []string `json:"warnings"`
		Port25   bool     `json:"port25"`
	}
	f.call(http.MethodGet, "/mail", nil, http.StatusOK, &st)
	if st.Port25 {
		t.Error("в настройках приём на 25 порту остался включённым")
	}
	found := false
	for _, w := range st.Warnings {
		if strings.Contains(w, "25") {
			found = true
		}
	}
	if !found {
		t.Errorf("статус не предупреждает про 25 порт: %v", st.Warnings)
	}
}

func TestMailboxAndAliasReachTheMaps(t *testing.T) {
	f := newMailFixture(t)
	f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "example.com", "user": "alex"}, http.StatusCreated, nil)

	var box struct {
		Mailbox  *store.Mailbox `json:"mailbox"`
		Password string         `json:"password"`
	}
	f.call(http.MethodPost, "/mail/mailboxes", map[string]any{"address": "ivan@example.com", "quota_mb": 512}, http.StatusCreated, &box)
	if box.Password == "" {
		t.Fatal("сгенерированный пароль не вернулся")
	}
	users, _ := f.agent.File("/etc/dovecot/monopanel/users")
	line := ""
	for _, l := range strings.Split(users, "\n") {
		if strings.HasPrefix(l, "ivan@example.com:") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("ящика нет в passwd-файле:\n%s", users)
	}
	// Формат dovecot: user:password:uid:gid:gecos:home:shell:extra. Поля
	// uid/gid/home приходят из userdb, а хвост с квотой содержит двоеточие —
	// dovecot собирает его обратно, поэтому считать двоеточия нельзя.
	rest, ok := strings.CutPrefix(line, "ivan@example.com:")
	if !ok {
		t.Fatalf("строка не начинается с адреса: %q", line)
	}
	hash, tail, _ := strings.Cut(rest, ":")
	if !strings.HasPrefix(hash, "{BLF-CRYPT}$2") {
		t.Errorf("пароль хранится не bcrypt-схемой dovecot: %q", hash)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(strings.TrimPrefix(hash, "{BLF-CRYPT}")), []byte(box.Password)); err != nil {
		t.Errorf("хеш не совпадает с выданным паролем: %v", err)
	}
	if tail != ":::::userdb_quota_rule=*:storage=512M" {
		t.Errorf("хвост строки passwd-файла: %q", tail)
	}
	boxes, _ := f.agent.File("/etc/postfix/monopanel/mailboxes")
	if !strings.Contains(boxes, "ivan@example.com example.com/ivan/") {
		t.Errorf("ящика нет в карте postfix:\n%s", boxes)
	}

	// Алиас и catch-all: письмо должно уходить в ящик, а отправлять от имени
	// алиаса разрешено тому, куда он ведёт.
	f.call(http.MethodPost, "/mail/aliases", map[string]any{"address": "info@example.com", "destinations": []string{"ivan@example.com"}}, http.StatusCreated, nil)
	f.call(http.MethodPost, "/mail/aliases", map[string]any{"address": "@example.com", "destinations": []string{"ivan@example.com"}}, http.StatusCreated, nil)
	aliases, _ := f.agent.File("/etc/postfix/monopanel/aliases")
	for _, want := range []string{"info@example.com ivan@example.com", "@example.com ivan@example.com"} {
		if !strings.Contains(aliases, want) {
			t.Errorf("карта алиасов без %q:\n%s", want, aliases)
		}
	}
	senders, _ := f.agent.File("/etc/postfix/monopanel/senders")
	if !strings.Contains(senders, "info@example.com ivan@example.com") {
		t.Errorf("алиас не попал в sender_login_maps:\n%s", senders)
	}

	// Выключенный ящик исчезает и из карты postfix, и из паролей dovecot:
	// почта на него не принимается, войти в IMAP нельзя.
	f.call(http.MethodPatch, "/mail/mailboxes/ivan@example.com", map[string]any{"active": false}, http.StatusOK, nil)
	users, _ = f.agent.File("/etc/dovecot/monopanel/users")
	boxes, _ = f.agent.File("/etc/postfix/monopanel/mailboxes")
	if strings.Contains(users, "ivan@example.com:") || strings.Contains(boxes, "ivan@example.com ") {
		t.Error("выключенный ящик остался в конфигурации")
	}
}

// Домен-приёмник обрывает список проверок на OK: письмо от отправителя с
// несуществующим доменом должно дойти до него, а не улететь в отказ.
func TestLenientDomainReachesTheAccessMap(t *testing.T) {
	f := newMailFixture(t)
	f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "example.com", "user": "alex"}, http.StatusCreated, nil)
	f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "sink.example.com", "user": "alex", "lenient": true}, http.StatusCreated, nil)

	lenient, _ := f.agent.File("/etc/postfix/monopanel/lenient")
	if !strings.Contains(lenient, "sink.example.com OK") {
		t.Errorf("домена-приёмника нет в карте:\n%s", lenient)
	}
	if strings.Contains(lenient, "\nexample.com OK") {
		t.Errorf("обычный домен попал в карту мягких проверок:\n%s", lenient)
	}
	main, _ := f.agent.File("/etc/postfix/main.cf")
	before := strings.Index(main, "check_recipient_access hash:/etc/postfix/monopanel/lenient")
	after := strings.Index(main, "reject_unknown_sender_domain")
	if before < 0 || after < 0 || before > after {
		t.Error("карта приёмников должна стоять перед строгими проверками отправителя")
	}

	// Обратное переключение снимает поблажку.
	f.call(http.MethodPatch, "/mail/domains/sink.example.com", map[string]any{"lenient": false}, http.StatusOK, nil)
	lenient, _ = f.agent.File("/etc/postfix/monopanel/lenient")
	if strings.Contains(lenient, "sink.example.com") {
		t.Errorf("домен остался в карте после выключения:\n%s", lenient)
	}
}

func TestMailDomainRejectsDuplicateAndBadNames(t *testing.T) {
	f := newMailFixture(t)
	f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "example.com", "user": "alex"}, http.StatusCreated, nil)
	f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "example.com", "user": "alex"}, http.StatusConflict, nil)
	f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "не домен", "user": "alex"}, http.StatusUnprocessableEntity, nil)
	// Ящик с адресом уже занятого алиаса перехватил бы чужую почту.
	f.call(http.MethodPost, "/mail/mailboxes", map[string]any{"address": "ivan@example.com"}, http.StatusCreated, nil)
	f.call(http.MethodPost, "/mail/aliases", map[string]any{"address": "ivan@example.com", "destinations": []string{"other@example.com"}}, http.StatusConflict, nil)
	f.call(http.MethodPost, "/mail/mailboxes", map[string]any{"address": "ivan@other.example"}, http.StatusUnprocessableEntity, nil)
}

func TestSplitAddress(t *testing.T) {
	for _, c := range []struct {
		in    string
		local string
		ok    bool
	}{
		{"user@example.com", "user", true},
		{"User@Example.COM", "user", true},
		{"first.last@mail.example.com", "first.last", true},
		{"@example.com", "@", true}, // catch-all
		{"user", "", false},
		{"user@localhost", "", false},
		{"иван@example.com", "", false},
		{"user name@example.com", "", false},
	} {
		local, _, err := splitAddress(c.in)
		if c.ok != (err == nil) {
			t.Errorf("splitAddress(%q): err = %v", c.in, err)
			continue
		}
		if c.ok && local != c.local {
			t.Errorf("splitAddress(%q) = %q, ожидалось %q", c.in, local, c.local)
		}
	}
}

// Ключ DKIM годен, только если TXT-запись собрана из того же ключа, что лежит
// у opendkim: иначе подпись есть, а проверка не проходит.
func TestDKIMKeyMatchesItsRecord(t *testing.T) {
	priv, pub, err := newDKIMKey()
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(priv))
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		t.Fatalf("opendkim ждёт PEM с приватным ключом, получил %v", block)
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	der, err := base64.StdEncoding.DecodeString(pub)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		t.Fatal(err)
	}
	if !key.PublicKey.Equal(parsed.(*rsa.PublicKey)) {
		t.Error("в TXT-записи оказался чужой открытый ключ")
	}
	if key.N.BitLen() != 2048 {
		t.Errorf("длина ключа %d бит", key.N.BitLen())
	}
}
