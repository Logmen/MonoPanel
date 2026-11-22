package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"monopanel/internal/systemd"
)

// Where the panel's units live. The package ships them in packageUnitDir and
// its binary as packageBinary, which they run. mp setup writes its own into
// systemUnitDir only on an install without the package: a unit there
// overrides the package's for good, and every change a later release makes
// to the package's unit would pass the host by — 0.9.0 needed a drop-in for
// the API's unit (agent.apiServiceDropInContent) because mp setup up to
// 0.8.19 had copied it there on every host.
const (
	systemUnitDir  = "/etc/systemd/system"
	packageUnitDir = "/usr/lib/systemd/system"
	packageBinary  = "/usr/bin/monopanel"
)

// panelUnit is one of the panel's units as mp setup writes it.
type panelUnit struct {
	name string
	// text is the unit; "%s" stands for the panel's binary in the units
	// that run it (binary), and the texts hold no other "%s".
	text   string
	binary bool
}

// panelUnits are the panel's units, the package's texts word for word
// (TestUnitsAreThePackages), in the order mp setup writes them.
func panelUnits() []panelUnit {
	return []panelUnit{
		{agentService, agentUnit, true},
		{apiSocket, apiSocketUnit, false},
		{apiService, apiUnit, true},
		// A template: the API enables its instances, mp setup none.
		{apacheUnitName, apacheUnit, false},
	}
}

// unitText is a unit's text as written for binary.
func unitText(text, binary string) string { return strings.ReplaceAll(text, "%s", binary) }

// The texts earlier releases wrote, word for word. mp setup wrote a unit
// only where none was in /etc/systemd/system, so each of these may still be
// there on a host set up back then.
const (
	// agentUnit060 is monopanel-agent.service of 0.6.0 to 0.8.12, mp
	// setup's and the package's: no fences yet.
	agentUnit060 = `[Unit]
Description=MonoPanel privileged agent
After=network.target

[Service]
Type=simple
ExecStart=%s agent
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
`
	// agentUnit0813 is mp setup's monopanel-agent.service of 0.8.13 to
	// 0.8.19: the package's fences without its comments.
	agentUnit0813 = `[Unit]
Description=MonoPanel privileged agent
After=network.target

[Service]
Type=simple
ExecStart=%s agent
Restart=on-failure
RestartSec=2
ProtectHome=read-only
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
PrivateTmp=yes
RestrictRealtime=yes
LockPersonality=yes
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
`
	// apiUnit060 is monopanel-api.service of 0.6.0 to 0.8.19, mp setup's
	// and the package's: no socket of the panel's port yet.
	apiUnit060 = `[Unit]
Description=MonoPanel API and web interface
After=network-online.target monopanel-agent.service
Wants=network-online.target
Requires=monopanel-agent.service

[Service]
Type=simple
User=monopanel
Group=monopanel
ExecStart=%s api
Restart=on-failure
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/var/lib/monopanel /run/monopanel /var/log/monopanel
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
`
	// apacheUnit090dev is monopanel-apache@.service of 0.9.0's development
	// builds before the graceful stop, which mp setup wrote into /etc on
	// the hosts it set up (the testbed, development servers): no release
	// shipped it, but such a copy would keep its plain SIGTERM stop for good.
	apacheUnit090dev = `[Unit]
Description=Apache for the sites of %i in Apache mode (MonoPanel)
After=network.target remote-fs.target nss-lookup.target
# MonoPanel writes the configuration of an account that has sites in Apache
# mode; without it there is nothing to start.
ConditionPathExists=/etc/apache2/monopanel/accounts/%i/httpd.conf

[Service]
Type=simple
ExecStart=/usr/sbin/apache2 -f /etc/apache2/monopanel/accounts/%i/httpd.conf -DFOREGROUND
ExecReload=/bin/kill -USR1 $MAINPID
KillMode=mixed
Restart=on-failure
RestartSec=2
RuntimeDirectory=monopanel-apache/%i
UMask=0027
LimitNOFILE=8192
PrivateTmp=yes
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
SystemCallArchitectures=native
CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_SETUID CAP_SETGID CAP_KILL CAP_CHOWN CAP_DAC_READ_SEARCH

[Install]
WantedBy=multi-user.target
`
)

// unitHistory is, unit by unit, every text a release of the panel wrote into
// /etc/systemd/system (mp setup) or shipped in /usr/lib/systemd/system (the
// package), in generations, oldest first: the texts of one generation do the
// same. They are how a copy is told for the panel's own and whether it lags
// the package's unit (pruneUnitCopies). A unit that changes gets a
// generation of its own, its former text staying where it was:
// TestUnitHistoryKeepsEveryRelease pins them.
func unitHistory() map[string][][]string {
	return map[string][][]string{
		agentService: {
			{agentUnit060},
			// mp setup's of 0.8.13 to 0.8.19, and today's: the package's
			// since 0.8.13, mp setup's since 0.9.0.
			{agentUnit0813, agentUnit},
		},
		apiService:     {{apiUnit060}, {apiUnit}},
		apiSocket:      {{apiSocketUnit}},
		apacheUnitName: {{apacheUnit090dev}, {apacheUnit}},
	}
}

// generationOf is the generation of gens whose text, written for binary,
// content is; -1 when none is.
func generationOf(gens [][]string, content []byte, binary string) int {
	for i, g := range gens {
		for _, t := range g {
			if string(content) == unitText(t, binary) {
				return i
			}
		}
	}
	return -1
}

// packagedFor tells whether the package's own unit u, in packaged, serves a
// setup for binary: the package's units of the agent and the API run the
// package's binary, and a setup for another one needs units of its own.
func packagedFor(packaged string, u panelUnit, binary string) bool {
	if u.binary && filepath.Clean(binary) != packageBinary {
		return false
	}
	st, err := os.Stat(filepath.Join(packaged, u.name))
	return err == nil && st.Mode().IsRegular()
}

// maxUnitSize bounds what is read of a unit to compare it: the panel's are a
// kilobyte or two.
const maxUnitSize = 64 << 10

// readUnit is a unit file's content, when it is a plain file of a unit's size.
func readUnit(path string) ([]byte, bool) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxUnitSize {
		return nil, false
	}
	b, err := os.ReadFile(path)
	return b, err == nil
}

// pruneResult is what pruneUnitCopies did.
type pruneResult struct {
	// Removed are the copies it removed: the package's units apply.
	Removed []string
	// Shadowing are the files it left that override a unit of the package:
	// the administrator's, or one it cannot tell lags the package's.
	Shadowing []string
}

// pruneUnitCopies removes from dir (/etc/systemd/system) the copies of the
// units the package ships in packaged (/usr/lib/systemd/system) for binary
// that are provably the panel's own and no newer than the package's: the
// copy's text and the package's are both texts of unitHistory, written for
// the package's binary, and the copy's generation is not the later one. mp
// setup up to 0.8.19 left such copies on every host it set up — the
// package's own units never applied there — and a downgrade below 0.9.0
// leaves Apache's template (preremove.sh). With a copy gone the package's
// unit applies, and so does whatever its next release changes in it.
// Everything else stays: the administrator's own unit, a copy for another
// binary, a symlink (a mask among them), a copy an alias leads to; those
// that override a unit of the package are Shadowing.
//
// The symlinks that enable a copy, in dir's .wants, .requires and .upholds
// directories, are pointed at the package's unit before the copy goes, so
// enabled units and the instances of a template stay enabled the way
// systemctl enable would have made them. A running unit keeps running and
// takes the package's unit at its next start, once systemd is reloaded,
// which the caller does when anything was removed.
func pruneUnitCopies(dir, packaged, binary string) (pruneResult, error) {
	var res pruneResult
	var errs []error
	history := unitHistory()
	for _, u := range panelUnits() {
		if !packagedFor(packaged, u, binary) {
			continue
		}
		// A symlink is nobody's copy: a mask (to /dev/null), or a link the
		// administrator made on purpose.
		path := filepath.Join(dir, u.name)
		if st, err := os.Lstat(path); err != nil || st.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		pkg, okPkg := readUnit(filepath.Join(packaged, u.name))
		copied, okCopy := readUnit(path)
		e, p := generationOf(history[u.name], copied, packageBinary), generationOf(history[u.name], pkg, packageBinary)
		if !okPkg || !okCopy || e < 0 || p < 0 || e > p {
			res.Shadowing = append(res.Shadowing, path)
			continue
		}
		aliased, err := repointLinks(dir, path, filepath.Join(packaged, u.name))
		if err != nil {
			errs = append(errs, err)
			res.Shadowing = append(res.Shadowing, path)
			continue
		}
		if aliased {
			res.Shadowing = append(res.Shadowing, path)
			continue
		}
		if err := os.Remove(path); err != nil {
			errs = append(errs, err)
			res.Shadowing = append(res.Shadowing, path)
			continue
		}
		res.Removed = append(res.Removed, path)
	}
	return res, errors.Join(errs...)
}

// repointLinks points the symlinks in dir's .wants, .requires and .upholds
// directories that lead to from at to, each replaced in one rename. aliased
// says that a symlink directly in dir leads to from: an alias of the unit,
// which nothing here repoints, so from must stay.
func repointLinks(dir, from, to string) (aliased bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	leadsTo := func(parent, link string) bool {
		target, err := os.Readlink(link)
		if err != nil {
			return false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(parent, target)
		}
		return filepath.Clean(target) == from
	}
	var links []string
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.Type()&fs.ModeSymlink != 0 {
			if leadsTo(dir, p) {
				return true, nil
			}
			continue
		}
		n := e.Name()
		if !e.IsDir() || !(strings.HasSuffix(n, ".wants") || strings.HasSuffix(n, ".requires") || strings.HasSuffix(n, ".upholds")) {
			continue
		}
		deps, err := os.ReadDir(p)
		if err != nil {
			return false, err
		}
		for _, l := range deps {
			if lp := filepath.Join(p, l.Name()); l.Type()&fs.ModeSymlink != 0 && leadsTo(p, lp) {
				links = append(links, lp)
			}
		}
	}
	for _, link := range links {
		// Made under a name systemd takes for no unit, next to the link.
		tmp := filepath.Join(filepath.Dir(link), "."+filepath.Base(link)+".mp-new")
		_ = os.Remove(tmp)
		if err := os.Symlink(to, tmp); err != nil {
			return false, err
		}
		if err := os.Rename(tmp, link); err != nil {
			_ = os.Remove(tmp)
			return false, err
		}
	}
	return false, nil
}

// PruneUnitCopies removes the copies of the panel's units that mp setup or a
// package downgrade left in /etc/systemd/system where the package's own
// apply (pruneUnitCopies), and reloads systemd when it removed one — unless
// systemd does not run (a chroot, an image being built), which reads the
// units afresh when it starts. It is monopanel prune-unit-copies, which the
// package's postinstall runs: the hosts upgraded by package stop overriding
// its units without a run of mp setup.
func PruneUnitCopies(ctx context.Context) ([]string, error) {
	res, err := pruneUnitCopies(systemUnitDir, packageUnitDir, packageBinary)
	if len(res.Removed) == 0 {
		return nil, err
	}
	// sd_booted(3): systemd runs as PID 1 when this directory is there.
	if _, serr := os.Stat("/run/systemd/system"); serr != nil {
		return res.Removed, err
	}
	sd, cerr := systemd.Connect(ctx)
	if cerr != nil {
		return res.Removed, errors.Join(err, fmt.Errorf("daemon-reload: %w", cerr))
	}
	defer sd.Close()
	if rerr := sd.DaemonReload(ctx); rerr != nil {
		err = errors.Join(err, fmt.Errorf("daemon-reload: %w", rerr))
	}
	return res.Removed, err
}
