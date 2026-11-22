package agent

import (
	"strings"

	"monopanel/internal/agent/confcheck"
)

// The agent acts on systemd units for the API: the panel's stack, its own
// units, and — for the administrator — any unit by name (mp service, the
// Services page). Some units are no services to manage, though. Started,
// they are a root shell (debug-shell.service on tty9), or they run root's
// commands on an argument the caller picks: the instance name of a template
// unit (modprobe@<module> loads any kernel module, systemd-nspawn@<name>
// boots a container, user@<uid> starts anybody's service manager). Those
// only report their state; the panel itself acts on none of them — but on
// its own template monopanel-apache@.service, an account's Apache. Its
// instance name, a login, only picks the configuration the Apache starts
// with, /etc/apache2/monopanel/accounts/<login>/httpd.conf, which the unit
// needs to start at all and config/apply writes only for a hosting account,
// in the shape confcheck allows: children that run as that account, its own
// address. So whatever login the API names, the instance runs an Apache of
// that account or nothing.

// statusOnlyUnits are plain units that only report their state, with why.
var statusOnlyUnits = map[string]string{
	"debug-shell.service":                    "it is a root shell on tty9",
	"emergency.service":                      "it is the emergency shell",
	"rescue.service":                         "it is the rescue shell",
	"console-getty.service":                  "it is a login prompt on the console",
	"systemd-halt.service":                   "it halts the machine",
	"systemd-poweroff.service":               "it powers the machine off",
	"systemd-reboot.service":                 "it reboots the machine",
	"systemd-kexec.service":                  "it boots another kernel",
	"systemd-soft-reboot.service":            "it restarts the whole userspace",
	"systemd-exit.service":                   "it powers the machine off (exit is poweroff outside a container)",
	"systemd-suspend.service":                "it suspends the machine",
	"systemd-hibernate.service":              "it hibernates the machine",
	"systemd-hybrid-sleep.service":           "it suspends and hibernates the machine",
	"systemd-suspend-then-hibernate.service": "it suspends, then hibernates the machine",
	"systemd-sysext.service":                 "it lays system extension images over /usr",
	"systemd-confext.service":                "it lays configuration extension images over /etc",
	// systemd-tmpfiles over every tmpfiles.d file at once creates, chowns
	// and empties paths as root, also below directories others may write
	// to (the panel's run directory); the panel runs none of these.
	"systemd-tmpfiles-setup.service":           "it runs systemd-tmpfiles over every tmpfiles.d file as root",
	"systemd-tmpfiles-setup-dev.service":       "it runs systemd-tmpfiles over every tmpfiles.d file as root",
	"systemd-tmpfiles-setup-dev-early.service": "it runs systemd-tmpfiles over every tmpfiles.d file as root",
	"systemd-tmpfiles-clean.service":           "it runs systemd-tmpfiles --clean over every tmpfiles.d file as root",
	"systemd-tmpfiles-clean.timer":             "it starts systemd-tmpfiles-clean.service",
}

// actionableKinds are the unit types the agent acts on; the others (targets,
// devices, slices, scopes) only report their state: starting a target is
// rescue, emergency, reboot or poweroff, and the panel uses none.
var actionableKinds = map[string]bool{
	"service": true, "socket": true, "timer": true, "path": true,
	"mount": true, "automount": true, "swap": true,
}

// serviceAllowed tells whether the agent acts on unit (a full unit name)
// with action; status and daemon-reload always pass.
func serviceAllowed(unit, action string) error {
	switch action {
	case "status", "", "daemon-reload":
		return nil
	}
	dot := strings.LastIndexByte(unit, '.')
	if dot <= 0 {
		return notAllowed(action+" of "+unit, "a unit is named with its type, as in nginx.service")
	}
	name, kind := unit[:dot], unit[dot+1:]
	if !actionableKinds[kind] {
		return notAllowed(action+" of "+unit, "the agent starts and stops services, sockets, timers, paths, mounts and swap; a ."+kind+" unit only reports its state")
	}
	if _, ok := confcheck.ApacheInstanceLoginOf(unit); ok {
		return nil
	}
	if strings.Contains(name, "@") {
		return notAllowed(action+" of "+unit, "an instance of a template unit runs root's commands on its instance name (modprobe@<module> loads any kernel module); only its state is reported, but for an account's Apache, monopanel-apache@<login>.service")
	}
	if why, ok := statusOnlyUnits[unit]; ok {
		return notAllowed(action+" of "+unit, why)
	}
	// The panel's own runs: the self-update the agent starts in
	// monopanel-update.service, and the package manager's runs in
	// monopanel-pkg-<id>.service (pkg_unit.go), which outlive the agent so
	// that dpkg and rpm finish their transactions. A stop or restart from
	// outside would break exactly that off halfway; the agent starts them
	// itself, and they only report their state.
	if unit == "monopanel-update.service" || strings.HasPrefix(unit, "monopanel-pkg-") {
		return notAllowed(action+" of "+unit, "it is a run of the panel's own (its self-update or the package manager), which a stop or restart would break off halfway")
	}
	return nil
}

// CheckService is serviceAllowed for the agent's doubles (agenttest, the
// demo), with the name completed as the agent completes it.
func CheckService(unit, action string) error {
	if err := serviceAllowed(fullUnit(unit), action); err != nil {
		return err
	}
	return nil
}

// fullUnit is a unit name with the .service suffix systemd implies when a
// name has none.
func fullUnit(unit string) string {
	if unit != "" && !strings.Contains(unit, ".") {
		return unit + ".service"
	}
	return unit
}

// CheckReloads is the check of the units config/apply and paths/remove
// reload or restart, for the agent's doubles.
func CheckReloads(units ...[]string) error { return checkUnits("restart", units...) }

// checkUnits vets the units config/apply and paths/remove reload or restart
// (a restart starts a unit that is not running).
func checkUnits(action string, units ...[]string) error {
	for _, list := range units {
		for _, u := range list {
			if !unitRe.MatchString(u) {
				return notAllowed(action+" of "+u, "not a unit name")
			}
			if err := serviceAllowed(fullUnit(u), action); err != nil {
				return err
			}
		}
	}
	return nil
}
