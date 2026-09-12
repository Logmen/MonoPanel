package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/sys/unix"

	"monopanel/internal/agent"
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
		l := s.layoutFor(site, user)
		suffix := map[string]string{"access": ".access.log", "error": ".error.log", "php": ".php.error.log", "slow": ".php.slow.log", "apache-access": ".apache.access.log", "apache-error": ".apache.error.log"}[in.Type]
		p := path.Join(l.data, "logs", site.Domain+suffix)
		res, err := s.agent.ReadFile(ctx, p, int64(in.Lines)*400)
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

func check(name, status, detail string) apitypes.Check {
	return apitypes.Check{Name: name, Status: status, Detail: detail}
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

	web := s.profile.Web()
	units := []string{web.NginxService}
	validators := [][]string{web.NginxCheckArgv}
	if v, _ := s.db.GetSetting(actx, settingApache); v == "installed" {
		units = append(units, web.ApacheService)
		validators = append(validators, web.ApacheCheckArgv)
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
		case st.Status.ActiveState == "active":
			add(check("service "+u, "ok", st.Status.SubState))
		default:
			add(check("service "+u, "fail", st.Status.ActiveState+" ("+st.Status.SubState+")"))
		}
	}
	for _, v := range validators {
		if _, err := s.agent.ApplyConfigSet(actx, &agent.ApplyConfigSetRequest{Validate: [][]string{v}, Origin: "doctor"}); err != nil {
			add(check("config "+path.Base(v[0]), "fail", err.Error()))
		} else {
			add(check("config "+path.Base(v[0]), "ok", "syntax ok"))
		}
	}
	if info, err := s.agent.SystemInfo(actx); err == nil {
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
			add(check("update", "warn", "проверка обновлений: "+c.LastError))
		case c.Latest != nil && updater.Newer(buildinfo.Version, c.Latest.Version):
			add(check("update", "warn", "доступна версия "+c.Latest.Version+" (mp update apply)"))
		default:
			add(check("update", "ok", "версия "+buildinfo.Version))
		}
	}
	if st, err := updater.ReadState(s.cfg.UpdatesDir()); err == nil && st != nil && (st.Status == updater.StatusFailed || st.Status == updater.StatusRolledBack) {
		add(check("update attempt", "warn", "установка "+st.To+": "+st.Status+", "+st.Error))
	}
	if jobs, err := s.db.ListJobs(actx, 200, store.JobFailed); err == nil {
		recent := 0
		for _, j := range jobs {
			if time.Since(j.CreatedAt.Std()) < 24*time.Hour {
				recent++
			}
		}
		if recent > 0 {
			add(check("jobs", "warn", fmt.Sprintf("%d failed in the last 24h (mp job list --status failed)", recent)))
		} else {
			add(check("jobs", "ok", "no failures in 24h"))
		}
	}
	if c := s.selinuxCheck(actx); c != nil {
		add(*c)
	}
	if h := s.cfg.Web.Hostname; h != "" {
		addrs, err := publicLookup(actx, h)
		local := map[string]bool{}
		for _, ip := range localIPv4s() {
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
	fails, warns := 0, 0
	for _, c := range out.Checks {
		switch c.Status {
		case "fail":
			fails++
		case "warn":
			warns++
		}
	}
	out.Summary = fmt.Sprintf("%d checks, %d failed, %d warnings", len(out.Checks), fails, warns)
	return out
}
