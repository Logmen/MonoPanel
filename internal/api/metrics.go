package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/sys/unix"

	"monopanel/internal/agent"
	"monopanel/internal/agent/confcheck"
	"monopanel/internal/apitypes"
	"monopanel/internal/buildinfo"
	"monopanel/internal/store"
	"monopanel/internal/updater"
)

// sampler collects host metrics every 10 s and stores one point per minute.
type sampler struct {
	s         *Server
	prevIdle  uint64
	prevTotal uint64
	prevRx    uint64
	prevTx    uint64
	minute    int64
	cpuSum    float64
	cpuN      int
	rxSum     int64
	txSum     int64
}

func readCPU() (idle, total uint64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, errors.New("empty /proc/stat")
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, errors.New("bad /proc/stat")
	}
	for i, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		total += v
		if i == 3 || i == 4 {
			idle += v
		}
	}
	return idle, total, nil
}

func readNet() (rx, tx uint64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "docker") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx
}

func readMem() (used, total int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var avail int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(strings.Fields(v)[0], 10, 64)
		switch k {
		case "MemTotal":
			total = n * 1024
		case "MemAvailable":
			avail = n * 1024
		}
	}
	return total - avail, total
}

func readDisk(mount string) (used, total int64) {
	var st unix.Statfs_t
	if err := unix.Statfs(mount, &st); err != nil {
		return 0, 0
	}
	total = int64(st.Blocks) * st.Bsize
	free := int64(st.Bavail) * st.Bsize
	return total - free, total
}

func (m *sampler) run(ctx context.Context) {
	m.prevIdle, m.prevTotal, _ = readCPU()
	m.prevRx, m.prevTx = readNet()
	m.minute = time.Now().Unix() / 60
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	prune := time.NewTicker(6 * time.Hour)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-prune.C:
			m.s.db.PruneMetrics(ctx, time.Now().Add(-30*24*time.Hour).Unix()) //nolint:errcheck // retention trim retries on the next tick
		case <-t.C:
			idle, total, err := readCPU()
			if err == nil && total > m.prevTotal {
				di, dt := float64(idle-m.prevIdle), float64(total-m.prevTotal)
				m.cpuSum += 100 * (1 - di/dt)
				m.cpuN++
			}
			m.prevIdle, m.prevTotal = idle, total
			rx, tx := readNet()
			if rx >= m.prevRx {
				m.rxSum += int64(rx - m.prevRx)
			}
			if tx >= m.prevTx {
				m.txSum += int64(tx - m.prevTx)
			}
			m.prevRx, m.prevTx = rx, tx
			if now := time.Now().Unix() / 60; now != m.minute {
				cpu := 0.0
				if m.cpuN > 0 {
					cpu = m.cpuSum / float64(m.cpuN)
				}
				var load [3]float64
				if b, err := os.ReadFile("/proc/loadavg"); err == nil {
					fmt.Sscanf(string(b), "%f %f %f", &load[0], &load[1], &load[2])
				}
				memUsed, memTotal := readMem()
				diskUsed, diskTotal := readDisk("/")
				_ = m.s.db.InsertMetric(ctx, &store.MetricPoint{TS: m.minute * 60, CPU: cpu, Load1: load[0], MemUsed: memUsed, MemTotal: memTotal, DiskUsed: diskUsed, DiskTotal: diskTotal, NetRx: m.rxSum, NetTx: m.txSum})
				m.minute, m.cpuSum, m.cpuN, m.rxSum, m.txSum = now, 0, 0, 0, 0
			}
		}
	}
}

type metricsInput struct {
	Range string `query:"range" enum:"1h,6h,24h,7d,30d" default:"24h"`
}

type metricsOutput struct {
	Body apitypes.Metrics
}

type siteLogsInput struct {
	Domain string `path:"domain"`
	Type   string `path:"type" enum:"access,error,php,slow,apache-access,apache-error"`
	Lines  int    `query:"lines" default:"200" minimum:"1" maximum:"5000"`
}

type serviceLogsInput struct {
	Unit  string `path:"unit" pattern:"^[A-Za-z0-9_.@:-]+$"`
	Lines int    `query:"lines" default:"200" minimum:"1" maximum:"5000"`
}

type logsOutput struct {
	Body apitypes.LogTail
}

type doctorOutput struct {
	Body apitypes.Doctor
}

func lastLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func (s *Server) registerMetrics() {
	huma.Register(s.api, huma.Operation{
		OperationID: "system-metrics", Method: http.MethodGet, Path: "/system/metrics", Summary: "Host metrics (CPU, load, memory, disk, network)", Tags: []string{"system"}, Security: secured,
	}, func(ctx context.Context, in *metricsInput) (*metricsOutput, error) {
		ranges := map[string][2]int64{"1h": {3600, 60}, "6h": {6 * 3600, 60}, "24h": {24 * 3600, 300}, "7d": {7 * 24 * 3600, 1800}, "30d": {30 * 24 * 3600, 3600}}
		r := ranges[in.Range]
		pts, err := s.db.QueryMetrics(ctx, time.Now().Unix()-r[0], r[1])
		if err != nil {
			return nil, err
		}
		return &metricsOutput{Body: apitypes.Metrics{Range: in.Range, StepSeconds: r[1], Points: pts}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "sites-logs", Method: http.MethodGet, Path: "/sites/{domain}/logs/{type}", Summary: "Tail of a site log", Tags: []string{"sites"}, Security: secured,
	}, func(ctx context.Context, in *siteLogsInput) (*logsOutput, error) {
		site, err := s.loadSiteFor(ctx, in.Domain)
		if err != nil {
			return nil, err
		}
		user, err := s.db.GetUserByID(ctx, site.UserID)
		if err != nil {
			return nil, err
		}
		suffix := map[string]string{"access": ".access.log", "error": ".error.log", "php": ".php.error.log", "slow": ".php.slow.log", "apache-access": ".apache.access.log", "apache-error": ".apache.error.log"}[in.Type]
		limit := int64(in.Lines) * 400
		p := path.Join(s.siteLogDir(user.Login), site.Domain+suffix)
		if s.siteLogsPending(ctx, site.ID) {
			// The site has not moved yet: its servers still write into the
			// account's data/logs, while the tree has an empty file. Root
			// reads nothing there for it (the agent refuses the homes); the
			// account does, as for a download. A log missing there may be
			// in the tree already, and one there may be stale while the
			// tree's is the live one (a partial apply in Apache mode moves
			// Apache's logs and not nginx's): the one written last wins.
			old := path.Join(s.layoutFor(site, user).data, "logs", site.Domain+suffix)
			out, err := s.fsop(ctx, user, nil, "tail", path.Join("data", "logs", site.Domain+suffix), strconv.FormatInt(limit, 10))
			if err != nil {
				return nil, err
			}
			var tail struct {
				Size    int64  `json:"size"`
				ModTime string `json:"mtime"`
				Content string `json:"content"`
				Missing bool   `json:"missing"`
			}
			if err := json.Unmarshal(out, &tail); err != nil {
				return nil, huma.Error502BadGateway("the log of " + old + ": " + err.Error())
			}
			if !tail.Missing {
				oldTail := &logsOutput{Body: apitypes.LogTail{Path: old, Size: tail.Size, Lines: lastLines(tail.Content, in.Lines)}}
				if !s.treeLogNewer(ctx, p, tail.ModTime) {
					return oldTail, nil
				}
				res, err := s.agent.ReadFile(ctx, p, limit)
				if err != nil {
					s.log.Warn("the log tree's file of a site that has not moved, written after the old one, cannot be read: the old one's tail stands", "site", site.Domain, "path", p, "err", err)
					return oldTail, nil
				}
				return &logsOutput{Body: apitypes.LogTail{Path: p, Size: res.Size, Lines: lastLines(res.Content, in.Lines)}}, nil
			}
		}
		res, err := s.agent.ReadFile(ctx, p, limit)
		if err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		return &logsOutput{Body: apitypes.LogTail{Path: p, Size: res.Size, Lines: lastLines(res.Content, in.Lines)}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "system-logs", Method: http.MethodGet, Path: "/system/logs/{unit}", Summary: "Journal of a service (admin)", Tags: []string{"system"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *serviceLogsInput) (*logsOutput, error) {
		unit := in.Unit
		if !strings.Contains(unit, ".") {
			unit += ".service"
		}
		res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "journalctl", Args: []string{"-u", unit, "-n", strconv.Itoa(in.Lines), "--no-pager", "-o", "short-iso"}, TimeoutSeconds: 30})
		if err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		return &logsOutput{Body: apitypes.LogTail{Path: "journal:" + unit, Lines: lastLines(res.Output, in.Lines)}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "system-doctor", Method: http.MethodGet, Path: "/system/doctor", Summary: "Health checks: services, configs, disk, certificates, DNS, drift", Tags: []string{"system"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*doctorOutput, error) {
		return &doctorOutput{Body: s.doctor(ctx)}, nil
	})
}

// treeLogNewer tells whether the tree's file p of a site that has not moved
// was written after its old file in the account's data/logs, which was last
// written at oldMod (RFC 3339, as fsop tail gives it): to the second, as
// the agent's stat and fsop tell them, and only once the tree's file holds
// something — the move makes it empty, ahead of the apply that has the
// servers write there. Not known either way — an fsop of an older binary
// gives no time, the stat fails —, the old file stays the one, as before.
func (s *Server) treeLogNewer(ctx context.Context, p, oldMod string) bool {
	was, err := time.Parse(time.RFC3339, oldMod)
	if err != nil {
		return false
	}
	st, err := s.agent.Stat(ctx, p)
	if err != nil || len(st.Entries) != 1 {
		return false
	}
	e := st.Entries[0]
	if !e.Exists || e.IsDir || e.Size == 0 {
		return false
	}
	mod, err := time.Parse(time.RFC3339, e.ModTime)
	return err == nil && mod.Truncate(time.Second).After(was.Truncate(time.Second))
}

func check(name, status, detail string) apitypes.Check {
	return apitypes.Check{Name: name, Status: status, Detail: detail}
}

// resticSandboxCheck is the doctor's line on how restic runs, from what the
// agent's ping says of its sandbox — and on whether the installed restic
// starts in it at all. One that does not (Red Hat's Go exits when the sandbox
// keeps /proc/sys/crypto/fips_enabled from it) fails every backup at once,
// and every other run the sandbox confines: a failure, whatever the sandbox
// is like. One whose restic version did not finish in time (ResticTimeout)
// may only have met a host too busy at that moment, and the agent tests it
// again ResticRetestAfter later: a warning that says so, and what to look at
// should it last — backups would hang the same way. An agent that does not
// tell a timeout from an error reports a failure, as it always did. The
// self-test's verdict comes first, where no rule of the testbed's
// doctor-expected.sh that allows a host's missing Landlock takes it for
// that, and the sandbox's own state follows it.
func resticSandboxCheck(sb *agent.SandboxInfo) apitypes.Check {
	c := resticSandboxState(sb)
	switch {
	case sb.ResticError != "" && sb.ResticTimeout:
		c.Status = "warn"
		c.Detail = fmt.Sprintf("restic took too long to start in its sandbox — %s version there: %s. The host may only have been too busy at that moment: the agent tests it again on the first ping %d minutes after that run (the doctor, the dashboard), and a later doctor shows what came of it. If it keeps timing out, backups hang the same way: look at the host's load, memory and disk I/O (uptime, free -m, vmstat 5), and at how long restic takes outside the panel (time %s version) — if it answers there at once, report this to the panel's developers. The sandbox: %s", sb.Restic, sb.ResticError, int(agent.ResticRetestAfter/time.Minute), sb.Restic, c.Detail)
	case sb.ResticError != "":
		c.Status = "fail"
		c.Detail = fmt.Sprintf("restic does not start in its sandbox — %s version there: %s. Every backup fails the same way, and so does every other restic run the sandbox confines; the agent does not run restic outside it. Report this error to the panel's developers — it names what the sandbox kept from restic — and upgrade the panel once a release fixes it. The sandbox: %s", sb.Restic, sb.ResticError, c.Detail)
	case sb.ResticVersion != "":
		c.Detail += "; restic starts as a backup's restic would (" + sb.ResticVersion + ")"
	}
	return c
}

// resticSandboxState is the doctor's line on how restic runs: Landlock, the
// view without symlinks, the private /etc.
func resticSandboxState(sb *agent.SandboxInfo) apitypes.Check {
	const name = "restic sandbox"
	if sb.Landlock < 1 {
		why := "no Landlock (" + sb.Error + ")"
		if sb.Hint != "" {
			why += "; " + sb.Hint
		}
		if sb.NoSymfollow {
			return check(name, "warn", why+": restic backs up and restores as root without its sandbox. A backup still reads the trees it saves in a view that follows no symlink, so a directory an account swaps for a symlink leads nowhere; nothing else confines restic")
		}
		detail := why + ": restic backs up and restores as root without its sandbox, and an account that swaps a directory for a symlink while a backup reads its home can have files outside it stored as its own"
		if sb.NoSymfollowError != "" {
			detail += "; the view without symlinks that would stop that needs the agent to mount (" + sb.NoSymfollowError + ")"
		}
		return check(name, "warn", detail)
	}
	var warns []string
	if !sb.NoSymfollow && sb.NoSymfollowError != "" {
		warns = append(warns, fmt.Sprintf("a backup follows the symlinks in the trees it reads (%s): during a server backup, which reads the panel's configuration, certificates and dumps as well, an account that swaps a directory of its own for a symlink can have those, or another account's files, stored as its own; the agent needs to mount (a mount namespace, mount_setattr of Linux 5.14)", sb.NoSymfollowError))
	}
	if !sb.PrivateEtc && sb.PrivateEtcError != "" {
		warns = append(warns, fmt.Sprintf("restic reads /etc/resolv.conf, /etc/hosts and /etc/passwd by inode (%s): when the host replaces one of them during a backup (NetworkManager, dhclient, useradd), restic loses name resolution, and with it a remote repository, or owner names; the agent needs to mount a tmpfs for a private /etc", sb.PrivateEtcError))
	}
	if len(warns) > 0 {
		return check(name, "warn", fmt.Sprintf("Landlock ABI %d, but %s", sb.Landlock, strings.Join(warns, "; and ")))
	}
	if sb.Landlock == 1 {
		return check(name, "ok", "Landlock ABI 1: backups run in it, restores without it (ABI 1 cannot let restic link files between directories)")
	}
	return check(name, "ok", fmt.Sprintf("Landlock ABI %d", sb.Landlock))
}

// doctor runs the health checks.
func (s *Server) doctor(ctx context.Context) apitypes.Doctor {
	actx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var out apitypes.Doctor
	add := func(c apitypes.Check) { out.Checks = append(out.Checks, c) }
	ping, err := s.agent.Ping(actx)
	if err != nil {
		add(check("agent", "fail", err.Error()))
		out.Summary = "agent unreachable"
		return out
	}
	add(check("agent", "ok", "version "+ping.Version))
	if sb := ping.Sandbox; sb != nil {
		add(resticSandboxCheck(sb))
	}

	web := s.profile.Web()
	units := []string{web.NginxService}
	validators := [][]string{web.NginxCheckArgv}
	// The accounts' Apaches: each its unit and apache2 -t on its httpd.conf
	// (whose runtime directory apache2 -t wants to see) — but those an
	// unfinished move has not reached yet, which have neither (apacheDoctor
	// says that they wait).
	wait := s.apacheMoveWaits(actx)
	for _, unit := range s.apacheUnits(actx) {
		login, _ := confcheck.ApacheInstanceLoginOf(unit)
		if wait.accounts[login] {
			continue
		}
		inst := apacheInstanceOf(login)
		units = append(units, unit)
		if err := s.ensureApacheRunDir(actx, inst); err == nil {
			validators = append(validators, inst.check)
		}
	}
	if s.apacheInstalled(actx) {
		for _, c := range s.apacheDoctor(actx, wait) {
			add(c)
		}
	}
	if versions, err := s.db.ListPHPVersions(actx); err == nil {
		for _, v := range versions {
			if v.Status == store.PHPInstalled {
				units = append(units, v.FPMService)
				if l := phpLayout(s, v.Version); l != nil {
					validators = append(validators, l.FPMCheckArgv)
				}
			}
		}
	}
	if inst, err := s.db.GetDBInstance(actx); err == nil && inst.Status == store.DBReady {
		units = append(units, inst.Service)
	}
	if f, _ := s.db.GetSetting(actx, settingFail2ban); f == "installed" {
		units = append(units, "fail2ban.service")
	}
	if f, _ := s.db.GetSetting(actx, settingFirewall); f == "yes" {
		units = append(units, nftUnit)
	}
	for _, u := range units {
		st, err := s.agent.Service(actx, u, "status")
		switch {
		case err != nil:
			add(check("service "+u, "fail", err.Error()))
		case st.Status.ActiveState == "active" && st.Status.BinaryReplaced:
			// An upgrade (the panel's or the distribution's) replaced its
			// binary: the old code, the vulnerable one, runs until a restart.
			add(s.replacedBinaryCheck(actx, u, st.Status))
		case st.Status.ActiveState == "active":
			add(check("service "+u, "ok", st.Status.SubState))
		default:
			add(check("service "+u, "fail", st.Status.ActiveState+" ("+st.Status.SubState+")"))
		}
	}
	for _, v := range validators {
		name := "config " + path.Base(v[0])
		if login, ok := confcheck.ApacheInstanceCheckLogin(v); ok {
			name = "config " + confcheck.ApacheInstanceUnit(login)
		}
		if _, err := s.agent.ApplyConfigSet(actx, &agent.ApplyConfigSetRequest{Validate: [][]string{v}, Origin: "doctor"}); err != nil {
			add(check(name, "fail", err.Error()))
		} else {
			add(check(name, "ok", "syntax ok"))
		}
	}
	container := ""
	if info, err := s.agent.SystemInfo(actx); err == nil {
		container = info.Container
		for _, d := range info.Disks {
			pct := 100.0
			if d.TotalBytes > 0 {
				pct = float64(d.FreeBytes) / float64(d.TotalBytes) * 100
			}
			status := "ok"
			if pct < 10 || d.FreeBytes < 2<<30 {
				status = "warn"
			}
			if pct < 3 {
				status = "fail"
			}
			add(check("disk "+d.Mount, status, fmt.Sprintf("%.1f%% free (%.1f GB)", pct, float64(d.FreeBytes)/1073741824)))
		}
		if info.MemTotalBytes > 0 {
			pct := float64(info.MemAvailableBytes) / float64(info.MemTotalBytes) * 100
			status := "ok"
			if pct < 10 {
				status = "warn"
			}
			add(check("memory", status, fmt.Sprintf("%.0f%% available", pct)))
		}
		if info.SwapTotalBytes == 0 && info.MemTotalBytes < 2<<30 {
			add(check("swap", "warn", "no swap on a small host"))
		}
	}
	if certs, err := s.db.ListCertificates(actx); err == nil {
		for _, c := range certs {
			switch {
			case c.Status == store.CertError:
				add(check("certificate "+c.Name, "warn", c.LastError))
			case c.NotAfter != nil && time.Until(*c.NotAfter) < 14*24*time.Hour:
				add(check("certificate "+c.Name, "warn", "expires "+c.NotAfter.Format("2006-01-02")))
			case c.Status == store.CertValid:
				add(check("certificate "+c.Name, "ok", "until "+c.NotAfter.Format("2006-01-02")))
			}
		}
	}
	if c := s.loadUpdateConfig(actx); c.Repo != "" {
		switch {
		case c.LastError != "":
			add(check("update", "warn", "update check: "+c.LastError))
		case c.Latest != nil && updater.Newer(buildinfo.Version, c.Latest.Version):
			add(check("update", "warn", "version "+c.Latest.Version+" available (mp update apply)"))
		default:
			add(check("update", "ok", "version "+buildinfo.Version))
		}
	}
	// An attempt whose version runs now, or an older one, is history: the
	// package came in some other way.
	if st, err := updater.ReadState(s.cfg.UpdatesDir()); err == nil && st != nil && (st.Status == updater.StatusFailed || st.Status == updater.StatusRolledBack) && updater.Newer(buildinfo.Version, st.To) {
		detail := "install of " + st.To + ": " + st.Status + ", " + st.Error
		if retryReinstalls(st) {
			detail += "; the package database lists " + st.To + " already, its package scripts ran, while the previous binary runs: mp update apply installs it again, which the automatic update does not do"
		}
		add(check("update attempt", "warn", detail))
	}
	if failed, err := s.db.ListJobs(actx, 200, store.JobFailed); err == nil {
		done, _ := s.db.ListJobs(actx, 500, store.JobDone)
		var domains []string
		if sites, err := s.db.ListSites(actx, 0); err == nil {
			for _, site := range sites {
				domains = append(domains, site.Domain)
			}
		}
		if recent := openFailures(failed, done, time.Now(), domains...); recent > 0 {
			add(check("jobs", "warn", fmt.Sprintf("%d failed in the last 24h (mp job list --status failed)", recent)))
		} else {
			add(check("jobs", "ok", "no failures in 24h"))
		}
	}
	if c := s.selinuxCheck(actx); c != nil {
		add(*c)
	}
	if c := s.crbLeftoverCheck(actx); c != nil {
		add(*c)
	}
	if c := s.fail2banLeftoverCheck(actx); c != nil {
		add(*c)
	}
	// The moves of an upgrade that have not finished, and what an
	// administrator changed of what the panel set up once.
	if c := s.siteLogsCheck(actx); c != nil {
		add(*c)
	}
	if c := s.phpRepoCheck(actx); c != nil {
		add(*c)
	}
	for _, c := range s.phpDistroChecks(actx) {
		add(c)
	}
	if c := s.phpCLICheck(actx); c != nil {
		add(*c)
	}
	// The vendor packages' updates, the pools CVE-2026-129291 needs, the
	// kernel modules (doctor_upgrade.go).
	for _, c := range s.upgradeChecks(actx) {
		add(c)
	}
	if c := s.catchWorkersCheck(actx); c != nil {
		add(*c)
	}
	if c := s.shareDoctorCheck(actx); c != nil {
		add(*c)
	}
	if c := s.mailPolicyCheck(actx); c != nil {
		add(*c)
	}
	add(s.kernelModulesCheck(container))
	if h := s.cfg.Web.Hostname; h != "" {
		addrs, err := s.lookup(actx, h)
		local := map[string]bool{}
		for _, ip := range s.hostIPs() {
			local[ip] = true
		}
		ok := false
		for _, a := range addrs {
			if local[a] {
				ok = true
			}
		}
		switch {
		case err != nil:
			add(check("dns "+h, "warn", err.Error()))
		case ok:
			add(check("dns "+h, "ok", strings.Join(addrs, ", ")))
		default:
			add(check("dns "+h, "warn", "points to "+strings.Join(addrs, ", ")+", not this host"))
		}
	}
	if off := s.sitesOffHost(actx); len(off) > 0 {
		add(check("site addresses", "fail", s.offHostHint(off)))
	}
	if sites, err := s.db.ListSites(actx, 0); err == nil {
		paths := []string{}
		for _, site := range sites {
			if site.Status != store.SiteActive {
				continue
			}
			if u, err := s.db.GetUserByID(actx, site.UserID); err == nil {
				l := s.layoutFor(site, u)
				paths = append(paths, l.nginxConf)
				if l.poolConf != "" {
					paths = append(paths, l.poolConf)
				}
			}
		}
		if len(paths) > 0 {
			if st, err := s.agent.Stat(actx, paths...); err == nil {
				missing := []string{}
				for _, e := range st.Entries {
					if !e.Exists {
						missing = append(missing, e.Path)
					}
				}
				if len(missing) > 0 {
					add(check("drift", "warn", "missing generated files: "+strings.Join(missing, ", ")+" (mp site apply)"))
				} else {
					add(check("drift", "ok", fmt.Sprintf("%d generated files present", len(paths))))
				}
			}
		}
	}
	if c := s.apiSocketCheck(actx); c != nil {
		add(*c)
	}
	fails, warns := 0, 0
	for _, c := range out.Checks {
		switch c.Status {
		case "fail":
			fails++
		case "warn":
			warns++
		}
	}
	count := func(n int, noun string) string {
		if n == 1 {
			return "1 " + noun
		}
		return fmt.Sprintf("%d %ss", n, noun)
	}
	out.Summary = fmt.Sprintf("%s, %d failed, %s", count(len(out.Checks), "check"), fails, count(warns, "warning"))
	return out
}

// openFailures counts the jobs that failed in the last 24 hours and that no
// later run of the same work fixed: a failure fixed since is history, not a
// problem — the site applied fine the next time. The same work is the same
// type and lock key, and for the stack's jobs, which share one lock to run
// one after another, the same component as well: an apache.backend that
// went through says nothing of the apache.migrate before it, nor the
// nightly upgrade of nginx, PHP and Apache of a failed mp stack install
// memcached. A failure on an object that a removal took away after it is
// history too (removedAfter): nothing is left to fix. The objects of a job
// are its lock key's and those its payload names beside it
// (failureObjects); a site.delete removes its site (site:<domain>), a
// user.delete its account (user:<login>) and, inline, without a site.delete
// of their own, the account's sites as they were when it was queued
// (userDeletePayload.Sites). One queued by an older panel names none, and
// their failures count until they are a day old. A job without a lock key
// stays a failure. The day counts from the failure, not from the queueing:
// a job may run for hours (jobLimits). sites are the domains of the sites
// there now (failureObjects).
func openFailures(failed, done []*store.Job, now time.Time, sites ...string) int {
	// did are the jobs that went through, by the works they did (jobFixes).
	did := map[string][]*store.Job{}
	for _, j := range done {
		if j.LockKey == "" {
			continue
		}
		for _, k := range jobFixes(j) {
			did[k] = append(did[k], j)
		}
	}
	there := map[string]bool{}
	for _, d := range sites {
		there[d] = true
	}
	n := 0
	for _, j := range failed {
		if now.Sub(failedAt(j)) >= 24*time.Hour {
			continue
		}
		if j.LockKey == "" {
			n++
			continue
		}
		later := func(r *store.Job) bool { return r.ID > j.ID }
		removed := func(o string) bool {
			return slices.ContainsFunc(did[lockWork(o)], func(r *store.Job) bool { return removedAfter(j, r) })
		}
		if !slices.ContainsFunc(did[jobWork(j)], later) && !slices.ContainsFunc(failureObjects(j, there), removed) {
			n++
		}
	}
	return n
}

// failedAt is when a job failed: when it ended, or, should the time be
// missing, when it was queued.
func failedAt(j *store.Job) time.Time {
	if j.FinishedAt != nil {
		return *j.FinishedAt
	}
	return j.CreatedAt.Std()
}

// failureObjects are the objects a failed job worked on, by their lock keys
// (site:<domain>, user:<login>): its own lock key's, and those its payload
// names beside it. A mail.webmail runs under "mail" in a site, its domain,
// for an account: the one the request named, for which the run makes the
// site when none of the name is there. Once the site, or that account, is
// gone the same request cannot go through any more. A site that is there
// (sites) keeps its owner, whichever account the request named, and its
// owner's removal takes it along (userDeletePayload.Sites): the requested
// account is then none of the job's objects, and a webmail into another
// account's site still counts after the account it named went. A payload
// without these fields names nothing more. Other job types may add theirs
// here. A backup.run does not: what fails there is the target, which the
// removal of the account or the site it saved fixes nothing of — a later
// backup to the target that goes through does (the same work).
func failureObjects(j *store.Job, sites map[string]bool) []string {
	objects := []string{j.LockKey}
	if j.Type == "mail.webmail" {
		var p webmailPayload
		_ = json.Unmarshal(j.Payload, &p)
		domain := strings.ToLower(p.Domain)
		if domain != "" {
			objects = append(objects, "site:"+domain)
		}
		if p.User != "" && !sites[domain] {
			objects = append(objects, "user:"+p.User)
		}
	}
	return objects
}

// removedAfter tells that r, a removal that went through, took an object of
// the failed job j away after j. Either r was queued after j: the jobs of
// one lock key run in the order they were queued, so a removal of j's own
// key queued later ran later, whatever the clock said meanwhile (a host
// that booted with its clock ahead, set right later); one of another key
// that ran while j waited — a webmail behind another mail job — left j an
// object that was going or gone, and only rarely one made again under the
// name meanwhile. Or r, run beside j under another lock key, ended after j
// had started: a webmail asked for once the removal of its account was
// queued fails as the account is being removed, with nothing left to fix
// once the removal ended. A removal queued before j and over before j
// started is not j's: j ran on a site or an account made again under the
// name. Nor is one of j's own key queued before j, whatever the clock says:
// it ran before j.
func removedAfter(j, r *store.Job) bool {
	if r.ID > j.ID {
		return true
	}
	return r.LockKey != j.LockKey && r.FinishedAt != nil && j.StartedAt != nil && r.FinishedAt.After(*j.StartedAt)
}

// jobFixes are the works a job that went through has done: its own, and
// for mp stack install apache — the repair the doctor and the journal name
// for a move of the sites onto their accounts' Apaches that failed — the
// move (apache.migrate) and the backend secrets (apache.backend) as well,
// which it runs in full, for every site in Apache mode, and goes through
// only when both did. Without them the jobs line would warn of the move for
// a day after the repair the doctor itself pointed to. A job that removed
// the object of its lock key has done every work on it (lockWork), and a
// user.delete every work on the sites it removed with the account as well.
// Each only one way: a site.apply that went through says nothing of a
// failed site.delete, nor the move of a failed mp stack install apache.
func jobFixes(j *store.Job) []string {
	own := jobWork(j)
	switch {
	case removesObject(j):
		fixes := []string{own, lockWork(j.LockKey)}
		if j.Type == "user.delete" {
			var p userDeletePayload
			_ = json.Unmarshal(j.Payload, &p)
			for _, d := range p.Sites {
				fixes = append(fixes, lockWork("site:"+d))
			}
		}
		return fixes
	case own == jobWork(&store.Job{Type: "stack.install", LockKey: j.LockKey, Payload: json.RawMessage(`{"component":"apache"}`)}):
		return []string{own, jobWork(&store.Job{Type: "apache.migrate", LockKey: j.LockKey}), jobWork(&store.Job{Type: "apache.backend", LockKey: j.LockKey})}
	}
	return []string{own}
}

// removesObject tells that a job removes the object of its lock key: a
// site.delete its site (site:<domain>), a user.delete its account
// (user:<login>). Once it went through, what failed on that site or account
// before — an apply, a fix, a CMS install, a provision — has nothing left
// to fix, and a site or an account made again under the name starts afresh.
func removesObject(j *store.Job) bool {
	switch j.Type {
	case "site.delete":
		return strings.HasPrefix(j.LockKey, "site:")
	case "user.delete":
		return strings.HasPrefix(j.LockKey, "user:")
	}
	return false
}

// lockWork names every work on the object of a lock key, which a job that
// removed it has done (removesObject); no job type is empty, so it is none
// of jobWork's names.
func lockWork(key string) string { return "\x00" + key }

// jobWork names the work a job did, for openFailures.
func jobWork(j *store.Job) string {
	key := j.Type + "\x00" + j.LockKey
	switch j.Type {
	case "stack.install", "stack.remove", "stack.upgrade":
		var p struct {
			Component string `json:"component"`
		}
		_ = json.Unmarshal(j.Payload, &p)
		if p.Component == "" && j.Type == "stack.upgrade" {
			p.Component = upgradeAll
		}
		key += "\x00" + p.Component
	}
	return key
}
