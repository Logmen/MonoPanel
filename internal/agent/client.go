package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"

	"monopanel/internal/sysinfo"
)

// Client calls the agent over its unix socket.
type Client struct {
	http   *http.Client
	socket string
}

// NewClient returns a client for the given socket path.
func NewClient(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: tr}, socket: socket}
}

// Socket returns the socket path.
func (c *Client) Socket() string { return c.socket }

func (c *Client) call(ctx context.Context, path string, req, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	// Whether the agent got the request: a connection that breaks after it
	// did, with no answer and the caller still waiting, is an agent that went
	// away in the middle of the call (restarted, stopped, killed) — not one
	// that was not there to take it.
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) { wrote.Store(i.Err == nil) }}
	hreq, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, "http://agent"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(hreq)
	if err != nil {
		return &Error{Message: "agent unreachable at " + c.socket, Output: err.Error(), gone: wrote.Load() && ctx.Err() == nil}
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		e := &Error{Status: res.StatusCode}
		_ = json.NewDecoder(res.Body).Decode(e)
		if e.Message == "" {
			e.Message = "agent: " + res.Status
		}
		return e
	}
	if resp != nil {
		return json.NewDecoder(res.Body).Decode(resp)
	}
	return nil
}

// Ping checks the agent.
func (c *Client) Ping(ctx context.Context) (*PingResponse, error) {
	var r PingResponse
	return &r, c.call(ctx, "/v1/ping", struct{}{}, &r)
}

// SystemInfo returns the host snapshot.
func (c *Client) SystemInfo(ctx context.Context) (*sysinfo.Info, error) {
	var r sysinfo.Info
	return &r, c.call(ctx, "/v1/system/info", struct{}{}, &r)
}

// ApplyConfigSet writes files transactionally.
func (c *Client) ApplyConfigSet(ctx context.Context, req *ApplyConfigSetRequest) (*ApplyConfigSetResponse, error) {
	var r ApplyConfigSetResponse
	return &r, c.call(ctx, "/v1/config/apply", req, &r)
}

// EnsureGroup creates a group when missing.
func (c *Client) EnsureGroup(ctx context.Context, req *EnsureGroupRequest) (*EnsureGroupResponse, error) {
	var r EnsureGroupResponse
	return &r, c.call(ctx, "/v1/group/ensure", req, &r)
}

// EnsureUnixUser creates a unix user when missing.
func (c *Client) EnsureUnixUser(ctx context.Context, req *EnsureUnixUserRequest) (*EnsureUnixUserResponse, error) {
	var r EnsureUnixUserResponse
	return &r, c.call(ctx, "/v1/user/ensure", req, &r)
}

// Service acts on a systemd unit.
func (c *Client) Service(ctx context.Context, unit, action string) (*ServiceResponse, error) {
	var r ServiceResponse
	return &r, c.call(ctx, "/v1/service", &ServiceRequest{Unit: unit, Action: action}, &r)
}

// Pkg drives the package manager. A change (update-index, install, remove,
// upgrade) runs in a unit of its own, monopanel-pkg-<run>.service, of this
// call's naming: should the agent go away in the middle, the run goes on to
// its end, and the error says where to look.
func (c *Client) Pkg(ctx context.Context, action string, packages ...string) (*PkgResponse, error) {
	req := &PkgRequest{Action: action, Packages: packages}
	if pkgChanges(action) {
		req.Run = newRunID()
	}
	var r PkgResponse
	err := c.call(ctx, "/v1/pkg", req, &r)
	if req.Run != "" {
		err = runGone(err, "the package manager ("+strings.TrimSpace(action+" "+strings.Join(packages, " "))+")", req.Run)
	}
	return &r, err
}

// runGone is err of a call that ran the package manager in run's unit,
// told apart when the agent went away in the middle of it: the run, if it
// had started, is not the agent's and goes on to its end, and the result is
// in its unit's journal, not in a connection error.
func runGone(err error, what, run string) error {
	var ae *Error
	if !errors.As(err, &ae) || !ae.gone {
		return err
	}
	unit := pkgUnit(run)
	return &Error{Message: fmt.Sprintf("the agent went away (restarted or stopped) while it ran %s: a run it had started is not stopped with it and goes on to its end in %s; journalctl -u %s shows how it ended",
		what, unit, unit), Output: ae.Output, gone: true}
}

// EnsureDirs creates directories with mode and ownership.
func (c *Client) EnsureDirs(ctx context.Context, req *EnsureDirsRequest) (*EnsureDirsResponse, error) {
	var r EnsureDirsResponse
	return &r, c.call(ctx, "/v1/dirs/ensure", req, &r)
}

// EnsureFile writes a file in a client's home.
func (c *Client) EnsureFile(ctx context.Context, req *EnsureFileRequest) (*EnsureFileResponse, error) {
	var r EnsureFileResponse
	return &r, c.call(ctx, "/v1/file/ensure", req, &r)
}

// EnsureSiteLogs prepares an account's site logs in the root-owned tree
// <log_dir>/sites/<login>/ before the web servers open them.
func (c *Client) EnsureSiteLogs(ctx context.Context, req *SiteLogsRequest) (*SiteLogsResponse, error) {
	var r SiteLogsResponse
	return &r, c.call(ctx, "/v1/sitelogs/ensure", req, &r)
}

// RemoveSiteLogs deletes an account's directory of site logs.
func (c *Client) RemoveSiteLogs(ctx context.Context, req *RemoveSiteLogsRequest) (*RemoveSiteLogsResponse, error) {
	var r RemoveSiteLogsResponse
	return &r, c.call(ctx, "/v1/sitelogs/remove", req, &r)
}

// EnsureSymlink creates a symlink in a client's home.
func (c *Client) EnsureSymlink(ctx context.Context, req *EnsureSymlinkRequest) error {
	return c.call(ctx, "/v1/symlink/ensure", req, nil)
}

// SetACL applies POSIX ACLs.
func (c *Client) SetACL(ctx context.Context, req *SetACLRequest) error {
	return c.call(ctx, "/v1/acl/set", req, nil)
}

// SiteACL sets the web group's ACLs on a whole site tree.
func (c *Client) SiteACL(ctx context.Context, req *SiteACLRequest) (*SiteACLResponse, error) {
	var r SiteACLResponse
	return &r, c.call(ctx, "/v1/acl/site", req, &r)
}

// RemovePaths deletes files or trees.
func (c *Client) RemovePaths(ctx context.Context, req *RemovePathsRequest) (*RemovePathsResponse, error) {
	var r RemovePathsResponse
	return &r, c.call(ctx, "/v1/paths/remove", req, &r)
}

// ApacheCtl switches a module, a configuration or a site of the
// distribution's Apache off (Debian); switching one on is refused.
func (c *Client) ApacheCtl(ctx context.Context, action, name string) (*ApacheCtlResponse, error) {
	var r ApacheCtlResponse
	return &r, c.call(ctx, "/v1/apache/ctl", &ApacheCtlRequest{Action: action, Name: name}, &r)
}

// ApacheLegacy reads what the shared Apache of 0.8.19 left behind: a site's
// custom directives (domain given) or the names in its sites directory.
func (c *Client) ApacheLegacy(ctx context.Context, domain string) (*ApacheLegacyResponse, error) {
	var r ApacheLegacyResponse
	return &r, c.call(ctx, "/v1/apache/legacy", &ApacheLegacyRequest{Domain: domain}, &r)
}

// Tool runs an allow-listed administrative tool. dnf install, which is the
// package manager and runs as /v1/pkg's changes do (planDnf), gets a run of
// this call's naming, as Pkg's changes do.
func (c *Client) Tool(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
	if req.Name == "dnf" && len(req.Args) > 1 && req.Args[1] == "install" && req.Run == "" {
		named := *req
		named.Run = newRunID()
		req = &named
	}
	var r ToolResponse
	err := c.call(ctx, "/v1/tool", req, &r)
	if req.Run != "" {
		err = runGone(err, strings.Join(append([]string{req.Name}, req.Args...), " "), req.Run)
	}
	return &r, err
}

// Stat checks paths.
func (c *Client) Stat(ctx context.Context, paths ...string) (*StatResponse, error) {
	var r StatResponse
	return &r, c.call(ctx, "/v1/stat", &StatRequest{Paths: paths}, &r)
}

// ReadFile returns the tail of a client's log file.
func (c *Client) ReadFile(ctx context.Context, path string, tail int64) (*ReadFileResponse, error) {
	var r ReadFileResponse
	return &r, c.call(ctx, "/v1/file/read", &ReadFileRequest{Path: path, TailBytes: tail}, &r)
}

// SetUnixPassword sets the login password of a client.
func (c *Client) SetUnixPassword(ctx context.Context, login, password string) error {
	return c.call(ctx, "/v1/user/password", &SetUnixPasswordRequest{Login: login, Password: password}, nil)
}

// SetUnixPasswordHash restores an already hashed password (a migration).
func (c *Client) SetUnixPasswordHash(ctx context.Context, login, hash string) error {
	return c.call(ctx, "/v1/user/password", &SetUnixPasswordRequest{Login: login, Password: hash, Encrypted: true}, nil)
}

// UnixShadow returns the password hash of a client account.
func (c *Client) UnixShadow(ctx context.Context, login string) (*UnixShadowResponse, error) {
	var r UnixShadowResponse
	return &r, c.call(ctx, "/v1/user/shadow", &UnixShadowRequest{Login: login}, &r)
}

// RunAsUser runs a file operation as a client via the helper.
func (c *Client) RunAsUser(ctx context.Context, req *RunAsUserRequest) (*RunAsUserResponse, error) {
	var r RunAsUserResponse
	return &r, c.call(ctx, "/v1/runas", req, &r)
}

// RemoveUnixUser deletes a client's unix account (and home when requested).
func (c *Client) RemoveUnixUser(ctx context.Context, req *RemoveUnixUserRequest) (*RemoveUnixUserResponse, error) {
	var r RemoveUnixUserResponse
	return &r, c.call(ctx, "/v1/user/remove", req, &r)
}

// InstallPanel replaces the panel with a staged package. It returns as soon as
// systemd has taken the job: the update restarts both daemons, so the reply to
// this call is the last thing this connection sees.
func (c *Client) InstallPanel(ctx context.Context, req *InstallPanelRequest) (*InstallPanelResponse, error) {
	var r InstallPanelResponse
	return &r, c.call(ctx, "/v1/panel/install", req, &r)
}

// SetPanelConfig changes the panel's own settings in config.yaml (the keys of
// config.PanelKeys, nothing else); they take effect when the API restarts.
func (c *Client) SetPanelConfig(ctx context.Context, req *PanelConfigRequest) (*PanelConfigResponse, error) {
	var r PanelConfigResponse
	return &r, c.call(ctx, "/v1/panel/config", req, &r)
}

// ApplyUnit writes one of the panel's systemd units (/etc/systemd/system is
// not on the write allow-list of ApplyConfigSet). The caller reloads systemd.
func (c *Client) ApplyUnit(ctx context.Context, req *ApplyUnitRequest) (*ApplyUnitResponse, error) {
	var r ApplyUnitResponse
	return &r, c.call(ctx, "/v1/units/apply", req, &r)
}

// RemoveUnit deletes one of the panel's systemd units.
func (c *Client) RemoveUnit(ctx context.Context, req *RemoveUnitRequest) (*RemoveUnitResponse, error) {
	var r RemoveUnitResponse
	return &r, c.call(ctx, "/v1/units/remove", req, &r)
}

// ListDir lists an allow-listed configuration directory.
func (c *Client) ListDir(ctx context.Context, path string) (*ListDirResponse, error) {
	var r ListDirResponse
	return &r, c.call(ctx, "/v1/dir/list", &ListDirRequest{Path: path}, &r)
}

// StreamOut runs a streaming tool and copies its stdout into dst. The exit
// status arrives in a trailer: the response is already on its way when the
// tool fails, so there is no status code left to use.
func (c *Client) StreamOut(ctx context.Context, req *StreamRequest, dst io.Writer) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/v1/stream/out", bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(hreq)
	if err != nil {
		return &Error{Message: "agent unreachable at " + c.socket, Output: err.Error()}
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		e := &Error{Status: res.StatusCode}
		_ = json.NewDecoder(res.Body).Decode(e)
		if e.Message == "" {
			e.Message = "agent: " + res.Status
		}
		return e
	}
	if _, err := io.Copy(dst, res.Body); err != nil {
		return err
	}
	if code := res.Trailer.Get("X-Exit-Code"); code != "" && code != "0" {
		return &Error{Message: req.Name + " exited with " + code, Output: res.Trailer.Get("X-Error")}
	}
	return nil
}

// StreamIn feeds src to a streaming tool's stdin (tar -x, mysql).
func (c *Client) StreamIn(ctx context.Context, req *StreamRequest, src io.Reader) (*StreamResponse, error) {
	spec, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/v1/stream/in", src)
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/octet-stream")
	hreq.Header.Set("X-Stream-Spec", base64.StdEncoding.EncodeToString(spec))
	res, err := c.http.Do(hreq)
	if err != nil {
		return nil, &Error{Message: "agent unreachable at " + c.socket, Output: err.Error()}
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		e := &Error{Status: res.StatusCode}
		_ = json.NewDecoder(res.Body).Decode(e)
		if e.Message == "" {
			e.Message = "agent: " + res.Status
		}
		return nil, e
	}
	var out StreamResponse
	return &out, json.NewDecoder(res.Body).Decode(&out)
}

// Chown fixes ownership of a client's tree after a root-side extraction.
func (c *Client) Chown(ctx context.Context, req *ChownRequest) (*ChownResponse, error) {
	var r ChownResponse
	return &r, c.call(ctx, "/v1/chown", req, &r)
}

// ShareBind has the agent bind a shared folder at its mount point.
func (c *Client) ShareBind(ctx context.Context, req *ShareBindRequest) (*ShareBindResponse, error) {
	var r ShareBindResponse
	return &r, c.call(ctx, "/v1/shares/bind", req, &r)
}

// ShareUnbind has the agent unmount a shared folder and forget it.
func (c *Client) ShareUnbind(ctx context.Context, req *ShareUnbindRequest) (*ShareUnbindResponse, error) {
	var r ShareUnbindResponse
	return &r, c.call(ctx, "/v1/shares/unbind", req, &r)
}

// ShareStatus tells what lies at the mount points of shared folders.
func (c *Client) ShareStatus(ctx context.Context, paths ...string) (*ShareStatusResponse, error) {
	var r ShareStatusResponse
	return &r, c.call(ctx, "/v1/shares/status", &ShareStatusRequest{Paths: paths}, &r)
}

// ShareRecords lists the agent's records of the shared folders it bound.
func (c *Client) ShareRecords(ctx context.Context) (*ShareRecordsResponse, error) {
	var r ShareRecordsResponse
	return &r, c.call(ctx, "/v1/shares/records", struct{}{}, &r)
}

// MailPolicyProbe asks whether the panel's mail policy answers postfix.
func (c *Client) MailPolicyProbe(ctx context.Context) (*MailPolicyProbeResponse, error) {
	var resp MailPolicyProbeResponse
	return &resp, c.call(ctx, "/v1/mailpolicy/probe", struct{}{}, &resp)
}

// PhpMyAdmin writes the panel's phpMyAdmin files and tickets, and sweeps
// them (PhpMyAdminRequest).
func (c *Client) PhpMyAdmin(ctx context.Context, req *PhpMyAdminRequest) (*PhpMyAdminResponse, error) {
	var resp PhpMyAdminResponse
	return &resp, c.call(ctx, "/v1/phpmyadmin", req, &resp)
}
