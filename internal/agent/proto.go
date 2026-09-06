// Package agent is the privileged half of MonoPanel. The API process talks to
// it over a unix socket with a closed, typed set of operations; nothing here
// accepts a shell string.
package agent

import (
	"strings"

	"monopanel/internal/sysinfo"
	"monopanel/internal/systemd"
)

// PingResponse answers /v1/ping.
type PingResponse struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version"`
	PeerUID  int    `json:"peer_uid"`
	Hostname string `json:"hostname"`
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
	Action   string   `json:"action"` // update-index install remove query
	Packages []string `json:"packages,omitempty"`
}

// PkgResponse returns output and installed versions.
type PkgResponse struct {
	Output    string            `json:"output,omitempty"`
	Installed map[string]string `json:"installed,omitempty"`
}

// SystemInfoResponse is the host snapshot.
type SystemInfoResponse = sysinfo.Info

// Error is returned by the agent for failed operations.
type Error struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
	Output  string `json:"output,omitempty"`
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

// EnsureSymlinkRequest creates or updates a symlink (below WWWRoot).
type EnsureSymlinkRequest struct {
	Path          string `json:"path"`
	Target        string `json:"target"`
	Owner         string `json:"owner"`
	Group         string `json:"group,omitempty"`
	OnlyIfMissing bool   `json:"only_if_missing,omitempty"`
}

// SetACLRequest applies POSIX ACL entries with setfacl.
type SetACLRequest struct {
	Path      string   `json:"path"`
	Entries   []string `json:"entries"`           // e.g. "g:monopanel-web:rX"
	Default   bool     `json:"default,omitempty"` // also set default entries (directories)
	Recursive bool     `json:"recursive,omitempty"`
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

// ApacheCtlRequest runs a2enmod/a2dismod/a2enconf/a2disconf/a2ensite/a2dissite (Debian).
type ApacheCtlRequest struct {
	Action string `json:"action"` // enmod dismod enconf disconf ensite dissite
	Name   string `json:"name"`
}

// ApacheCtlResponse returns the tool output.
type ApacheCtlResponse struct {
	Output string `json:"output,omitempty"`
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
}

// StatResponse lists the entries.
type StatResponse struct {
	Entries []StatEntry `json:"entries"`
}

// ReadFileRequest returns the tail of a file inside a client's home (logs).
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
type SetUnixPasswordRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
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
