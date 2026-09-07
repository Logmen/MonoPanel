package api

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"monopanel/internal/acme"
	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

// mailPackages is the stack: postfix delivers over LMTP to dovecot, which owns
// the mailboxes, the passwords and the sieve filters; opendkim signs.
var mailPackages = []string{
	"postfix", "dovecot-core", "dovecot-imapd", "dovecot-pop3d", "dovecot-lmtpd",
	"dovecot-sieve", "dovecot-managesieved", "opendkim", "opendkim-tools",
}

// mailPorts are the listeners the panel manages, in the order the UI shows them.
var mailPorts = []struct {
	Port int
	Name string
}{
	{25, "SMTP"}, {587, "Submission"}, {465, "SMTPS"},
	{143, "IMAP"}, {993, "IMAPS"}, {110, "POP3"}, {995, "POP3S"}, {4190, "Sieve"},
}

var dovecotVersionRe = regexp.MustCompile(`([0-9]+)\.([0-9]+)\.([0-9]+)`)

type mailInstallPayload struct {
	Hostname string `json:"hostname,omitempty"`
	POP3     *bool  `json:"pop3,omitempty"`
}

// jobMailInstall installs and configures postfix, dovecot and opendkim.
func (s *Server) jobMailInstall(ctx context.Context, jc *jobs.Context) error {
	var p mailInstallPayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	if s.profile.Family() != osprofile.FamilyDebian {
		return errors.New("почтовый сервер пока реализован для Debian/Ubuntu")
	}
	c := s.loadMailConfig(ctx)
	if p.Hostname != "" {
		c.Hostname = acme.NormalizeName(p.Hostname)
	}
	if c.Hostname == "" {
		info, err := s.agent.SystemInfo(ctx)
		if err != nil {
			return err
		}
		c.Hostname = info.Hostname
	}
	if !strings.Contains(c.Hostname, ".") {
		return fmt.Errorf("имя почтового сервера должно быть полным: %q не подойдёт для HELO и сертификата", c.Hostname)
	}
	if p.POP3 != nil {
		c.POP3 = *p.POP3
	}
	jc.Logf("почтовый сервер: %s", c.Hostname)

	// Порты проверяются до установки: пакет postfix стартует сам, и если 25-й
	// уже занят чужим демоном, установка упала бы на его postinst.
	installed, err := s.agent.Pkg(ctx, "query", "postfix", "dovecot-core")
	if err != nil {
		return err
	}
	fresh := installed.Installed["postfix"] == ""
	// Чужой демон на 25 порту — не редкость (на этом хосте его занимает
	// приёмник mail-tester). Свой postfix узнаётся по баннеру с именем хоста.
	if open, banner := s.probe(25); open && !strings.Contains(banner, c.Hostname) {
		c.Port25 = false
		jc.Logf("порт 25 занят другим сервисом (%s) — приём почты снаружи выключен; освободите порт и включите его в настройках", defaultBanner(firstLine(banner)))
	}
	if fresh {
		jc.Progress(5, "заготовка конфигурации postfix")
		if err := s.stubPostfix(ctx, c); err != nil {
			return err
		}
	}

	jc.Progress(10, "установка пакетов")
	if err := s.ensurePackages(ctx, jc, mailPackages...); err != nil {
		return err
	}
	q, err := s.agent.Pkg(ctx, "query", "postfix", "dovecot-core", "opendkim")
	if err != nil {
		return err
	}
	c.PostfixVersion, c.DovecotVersion = q.Installed["postfix"], q.Installed["dovecot-core"]
	jc.Logf("postfix %s, dovecot %s, opendkim %s", c.PostfixVersion, c.DovecotVersion, q.Installed["opendkim"])
	if m := dovecotVersionRe.FindStringSubmatch(c.DovecotVersion); m != nil && (m[1] > "2" || (m[1] == "2" && m[2] >= "4")) {
		return fmt.Errorf("dovecot %s.%s: панель пишет конфигурацию в синтаксисе 2.3, для 2.4 он изменился — обновление шаблонов ещё впереди", m[1], m[2])
	}

	jc.Progress(35, "системный пользователь vmail")
	if _, err := s.agent.EnsureGroup(ctx, &agent.EnsureGroupRequest{Name: vmailUser, System: true}); err != nil {
		return err
	}
	vm, err := s.agent.EnsureUnixUser(ctx, &agent.EnsureUnixUserRequest{
		Login: vmailUser, System: true, Home: mailBase, PrimaryGroup: vmailUser, Comment: "MonoPanel virtual mail",
	})
	if err != nil {
		return err
	}
	c.VmailUID, c.VmailGID = vm.UID, vm.GID
	if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{
		{Path: mailBase, Mode: 0o750, Owner: vmailUser, Group: vmailUser},
	}}); err != nil {
		return err
	}
	jc.Logf("почта хранится в %s (%s, uid %d)", mailBase, vmailUser, vm.UID)

	// smtpd обращается к сокету opendkim, а тот открыт для группы opendkim.
	if c.DKIM {
		if _, err := s.agent.EnsureUnixUser(ctx, &agent.EnsureUnixUserRequest{Login: "postfix", System: true, Groups: []string{"opendkim"}}); err != nil {
			jc.Logf("предупреждение: не удалось добавить postfix в группу opendkim: %v", err)
		}
	}
	s.agent.Tool(ctx, &agent.ToolRequest{Name: "newaliases", TimeoutSeconds: 30}) //nolint:errcheck // /etc/aliases пересобирается на всякий случай

	jc.Progress(55, "сертификат")
	c.Installed = true
	if err := s.saveMailConfig(ctx, c); err != nil {
		return err
	}
	s.orderMailCertificate(ctx, jc, c)

	jc.Progress(70, "конфигурация postfix и dovecot")
	// Установка перезагружает сервисы даже когда файлы не изменились: пакеты
	// только что поставлены и демоны стартовали с чужой конфигурацией.
	if err := s.applyMailConfig(ctx, jc.Logf, true); err != nil {
		return err
	}
	for _, unit := range []string{postfixService, dovecotService, dkimService} {
		if unit == dkimService && !c.DKIM {
			continue
		}
		if _, err := s.agent.Service(ctx, unit, "enable"); err != nil {
			return err
		}
		st, err := s.agent.Service(ctx, unit, "status")
		if err != nil {
			return err
		}
		jc.Logf("%s: %s (%s)", unit, st.Status.ActiveState, st.Status.SubState)
		if st.Status.ActiveState != "active" {
			return fmt.Errorf("%s после установки в состоянии %s", unit, st.Status.ActiveState)
		}
	}

	jc.Progress(90, "порты в firewall")
	if enabled, err := s.db.GetSetting(ctx, settingFirewall); err == nil && enabled == "yes" {
		if _, err := s.applyFirewall(ctx); err != nil {
			jc.Logf("предупреждение: firewall не перезагрузился: %v", err)
		} else {
			jc.Logf("firewall: открыты порты %s", mailPortList(c))
		}
	}
	probes := s.probePorts(allMailPorts())
	open := []string{}
	for _, mp := range mailPorts {
		if probes[mp.Port].Open {
			open = append(open, fmt.Sprintf("%d", mp.Port))
		}
	}
	jc.Logf("слушают порты: %s", strings.Join(open, ", "))
	// systemd отвечает «active» и когда мастер postfix не поднялся: у него
	// юнит-обёртка. Верить можно только тому, что порт действительно открыт.
	if missing := missingMailPorts(c, probes); len(missing) > 0 {
		return fmt.Errorf("не поднялись порты %s — смотрите journalctl -u postfix -u dovecot", strings.Join(missing, ", "))
	}
	jc.Progress(100, "почтовый сервер готов")
	return nil
}

// missingMailPorts lists the listeners the settings promise but which nothing
// answers on, out of probes already taken.
func missingMailPorts(c mailConfig, probes map[int]probeResult) []string {
	if !c.Installed {
		return nil
	}
	names := map[int]string{}
	for _, mp := range mailPorts {
		names[mp.Port] = mp.Name
	}
	missing := []string{}
	for _, port := range mailPortsFor(c) {
		if !probes[port].Open {
			missing = append(missing, fmt.Sprintf("%d/%s", port, names[port]))
		}
	}
	sort.Strings(missing)
	return missing
}

// allMailPorts is every port the panel knows about, managed or not.
func allMailPorts() []int {
	out := make([]int, 0, len(mailPorts))
	for _, mp := range mailPorts {
		out = append(out, mp.Port)
	}
	return out
}

// firstLine trims a service banner to its first line; TLS-wrapped ports answer
// with nothing readable, and an empty string is the honest answer there.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func defaultBanner(s string) string {
	if s == "" {
		return "баннер не получен"
	}
	return s
}

// mailPortList is the human list of ports the firewall opens.
func mailPortList(c mailConfig) string {
	out := []string{}
	for _, p := range mailPortsFor(c) {
		out = append(out, fmt.Sprint(p))
	}
	return strings.Join(out, ", ")
}

// mailPortsFor is what the firewall has to allow for the current settings.
func mailPortsFor(c mailConfig) []int {
	if !c.Installed {
		return nil
	}
	ports := []int{587, 465, 143, 993, 4190}
	if c.Port25 {
		ports = append(ports, 25)
	}
	if c.POP3 {
		ports = append(ports, 110, 995)
	}
	return ports
}

// stubPostfix writes a configuration that binds nothing, so installing the
// package cannot fail on a port another daemon already holds. The real
// configuration is written a few steps later.
func (s *Server) stubPostfix(ctx context.Context, c mailConfig) error {
	main := fmt.Sprintf("# MonoPanel: временная конфигурация на время установки пакета.\ncompatibility_level = 3.6\nmyhostname = %s\ninet_interfaces = loopback-only\nmydestination = localhost\n", c.Hostname)
	master := "# MonoPanel: без inet-сервисов, чтобы установка не заняла порты.\npickup unix n - n 60 1 pickup\ncleanup unix n - n - 0 cleanup\nqmgr unix n - n 300 1 qmgr\nrewrite unix - - n - - trivial-rewrite\nbounce unix - - n - 0 bounce\ndefer unix - - n - 0 bounce\ntrace unix - - n - 0 bounce\nverify unix - - n - 1 verify\nflush unix n - n 1000? 0 flush\nsmtp unix - - n - - smtp\nrelay unix - - n - - smtp\nshowq unix n - n - - showq\nerror unix - - n - - error\nretry unix - - n - - error\ndiscard unix - - n - - discard\nlocal unix - n n - - local\nvirtual unix - n n - - virtual\nlmtp unix - - n - - lmtp\nanvil unix - - n - 1 anvil\nscache unix - - n - 1 scache\npostlog unix-dgram n - n - 1 postlogd\n"
	_, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{
		{Path: postfixDir + "/main.cf", Content: main, Mode: 0o644},
		{Path: postfixDir + "/master.cf", Content: master, Mode: 0o644},
	}, Origin: "mail:stub"})
	return err
}

// orderMailCertificate asks for a Let's Encrypt certificate for the mail
// hostname when it resolves to this host; until it arrives the services use a
// self-signed pair.
func (s *Server) orderMailCertificate(ctx context.Context, jc *jobs.Context, c mailConfig) {
	if existing, err := s.db.GetCertificateByName(ctx, c.Hostname); err == nil && existing.Status == store.CertValid && existing.NotAfter != nil && time.Now().Before(*existing.NotAfter) {
		jc.Logf("сертификат для %s уже выпущен, действует до %s", c.Hostname, existing.NotAfter.Format("2006-01-02"))
		return
	}
	local := map[string]bool{}
	for _, ip := range localIPv4s() {
		local[ip] = true
	}
	addrs, err := publicLookup(ctx, c.Hostname)
	points := false
	for _, a := range addrs {
		if local[a] {
			points = true
		}
	}
	if err != nil || !points {
		jc.Logf("сертификат: %s пока не указывает на этот сервер — заведите A-запись и выпустите сертификат (mp ssl issue %s)", c.Hostname, c.Hostname)
		return
	}
	email, _ := s.db.GetSetting(ctx, settingACMEEmail)
	cert := &store.Certificate{Name: c.Hostname, Names: []string{c.Hostname}, Kind: store.CertKindACME, DirectoryURL: acme.LetsEncrypt, Email: email, KeyType: "ec256", AutoRenew: true, Status: store.CertPending}
	if existing, err := s.db.GetCertificateByName(ctx, c.Hostname); err == nil {
		cert.ID = existing.ID
	}
	if err := s.db.UpsertCertificate(ctx, cert); err != nil {
		jc.Logf("сертификат: %v", err)
		return
	}
	if _, err := s.jobs.Enqueue(ctx, "cert.issue", certIssuePayload{CertID: cert.ID}, jobs.WithLockKey("cert:"+cert.Name), jobs.WithRequestedBy(jc.RequestedBy)); err != nil {
		jc.Logf("сертификат: %v", err)
		return
	}
	jc.Logf("сертификат для %s заказан; почтовые сервисы переключатся на него автоматически", c.Hostname)
}

// jobMailApply re-renders the configuration and reloads the services even when
// nothing changed — an administrator asks for it exactly when something looks
// off.
func (s *Server) jobMailApply(ctx context.Context, jc *jobs.Context) error {
	return s.applyMailConfig(ctx, jc.Logf, true)
}

// mailStatus is the payload of GET /mail.
func (s *Server) mailStatus(ctx context.Context) apitypes.MailStatus {
	c := s.loadMailConfig(ctx)
	_, _, kind, until := s.mailTLS(ctx, c)
	out := apitypes.MailStatus{
		Installed: c.Installed, Hostname: c.Hostname, POP3: c.POP3, DKIM: c.DKIM, Port25: c.Port25,
		MaxSizeMB: c.MaxSizeMB, RBL: c.RBL, Webmail: c.Webmail, TLS: kind, CertName: c.Hostname, CertUntil: until,
		Versions: map[string]string{"postfix": c.PostfixVersion, "dovecot": c.DovecotVersion, "roundcube": c.WebmailVersion},
	}
	if !c.Installed {
		out.TLS = ""
	}
	if c.Webmail != "" {
		out.WebmailURL = "https://" + c.Webmail + "/"
	}
	if domains, err := s.db.ListMailDomains(ctx, 0); err == nil {
		out.Domains = len(domains)
		for _, d := range domains {
			out.Mailboxes += d.Mailboxes
			out.Aliases += d.Aliases
		}
	}
	if c.Installed {
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		units := []string{postfixService, dovecotService}
		if c.DKIM {
			units = append(units, dkimService)
		}
		for _, u := range units {
			if st, err := s.agent.Service(actx, u, "status"); err == nil {
				out.Services = append(out.Services, st.Status)
			}
		}
		managed := map[int]bool{}
		for _, port := range mailPortsFor(c) {
			managed[port] = true
		}
		probes := s.probePorts(allMailPorts())
		for _, mp := range mailPorts {
			r := probes[mp.Port]
			p := apitypes.MailPort{Port: mp.Port, Name: mp.Name, Open: r.Open, Managed: managed[mp.Port]}
			if r.Open {
				p.Owner = firstLine(r.Banner)
			}
			out.Ports = append(out.Ports, p)
		}
		out.Warnings = s.mailWarnings(ctx, c, kind, probes)
	}
	return out
}
