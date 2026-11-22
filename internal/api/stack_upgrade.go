package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/agent/confcheck"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
	"monopanel/internal/systemd"
)

// mp stack upgrade brings the panel's vendor packages — nginx from
// nginx.org, the PHP branches from Sury, ppa:ondrej/php or Remi, Apache, the
// database server from Percona or MySQL — to what the repositories the panel
// added offer, and then what runs them: validated first, reloaded or
// restarted after. A daily run does it for nginx, PHP and Apache. Up to
// 0.8.20 a server kept what it got at install time — unattended-upgrades
// takes only the distribution's origins, EL has no dnf-automatic — and a
// fixed nginx (1.30.7) or PHP (8.4.28) waited in the repository while the old
// one served. Adding the vendors' origins to unattended-upgrades instead would
// mean writing /etc/apt/apt.conf.d, whose commands apt runs as root: the API
// never writes there.

const (
	// settingAutoUpgrade holds the daily upgrade's settings: without it the
	// upgrade runs every day at defaultUpgradeHour.
	settingAutoUpgrade = "stack.upgrade.auto"
	// settingUpgradeLast is the outcome of the last upgrade (StackUpgradeRun).
	settingUpgradeLast = "stack.upgrade.last"
	defaultUpgradeHour = 4
	// upgradeAll is nginx, PHP and Apache: the database server is upgraded
	// only when asked for by name, its restart takes the sites' databases away.
	upgradeAll = "all"
)

// autoUpgradeConfig is the stored half of the daily upgrade. It is on unless
// switched off: a server that never chose keeps getting its fixes.
type autoUpgradeConfig struct {
	Off  bool `json:"off,omitempty"`
	Hour *int `json:"hour,omitempty"`
}

func (c autoUpgradeConfig) hour() int {
	if c.Hour == nil || *c.Hour < 0 || *c.Hour > 23 {
		return defaultUpgradeHour
	}
	return *c.Hour
}

func (s *Server) loadAutoUpgrade(ctx context.Context) autoUpgradeConfig {
	var c autoUpgradeConfig
	if raw, err := s.db.GetSetting(ctx, settingAutoUpgrade); err == nil {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	return c
}

func (s *Server) saveAutoUpgrade(ctx context.Context, c autoUpgradeConfig) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.db.SetSetting(ctx, settingAutoUpgrade, string(b))
}

// lastUpgrade is the outcome of the last upgrade, nil before the first one.
func (s *Server) lastUpgrade(ctx context.Context) *apitypes.StackUpgradeRun {
	raw, err := s.db.GetSetting(ctx, settingUpgradeLast)
	if err != nil {
		return nil
	}
	var r apitypes.StackUpgradeRun
	if json.Unmarshal([]byte(raw), &r) != nil || r.JobID == 0 {
		return nil
	}
	return &r
}

func (s *Server) saveUpgradeRun(ctx context.Context, r apitypes.StackUpgradeRun) {
	b, _ := json.Marshal(r)
	if err := s.db.SetSetting(ctx, settingUpgradeLast, string(b)); err != nil {
		s.log.Warn("the outcome of the upgrade is not recorded", "job", r.JobID, "err", err)
	}
}

// autoUpgradeView is the daily upgrade for the clients.
func (s *Server) autoUpgradeView(ctx context.Context, now time.Time) apitypes.StackAutoUpgrade {
	c := s.loadAutoUpgrade(ctx)
	v := apitypes.StackAutoUpgrade{Enabled: !c.Off, Hour: c.hour(), Timezone: zoneName(now), Last: s.lastUpgrade(ctx)}
	if v.Enabled {
		next := nextUpgradeAt(now, v.Hour, s.upgradeQueuedOn(ctx, now))
		v.NextRun = &next
	}
	return v
}

// upgradeQueuedOn tells whether the scheduler queued the daily upgrade of
// now's day already: its idempotency key is the day's (scheduledStackUpgrade),
// so it queues none more that day, at whatever hour — not after the hour
// was moved later, nor after an upgrade by hand. The record of the last
// upgrade cannot tell: a run by hand after the day's replaces it.
func (s *Server) upgradeQueuedOn(ctx context.Context, now time.Time) bool {
	_, err := s.db.JobByIdempotencyKey(ctx, "stack.upgrade:"+now.Format(time.DateOnly))
	return err == nil
}

// nextUpgradeAt is when the daily upgrade runs next: today at the hour, or —
// once the scheduler queued today's, or the hour is over — tomorrow.
func nextUpgradeAt(now time.Time, hour int, queuedToday bool) time.Time {
	today := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if !queuedToday && (now.Before(today) || now.Hour() == hour) {
		return today
	}
	return today.AddDate(0, 0, 1)
}

// zoneName names the zone of the server's clock: Asia/Almaty from
// /etc/localtime, else its abbreviation and offset.
func zoneName(now time.Time) string {
	if n := now.Location().String(); n != "Local" && n != "" {
		return n
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok && name != "" {
			return name
		}
	}
	return now.Format("MST -07:00")
}

type stackUpgradeInput struct {
	// A pointer: the body is optional, none is all (huma requires a body
	// held by value).
	Body *apitypes.StackUpgradeRequest
}

type autoUpgradeOutput struct {
	Body apitypes.StackAutoUpgrade
}

type autoUpgradeInput struct {
	Body apitypes.StackAutoUpgradeUpdate
}

func (s *Server) registerStackUpgrade() {
	huma.Register(s.api, huma.Operation{
		OperationID: "stack-upgrade", Method: http.MethodPost, Path: "/stack/upgrade", Summary: "Upgrade the panel's packages of a component from their repositories (async)", Tags: []string{"stack"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *stackUpgradeInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		comp := ""
		if in.Body != nil {
			comp = in.Body.Component
		}
		if comp == "" {
			comp = upgradeAll
		}
		if err := s.upgradeWanted(ctx, comp); err != nil {
			return nil, err
		}
		job, err := s.jobs.Enqueue(ctx, "stack.upgrade", stackUpgradePayload{Component: comp}, jobs.WithLockKey("stack"), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "stack.upgrade", Target: comp, IP: requestInfo(ctx).IP})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "stack-auto-upgrade", Method: http.MethodGet, Path: "/stack/auto-upgrade", Summary: "The daily upgrade of nginx, PHP and Apache and how the last upgrade went", Tags: []string{"stack"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*autoUpgradeOutput, error) {
		return &autoUpgradeOutput{Body: s.autoUpgradeView(ctx, time.Now())}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "stack-auto-upgrade-set", Method: http.MethodPut, Path: "/stack/auto-upgrade", Summary: "Switch the daily upgrade of nginx, PHP and Apache on or off, or move it to another hour", Tags: []string{"stack"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *autoUpgradeInput) (*autoUpgradeOutput, error) {
		p := principalFrom(ctx)
		c := s.loadAutoUpgrade(ctx)
		if in.Body.Enabled != nil {
			c.Off = !*in.Body.Enabled
		}
		if in.Body.Hour != nil {
			h := *in.Body.Hour
			c.Hour = &h
		}
		if err := s.saveAutoUpgrade(ctx, c); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "stack.auto-upgrade", IP: requestInfo(ctx).IP, Details: map[string]any{"enabled": !c.Off, "hour": c.hour()}})
		return &autoUpgradeOutput{Body: s.autoUpgradeView(ctx, time.Now())}, nil
	})
}

// upgradeWanted refuses an upgrade of a component the panel did not install.
func (s *Server) upgradeWanted(ctx context.Context, comp string) error {
	switch comp {
	case osprofile.UpgradeNginx:
		if q, err := s.agent.Pkg(ctx, "query", "nginx"); err == nil && q.Installed["nginx"] == "" {
			return huma.Error422UnprocessableEntity("nginx is not installed (mp stack install nginx)")
		}
	case osprofile.UpgradeApache:
		if !s.apacheInstalled(ctx) {
			return huma.Error422UnprocessableEntity("Apache is not installed (mp stack install apache)")
		}
	case osprofile.UpgradePHP:
		if len(s.upgradeTargets(ctx, comp)) == 0 {
			return huma.Error422UnprocessableEntity("no PHP branch is installed (mp php install <version>)")
		}
	case osprofile.UpgradeDB:
		if _, err := s.dbInstance(ctx); err != nil {
			return huma.Error422UnprocessableEntity(err.Error())
		}
	}
	return nil
}

// upgradeTarget is a row of the stack list mp stack upgrade upgrades: one
// component's packages on this host.
type upgradeTarget struct {
	component string // nginx, php, apache, db
	name      string // the row: nginx, apache, php-8.4, percona, mysql
	php       string // the branch of a php row
	main      string // the package whose version the row shows
	packages  []string
}

// upgradeTargets are the rows of the components asked for (upgradeAll:
// nginx, PHP and Apache): nginx, the panel's Apache, every installed PHP
// branch, the ready database server.
func (s *Server) upgradeTargets(ctx context.Context, comps ...string) []upgradeTarget {
	var out []upgradeTarget
	for _, c := range comps {
		switch c {
		case upgradeAll:
			out = append(out, s.upgradeTargets(ctx, osprofile.UpgradeNginx, osprofile.UpgradePHP, osprofile.UpgradeApache)...)
		case osprofile.UpgradeNginx:
			out = append(out, upgradeTarget{component: c, name: "nginx", main: "nginx", packages: osprofile.NginxPackages(s.profile)})
		case osprofile.UpgradeApache:
			if pkgs := osprofile.ApachePackages(s.profile); len(pkgs) > 0 && s.apacheInstalled(ctx) {
				out = append(out, upgradeTarget{component: c, name: "apache", main: pkgs[0], packages: pkgs})
			}
		case osprofile.UpgradePHP:
			versions, err := s.db.ListPHPVersions(ctx)
			if err != nil {
				continue
			}
			var branches []upgradeTarget
			for _, v := range versions {
				if l := phpLayout(s, v.Version); l != nil && v.Status == store.PHPInstalled {
					branches = append(branches, upgradeTarget{component: c, name: "php-" + v.Version, php: v.Version, main: l.FPMPackage, packages: osprofile.PHPPackages(s.profile, v.Version)})
				}
			}
			// What every branch depends on (Sury's php-common) goes with the
			// newest one: the list is by version.
			if n := len(branches); n > 0 {
				branches[n-1].packages = append(branches[n-1].packages, osprofile.PHPSharedPackages(s.profile)...)
			}
			out = append(out, branches...)
		case osprofile.UpgradeDB:
			if inst, err := s.dbInstance(ctx); err == nil {
				if pkgs := osprofile.DBPackages(s.profile, inst.Engine); len(pkgs) > 0 {
					out = append(out, upgradeTarget{component: c, name: inst.Engine, main: osprofile.DB(s.profile, inst.Engine).Packages[0], packages: pkgs})
				}
			}
		}
	}
	return out
}

// upgradeState is what is installed of the targets' packages and what their
// repositories offer, by row; held is what the administrator holds back
// (apt-mark hold), which the panel leaves as it is.
type upgradeState struct {
	installed map[string]string
	byName    map[string]*apitypes.StackUpgrade
	held      map[string]*apitypes.StackUpgrade
}

// present tells whether a target's component is installed at all.
func (st *upgradeState) present(t upgradeTarget) bool { return st.installed[t.main] != "" }

// upgradeState asks the agent about the targets' packages, from the index as
// the host last fetched it.
func (s *Server) upgradeState(ctx context.Context, targets []upgradeTarget) (*upgradeState, error) {
	var names []string
	for _, t := range targets {
		for _, p := range t.packages {
			if !slices.Contains(names, p) {
				names = append(names, p)
			}
		}
	}
	st := &upgradeState{installed: map[string]string{}, byName: map[string]*apitypes.StackUpgrade{}, held: map[string]*apitypes.StackUpgrade{}}
	if len(names) == 0 {
		return st, nil
	}
	res, err := s.agent.Pkg(ctx, "candidates", names...)
	if err != nil {
		return nil, err
	}
	if res.Installed != nil {
		st.installed = res.Installed
	}
	for _, t := range targets {
		if up := stackUpgradeOf(t, st.installed, res.Upgrades); up != nil {
			st.byName[t.name] = up
		}
		if up := stackUpgradeOf(t, st.installed, res.Held); up != nil {
			st.held[t.name] = up
		}
	}
	return st, nil
}

// stackUpgradeOf is a row's update among the candidates, led by its main
// package when that has one; nil when none of its packages has.
func stackUpgradeOf(t upgradeTarget, installed map[string]string, cands map[string]osprofile.Candidate) *apitypes.StackUpgrade {
	var pkgs []apitypes.PackageUpgrade
	for _, p := range t.packages {
		if c, ok := cands[p]; ok {
			pkgs = append(pkgs, apitypes.PackageUpgrade{Name: p, From: installed[p], To: c.Version, Origin: c.Origin})
		}
	}
	if len(pkgs) == 0 {
		return nil
	}
	up := &apitypes.StackUpgrade{Component: t.component, Packages: pkgs}
	lead := pkgs[0]
	for _, pu := range pkgs {
		if pu.Name == t.main {
			lead = pu
		}
	}
	up.From, up.To, up.Origin = lead.From, lead.To, lead.Origin
	return up
}

// heldNames are the packages of an update, "name version".
func heldNames(up *apitypes.StackUpgrade) (names, list []string) {
	for _, p := range up.Packages {
		names = append(names, p.Name)
		list = append(list, p.Name+" "+p.From)
	}
	return names, list
}

// annotateUpgrades puts the updates the repositories offer on the stack
// list's rows, and the PHP rows' versions as they are installed now (the
// recorded one is that of the branch's install). A listing that cannot ask
// stays without them.
func (s *Server) annotateUpgrades(ctx context.Context, comps []apitypes.StackComponent) {
	targets := s.upgradeTargets(ctx, upgradeAll, osprofile.UpgradeDB)
	st, err := s.upgradeState(ctx, targets)
	if err != nil {
		s.log.Debug("stack list: the repositories' versions", "err", err)
		return
	}
	mains := map[string]string{}
	for _, t := range targets {
		mains[t.name] = t.main
	}
	for i := range comps {
		c := &comps[i]
		if !c.Installed {
			continue
		}
		c.Upgrade = st.byName[c.Name]
		if v := st.installed[mains[c.Name]]; v != "" && c.Kind == "php" {
			c.Version = v
		}
	}
}

type stackUpgradePayload struct {
	Component string `json:"component"`
	Scheduled bool   `json:"scheduled,omitempty"`
}

// jobStackUpgrade runs mp stack upgrade and records how it went: the doctor
// reports a failed one until an upgrade succeeds, and a failed job goes to
// the webhooks of job.failed and stack.upgrade.failed.
func (s *Server) jobStackUpgrade(ctx context.Context, jc *jobs.Context) error {
	var p stackUpgradePayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	if p.Component == "" {
		p.Component = upgradeAll
	}
	run := apitypes.StackUpgradeRun{JobID: jc.ID, Component: p.Component, Scheduled: p.Scheduled, At: time.Now().UTC()}
	err := s.stackUpgrade(ctx, jc, p.Component, &run)
	if errors.Is(ctx.Err(), context.Canceled) {
		// The panel is stopping: the job runs again after the restart.
		return err
	}
	if err != nil {
		// The record is what the doctor, Settings and the TUI show until an
		// upgrade succeeds: a line, not the package manager's whole output
		// (an apt-get that failed brings up to 256 KB of it), which stays in
		// the job's error and log (mp job show). Its end stays in the line:
		// that is where apt-get and dnf say why.
		run.Error = oneLineEnds(err.Error(), upgradeErrorLen)
	}
	s.saveUpgradeRun(context.WithoutCancel(ctx), run)
	return err
}

// upgradeErrorLen is about how much of a failed upgrade's error the record
// keeps.
const upgradeErrorLen = 300

// stackUpgrade upgrades the component's packages that have an update, one
// component after the other, and settles what runs them. A component that
// fails does not stop the others; the job fails with all their errors.
func (s *Server) stackUpgrade(ctx context.Context, jc *jobs.Context, component string, run *apitypes.StackUpgradeRun) error {
	comps := []string{component}
	if component == upgradeAll {
		comps = []string{osprofile.UpgradeNginx, osprofile.UpgradePHP, osprofile.UpgradeApache}
	}
	targets := s.upgradeTargets(ctx, comps...)
	var errs []string
	jc.Progress(5, "package index")
	if res, err := s.agent.Pkg(ctx, "update-index"); err != nil {
		// What the index knew before still upgrades; the failure may hide a
		// fix, so the job reports it in the end.
		jc.Logf("the package index was not updated, upgrading from what it knew before: %v", err)
		errs = append(errs, "package index: "+err.Error())
	} else {
		logTail(jc, res.Output, 2)
	}
	jc.Progress(15, "what the repositories offer")
	st, err := s.upgradeState(ctx, targets)
	if err != nil {
		return err
	}
	var present []upgradeTarget
	for _, t := range targets {
		if !st.present(t) {
			continue
		}
		present = append(present, t)
		if up := st.byName[t.name]; up != nil {
			more := ""
			if n := len(up.Packages); n > 1 {
				more = fmt.Sprintf(" (%d packages)", n)
			}
			jc.Logf("%s: %s %s → %s%s%s", t.name, t.main, up.From, up.To, fromOrigin(up.Origin), more)
		} else if st.held[t.name] == nil {
			jc.Logf("%s: %s %s, nothing newer", t.name, t.main, st.installed[t.main])
		}
		if up := st.held[t.name]; up != nil {
			names, list := heldNames(up)
			jc.Logf("%s: held back by the administrator (apt-mark hold), left as they are: %s; apt-mark unhold %s lets the upgrade take them", t.name, strings.Join(list, ", "), strings.Join(names, " "))
		}
	}
	if len(present) == 0 {
		jc.Logf("nothing of %s is installed", component)
	}
	pct := 20
	for _, comp := range []string{osprofile.UpgradeNginx, osprofile.UpgradePHP, osprofile.UpgradeApache, osprofile.UpgradeDB} {
		var ts []upgradeTarget
		for _, t := range present {
			if t.component == comp {
				ts = append(ts, t)
			}
		}
		if len(ts) == 0 {
			continue
		}
		jc.Progress(pct, comp)
		pct += 20
		if err := s.upgradeComponent(ctx, jc, comp, ts, st, run); err != nil {
			jc.Logf("%s: %v", comp, err)
			errs = append(errs, comp+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	if len(run.Upgraded) == 0 {
		jc.Progress(100, "nothing to upgrade")
	} else {
		jc.Progress(100, fmt.Sprintf("%d package(s) upgraded", len(run.Upgraded)))
	}
	return nil
}

func fromOrigin(origin string) string {
	if origin == "" {
		return ""
	}
	return " from " + origin
}

// upgradeComponent upgrades the packages of a component that have an update
// and brings what runs them onto the new version: each configuration is
// checked with the version that runs before (upgradePrecheck) and with the
// new one after. A check that fails stops there for its service, which runs
// on as it was; nothing is rolled back.
func (s *Server) upgradeComponent(ctx context.Context, jc *jobs.Context, comp string, ts []upgradeTarget, st *upgradeState, run *apitypes.StackUpgradeRun) error {
	ts, refused := s.upgradePrecheck(ctx, ts, st)
	if len(ts) == 0 {
		return errors.New(strings.Join(refused, "; "))
	}
	err := s.upgradeAndSettle(ctx, jc, comp, ts, st, run)
	if len(refused) == 0 {
		return err
	}
	if err != nil {
		refused = append(refused, err.Error())
	}
	return errors.New(strings.Join(refused, "; "))
}

// upgradeCheckArgv is the check of a target whose packages restart its
// service themselves, nil for the others: nginx.org's nginx does its binary
// upgrade (and on Debian kills the old master when the new one does not come
// up), Debian's php-fpm and the database server's packages restart the
// server and go on when that fails. Apache's packages restart none of the
// accounts' Apaches, each of which is checked before its own restart; the
// shared apache2.service they do restart, which an unfinished move keeps
// running, upgradePrecheck checks.
func (s *Server) upgradeCheckArgv(t upgradeTarget) []string {
	switch t.component {
	case osprofile.UpgradeNginx:
		return s.profile.Web().NginxCheckArgv
	case osprofile.UpgradePHP:
		if l := phpLayout(s, t.php); l != nil {
			return l.FPMCheckArgv
		}
	case osprofile.UpgradeDB:
		return []string{mysqldBinary, "--validate-config", "--user=mysql"}
	}
	return nil
}

// upgradePrecheck keeps out of the upgrade the targets whose configuration
// fails its check already with the version that runs: an edit nobody
// reloaded, a certificate or an include gone. The service runs on that
// version only because nothing restarted it since, and its package's own
// restart in the middle of the upgrade would take it down — at night, with
// nobody there. Such a target is left as it is, and the job says why.
func (s *Server) upgradePrecheck(ctx context.Context, ts []upgradeTarget, st *upgradeState) ([]upgradeTarget, []string) {
	var keep []upgradeTarget
	var refused []string
	for _, t := range ts {
		argv := s.upgradeCheckArgv(t)
		restarts := "its package would restart it"
		if t.component == osprofile.UpgradeApache && st.byName[t.name] != nil {
			// Apache's packages restart the shared apache2.service, which
			// an unfinished move keeps running for the sites still on it.
			if s.sharedApacheRuns(ctx) {
				argv = confcheck.ApacheSharedCheck()
				restarts = "its package would restart " + s.profile.Web().ApacheService + ", which still serves the sites that have not moved,"
			}
		}
		if st.byName[t.name] == nil || argv == nil {
			keep = append(keep, t)
			continue
		}
		// Restore: nginx -t makes the pid file with the agent's SELinux
		// label when nginx is down (stack.go).
		req := &agent.ApplyConfigSetRequest{Validate: [][]string{argv}, Origin: "stack:upgrade"}
		if t.component == osprofile.UpgradeNginx {
			req.Restore = []string{"/run/nginx.pid"}
		}
		if _, err := s.agent.ApplyConfigSet(ctx, req); err != nil {
			refused = append(refused, fmt.Sprintf("%s: %s fails with the version that runs now: %v; not upgraded, as %s on this configuration and take it down (fix the configuration, then mp stack upgrade %s)",
				t.name, strings.Join(argv, " "), err, restarts, t.component))
			continue
		}
		keep = append(keep, t)
	}
	return keep, refused
}

// upgradeAndSettle upgrades the targets' packages that have an update and
// settles what runs them.
func (s *Server) upgradeAndSettle(ctx context.Context, jc *jobs.Context, comp string, ts []upgradeTarget, st *upgradeState, run *apitypes.StackUpgradeRun) error {
	before := map[string]systemd.Status{}
	for _, u := range s.upgradeUnits(ctx, comp, ts) {
		if res, err := s.agent.Service(ctx, u, "status"); err == nil {
			before[u] = res.Status
		}
	}
	var pkgs, kept []string
	var ups []apitypes.PackageUpgrade
	changed := map[string]bool{}
	for _, t := range ts {
		up := st.byName[t.name]
		if up == nil {
			continue
		}
		changed[t.name] = true
		for _, p := range up.Packages {
			if !slices.Contains(pkgs, p.Name) {
				pkgs = append(pkgs, p.Name)
				ups = append(ups, p)
			}
		}
	}
	if len(pkgs) > 0 {
		jc.Logf("%s: upgrading %s", comp, strings.Join(pkgs, " "))
		res, err := s.agent.Pkg(ctx, "upgrade", pkgs...)
		if err != nil {
			// What a package's maintainer scripts did before the failure
			// (Debian's restart php-fpm or the database server) is theirs.
			return fmt.Errorf("%w (the panel restarted no service; a package's own scripts may have stopped or restarted one)", err)
		}
		logTail(jc, res.Output, 4)
		for _, p := range ups {
			if got := res.Installed[p.Name]; got != "" && got != p.From {
				run.Upgraded = append(run.Upgraded, fmt.Sprintf("%s %s → %s", p.Name, p.From, got))
				jc.Logf("%s %s → %s", p.Name, p.From, got)
			} else {
				kept = append(kept, p.Name+" "+p.From)
			}
		}
		if len(kept) > 0 {
			jc.Logf("%s: still at the old version although a newer one is offered: %s", comp, strings.Join(kept, ", "))
		}
	}
	upgraded := len(pkgs) > 0
	var err error
	switch comp {
	case osprofile.UpgradeNginx:
		err = s.settleNginx(ctx, jc, before, upgraded)
	case osprofile.UpgradePHP:
		err = s.settlePHP(ctx, jc, ts, before, changed)
	case osprofile.UpgradeApache:
		err = s.settleApache(ctx, jc, before, upgraded)
	case osprofile.UpgradeDB:
		err = s.settleDB(ctx, jc, before, upgraded)
	}
	if len(kept) > 0 {
		// The package manager said yes and kept them (a pin, a dependency
		// it could not meet without removing a package): the fix did not
		// arrive.
		held := fmt.Errorf("not upgraded although a newer version is offered: %s", strings.Join(kept, ", "))
		if err != nil {
			held = fmt.Errorf("%w; %v", err, held)
		}
		err = held
	}
	return err
}

// upgradeUnits are the services that run a component.
func (s *Server) upgradeUnits(ctx context.Context, comp string, ts []upgradeTarget) []string {
	switch comp {
	case osprofile.UpgradeNginx:
		return []string{s.profile.Web().NginxService}
	case osprofile.UpgradePHP:
		var out []string
		for _, t := range ts {
			if l := phpLayout(s, t.php); l != nil {
				out = append(out, l.FPMService)
			}
		}
		return out
	case osprofile.UpgradeApache:
		// The accounts' Apaches are read one by one as they are settled;
		// the shared one when it is in the way (settleSharedApache).
		if s.sharedApacheRuns(ctx) {
			return []string{s.profile.Web().ApacheService}
		}
	case osprofile.UpgradeDB:
		if inst, err := s.dbInstance(ctx); err == nil {
			return []string{inst.Service}
		}
	}
	return nil
}

// restartedSince tells that a service started again after the before state
// was taken: its package's scripts restarted it during the upgrade (the
// database server's, whose package holds the server and its plugins), so it
// runs the new version.
func restartedSince(before, now systemd.Status) bool {
	return now.ActiveState == "active" && !now.ActiveSince.IsZero() && now.ActiveSince.After(before.ActiveSince)
}

// settleNginx validates the configuration with the new nginx and reloads
// it. nginx.org's packages upgrade the running binary themselves (USR2, then
// the old master leaves once its requests are done; the agent counts nginx
// as on the new binary as soon as the new master names itself in the PID
// file): a reload re-reads the configuration only, so an nginx still on the
// replaced binary is restarted instead. Nothing happens when nothing changed
// and nginx runs the binary in place.
func (s *Server) settleNginx(ctx context.Context, jc *jobs.Context, before map[string]systemd.Status, changed bool) error {
	web := s.profile.Web()
	unit := web.NginxService
	cur, err := s.agent.Service(ctx, unit, "status")
	if err != nil {
		return err
	}
	now := cur.Status
	if !changed && !now.BinaryReplaced {
		return nil
	}
	if changed && now.BinaryReplaced {
		now = s.waitOldMaster(ctx, unit, now)
	}
	was := before[unit]
	if now.ActiveState != "active" && was.ActiveState != "active" {
		jc.Logf("%s did not run before the upgrade and stays so", unit)
		return nil
	}
	req := &agent.ApplyConfigSetRequest{Validate: [][]string{web.NginxCheckArgv}, Restore: []string{"/run/nginx.pid"}, Force: true, Origin: "stack:upgrade"}
	how := "reloaded: it runs the new nginx (the package's own binary upgrade)"
	switch {
	case now.ActiveState != "active":
		req.Restart, how = []string{unit}, "started again: it stopped during the upgrade"
	case now.BinaryReplaced:
		req.Restart, how = []string{unit}, "restarted: it still ran the nginx the upgrade replaced"
	default:
		req.Reload = []string{unit}
	}
	if _, err := s.agent.ApplyConfigSet(ctx, req); err != nil {
		return fmt.Errorf("%s with the new nginx: %w; %s", strings.Join(web.NginxCheckArgv, " "), err, s.settleFailure(ctx, unit, now, err, "nginx was not reloaded and runs on as it was"))
	}
	jc.Logf("%s: the configuration is valid with the new nginx; %s %s", strings.Join(web.NginxCheckArgv, " "), unit, how)
	return s.upgradedUnitRuns(ctx, unit)
}

// waitOldMaster gives nginx's binary upgrade the time to get there: until
// the new master writes the PID file, systemd's main process, the old
// master, is all the agent sees, on the replaced binary. The wait is the one
// for nginx after a reload (SetReadinessWaits).
func (s *Server) waitOldMaster(ctx context.Context, unit string, now systemd.Status) systemd.Status {
	deadline := time.Now().Add(s.nginxWait)
	for now.BinaryReplaced && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return now
		case <-time.After(500 * time.Millisecond):
		}
		if st, err := s.agent.Service(ctx, unit, "status"); err == nil {
			now = st.Status
		}
	}
	return now
}

// settlePHP restarts the php-fpm of every branch whose packages changed,
// after php-fpm -t, once the whole upgrade is in: a php-fpm loads its
// extensions when it starts, and one its own package restarted halfway
// through (Debian's do) may have loaded an extension the run replaced only
// after that.
func (s *Server) settlePHP(ctx context.Context, jc *jobs.Context, ts []upgradeTarget, before map[string]systemd.Status, changed map[string]bool) error {
	var errs []string
	for _, t := range ts {
		l := phpLayout(s, t.php)
		if l == nil {
			continue
		}
		cur, err := s.agent.Service(ctx, l.FPMService, "status")
		if err != nil {
			errs = append(errs, fmt.Sprintf("PHP %s: %v", t.php, err))
			continue
		}
		now, was := cur.Status, before[l.FPMService]
		switch {
		case !changed[t.name] && !now.BinaryReplaced:
			continue
		case now.ActiveState != "active" && was.ActiveState != "active":
			jc.Logf("%s did not run before the upgrade and stays so", l.FPMService)
		default:
			if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Validate: [][]string{l.FPMCheckArgv}, Restart: []string{l.FPMService}, Force: true, Origin: "php:" + t.php}); err != nil {
				errs = append(errs, fmt.Sprintf("PHP %s: %s with the new php-fpm: %v; %s", t.php, strings.Join(l.FPMCheckArgv, " "), err, s.settleFailure(ctx, l.FPMService, now, err, "php-fpm was not restarted and runs on as it was")))
				continue
			}
			jc.Logf("%s: php-fpm -t passed with the new PHP %s; restarted", l.FPMService, t.php)
			if err := s.upgradedUnitRuns(ctx, l.FPMService); err != nil {
				errs = append(errs, fmt.Sprintf("PHP %s: %v", t.php, err))
				continue
			}
		}
		s.recordPHPPackage(ctx, t.php, l.FPMPackage)
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// recordPHPPackage keeps the branch's recorded package version with what is
// installed now (the PHP page shows it).
func (s *Server) recordPHPPackage(ctx context.Context, version, pkg string) {
	q, err := s.agent.Pkg(ctx, "query", pkg)
	if err != nil || q.Installed[pkg] == "" {
		return
	}
	row, err := s.db.GetPHPVersion(ctx, version)
	if err != nil || row.PackageVersion == q.Installed[pkg] {
		return
	}
	row.PackageVersion = q.Installed[pkg]
	if err := s.db.UpsertPHPVersion(context.WithoutCancel(ctx), row); err != nil {
		s.log.Warn("the PHP branch's package version", "version", version, "err", err)
	}
}

// settleApache brings every account's Apache onto the new Apache, after
// apache2 -t on its httpd.conf: a graceful restart when the binary it runs is
// still in place (its modules load anew), a restart when the upgrade replaced
// it — a graceful restart keeps the parent, and with it the old binary,
// running. The package restarts none of them: they are the panel's units.
// An Apache that does not run is left to the site.apply that needs it.
func (s *Server) settleApache(ctx context.Context, jc *jobs.Context, before map[string]systemd.Status, changed bool) error {
	var errs []string
	if err := s.settleSharedApache(ctx, jc, before[s.profile.Web().ApacheService], changed); err != nil {
		errs = append(errs, err.Error())
	}
	users, err := s.apacheAccounts(ctx)
	if err != nil {
		return errors.New(strings.Join(append(errs, err.Error()), "; "))
	}
	for _, u := range users {
		inst, err := s.apacheInstanceFor(u)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if err := s.upgradeApacheInstance(ctx, jc, u, inst, changed); err != nil {
			errs = append(errs, fmt.Sprintf("the Apache of %s: %v", u.Login, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (s *Server) upgradeApacheInstance(ctx context.Context, jc *jobs.Context, u *store.User, inst *apacheInstance, changed bool) error {
	defer s.lockApache(u.Login)()
	cur, err := s.agent.Service(ctx, inst.unit, "status")
	if err != nil {
		return err
	}
	now := cur.Status
	if now.ActiveState != "active" || (!changed && !now.BinaryReplaced) {
		return nil
	}
	if err := s.ensureApacheRunDir(ctx, inst); err != nil {
		return err
	}
	req := &agent.ApplyConfigSetRequest{Validate: [][]string{inst.check}, Force: true, Origin: "stack:upgrade"}
	how := "graceful restart"
	if now.BinaryReplaced {
		req.Restart, how = []string{inst.unit}, "restarted: a graceful restart keeps the replaced binary running"
	} else {
		req.Reload = []string{inst.unit}
	}
	if _, err := s.agent.ApplyConfigSet(ctx, req); err != nil {
		return fmt.Errorf("apache2 -t with the new Apache: %w; %s", err, s.settleFailure(ctx, inst.unit, now, err, "it was not restarted and runs on as it was"))
	}
	if now.BinaryReplaced {
		if err := s.waitApache(ctx, inst); err != nil {
			return err
		}
	}
	jc.Logf("Apache of %s: apache2 -t passed; %s %s", u.Login, inst.unit, how)
	return s.upgradedUnitRuns(ctx, inst.unit)
}

// sharedApacheRuns tells whether the shared apache2.service of 0.8.19 runs
// and is the upgrade's to look after: while the move of an upgrade has not
// finished it serves the sites that did not move, and Apache's packages
// restart or reload it themselves (Debian's scripts act on an enabled unit)
// — the upgrade checks its configuration before and settles it after, as it
// does nginx's. Once the move finished it is off and none of the panel's.
func (s *Server) sharedApacheRuns(ctx context.Context) bool {
	if s.profile.Family() != osprofile.FamilyDebian {
		return false
	}
	if v, _ := s.db.GetSetting(ctx, settingApacheInstances); v == "yes" {
		return false
	}
	st, err := s.agent.Service(ctx, s.profile.Web().ApacheService, "status")
	return err == nil && st.Status.ActiveState == "active"
}

// settleSharedApache brings the shared apache2.service, which ran before
// the upgrade (was), onto the new Apache as the accounts' Apaches are
// brought there, after apache2ctl -t: a graceful restart when the binary it
// runs is in place (its modules load anew), a restart when it still runs the
// one the upgrade replaced — a graceful reload by its package keeps the
// parent, and with it the old binary — or when the package's restart took
// it down. One the check refuses stays as it is, and the job says so.
func (s *Server) settleSharedApache(ctx context.Context, jc *jobs.Context, was systemd.Status, changed bool) error {
	unit := s.profile.Web().ApacheService
	if was.ActiveState != "active" {
		return nil
	}
	cur, err := s.agent.Service(ctx, unit, "status")
	if err != nil {
		return err
	}
	now := cur.Status
	req := &agent.ApplyConfigSetRequest{Validate: [][]string{confcheck.ApacheSharedCheck()}, Force: true, Origin: "stack:upgrade"}
	how := "graceful restart"
	switch {
	case !changed && !now.BinaryReplaced:
		return nil
	case now.ActiveState != "active":
		req.Restart, how = []string{unit}, "started again: it stopped during the upgrade"
	case now.BinaryReplaced:
		req.Restart, how = []string{unit}, "restarted: a graceful restart keeps the replaced binary running"
	default:
		req.Reload = []string{unit}
	}
	if _, err := s.agent.ApplyConfigSet(ctx, req); err != nil {
		return fmt.Errorf("%s with the new Apache: %w; %s", strings.Join(confcheck.ApacheSharedCheck(), " "), err, s.settleFailure(ctx, unit, now, err, unit+" was not restarted and runs on as it was"))
	}
	jc.Logf("%s: the shared configuration is valid with the new Apache; %s %s", strings.Join(confcheck.ApacheSharedCheck(), " "), unit, how)
	return s.upgradedUnitRuns(ctx, unit)
}

// settleDB brings the database server onto its new version when its
// package's scripts did not: mysqld --validate-config with the new server,
// then the restart that takes the sites' databases away for a while — the
// reason db is upgraded only when asked for by name. The record keeps the
// version the server reports.
func (s *Server) settleDB(ctx context.Context, jc *jobs.Context, before map[string]systemd.Status, changed bool) error {
	inst, err := s.dbInstance(ctx)
	if err != nil {
		return err
	}
	cur, err := s.agent.Service(ctx, inst.Service, "status")
	if err != nil {
		return err
	}
	now, was := cur.Status, before[inst.Service]
	switch {
	case !changed && !now.BinaryReplaced:
		return nil
	case !now.BinaryReplaced && restartedSince(was, now):
		jc.Logf("%s: its package restarted it during the upgrade", inst.Service)
	default:
		if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Validate: [][]string{{mysqldBinary, "--validate-config", "--user=mysql"}}, Restart: []string{inst.Service}, Force: true, Origin: "db:upgrade"}); err != nil {
			return fmt.Errorf("mysqld --validate-config with the new server: %w; %s", err, s.settleFailure(ctx, inst.Service, now, err, "it was not restarted and runs on as it was"))
		}
		jc.Logf("%s: mysqld --validate-config passed; restarted", inst.Service)
	}
	var out string
	for i := 0; i < 30; i++ {
		if out, err = s.mysqlExec(ctx, "SELECT VERSION();"); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dbUpgradeWait):
		}
	}
	if err != nil {
		return fmt.Errorf("the database server does not answer after the upgrade: %v (journalctl -u %s)", err, inst.Service)
	}
	if v := strings.TrimSpace(out); v != "" && v != inst.Version {
		jc.Logf("%s %s → %s", inst.Engine, inst.Version, v)
		inst.Version = v
		if err := s.db.UpsertDBInstance(context.WithoutCancel(ctx), inst); err != nil {
			return err
		}
	}
	return s.upgradedUnitRuns(ctx, inst.Service)
}

// dbUpgradeWait paces the wait for the database server after its upgrade
// (30 tries); tests shorten it.
var dbUpgradeWait = 2 * time.Second

// leftAs says what became of a service whose check failed after the
// upgrade, so the panel did not reload or restart it: it runs on as it was
// (ran) — unless its package's own restart during the upgrade took it down
// on this configuration (Debian's php-fpm and the database server's
// packages go on when their restart fails), and then it stays down.
func leftAs(unit string, now systemd.Status, ran string) string {
	if now.ActiveState == "active" {
		return ran
	}
	return fmt.Sprintf("%s is %s since its package restarted it during the upgrade, and the panel did not start it again: fix the configuration, then systemctl start %s (journalctl -u %s)", unit, now.ActiveState, unit, unit)
}

// settleFailure says what became of a service after the config/apply that
// was to check its configuration with the new version and then reload or
// restart it failed, from its status read again (now, read before, when
// that read fails): a check that failed (422) touched nothing, and the
// service runs on as it was (ran) unless its package's own restart took it
// down (leftAs); a reload or restart that failed after the check passed
// ("files applied but restart failed") took it down itself, and saying that
// it runs on would hide that its sites are down.
func (s *Server) settleFailure(ctx context.Context, unit string, now systemd.Status, err error, ran string) string {
	if st, serr := s.agent.Service(ctx, unit, "status"); serr == nil {
		now = st.Status
	}
	var ae *agent.Error
	if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity {
		return leftAs(unit, now, ran)
	}
	if now.ActiveState != "active" {
		return fmt.Sprintf("%s was restarted but did not come up (%s): journalctl -u %s", unit, now.ActiveState, unit)
	}
	return ran
}

// upgradedUnitRuns checks a service after it was settled: running, and on
// the binary in place.
func (s *Server) upgradedUnitRuns(ctx context.Context, unit string) error {
	st, err := s.agent.Service(ctx, unit, "status")
	if err != nil {
		return err
	}
	switch {
	case st.Status.ActiveState != "active":
		return fmt.Errorf("%s is %s after the upgrade (journalctl -u %s)", unit, st.Status.ActiveState, unit)
	case st.Status.BinaryReplaced:
		return fmt.Errorf("%s still runs the binary the upgrade replaced", unit)
	}
	return nil
}

// upgradeLoop queues the daily upgrade of nginx, PHP and Apache in the hour
// the administrator chose, looking every ten minutes: the hourly loop of the
// backups ticks an hour after the API started and would miss the hour
// whenever a restart fell into it.
func (s *Server) upgradeLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.scheduledStackUpgrade(ctx, time.Now())
		}
	}
}

// scheduledStackUpgrade queues the day's upgrade when it is on and its hour
// (server time) has come: once a day, whatever restarts the API had or how
// often it looks.
func (s *Server) scheduledStackUpgrade(ctx context.Context, now time.Time) *store.Job {
	c := s.loadAutoUpgrade(ctx)
	if c.Off || now.Hour() != c.hour() {
		return nil
	}
	job, err := s.jobs.Enqueue(ctx, "stack.upgrade", stackUpgradePayload{Component: upgradeAll, Scheduled: true},
		jobs.WithLockKey("stack"), jobs.WithRequestedBy("scheduler"), jobs.WithIdempotencyKey("stack.upgrade:"+now.Format(time.DateOnly)))
	if err != nil {
		s.log.Warn("daily upgrade of nginx, PHP and Apache", "err", err)
		return nil
	}
	return job
}
