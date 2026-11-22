package agent

import (
	"strings"
)

// The agent acts on systemd units for the API: the panel's stack, its own
// units, and — for the administrator — any unit by name (mp service, the
// Services page). Some units are no services to manage, though. Started,
// they are a root shell (debug-shell.service on tty9), or they run root's
// commands on an argument the caller picks: the instance name of a template
// unit (modprobe@<module> loads any kernel module, systemd-nspawn@<name>
// boots a container, user@<uid> starts anybody's service manager). Those
// only report their state; the panel itself acts on none of them.

// statusOnlyUnits are plain units that only report their state, with why.
var statusOnlyUnits = map[string]string{
	"debug-shell.service":         "it is a root shell on tty9",
	"emergency.service":           "it is the emergency shell",
	"rescue.service":              "it is the rescue shell",
	"console-getty.service":       "it is a login prompt on the console",
	"systemd-halt.service":        "it halts the machine",
	"systemd-poweroff.service":    "it powers the machine off",
	"systemd-reboot.service":      "it reboots the machine",
	"systemd-kexec.service":       "it boots another kernel",
	"systemd-soft-reboot.service": "it restarts the whole userspace",
	"systemd-sysext.service":      "it lays system extension images over /usr",
	"systemd-confext.service":     "it lays configuration extension images over /etc",
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
	if strings.Contains(name, "@") {
		return notAllowed(action+" of "+unit, "an instance of a template unit runs root's commands on its instance name (modprobe@<module> loads any kernel module); only its state is reported")
	}
	if why, ok := statusOnlyUnits[unit]; ok {
		return notAllowed(action+" of "+unit, why)
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
