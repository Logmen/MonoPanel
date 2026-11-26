// Package agent is the privileged half of MonoPanel. The API process talks to
// it over a unix socket with a closed, typed set of operations; nothing here
// accepts a shell string.
package agent

import (
	"strings"

	"monopanel/internal/osprofile"
	"monopanel/internal/sysinfo"
	"monopanel/internal/systemd"
)

// PingResponse answers /v1/ping.
type PingResponse struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version"`
	PeerUID  int    `json:"peer_uid"`
	Hostname string `json:"hostname"`
	// Sandbox is how the agent confines restic; an agent that does not say
	// leaves it out.
	Sandbox *SandboxInfo `json:"sandbox,omitempty"`
}

// SandboxInfo tells whether restic runs in its Landlock sandbox
// (restic_sandbox.go), for the doctor.
type SandboxInfo struct {
	// Landlock is the kernel's Landlock ABI version; 0 when the kernel has
	// none, and restic runs as root without the sandbox.
	Landlock int `json:"landlock"`
	// Error says why there is no Landlock, and Hint how to turn it on where
	// the kernel has it built in but the boot left it out (Oracle Linux's
	// UEK).
	Error string `json:"error,omitempty"`
	Hint  string `json:"hint,omitempty"`
	// NoSymfollow: a backup reads the trees it saves in a view in which no
	// symlink is followed, Landlock or not, so that a symlink in a home leads
	// nowhere — not into the panel's configuration, certificates and dumps
	// either, which a server backup reads too. NoSymfollowError says why not.
	NoSymfollow      bool   `json:"nosymfollow,omitempty"`
	NoSymfollowError string `json:"nosymfollow_error,omitempty"`
	// PrivateEtc: restic in its Landlock sandbox reads the system's files of
	// /etc in a private /etc, so that the host renaming resolv.conf, hosts or
	// passwd during a run does not take them away from it.
	// PrivateEtcError says why not.
	PrivateEtc      bool   `json:"private_etc,omitempty"`
	PrivateEtcError string `json:"private_etc_error,omitempty"`
	// Restic is the restic binary the agent ran in the sandbox a backup's
	// restic gets, with restic version — as it started, and since whenever
	// the binary changed: ResticVersion is what it said, ResticError
	// restic's own error line and its exit status when it did not start
	// there — and then every backup fails the same way, and every other run
	// the sandbox confines —, or that it did not finish in time
	// (ResticTimeout). All of them are empty (false) where restic is not
	// installed or runs without a sandbox, and while the first run of a
	// binary takes longer than a ping waits for it.
	Restic        string `json:"restic,omitempty"`
	ResticVersion string `json:"restic_version,omitempty"`
	ResticError   string `json:"restic_error,omitempty"`
	// ResticTimeout: ResticError is restic version not finishing within the
	// self-test's time rather than restic ending with an error. A host too
	// busy at that moment does that as well, and the agent tests it again
	// ResticRetestAfter later: the doctor warns where it fails on an error.
	// An agent that does not tell leaves it out, and its ResticError stays
	// the failure it always meant.
	ResticTimeout bool `json:"restic_timeout,omitempty"`
}

// FileSpec is one file to write. Mode is a unix permission (0o644); Owner and
// Group are names and may be empty to keep root:root.
type FileSpec struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// ContentBase64 carries binary files (GPG keyrings); it wins over Content.
	ContentBase64 string `json:"content_base64,omitempty"`
	Mode          uint32 `json:"mode,omitempty"`
	Owner         string `json:"owner,omitempty"`
	Group         string `json:"group,omitempty"`
}

// ApplyConfigSetRequest writes a set of files transactionally: backup, write,
// validate, reload; any validator failure restores every file.
type ApplyConfigSetRequest struct {
	Files    []FileSpec `json:"files"`
	Validate [][]string `json:"validate,omitempty"`
	Reload   []string   `json:"reload,omitempty"`
	Restart  []string   `json:"restart,omitempty"`
	// Force reloads even when no file changed.
	Force bool `json:"force,omitempty"`
	// Restore lists paths whose SELinux label is restored after validation
	// and before the reload: a validator run by the agent creates files
	// (nginx -t makes the pid file) with the agent's label, which the
	// service's confined domain may not open. Ignored without SELinux.
	Restore []string `json:"restore,omitempty"`
	// Origin is recorded in logs/history (e.g. "site:example.com").
	Origin string `json:"origin,omitempty"`
}

// ApplyConfigSetResponse reports what happened.
type ApplyConfigSetResponse struct {
	Written   []string `json:"written"`
	Unchanged []string `json:"unchanged"`
	Validated int      `json:"validated"`
	Reloaded  []string `json:"reloaded"`
}

// EnsureGroupRequest creates a group when missing.
type EnsureGroupRequest struct {
	Name   string `json:"name"`
	System bool   `json:"system,omitempty"`
}

// EnsureGroupResponse reports the gid.
type EnsureGroupResponse struct {
	GID     int  `json:"gid"`
	Created bool `json:"created"`
}

// EnsureUnixUserRequest creates a unix user when missing and syncs groups.
type EnsureUnixUserRequest struct {
	Login  string `json:"login"`
	Home   string `json:"home,omitempty"`
	Shell  string `json:"shell,omitempty"`
	System bool   `json:"system,omitempty"`
	// PrimaryGroup uses an existing group (-g) instead of creating one (-U).
	PrimaryGroup string   `json:"primary_group,omitempty"`
	CreateHome   bool     `json:"create_home,omitempty"`
	Groups       []string `json:"groups,omitempty"`
	// RemoveGroups drops supplementary group memberships (gpasswd -d).
	RemoveGroups []string `json:"remove_groups,omitempty"`
	// UpdateShell applies Shell to an existing user as well (usermod -s).
	UpdateShell bool   `json:"update_shell,omitempty"`
	Comment     string `json:"comment,omitempty"`
}

// EnsureUnixUserResponse reports the identity.
type EnsureUnixUserResponse struct {
	UID     int    `json:"uid"`
	GID     int    `json:"gid"`
	Home    string `json:"home"`
	Created bool   `json:"created"`
}

// ServiceRequest acts on a systemd unit.
type ServiceRequest struct {
	Unit   string `json:"unit"`
	Action string `json:"action"` // start stop reload restart reload-or-restart enable disable status daemon-reload
}

// ServiceResponse returns the unit state after the action.
type ServiceResponse struct {
	Status systemd.Status `json:"status"`
}

// PkgRequest drives the native package manager.
type PkgRequest struct {
	Action   string   `json:"action"` // update-index install remove query available candidates upgrade
	Packages []string `json:"packages,omitempty"`
	// Run names the change's run, 16 hex digits: it goes on, whatever
	// becomes of the agent, in monopanel-pkg-<run>.service. The caller picks
	// it so as to know where to look should the agent go away meanwhile;
	// the agent picks one when it is empty.
	Run string `json:"run,omitempty"`
}

// PkgResponse returns output and installed versions; for "available" the
// candidate versions the repositories offer (packages nobody carries are
// simply absent).
type PkgResponse struct {
	Output    string            `json:"output,omitempty"`
	Installed map[string]string `json:"installed,omitempty"`
	Available map[string]string `json:"available,omitempty"`
	// Upgrades, for "candidates", are the installed ones of the packages a
	// repository offers another version of, with that version and where it
	// comes from; Installed has what is installed of them all. Held are those
	// of them the administrator holds back (apt-mark hold), which an
	// upgrade leaves as they are.
	Upgrades map[string]osprofile.Candidate `json:"upgrades,omitempty"`
	Held     map[string]osprofile.Candidate `json:"held,omitempty"`
}

// SystemInfoResponse is the host snapshot.
type SystemInfoResponse = sysinfo.Info

// Error is returned by the agent for failed operations.
type Error struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
	Output  string `json:"output,omitempty"`
	// gone marks, at the client, a call the agent took and then went away
	// in the middle of, without an answer (Client.call).
	gone bool
}

func (e *Error) Error() string {
	if e.Output != "" {
		return e.Message + ": " + strings.TrimSpace(e.Output)
	}
	return e.Message
}

// DirSpec is a directory to create or fix up.
type DirSpec struct {
	Path  string `json:"path"`
	Mode  uint32 `json:"mode,omitempty"`
	Owner string `json:"owner,omitempty"`
	Group string `json:"group,omitempty"`
}

// EnsureDirsRequest creates directories (root-owned config trees or client
// home layouts) with the given mode and ownership.
type EnsureDirsRequest struct {
	Dirs []DirSpec `json:"dirs"`
}

// EnsureDirsResponse lists newly created directories.
type EnsureDirsResponse struct {
	Created []string `json:"created"`
}

// EnsureFileRequest creates a file inside a client's home (below WWWRoot) as
// root and chowns it; every path component is checked against symlinks.
type EnsureFileRequest struct {
	Path          string `json:"path"`
	Content       string `json:"content,omitempty"`
	ContentBase64 string `json:"content_base64,omitempty"`
	Mode          uint32 `json:"mode,omitempty"`
	Owner         string `json:"owner"`
	Group         string `json:"group,omitempty"`
	OnlyIfMissing bool   `json:"only_if_missing,omitempty"`
	// OnlyIfDirEmpty writes only when the parent directory has no entries
	// (welcome pages must never shadow uploaded content).
	OnlyIfDirEmpty bool `json:"only_if_dir_empty,omitempty"`
}

// EnsureFileResponse reports whether the file was written.
type EnsureFileResponse struct {
	Written bool `json:"written"`
}

// SiteLogsRequest prepares an account's site logs in <log_dir>/sites/<login>/,
// a tree only root writes: nginx, Apache and the php-fpm master open these
// files as root, so no account may be able to swap one for a symlink. The
// agent makes <log_dir>/sites (root:root 0711) and the account's directory
// (root:<login> 0751), and the files: the servers' logs root:<login> 0640,
// a PHP error log (*.php.error.log, which the account's PHP workers open for
// every write) <login>:<login> 0660. Files that exist get that owner and mode
// back. Files are plain names ending in .log.
type SiteLogsRequest struct {
	Login string   `json:"login"`
	Files []string `json:"files,omitempty"`
}

// SiteLogsResponse names the account's directory and what was made in it or
// set anew.
type SiteLogsResponse struct {
	Dir     string   `json:"dir"`
	Created []string `json:"created"`
	Changed int      `json:"changed"`
}

// RemoveSiteLogsRequest deletes an account's directory of site logs with
// everything in it (the account is purged).
type RemoveSiteLogsRequest struct {
	Login string `json:"login"`
	// Domain narrows the removal to the logs of one site of the account,
	// rotated copies included; the directory and the other sites' stay.
	Domain string `json:"domain,omitempty"`
}

// RemoveSiteLogsResponse tells whether there was a directory to remove, or
// which files of a site's went (Domain).
type RemoveSiteLogsResponse struct {
	Removed bool     `json:"removed"`
	Files   []string `json:"files,omitempty"`
}

// EnsureSymlinkRequest creates or updates a symlink (below WWWRoot).
type EnsureSymlinkRequest struct {
	Path   string `json:"path"`
	Target string `json:"target"`
	Owner  string `json:"owner"`
	Group  string `json:"group,omitempty"`
}

// EnsureSymlinkResponse says whether the link was made or pointed elsewhere
// (Was, the target it had then).
type EnsureSymlinkResponse struct {
	Changed bool   `json:"changed"`
	Was     string `json:"was,omitempty"`
}

// SetACLRequest applies POSIX ACL entries with setfacl.
type SetACLRequest struct {
	Path      string   `json:"path"`
	Entries   []string `json:"entries"`           // e.g. "g:monopanel-web:rX"
	Default   bool     `json:"default,omitempty"` // also set default entries (directories)
	Recursive bool     `json:"recursive,omitempty"`
	// Remove drops entries (setfacl -x), e.g. "u:alex"; with Default the
	// matching default entries go too.
	Remove []string `json:"remove,omitempty"`
}

// SiteACLRequest gives the web group read access to a site tree: r-x and a
// matching default ACL on directories, read on files (r-x only on files the
// owner may execute). The files' mask is set explicitly: recalculated, it
// would take the x of the group entry every file inherits from its
// directory, and files would show as 650/654 instead of 640/644.
type SiteACLRequest struct {
	Root  string `json:"root"`
	Group string `json:"group"`
}

// SiteACLResponse counts what was set.
type SiteACLResponse struct {
	Dirs  int `json:"dirs"`
	Files int `json:"files"`
}

// RemovePathsRequest deletes files or trees. Trees are only removed below
// WWWRoot; single files must be inside the allow-list.
type RemovePathsRequest struct {
	Paths     []string `json:"paths"`
	Recursive bool     `json:"recursive,omitempty"`
	// Validate/Reload run after removal (e.g. nginx -t, reload nginx).
	Validate [][]string `json:"validate,omitempty"`
	Reload   []string   `json:"reload,omitempty"`
}

// RemovePathsResponse lists what was removed.
type RemovePathsResponse struct {
	Removed []string `json:"removed"`
}

// ApacheCtlRequest runs a2dismod/a2disconf/a2dissite (Debian): the upgrade
// from 0.8.19 switches the panel's configuration of the shared Apache off.
// enmod, enconf and ensite are refused (CheckApacheCtl).
type ApacheCtlRequest struct {
	Action string `json:"action"` // dismod disconf dissite
	Name   string `json:"name"`
}

// ApacheCtlResponse returns the tool output.
type ApacheCtlResponse struct {
	Output string `json:"output,omitempty"`
}

// ApacheLegacyRequest reads what the shared Apache of 0.8.19 and older left
// in confcheck.ApacheLegacySites, for the upgrade to carry over: with a
// domain that site's custom directives (<domain>.d/*.conf), without one the
// names of the directory's entries. The files are the administrators',
// root's: the agent reads them for the API (ReadApacheLegacy).
type ApacheLegacyRequest struct {
	Domain string `json:"domain,omitempty"`
}

// ApacheLegacyResponse holds the directory's entries (no domain asked) or
// the site's custom directive files, by name; nothing when there is none.
type ApacheLegacyResponse struct {
	Names []string           `json:"names,omitempty"`
	Files []ApacheLegacyFile `json:"files,omitempty"`
}

// ApacheLegacyFile is one file of a site's custom Apache directives, with
// its permission bits (0: not known).
type ApacheLegacyFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	Mode    uint32 `json:"mode,omitempty"`
}

// ToolRequest runs one of the allow-listed administrative tools (mysql,
// mysqldump, percona-release, restic, fail2ban-client, nft, crontab …) with
// argv and optional stdin. Output is captured (tail) unless OutputFile is set.
type ToolRequest struct {
	Name       string   `json:"name"`
	Args       []string `json:"args,omitempty"`
	Stdin      string   `json:"stdin,omitempty"`
	Env        []string `json:"env,omitempty"`
	OutputFile string   `json:"output_file,omitempty"`
	// TimeoutSeconds bounds the run (default 10 minutes).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Run names the run of a tool that is the package manager (dnf
	// install), which goes on in monopanel-pkg-<run>.service as /v1/pkg's
	// changes do (PkgRequest.Run).
	Run string `json:"run,omitempty"`
}

// ToolResponse returns the exit status and output.
type ToolResponse struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"`
}

// StatRequest checks paths.
type StatRequest struct {
	Paths []string `json:"paths"`
}

// StatEntry describes one path.
type StatEntry struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode,omitempty"`
	UID     int    `json:"uid"`
	GID     int    `json:"gid"`
	ModTime string `json:"mod_time,omitempty"`
	Link    string `json:"link,omitempty"` // a symlink's target
}

// StatResponse lists the entries.
type StatResponse struct {
	Entries []StatEntry `json:"entries"`
}

// ReadFileRequest returns the tail of a log: a site log or another file in
// the panel's log directory, a service log the agent knows, or a PHP
// configuration file.
type ReadFileRequest struct {
	Path      string `json:"path"`
	TailBytes int64  `json:"tail_bytes,omitempty"`
}

// ReadFileResponse carries the content.
type ReadFileResponse struct {
	Content   string `json:"content"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}

// SetUnixPasswordRequest sets a login password (chpasswd) for SFTP/SSH.
// Encrypted passes an already hashed password (chpasswd -e): переезд между
// панелями переносит хеш, а не пароль, поэтому пользователь входит тем же.
type SetUnixPasswordRequest struct {
	Login     string `json:"login"`
	Password  string `json:"password"`
	Encrypted bool   `json:"encrypted,omitempty"`
}

// UnixShadowRequest asks for the password hash of a client account.
type UnixShadowRequest struct {
	Login string `json:"login"`
}

// UnixShadowResponse carries the hash from shadow(5). It is a hash, not a
// password, but it is still worth exactly one offline attack — the panel hands
// it over only for a migration.
type UnixShadowResponse struct {
	Hash string `json:"hash"`
}

// RemoveUnixUserRequest deletes a client's unix account: processes of the
// uid are terminated first; the home is removed only when RemoveHome is set
// and the directory lies under HomeUnder (the panel's www root).
type RemoveUnixUserRequest struct {
	Login      string `json:"login"`
	RemoveHome bool   `json:"remove_home,omitempty"`
	HomeUnder  string `json:"home_under,omitempty"`
	// Home is the directory to remove (default: the passwd entry); it is
	// removed by the agent itself, since userdel -r refuses root-owned
	// (SFTP chroot) homes.
	Home string `json:"home,omitempty"`
}

// RemoveUnixUserResponse reports what happened.
type RemoveUnixUserResponse struct {
	Removed     bool `json:"removed"`
	Killed      int  `json:"killed"`
	HomeRemoved bool `json:"home_removed"`
}

// InstallPanelRequest asks the agent to replace the panel itself with a
// package the API process has already downloaded and verified. The agent
// checks the same package again — the API is unprivileged and must not be the
// only thing standing between a release key and root.
type InstallPanelRequest struct {
	Package string `json:"package"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
	// Sums and Sig are the release checksum list and its detached signature;
	// they are required when a release key is configured.
	Sums string `json:"sums,omitempty"`
	Sig  string `json:"sig,omitempty"`
}

// InstallPanelResponse reports that the update was handed to systemd. The
// installation itself outlives this request: it restarts the agent.
type InstallPanelResponse struct {
	Unit    string `json:"unit"`
	Started bool   `json:"started"`
	// Signed is false when the panel has no release key and the package was
	// accepted on its digest alone.
	Signed bool `json:"signed"`
}

// PanelConfigRequest changes the panel's own settings in config.yaml: the
// keys of config.PanelKeys (web.hostname, web.listen, log.level, log.format,
// jobs.workers) and nothing else. The release key, the paths and the write
// allow-list stay root's to edit by hand — the panel must not move its own
// trust anchor. Path is the file the API process reads: the agent writes only
// its own configuration file and refuses when the two differ.
type PanelConfigRequest struct {
	Path string            `json:"path,omitempty"`
	Set  map[string]string `json:"set"`
}

// PanelConfigResponse names the file and the keys whose value changed.
// SocketError says why the socket that holds the panel's port could not
// follow a new web.listen: config.yaml is changed all the same, and the API
// listens on the new address itself.
type PanelConfigResponse struct {
	Path        string   `json:"path"`
	Changed     []string `json:"changed"`
	SocketError string   `json:"socket_error,omitempty"`
}

// ApplyUnitRequest writes one of the panel's systemd units, or the drop-in
// it keeps for a system service: Name is the file below /etc/systemd/system
// ("monopanel-app-alex-bot.service", "nginx.service.d/monopanel.conf"). The
// agent accepts only the units the panel writes, in the form it writes them
// (see ops_units.go). daemon-reload is the caller's, through /v1/service.
type ApplyUnitRequest struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	// Origin is recorded in the log (e.g. "app:monopanel-app-alex-bot.service").
	Origin string `json:"origin,omitempty"`
}

// ApplyUnitResponse names the file and tells whether its content changed.
type ApplyUnitResponse struct {
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
}

// RemoveUnitRequest deletes one of the panel's units by name.
type RemoveUnitRequest struct {
	Name string `json:"name"`
}

// RemoveUnitResponse tells whether there was a file to remove.
type RemoveUnitResponse struct {
	Path    string `json:"path"`
	Removed bool   `json:"removed"`
}

// ListDirRequest lists the names in a directory. Only a few configuration
// directories may be listed: this is for reading the panel's own domain
// (which PHP modules exist), not a file browser.
type ListDirRequest struct {
	Path string `json:"path"`
}

// ListDirResponse is the directory content, names only.
type ListDirResponse struct {
	Entries []ListDirEntry `json:"entries"`
}

// ListDirEntry is one name in a directory.
type ListDirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
}

// ShareBindRequest has the agent bind a shared folder: the site folder What,
// a directory of its owner's in the owner's home, at the mount point Where —
// in the root-owned home of an SFTP-only guest, or
// <www_root>/.shares/<guest>/<name> for a guest with a shell. The agent opens
// both without following a symlink and mounts the very folder it opened, in
// the host's mount namespace (sharebind.go); the folder mounted there
// already stays.
type ShareBindRequest struct {
	What  string `json:"what"`
	Where string `json:"where"`
	// Origin is recorded in the log (e.g. "share:guest/kaspy").
	Origin string `json:"origin,omitempty"`
}

// ShareBindResponse says that the folder is mounted, and whether this call
// mounted it.
type ShareBindResponse struct {
	Mounted bool `json:"mounted"`
	Changed bool `json:"changed"`
}

// ShareUnbindRequest unmounts the shared folder at the mount point Where and
// has the agent forget it; a mount unit a panel before 0.9.0 wrote for it
// goes first.
type ShareUnbindRequest struct {
	Where string `json:"where"`
}

// ShareUnbindResponse says what went.
type ShareUnbindResponse struct {
	Unmounted   bool `json:"unmounted"`
	UnitRemoved bool `json:"unit_removed"`
}

// ShareStatusRequest asks what lies at the mount points of shared folders.
type ShareStatusRequest struct {
	Paths []string `json:"paths"`
}

// ShareStatusResponse has an entry per path, in their order.
type ShareStatusResponse struct {
	Entries []ShareMountState `json:"entries"`
}

// ShareMountState is what the agent finds at a mount point: something
// mounted on it (in the host's mount namespace), its record of a shared
// folder it bound there, a mount unit an older panel wrote for it.
type ShareMountState struct {
	Where    string `json:"where"`
	Mounted  bool   `json:"mounted"`
	Recorded bool   `json:"recorded,omitempty"`
	Unit     bool   `json:"unit,omitempty"`
}

// ShareRecordsResponse lists the agent's records of the shared folders
// (/v1/shares/records): one per folder it bound, or took over from a unit
// of an older panel's, by mount point.
type ShareRecordsResponse struct {
	Records []ShareRecordState `json:"records"`
}

// ShareRecordState is one record: its mount point, the folder — empty for a
// record whose content cannot be read, which guards its mount point all the
// same — and whether something is mounted there (in the host's mount
// namespace).
type ShareRecordState struct {
	Where   string `json:"where"`
	What    string `json:"what,omitempty"`
	Mounted bool   `json:"mounted"`
}

// MailPolicyProbeResponse answers /v1/mailpolicy/probe: whether the panel's
// mail policy (monopanel-mailpolicy.service) answers the milter protocol's
// option negotiation where postfix reaches it — and whatever listens there
// is the panel's —, and whether main.cf has postfix ask it first.
type MailPolicyProbeResponse struct {
	OK bool `json:"ok"`
	// Listener is where the agent asked (the socket, or 127.0.0.1:8890).
	Listener string `json:"listener"`
	Error    string `json:"error,omitempty"`
	// Configured: smtpd_milters and non_smtpd_milters of main.cf begin
	// with the policy; ConfigError says what is amiss otherwise.
	Configured  bool   `json:"configured"`
	ConfigError string `json:"config_error,omitempty"`
}

// PhpMyAdminRequest is /v1/phpmyadmin: the panel's phpMyAdmin
// (internal/phpmyadmin) at its fixed places, which no generic call writes
// with these owners.
//
//   - "files": the panel's two files in the tree of Version, which must be
//     there, root's (dirs/ensure, the tar extraction): Config as
//     config.inc.php, root:<pool user> 0640, Signon as the sign-in script,
//     root's 0644;
//   - "ticket": a one-time ticket Name (the sha256 of its token) with
//     Content, the pool user's 0400 in the ticket directory;
//   - "sweep": tickets past their life and stray entries go; the sign-out
//     marks of temporary accounts are answered (Logouts) and go;
//   - "prune": the trees of other releases than Version go.
type PhpMyAdminRequest struct {
	Action  string `json:"action"`
	Version string `json:"version,omitempty"`
	Config  string `json:"config,omitempty"`
	Signon  string `json:"signon,omitempty"`
	Name    string `json:"name,omitempty"`
	Content string `json:"content,omitempty"`
}

// PhpMyAdminResponse answers /v1/phpmyadmin.
type PhpMyAdminResponse struct {
	// Written are the files "files" changed.
	Written []string `json:"written,omitempty"`
	// Logouts are the temporary accounts whose sessions signed out.
	Logouts []string `json:"logouts,omitempty"`
	// Removed are the tickets, marks or trees that went.
	Removed []string `json:"removed,omitempty"`
}
