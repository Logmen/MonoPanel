package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"

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
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent"+path, bytes.NewReader(body))
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

// Pkg drives the package manager.
func (c *Client) Pkg(ctx context.Context, action string, packages ...string) (*PkgResponse, error) {
	var r PkgResponse
	return &r, c.call(ctx, "/v1/pkg", &PkgRequest{Action: action, Packages: packages}, &r)
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

// EnsureSymlink creates a symlink in a client's home.
func (c *Client) EnsureSymlink(ctx context.Context, req *EnsureSymlinkRequest) error {
	return c.call(ctx, "/v1/symlink/ensure", req, nil)
}

// SetACL applies POSIX ACLs.
func (c *Client) SetACL(ctx context.Context, req *SetACLRequest) error {
	return c.call(ctx, "/v1/acl/set", req, nil)
}

// RemovePaths deletes files or trees.
func (c *Client) RemovePaths(ctx context.Context, req *RemovePathsRequest) (*RemovePathsResponse, error) {
	var r RemovePathsResponse
	return &r, c.call(ctx, "/v1/paths/remove", req, &r)
}

// ApacheCtl toggles Apache modules/configs/sites on Debian.
func (c *Client) ApacheCtl(ctx context.Context, action, name string) (*ApacheCtlResponse, error) {
	var r ApacheCtlResponse
	return &r, c.call(ctx, "/v1/apache/ctl", &ApacheCtlRequest{Action: action, Name: name}, &r)
}

// Tool runs an allow-listed administrative tool.
func (c *Client) Tool(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
	var r ToolResponse
	return &r, c.call(ctx, "/v1/tool", req, &r)
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
