package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"monopanel/internal/agent"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

// Where the mail stack keeps its state. The panel owns every one of these
// files: it rewrites them in full on each change, so nothing here is a place
// for hand edits.
const (
	settingMail    = "mail"
	mailBase       = "/var/mail/monopanel"
	postfixDir     = "/etc/postfix"
	postfixMapsDir = "/etc/postfix/monopanel"
	dovecotConf    = "/etc/dovecot/dovecot.conf"
	dovecotDir     = "/etc/dovecot/monopanel"
	dovecotUsers   = "/etc/dovecot/monopanel/users"
	dkimDir        = "/etc/opendkim"
	dkimConf       = "/etc/opendkim.conf"
	dkimDefaults   = "/etc/default/opendkim"
	dkimSocketPath = "/run/opendkim/opendkim.sock"
	vmailUser      = "vmail"

	postfixService = "postfix.service"
	dovecotService = "dovecot.service"
	dkimService    = "opendkim.service"
)

var (
	mailDomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	localPartRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)
)

// mailConfig is the server-wide half of the mail settings; domains, mailboxes
// and aliases live in their own tables.
type mailConfig struct {
	// Булевы поля пишутся всегда: с omitempty выключенный порт 25 исчезал бы
	// из JSON и при следующей загрузке снова становился включённым.
	Installed      bool     `json:"installed"`
	Hostname       string   `json:"hostname,omitempty"`
	MaxSizeMB      int      `json:"max_size_mb,omitempty"`
	POP3           bool     `json:"pop3"`
	DKIM           bool     `json:"dkim"`
	Port25         bool     `json:"port25"`
	RBL            []string `json:"rbl,omitempty"`
	VmailUID       int      `json:"vmail_uid,omitempty"`
	VmailGID       int      `json:"vmail_gid,omitempty"`
	Webmail        string   `json:"webmail,omitempty"`
	WebmailVersion string   `json:"webmail_version,omitempty"`
	PostfixVersion string   `json:"postfix_version,omitempty"`
	DovecotVersion string   `json:"dovecot_version,omitempty"`
}

func (s *Server) loadMailConfig(ctx context.Context) mailConfig {
	c := mailConfig{MaxSizeMB: 50, POP3: true, DKIM: true, Port25: true}
	if raw, err := s.db.GetSetting(ctx, settingMail); err == nil {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	if c.MaxSizeMB <= 0 {
		c.MaxSizeMB = 50
	}
	return c
}

func (s *Server) saveMailConfig(ctx context.Context, c mailConfig) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.db.SetSetting(ctx, settingMail, string(b))
}

// mailTLS returns the certificate the mail services should present. A valid
// panel certificate for the mail hostname wins; otherwise a self-signed pair
// under <data>/mail keeps the daemons able to start (and clients complaining).
func (s *Server) mailTLS(ctx context.Context, c mailConfig) (certPath, keyPath, kind string, until *time.Time) {
	if c.Hostname == "" {
		return "", "", "none", nil
	}
	if cert, err := s.db.GetCertificateByName(ctx, c.Hostname); err == nil {
		if cert.CertPath != "" && cert.KeyPath != "" && cert.NotAfter != nil && time.Now().Before(*cert.NotAfter) {
			if _, err := os.Stat(cert.CertPath); err == nil {
				return cert.CertPath, cert.KeyPath, cert.Kind, cert.NotAfter
			}
		}
	}
	dir := filepath.Join(s.cfg.DataDir, "mail")
	cp, kp := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", "", "none", nil
	}
	if _, err := EnsureSelfSigned(cp, kp, []string{c.Hostname}); err != nil {
		s.log.Warn("mail: self-signed certificate", "err", err)
		return "", "", "none", nil
	}
	return cp, kp, store.CertKindSelfSigned, nil
}

// mailMaps renders the data files: what postfix accepts, where dovecot finds
// passwords and which keys opendkim signs with.
func (s *Server) mailMaps(ctx context.Context, c mailConfig) ([]agent.FileSpec, []string, error) {
	domains, err := s.db.ListMailDomains(ctx, 0)
	if err != nil {
		return nil, nil, err
	}
	boxes, err := s.db.ListMailboxes(ctx, 0, 0)
	if err != nil {
		return nil, nil, err
	}
	aliases, err := s.db.ListMailAliases(ctx, 0, 0)
	if err != nil {
		return nil, nil, err
	}
	byID := map[int64]*store.MailDomain{}
	var domainLines, users, keyTable, signingTable []string
	trusted := []string{"127.0.0.1", "::1", "localhost"}
	if c.Hostname != "" {
		trusted = append(trusted, c.Hostname)
	}
	files := []agent.FileSpec{}
	for _, d := range domains {
		byID[d.ID] = d
		if !d.Active {
			continue
		}
		domainLines = append(domainLines, d.Name+" OK")
		trusted = append(trusted, d.Name)
		if !c.DKIM || d.DKIMSelector == "" || d.DKIMKeyEnc == "" || s.secrets == nil {
			continue
		}
		key, err := s.secrets.Decrypt(d.DKIMKeyEnc)
		if err != nil {
			s.log.Warn("mail: DKIM key unreadable", "domain", d.Name, "err", err)
			continue
		}
		path := fmt.Sprintf("%s/keys/%s/%s.private", dkimDir, d.Name, d.DKIMSelector)
		files = append(files, agent.FileSpec{Path: path, Content: key, Mode: 0o600, Owner: "opendkim", Group: "opendkim"})
		keyTable = append(keyTable, fmt.Sprintf("%s._domainkey.%s %s:%s:%s", d.DKIMSelector, d.Name, d.Name, d.DKIMSelector, path))
		signingTable = append(signingTable, fmt.Sprintf("*@%s %s._domainkey.%s", d.Name, d.DKIMSelector, d.Name))
	}

	// Ящик, снятый с публикации, не принимает почту и не пускает в IMAP:
	// его нет ни в карте postfix, ни в файле паролей dovecot.
	var boxLines, senderLines []string
	byAddress := map[string]*store.Mailbox{}
	for _, b := range boxes {
		d := byID[b.DomainID]
		if d == nil || !d.Active || !b.Active {
			continue
		}
		byAddress[b.Address] = b
		boxLines = append(boxLines, fmt.Sprintf("%s %s/%s/", b.Address, d.Name, b.LocalPart))
		senderLines = append(senderLines, b.Address+" "+b.Address)
		quota := "*:storage=" + fmt.Sprint(b.QuotaMB) + "M"
		if b.QuotaMB <= 0 {
			quota = "*:storage=0"
		}
		users = append(users, fmt.Sprintf("%s:%s::::::userdb_quota_rule=%s", b.Address, b.PasswordHash, quota))
	}

	var aliasLines []string
	for _, a := range aliases {
		d := byID[a.DomainID]
		if d == nil || !d.Active || !a.Active {
			continue
		}
		dest := strings.Join(a.Destinations(), ", ")
		if a.Source == "@" {
			aliasLines = append(aliasLines, "@"+d.Name+" "+dest)
		} else {
			aliasLines = append(aliasLines, a.Source+"@"+d.Name+" "+dest)
		}
		// Отправлять от имени алиаса могут те ящики, куда он ведёт.
		local := []string{}
		for _, to := range a.Destinations() {
			if byAddress[to] != nil {
				local = append(local, to)
			}
		}
		if len(local) > 0 && a.Source != "@" {
			senderLines = append(senderLines, a.Source+"@"+d.Name+" "+strings.Join(local, ","))
		}
	}

	sort.Strings(domainLines)
	sort.Strings(boxLines)
	sort.Strings(aliasLines)
	sort.Strings(senderLines)
	sort.Strings(users)
	header := "# Generated by MonoPanel — правки будут перезаписаны.\n"
	maps := []string{"domains", "mailboxes", "aliases", "senders"}
	content := [][]string{domainLines, boxLines, aliasLines, senderLines}
	for i, name := range maps {
		files = append(files, agent.FileSpec{Path: postfixMapsDir + "/" + name, Content: header + strings.Join(content[i], "\n") + "\n", Mode: 0o644})
	}
	files = append(files,
		agent.FileSpec{Path: postfixMapsDir + "/submission_header_checks", Mode: 0o644,
			Content: header + "# Письмо, отправленное клиентом через 587/465, не должно выдавать его адрес.\n/^Received:.*with ESMTPSA/ IGNORE\n/^X-Originating-IP:/ IGNORE\n"},
		agent.FileSpec{Path: dovecotUsers, Content: header + strings.Join(users, "\n") + "\n", Mode: 0o640, Owner: "root", Group: "dovecot"},
	)
	if c.DKIM {
		files = append(files,
			agent.FileSpec{Path: dkimDir + "/KeyTable", Content: header + strings.Join(keyTable, "\n") + "\n", Mode: 0o644},
			agent.FileSpec{Path: dkimDir + "/SigningTable", Content: header + strings.Join(signingTable, "\n") + "\n", Mode: 0o644},
			agent.FileSpec{Path: dkimDir + "/TrustedHosts", Content: header + strings.Join(trusted, "\n") + "\n", Mode: 0o644},
		)
	}
	return files, maps, nil
}

// applyMail regenerates every mail configuration file and reloads what the
// change actually touched. It runs after each edit, so it must be cheap:
// adding a mailbox writes two files and must not drop anybody's IMAP session.
func (s *Server) applyMail(ctx context.Context, logf func(string, ...any)) error {
	return s.applyMailConfig(ctx, logf, false)
}

// applyMailConfig is applyMail with a say in whether the services are reloaded
// even though no file changed — which is what a renewed certificate needs: the
// paths in main.cf stay the same while the file behind them is new.
func (s *Server) applyMailConfig(ctx context.Context, logf func(string, ...any), force bool) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	c := s.loadMailConfig(ctx)
	if !c.Installed {
		return errors.New("почтовый сервер не установлен: mp mail install")
	}
	files, maps, err := s.mailMaps(ctx, c)
	if err != nil {
		return err
	}
	data, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Origin: "mail:data"})
	if err != nil {
		return err
	}
	// Таблицы ключей opendkim читает при старте: пока его не перезагрузить,
	// письма нового домена уйдут без подписи.
	dkimChanged := false
	for _, p := range data.Written {
		if strings.HasPrefix(p, dkimDir+"/") {
			dkimChanged = true
		}
	}
	if dkimChanged && c.DKIM {
		if _, err := s.agent.Service(ctx, dkimService, "reload-or-restart"); err != nil {
			return err
		}
		logf("opendkim перезапущен: таблицы ключей изменились")
	}
	for _, m := range maps {
		res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "postmap", Args: []string{postfixMapsDir + "/" + m}, TimeoutSeconds: 60})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("postmap %s: %s", m, strings.TrimSpace(res.Output))
		}
	}

	certPath, keyPath, kind, _ := s.mailTLS(ctx, c)
	tls := certPath != ""
	main, err := s.render.Render("mail/main.cf.tmpl", render.Postfix{
		Hostname: c.Hostname, MapsDir: postfixMapsDir, MailBase: mailBase,
		MessageSizeBytes: int64(c.MaxSizeMB) * 1024 * 1024, VmailUID: c.VmailUID, VmailGID: c.VmailGID,
		TLS: tls, CertPath: certPath, KeyPath: keyPath, RBL: c.RBL, Milter: mailMilter(c),
	})
	if err != nil {
		return err
	}
	master, err := s.render.Render("mail/master.cf.tmpl", render.PostfixMaster{MapsDir: postfixMapsDir, TLS: tls, Port25: c.Port25})
	if err != nil {
		return err
	}
	dove, err := s.render.Render("mail/dovecot.conf.tmpl", render.Dovecot{
		Hostname: c.Hostname, MailBase: mailBase, UsersFile: dovecotUsers, VmailUser: vmailUser,
		VmailUID: c.VmailUID, VmailGID: c.VmailGID, TLS: tls, CertPath: certPath, KeyPath: keyPath,
		POP3: c.POP3, Postmaster: "postmaster@" + c.Hostname,
	})
	if err != nil {
		return err
	}
	confFiles := []agent.FileSpec{
		{Path: postfixDir + "/main.cf", Content: main, Mode: 0o644},
		{Path: postfixDir + "/master.cf", Content: master, Mode: 0o644},
		{Path: dovecotConf, Content: dove, Mode: 0o644},
	}
	reload := []string{postfixService, dovecotService}
	validate := [][]string{{"/usr/sbin/postfix", "check"}, {"/usr/bin/doveconf", "-n"}}
	if c.DKIM {
		dkim, err := s.render.Render("mail/opendkim.conf.tmpl", render.OpenDKIM{
			Socket: "local:" + dkimSocketPath, KeyTable: dkimDir + "/KeyTable",
			SigningTable: dkimDir + "/SigningTable", TrustedHosts: dkimDir + "/TrustedHosts",
		})
		if err != nil {
			return err
		}
		confFiles = append(confFiles,
			agent.FileSpec{Path: dkimConf, Content: dkim, Mode: 0o644},
			agent.FileSpec{Path: dkimDefaults, Mode: 0o644,
				Content: "# Generated by MonoPanel\nRUNDIR=/run/opendkim\nSOCKET=\"local:" + dkimSocketPath + "\"\nUSER=opendkim\nGROUP=opendkim\nPIDFILE=$RUNDIR/$NAME.pid\n"},
		)
		reload = append(reload, dkimService)
	}
	apply, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		Files: confFiles, Validate: validate, Reload: reload, Force: force, Origin: "mail",
	})
	if err != nil {
		return err
	}
	logf("почта: %d файлов записано, %d без изменений, TLS: %s", len(apply.Written)+len(data.Written), len(apply.Unchanged)+len(data.Unchanged), kind)
	return nil
}

func mailMilter(c mailConfig) string {
	if !c.DKIM {
		return ""
	}
	return "local:" + dkimSocketPath
}

// newDKIMKey generates a signing key: the PEM opendkim reads and the base64
// public key that goes into the TXT record.
func newDKIMKey() (private, public string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	private = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	return private, base64.StdEncoding.EncodeToString(der), nil
}

// probePort reports whether something answers on the loopback port and, for
// SMTP-like services, what it says: the panel has to tell an administrator
// that another daemon already owns port 25.
func probePort(port int) (bool, string) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 700*time.Millisecond)
	if err != nil {
		return false, ""
	}
	defer conn.Close()                                           //nolint:errcheck // проба порта, закрывать нечего
	conn.SetReadDeadline(time.Now().Add(700 * time.Millisecond)) //nolint:errcheck // best effort: the banner is decoration
	buf := make([]byte, 200)
	n, _ := conn.Read(buf)
	return true, strings.TrimSpace(string(buf[:n]))
}

// hashMailPassword produces what dovecot's passwd-file expects. BLF-CRYPT is
// dovecot's own blowfish implementation, so it does not depend on what the
// host's libc crypt supports.
func hashMailPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), 11)
	if err != nil {
		return "", err
	}
	return "{BLF-CRYPT}" + string(h), nil
}

// splitAddress validates and splits user@example.com.
func splitAddress(addr string) (local, domain string, err error) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	local, domain, ok := strings.Cut(addr, "@")
	if !ok || !mailDomainRe.MatchString(domain) {
		return "", "", errors.New("адрес должен быть вида user@example.com")
	}
	// "@example.com" — catch-all домена; внутри он живёт под именем "@".
	if local == "" {
		local = "@"
	}
	if local != "@" && !localPartRe.MatchString(local) {
		return "", "", errors.New("в имени ящика допустимы латиница, цифры, точка, дефис и подчёркивание")
	}
	if len(local) > 64 {
		return "", "", errors.New("имя ящика длиннее 64 символов")
	}
	return local, domain, nil
}

// mailDomainFor resolves a domain and checks that the caller owns it.
func (s *Server) mailDomainFor(ctx context.Context, name string) (*store.MailDomain, error) {
	d, err := s.db.GetMailDomain(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errors.New("домен " + name + " не обслуживается панелью")
	}
	if err != nil {
		return nil, err
	}
	p := principalFrom(ctx)
	if p.Role != store.RoleAdmin && d.UserID != p.UserID {
		return nil, errors.New("домен " + name + " не обслуживается панелью")
	}
	return d, nil
}

// mailWarnings collects what an administrator still has to do.
func (s *Server) mailWarnings(ctx context.Context, c mailConfig, tls string) []string {
	var out []string
	if tls == store.CertKindSelfSigned {
		out = append(out, "сертификат самоподписанный: почтовые клиенты будут ругаться, выпустите сертификат для "+c.Hostname)
	} else if tls == "none" {
		out = append(out, "сертификата нет: submission (587/465) не работает, пароли ходили бы открытым текстом")
	}
	if !c.Port25 {
		out = append(out, "приём на 25 порту выключен: почта снаружи приходить не будет")
	}
	if missing := s.missingMailPorts(c); len(missing) > 0 {
		out = append(out, "не слушают порты "+strings.Join(missing, ", ")+" — смотрите journalctl -u postfix -u dovecot")
	}
	if c.DKIM && s.secrets == nil {
		out = append(out, "ключ шифрования панели недоступен: ключи DKIM прочитать нельзя")
	}
	if n, err := s.db.ListMailDomains(ctx, 0); err == nil && len(n) == 0 {
		out = append(out, "не добавлено ни одного домена")
	}
	return out
}
