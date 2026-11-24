// Package phpmyadmin holds what the API, the agent and its configuration
// checks agree on about the panel's phpMyAdmin: the pinned release, its
// system user and pool, and the fixed places of its tree, its state and the
// one-time sign-in tickets.
//
// phpMyAdmin is the panel's, like the panel itself, not a site of an
// account: its code lies in a root-owned tree (Root/<version>) that the pool
// user reads and never writes, it runs in a php-fpm pool of its own as
// User, and nginx serves it on a port of its own with the panel's name and
// certificate. Nobody signs in as MySQL's root (auth_socket): the login form
// takes a database user's name and password, and the panel's button hands a
// temporary MySQL account, granted on one database, over a ticket only the
// pool user can read.
package phpmyadmin

import (
	"fmt"
	"path"
	"regexp"
	"time"
)

// Version is the pinned phpMyAdmin release, the -all-languages tarball of
// which the panel downloads. 5.2.3 runs on PHP 7.2.5 up to 8.5 (its
// composer.json and ChangeLog: the 8.4 and 8.5 deprecations fixed).
const Version = "5.2.3"

// SHA256 is the official digest of that tarball
// (files.phpmyadmin.net/…/phpMyAdmin-<v>-all-languages.tar.gz.sha256) — a
// variable only so that the tests can hand the job a small archive.
var SHA256 = "12ba1c425fa4071abbd4e7668c9ebdeac0b0755a467a6d6d5026122bb47c102b"

// URL is where the tarball comes from.
func URL() string {
	return fmt.Sprintf("https://files.phpmyadmin.net/phpMyAdmin/%s/%s", Version, Archive())
}

// Archive is the tarball's file name.
func Archive() string { return fmt.Sprintf("phpMyAdmin-%s-all-languages.tar.gz", Version) }

const (
	// User is the system account the pool runs as (and its group).
	User = "monopanel-pma"
	// Pool is the php-fpm pool's name and its file (<pool dir>/<Pool>.conf):
	// a name no site's domain has (no dot).
	Pool = "monopanel-phpmyadmin"
	// Socket is the pool's socket in the panel's socket directory
	// (<run_dir>/php).
	Socket = Pool + ".sock"
	// Root holds the unpacked releases, Root/<version>, root's alone.
	Root = "/opt/monopanel/phpmyadmin"
	// StateDir holds what phpMyAdmin writes (sessions, temporary files),
	// the pool user's alone (0700).
	StateDir = "/var/lib/monopanel-pma"
	// RunDir holds the tickets and the sign-out marks (tmpfs: a ticket's
	// password never reaches a disk).
	RunDir = "/run/monopanel-pma"
	// ConfigFile and SignonScript are the panel's two files in a release's
	// tree: root's, of the pool user's group, 0640, and root's 0644.
	ConfigFile   = "config.inc.php"
	SignonScript = "monopanel-signon.php"
	// DefaultPort is the port nginx serves phpMyAdmin on unless told
	// otherwise: cPanel's numbering, like the webmail's 2096, and one of
	// the HTTPS ports Cloudflare proxies.
	DefaultPort = 2083
	// TicketTTL is how long a ticket may wait for its sign-in.
	TicketTTL = 60 * time.Second
	// SessionTTL is how long a temporary MySQL account of the button
	// lives: the sweep drops it then, or once its session signed out.
	SessionTTL = 8 * time.Hour
)

// Tree is the directory of a release.
func Tree(version string) string { return path.Join(Root, version) }

// TicketDir and LogoutDir are the pool user's directories below RunDir.
func TicketDir() string { return path.Join(RunDir, "tickets") }

// LogoutDir takes a mark per temporary account whose session signed out.
func LogoutDir() string { return path.Join(RunDir, "logout") }

var (
	// VersionRe is a release's directory name.
	VersionRe = regexp.MustCompile(`^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$`)
	// TempUserRe is a temporary MySQL account of the button: TempUserPrefix
	// and 16 hex digits.
	TempUserRe = regexp.MustCompile(`^mp\.pma\.[0-9a-f]{16}$`)
	// TicketNameRe is a ticket's file name: the sha256 of its token, hex.
	TicketNameRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// TempUserPrefix begins the name of a temporary MySQL account of the
// button. No name the panel gives or takes has a dot — an account's
// databases and their users are <login>_<name> of [a-z0-9_-], a migration
// brings [a-z0-9_-] — so the sweep, which drops every account of this shape
// that its list does not know, never meets a database user (MySQL's own
// mysql.sys/mysql.session are not of the shape either).
const TempUserPrefix = "mp.pma."

// MaxTicket bounds a ticket's content.
const MaxTicket = 4096
