package confcheck

import (
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
)

// Some settings name the account a daemon runs a program as: a php-fpm
// pool, a postfix pipe, the uid dovecot and postfix deliver mail with. An
// account of the API's choosing there is as good as root when it can sudo
// (the default user of a cloud image can, without a password) or sits in a
// group that owns the system. So those names are looked up in the local
// account files, the way useradd made the panel's accounts.

// etcDir holds passwd, group and login.defs; tests point it at fixtures.
var etcDir = "/etc"

type account struct {
	name string
	uid  int
	gid  int
	home string
}

type groupEnt struct {
	name    string
	gid     int
	members []string
}

type accountDB struct {
	users  map[string]account
	byUID  map[int]account
	groups map[string]groupEnt
	byGID  map[int]groupEnt
	uidMin int
}

func readAccounts() (*accountDB, error) {
	db := &accountDB{users: map[string]account{}, byUID: map[int]account{}, groups: map[string]groupEnt{}, byGID: map[int]groupEnt{}, uidMin: 1000}
	pw, err := os.ReadFile(path.Join(etcDir, "passwd"))
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(pw), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 7 || f[0] == "" || strings.HasPrefix(f[0], "+") || strings.HasPrefix(f[0], "-") {
			continue
		}
		uid, err1 := strconv.Atoi(f[2])
		gid, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			continue
		}
		a := account{name: f[0], uid: uid, gid: gid, home: f[5]}
		if _, dup := db.users[a.name]; !dup {
			db.users[a.name] = a
		}
		if _, dup := db.byUID[uid]; !dup {
			db.byUID[uid] = a
		}
	}
	gr, err := os.ReadFile(path.Join(etcDir, "group"))
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(gr), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 || f[0] == "" {
			continue
		}
		gid, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		g := groupEnt{name: f[0], gid: gid}
		for _, m := range strings.Split(f[3], ",") {
			if m = strings.TrimSpace(m); m != "" {
				g.members = append(g.members, m)
			}
		}
		if _, dup := db.groups[g.name]; !dup {
			db.groups[g.name] = g
		}
		if _, dup := db.byGID[gid]; !dup {
			db.byGID[gid] = g
		}
	}
	if b, err := os.ReadFile(path.Join(etcDir, "login.defs")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "UID_MIN" {
				if n, err := strconv.Atoi(f[1]); err == nil && n > 0 {
					db.uidMin = n
				}
			}
		}
	}
	return db, nil
}

// groupsOf lists the primary and the supplementary groups of a. A group
// with gid 0 is listed as "0" whatever its name: it is root's group.
func (db *accountDB) groupsOf(a account) []string {
	var out []string
	if g, ok := db.byGID[a.gid]; ok && a.gid != 0 {
		out = append(out, g.name)
	} else {
		out = append(out, strconv.Itoa(a.gid))
	}
	for _, g := range db.groups {
		for _, m := range g.members {
			if m == a.name {
				if g.gid == 0 {
					out = append(out, "0")
				} else {
					out = append(out, g.name)
				}
			}
		}
	}
	return out
}

// privilegedGroups are root, or a step from it: sudo rights, the docker or
// lxd socket, raw disks, the shadow file, the crontabs, /usr/local (staff),
// the journal and the logs, private keys.
var privilegedGroups = map[string]bool{
	"root": true, "sudo": true, "wheel": true, "admin": true, "adm": true, "docker": true, "lxd": true, "incus": true,
	"incus-admin": true, "shadow": true, "disk": true, "systemd-journal": true, "kvm": true, "libvirt": true, "kmem": true,
	"staff": true, "tty": true, "sys": true, "bin": true, "daemon": true, "utmp": true, "lpadmin": true, "crontab": true,
	"ssl-cert": true, "mail": true,
}

// groupAllowed explains why a group may not be handed to a process root
// starts: root's, a privileged one, or the API's own.
func (c *checker) groupAllowed(name string) error {
	if name == "" {
		return nil
	}
	if name == "0" || privilegedGroups[name] {
		return fmt.Errorf("group %s is root's or one that leads to it", name)
	}
	if name == c.l.ServiceGroup {
		return fmt.Errorf("group %s is the API process's own", name)
	}
	if db, err := c.accounts(); err == nil {
		if g, ok := db.groups[name]; ok && g.gid == 0 {
			return fmt.Errorf("group %s has root's gid 0", name)
		}
	}
	return nil
}

// privilegedMember names the first group of a that groupAllowed refuses.
func (c *checker) privilegedMember(db *accountDB, a account) string {
	for _, g := range db.groupsOf(a) {
		if c.groupAllowed(g) != nil {
			return g
		}
	}
	return ""
}

// panelAccount explains why name is not one of the panel's accounts: a
// local user with a uid of an ordinary account, its home right below
// WWWRoot, in no privileged group and not the API's own user. Only such an
// account may run a site's PHP.
func (c *checker) panelAccount(name string) error {
	db, err := c.accounts()
	if err != nil {
		return fmt.Errorf("cannot read the accounts to check %s: %v", name, err)
	}
	a, ok := db.users[name]
	if !ok {
		return fmt.Errorf("%s is not a local account", name)
	}
	if a.uid == 0 || a.name == "root" {
		return fmt.Errorf("%s is root", name)
	}
	if a.uid < db.uidMin {
		return fmt.Errorf("%s is a system account (uid %d)", name, a.uid)
	}
	if a.name == c.l.ServiceUser {
		return fmt.Errorf("%s runs the API process", name)
	}
	if path.Dir(path.Clean(a.home)) != c.l.WWWRoot {
		return fmt.Errorf("%s is not an account of the panel: its home %s is not in %s", name, a.home, c.l.WWWRoot)
	}
	if g := c.privilegedMember(db, a); g != "" {
		return fmt.Errorf("%s is in the group %s", name, g)
	}
	return nil
}

// runAsAllowed explains why name may not be the account a root daemon runs
// a program as (a postfix pipe): root, the API's user, an account in a
// privileged group, or an ordinary account the panel does not own (such as
// the cloud image's sudoer) are not.
func (c *checker) runAsAllowed(name string) error {
	db, err := c.accounts()
	if err != nil {
		return fmt.Errorf("cannot read the accounts to check %s: %v", name, err)
	}
	a, ok := db.users[name]
	if !ok {
		if n, err := strconv.Atoi(name); err == nil {
			if a, ok = db.byUID[n]; !ok {
				return fmt.Errorf("uid %s is no local account", name)
			}
		} else {
			return fmt.Errorf("%s is not a local account", name)
		}
	}
	if a.uid == 0 {
		return fmt.Errorf("%s is root", name)
	}
	if a.name == c.l.ServiceUser {
		return fmt.Errorf("%s runs the API process", name)
	}
	if g := c.privilegedMember(db, a); g != "" {
		return fmt.Errorf("%s is in the group %s", name, g)
	}
	if a.uid >= db.uidMin {
		return c.panelAccount(a.name)
	}
	return nil
}

// vmailIDs are the uid and gid of the panel's mail user: postfix and
// dovecot deliver and read mail as it and as nobody else.
func (c *checker) vmailIDs() (uid, gid int, err error) {
	db, err := c.accounts()
	if err != nil {
		return 0, 0, fmt.Errorf("cannot read the accounts: %v", err)
	}
	a, ok := db.users[vmailUser]
	if !ok {
		return 0, 0, fmt.Errorf("the mail user %s does not exist", vmailUser)
	}
	if a.uid == 0 || a.gid == 0 {
		return 0, 0, fmt.Errorf("the mail user %s has root's ids", vmailUser)
	}
	return a.uid, a.gid, nil
}

// isVmailUID reports whether v (a number or a name) is the mail user.
func (c *checker) isVmailUID(v string) error {
	uid, _, err := c.vmailIDs()
	if err != nil {
		return err
	}
	if v == vmailUser || v == strconv.Itoa(uid) {
		return nil
	}
	return fmt.Errorf("%s is not the mail user %s (uid %d)", v, vmailUser, uid)
}

// isVmailGID reports whether v (a number or a name) is the mail user's group.
func (c *checker) isVmailGID(v string) error {
	_, gid, err := c.vmailIDs()
	if err != nil {
		return err
	}
	if v == strconv.Itoa(gid) {
		return nil
	}
	db, _ := c.accounts()
	if g, ok := db.groups[v]; ok && g.gid == gid {
		return nil
	}
	return fmt.Errorf("%s is not the group of the mail user %s (gid %d)", v, vmailUser, gid)
}
