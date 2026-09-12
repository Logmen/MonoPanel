// Package client is the Go client for the MonoPanel REST API, used by the CLI,
// the TUI and `mp setup`. Locally it speaks over the unix socket (peer-cred
// auth); remotely over HTTPS with a Bearer token.
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
	"monopanel/internal/systemd"
)

// Client talks to /api/v1.
type Client struct {
	base  string
	http  *http.Client
	token string
}

// NewUnix connects through the local API socket.
func NewUnix(socket string) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	return &Client{base: "http://monopanel/api/v1", http: &http.Client{Transport: tr}}
}

// NewRemote connects to https://host:port with a Bearer token.
func NewRemote(server, token string, insecure bool) *Client {
	server = strings.TrimRight(server, "/")
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure, MinVersion: tls.VersionTLS12}} //nolint:gosec // opt-in flag
	return &Client{base: server + "/api/v1", http: &http.Client{Transport: tr}, token: token}
}

// APIError is an RFC 9457 problem returned by the API.
type APIError struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Errors []struct {
		Message  string `json:"message"`
		Location string `json:"location"`
	} `json:"errors"`
}

func (e *APIError) Error() string {
	msg := e.Detail
	if msg == "" {
		msg = e.Title
	}
	if len(e.Errors) > 0 {
		parts := make([]string, 0, len(e.Errors))
		for _, x := range e.Errors {
			parts = append(parts, strings.TrimPrefix(x.Location, "body.")+": "+x.Message)
		}
		msg += " (" + strings.Join(parts, "; ") + ")"
	}
	return fmt.Sprintf("%s [HTTP %d]", msg, e.Status)
}

func (c *Client) do(ctx context.Context, method, p string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+p, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("panel unreachable: %w", err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		e := &APIError{Status: res.StatusCode}
		_ = json.Unmarshal(data, e)
		if e.Title == "" {
			e.Title = res.Status
		}
		return e
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Health is public.
func (c *Client) Health(ctx context.Context) (*apitypes.Health, error) {
	var h apitypes.Health
	return &h, c.do(ctx, http.MethodGet, "/health", nil, &h)
}

// Status returns the dashboard.
func (c *Client) Status(ctx context.Context) (*apitypes.SystemStatus, error) {
	var s apitypes.SystemStatus
	return &s, c.do(ctx, http.MethodGet, "/system/status", nil, &s)
}

// Me returns the caller.
func (c *Client) Me(ctx context.Context) (*apitypes.Principal, error) {
	var p apitypes.Principal
	return &p, c.do(ctx, http.MethodGet, "/auth/me", nil, &p)
}

// ListUsers lists users (admin).
func (c *Client) ListUsers(ctx context.Context) ([]*store.User, error) {
	var out []*store.User
	return out, c.do(ctx, http.MethodGet, "/users", nil, &out)
}

// GetUser returns one user.
func (c *Client) GetUser(ctx context.Context, login string) (*store.User, error) {
	var u store.User
	return &u, c.do(ctx, http.MethodGet, "/users/"+login, nil, &u)
}

// CreateUser creates a user and returns the provisioning job.
func (c *Client) CreateUser(ctx context.Context, req apitypes.CreateUserRequest) (*apitypes.UserWithJob, error) {
	var out apitypes.UserWithJob
	return &out, c.do(ctx, http.MethodPost, "/users", req, &out)
}

// ListJobs lists recent jobs.
func (c *Client) ListJobs(ctx context.Context, limit int, status string) ([]*store.Job, error) {
	var out []*store.Job
	q := fmt.Sprintf("/jobs?limit=%d", limit)
	if status != "" {
		q += "&status=" + status
	}
	return out, c.do(ctx, http.MethodGet, q, nil, &out)
}

// GetJob returns a job with its log.
func (c *Client) GetJob(ctx context.Context, id int64) (*store.Job, error) {
	var j store.Job
	return &j, c.do(ctx, http.MethodGet, fmt.Sprintf("/jobs/%d", id), nil, &j)
}

// CreateToken mints an API token.
func (c *Client) CreateToken(ctx context.Context, req apitypes.CreateTokenRequest) (*apitypes.CreateTokenResponse, error) {
	var out apitypes.CreateTokenResponse
	return &out, c.do(ctx, http.MethodPost, "/tokens", req, &out)
}

// ListTokens lists the caller's tokens.
func (c *Client) ListTokens(ctx context.Context, user string) ([]*store.APIToken, error) {
	var out []*store.APIToken
	p := "/tokens"
	if user != "" {
		p += "?user=" + url.QueryEscape(user)
	}
	return out, c.do(ctx, http.MethodGet, p, nil, &out)
}

// DeleteToken revokes a token.
func (c *Client) DeleteToken(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/tokens/%d", id), nil, nil)
}

// Services lists managed units.
func (c *Client) Services(ctx context.Context) ([]systemd.Status, error) {
	var out []systemd.Status
	return out, c.do(ctx, http.MethodGet, "/services", nil, &out)
}

// Service returns one unit.
func (c *Client) Service(ctx context.Context, unit string) (*systemd.Status, error) {
	var out systemd.Status
	return &out, c.do(ctx, http.MethodGet, "/services/"+unit, nil, &out)
}

// ServiceAction acts on a unit.
func (c *Client) ServiceAction(ctx context.Context, unit, action string) (*systemd.Status, error) {
	var out systemd.Status
	return &out, c.do(ctx, http.MethodPost, "/services/"+unit, apitypes.ServiceActionRequest{Action: action}, &out)
}

// Stack lists components.
func (c *Client) Stack(ctx context.Context) ([]apitypes.StackComponent, error) {
	var out []apitypes.StackComponent
	return out, c.do(ctx, http.MethodGet, "/stack", nil, &out)
}

// StackInstall starts an install job.
func (c *Client) StackInstall(ctx context.Context, component string) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	return &out, c.do(ctx, http.MethodPost, "/stack/install", apitypes.StackInstallRequest{Component: component}, &out)
}

// JobEvents streams SSE events for a job until fn returns false or the
// stream ends.
func (c *Client) JobEvents(ctx context.Context, id int64, fn func(event string, data json.RawMessage) bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/jobs/%d/events", c.base, id), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("panel unreachable: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		data, _ := io.ReadAll(res.Body)
		e := &APIError{Status: res.StatusCode}
		_ = json.Unmarshal(data, e)
		if e.Title == "" {
			e.Title = res.Status
		}
		return e
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var event string
	var data bytes.Buffer
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				payload := json.RawMessage(append([]byte(nil), data.Bytes()...))
				if !fn(event, payload) {
					return nil
				}
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return ctx.Err()
}

func finished(status string) bool {
	return status == store.JobDone || status == store.JobFailed || status == store.JobCancelled
}

// WaitJob follows a job to completion; onEvent receives log/progress events.
func (c *Client) WaitJob(ctx context.Context, id int64, onEvent func(jobs.Event)) (*store.Job, error) {
	err := c.JobEvents(ctx, id, func(ev string, data json.RawMessage) bool {
		switch ev {
		case "snapshot":
			var snap struct {
				Job *store.Job `json:"job"`
			}
			_ = json.Unmarshal(data, &snap)
			if snap.Job != nil {
				if onEvent != nil {
					for _, l := range strings.Split(strings.TrimRight(snap.Job.Log, "\n"), "\n") {
						if l != "" {
							onEvent(jobs.Event{JobID: id, Type: "log", Line: l})
						}
					}
				}
				return !finished(snap.Job.Status)
			}
		case "ping", "":
		default:
			var e jobs.Event
			_ = json.Unmarshal(data, &e)
			if onEvent != nil {
				onEvent(e)
			}
			if ev == "done" || ev == "failed" {
				return false
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return c.GetJob(ctx, id)
}

// Certificates lists certificates.
func (c *Client) Certificates(ctx context.Context) ([]*store.Certificate, error) {
	var out []*store.Certificate
	return out, c.do(ctx, http.MethodGet, "/certificates", nil, &out)
}

// GetCertificate returns one certificate.
func (c *Client) GetCertificate(ctx context.Context, id int64) (*store.Certificate, error) {
	var out store.Certificate
	return &out, c.do(ctx, http.MethodGet, fmt.Sprintf("/certificates/%d", id), nil, &out)
}

// IssueCertificate orders a certificate.
func (c *Client) IssueCertificate(ctx context.Context, req apitypes.IssueCertificateRequest) (*apitypes.CertificateWithJob, error) {
	var out apitypes.CertificateWithJob
	return &out, c.do(ctx, http.MethodPost, "/certificates", req, &out)
}

// RenewCertificate re-issues a certificate.
func (c *Client) RenewCertificate(ctx context.Context, id int64) (*apitypes.CertificateWithJob, error) {
	var out apitypes.CertificateWithJob
	return &out, c.do(ctx, http.MethodPost, fmt.Sprintf("/certificates/%d/renew", id), nil, &out)
}

// DeleteCertificate removes a certificate.
func (c *Client) DeleteCertificate(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/certificates/%d", id), nil, nil)
}

// WebTLS describes the panel's own certificate.
func (c *Client) WebTLS(ctx context.Context) (*apitypes.TLSInfo, error) {
	var out apitypes.TLSInfo
	return &out, c.do(ctx, http.MethodGet, "/web/tls", nil, &out)
}

// PanelTLS is the panel's own certificate with the record behind it.
func (c *Client) PanelTLS(ctx context.Context) (*apitypes.PanelTLS, error) {
	var out apitypes.PanelTLS
	return &out, c.do(ctx, http.MethodGet, "/ssl/panel", nil, &out)
}

// PanelTLSIssue orders a certificate for the panel hostname.
func (c *Client) PanelTLSIssue(ctx context.Context, req apitypes.PanelTLSIssueRequest) (*apitypes.CertificateWithJob, error) {
	var out apitypes.CertificateWithJob
	return &out, c.do(ctx, http.MethodPost, "/ssl/panel/issue", req, &out)
}

// PanelTLSImport installs an existing certificate for the panel hostname.
func (c *Client) PanelTLSImport(ctx context.Context, req apitypes.PanelTLSImportRequest) (*store.Certificate, error) {
	var out store.Certificate
	return &out, c.do(ctx, http.MethodPost, "/ssl/panel/import", req, &out)
}

// PanelTLSReset drops the panel's certificate: back to self-signed.
func (c *Client) PanelTLSReset(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/ssl/panel", nil, nil)
}

// SiteTLSIssue orders a certificate for a site and switches it to HTTPS.
func (c *Client) SiteTLSIssue(ctx context.Context, domain string, req apitypes.SiteTLSIssueRequest) (*apitypes.CertificateWithJob, error) {
	var out apitypes.CertificateWithJob
	return &out, c.do(ctx, http.MethodPost, "/sites/"+url.PathEscape(domain)+"/tls/issue", req, &out)
}

// PHPVersions lists installed and available PHP branches.
func (c *Client) PHPVersions(ctx context.Context) (*apitypes.PHPVersions, error) {
	var out apitypes.PHPVersions
	return &out, c.do(ctx, http.MethodGet, "/php/versions", nil, &out)
}

// PHPInstall starts installing a branch.
func (c *Client) PHPInstall(ctx context.Context, version string) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	return &out, c.do(ctx, http.MethodPost, "/php/versions", apitypes.PHPInstallRequest{Version: version}, &out)
}

// PHPRemove starts removing a branch.
func (c *Client) PHPRemove(ctx context.Context, version string) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	return &out, c.do(ctx, http.MethodDelete, "/php/versions/"+version, nil, &out)
}

// Sites lists sites.
func (c *Client) Sites(ctx context.Context) ([]*store.Site, error) {
	var out []*store.Site
	return out, c.do(ctx, http.MethodGet, "/sites", nil, &out)
}

// GetSite returns one site.
func (c *Client) GetSite(ctx context.Context, domain string) (*store.Site, error) {
	var out store.Site
	return &out, c.do(ctx, http.MethodGet, "/sites/"+domain, nil, &out)
}

// CreateSite creates a site.
func (c *Client) CreateSite(ctx context.Context, req apitypes.SiteRequest) (*apitypes.SiteWithJob, error) {
	var out apitypes.SiteWithJob
	return &out, c.do(ctx, http.MethodPost, "/sites", req, &out)
}

// UpdateSite patches a site.
func (c *Client) UpdateSite(ctx context.Context, domain string, req apitypes.SiteUpdateRequest) (*apitypes.SiteWithJob, error) {
	var out apitypes.SiteWithJob
	return &out, c.do(ctx, http.MethodPatch, "/sites/"+domain, req, &out)
}

// SiteAction runs apply, suspend or unsuspend.
func (c *Client) SiteAction(ctx context.Context, domain, action string) (*apitypes.SiteWithJob, error) {
	var out apitypes.SiteWithJob
	return &out, c.do(ctx, http.MethodPost, "/sites/"+domain+"/"+action, nil, &out)
}

// DeleteSite removes a site.
func (c *Client) DeleteSite(ctx context.Context, domain string, purge bool) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	q := ""
	if purge {
		q = "?purge=true"
	}
	return &out, c.do(ctx, http.MethodDelete, "/sites/"+domain+q, nil, &out)
}

// StackRemove uninstalls a tool (memcached, jpegoptim, git, composer).
func (c *Client) StackRemove(ctx context.Context, component string) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	return &out, c.do(ctx, http.MethodDelete, "/stack/"+url.PathEscape(component), nil, &out)
}

// MemcachedSettings reads the panel's memcached configuration.
func (c *Client) MemcachedSettings(ctx context.Context) (*apitypes.MemcachedSettings, error) {
	var out apitypes.MemcachedSettings
	return &out, c.do(ctx, http.MethodGet, "/stack/memcached", nil, &out)
}

// SetMemcachedSettings stores and applies the memcached configuration.
func (c *Client) SetMemcachedSettings(ctx context.Context, req apitypes.MemcachedUpdate) (*apitypes.MemcachedSettings, error) {
	var out apitypes.MemcachedSettings
	return &out, c.do(ctx, http.MethodPut, "/stack/memcached", req, &out)
}

// DBEngineTune re-renders the MySQL configuration and restarts the server.
func (c *Client) DBEngineTune(ctx context.Context) (*apitypes.DBEngineStatus, error) {
	var out apitypes.DBEngineStatus
	return &out, c.do(ctx, http.MethodPost, "/db/engine/tune", nil, &out)
}

// DBEngine returns the database server status.
func (c *Client) DBEngine(ctx context.Context) (*apitypes.DBEngineStatus, error) {
	var out apitypes.DBEngineStatus
	return &out, c.do(ctx, http.MethodGet, "/db/engine", nil, &out)
}

// Databases lists databases.
func (c *Client) Databases(ctx context.Context) ([]*store.Database, error) {
	var out []*store.Database
	return out, c.do(ctx, http.MethodGet, "/databases", nil, &out)
}

// CreateDatabase creates a database and account.
func (c *Client) CreateDatabase(ctx context.Context, req apitypes.DatabaseRequest) (*apitypes.DatabaseResponse, error) {
	var out apitypes.DatabaseResponse
	return &out, c.do(ctx, http.MethodPost, "/databases", req, &out)
}

// DeleteDatabase drops a database.
func (c *Client) DeleteDatabase(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/databases/"+name, nil, nil)
}

// DatabasePassword resets the account password.
func (c *Client) DatabasePassword(ctx context.Context, name, password string) (*apitypes.DatabaseResponse, error) {
	var out apitypes.DatabaseResponse
	return &out, c.do(ctx, http.MethodPost, "/databases/"+name+"/password", apitypes.PasswordRequest{Password: password}, &out)
}

// CronJobs lists a user's cron jobs.
func (c *Client) CronJobs(ctx context.Context, login string) ([]*store.CronJob, error) {
	var out []*store.CronJob
	return out, c.do(ctx, http.MethodGet, "/users/"+login+"/cron", nil, &out)
}

// CronAdd adds a cron job.
func (c *Client) CronAdd(ctx context.Context, login string, req apitypes.CronRequest) (*store.CronJob, error) {
	var out store.CronJob
	return &out, c.do(ctx, http.MethodPost, "/users/"+login+"/cron", req, &out)
}

// CronUpdate edits a cron job.
func (c *Client) CronUpdate(ctx context.Context, login string, id int64, req apitypes.CronUpdateRequest) (*store.CronJob, error) {
	var out store.CronJob
	return &out, c.do(ctx, http.MethodPatch, fmt.Sprintf("/users/%s/cron/%d", login, id), req, &out)
}

// CronDelete removes a cron job.
func (c *Client) CronDelete(ctx context.Context, login string, id int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/users/%s/cron/%d", login, id), nil, nil)
}

// Firewall returns firewall status.
func (c *Client) Firewall(ctx context.Context) (*apitypes.FirewallStatus, error) {
	var out apitypes.FirewallStatus
	return &out, c.do(ctx, http.MethodGet, "/firewall", nil, &out)
}

// FirewallAction runs apply, enable or disable.
func (c *Client) FirewallAction(ctx context.Context, action string) (*apitypes.FirewallStatus, error) {
	var out apitypes.FirewallStatus
	return &out, c.do(ctx, http.MethodPost, "/firewall/"+action, nil, &out)
}

// FirewallRuleAdd adds a rule.
func (c *Client) FirewallRuleAdd(ctx context.Context, req apitypes.FirewallRuleRequest) (*store.FirewallRule, error) {
	var out store.FirewallRule
	return &out, c.do(ctx, http.MethodPost, "/firewall/rules", req, &out)
}

// FirewallRuleDelete removes a rule.
func (c *Client) FirewallRuleDelete(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/firewall/rules/%d", id), nil, nil)
}

// FirewallBan bans or unbans an address.
func (c *Client) FirewallBan(ctx context.Context, action, ip string) (*apitypes.FirewallStatus, error) {
	var out apitypes.FirewallStatus
	return &out, c.do(ctx, http.MethodPost, "/firewall/"+action, apitypes.BanRequest{IP: ip}, &out)
}

// Metrics returns host metrics.
func (c *Client) Metrics(ctx context.Context, rng string) (*apitypes.Metrics, error) {
	var out apitypes.Metrics
	return &out, c.do(ctx, http.MethodGet, "/system/metrics?range="+rng, nil, &out)
}

// SiteLogs returns a site log tail.
func (c *Client) SiteLogs(ctx context.Context, domain, typ string, lines int) (*apitypes.LogTail, error) {
	var out apitypes.LogTail
	return &out, c.do(ctx, http.MethodGet, fmt.Sprintf("/sites/%s/logs/%s?lines=%d", domain, typ, lines), nil, &out)
}

// ServiceLogs returns a service journal tail.
func (c *Client) ServiceLogs(ctx context.Context, unit string, lines int) (*apitypes.LogTail, error) {
	var out apitypes.LogTail
	return &out, c.do(ctx, http.MethodGet, fmt.Sprintf("/system/logs/%s?lines=%d", unit, lines), nil, &out)
}

// CMSList names the CMS the panel installs.
func (c *Client) CMSList(ctx context.Context) ([]apitypes.CMSInfo, error) {
	var out []apitypes.CMSInfo
	return out, c.do(ctx, http.MethodGet, "/cms", nil, &out)
}

// CMSInstall installs a CMS into a site; the credentials come back once.
func (c *Client) CMSInstall(ctx context.Context, domain string, req apitypes.CMSInstallRequest) (*apitypes.CMSInstallResult, error) {
	var out apitypes.CMSInstallResult
	return &out, c.do(ctx, http.MethodPost, "/sites/"+domain+"/cms", req, &out)
}

// SELinux reads the host's SELinux mode.
func (c *Client) SELinux(ctx context.Context) (*apitypes.SELinuxStatus, error) {
	var out apitypes.SELinuxStatus
	return &out, c.do(ctx, http.MethodGet, "/system/selinux", nil, &out)
}

// SetSELinux switches the mode, now and for the next boot.
func (c *Client) SetSELinux(ctx context.Context, mode string) (*apitypes.SELinuxStatus, error) {
	var out apitypes.SELinuxStatus
	return &out, c.do(ctx, http.MethodPut, "/system/selinux", apitypes.SELinuxRequest{Mode: mode}, &out)
}

// Doctor runs health checks.
func (c *Client) Doctor(ctx context.Context) (*apitypes.Doctor, error) {
	var out apitypes.Doctor
	return &out, c.do(ctx, http.MethodGet, "/system/doctor", nil, &out)
}

// BackupTargets lists targets.
func (c *Client) BackupTargets(ctx context.Context) ([]*store.BackupTarget, error) {
	var out []*store.BackupTarget
	return out, c.do(ctx, http.MethodGet, "/backups/targets", nil, &out)
}

// BackupTargetCreate registers a target.
func (c *Client) BackupTargetCreate(ctx context.Context, req apitypes.BackupTargetRequest) (*apitypes.BackupTargetResponse, error) {
	var out apitypes.BackupTargetResponse
	return &out, c.do(ctx, http.MethodPost, "/backups/targets", req, &out)
}

// BackupTargetDelete forgets a target.
func (c *Client) BackupTargetDelete(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/backups/targets/"+ref, nil, nil)
}

// BackupSnapshots lists snapshots.
func (c *Client) BackupSnapshots(ctx context.Context, ref string) ([]apitypes.Snapshot, error) {
	var out []apitypes.Snapshot
	return out, c.do(ctx, http.MethodGet, "/backups/targets/"+ref+"/snapshots", nil, &out)
}

// Backups lists runs.
func (c *Client) Backups(ctx context.Context, target string, limit int) ([]*store.Backup, error) {
	var out []*store.Backup
	q := fmt.Sprintf("/backups?limit=%d", limit)
	if target != "" {
		q += "&target=" + target
	}
	return out, c.do(ctx, http.MethodGet, q, nil, &out)
}

// BackupRun starts a backup.
func (c *Client) BackupRun(ctx context.Context, target, scope string) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	return &out, c.do(ctx, http.MethodPost, "/backups/run", apitypes.BackupRunRequest{Target: target, Scope: scope}, &out)
}

// BackupRestore restores a snapshot.
func (c *Client) BackupRestore(ctx context.Context, req apitypes.RestoreRequest) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	return &out, c.do(ctx, http.MethodPost, "/backups/restore", req, &out)
}

// TOTPSetup starts 2FA enrolment.
func (c *Client) TOTPSetup(ctx context.Context) (*apitypes.TOTPSetup, error) {
	var out apitypes.TOTPSetup
	return &out, c.do(ctx, http.MethodPost, "/auth/totp/setup", struct{}{}, &out)
}

// TOTPEnable confirms enrolment.
func (c *Client) TOTPEnable(ctx context.Context, code string) (*apitypes.TOTPStatus, error) {
	var out apitypes.TOTPStatus
	return &out, c.do(ctx, http.MethodPost, "/auth/totp/enable", apitypes.TOTPCode{Code: code}, &out)
}

// TOTPReset removes a user's 2FA (admin).
func (c *Client) TOTPReset(ctx context.Context, login string) error {
	return c.do(ctx, http.MethodPost, "/users/"+login+"/totp/reset", struct{}{}, nil)
}

// Webhooks lists subscribers.
func (c *Client) Webhooks(ctx context.Context) ([]apitypes.Webhook, error) {
	var out []apitypes.Webhook
	return out, c.do(ctx, http.MethodGet, "/webhooks", nil, &out)
}

// WebhookCreate adds a subscriber.
func (c *Client) WebhookCreate(ctx context.Context, req apitypes.WebhookRequest) (*apitypes.Webhook, error) {
	var out apitypes.Webhook
	return &out, c.do(ctx, http.MethodPost, "/webhooks", req, &out)
}

// WebhookDelete removes a subscriber.
func (c *Client) WebhookDelete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/webhooks/"+id, nil, nil)
}

// WebhookTest sends a test event.
func (c *Client) WebhookTest(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/webhooks/"+id+"/test", struct{}{}, nil)
}

// UpdateUser changes account settings.
func (c *Client) UpdateUser(ctx context.Context, login string, req apitypes.UserUpdateRequest) (*apitypes.UserWithJob, error) {
	var out apitypes.UserWithJob
	return &out, c.do(ctx, http.MethodPatch, "/users/"+login, req, &out)
}

// FilesList lists a directory.
func (c *Client) FilesList(ctx context.Context, user, path string) (*apitypes.FileList, error) {
	var out apitypes.FileList
	return &out, c.do(ctx, http.MethodGet, "/files?user="+url.QueryEscape(user)+"&path="+url.QueryEscape(path), nil, &out)
}

// FilesOp runs a file operation.
func (c *Client) FilesOp(ctx context.Context, req apitypes.FileOpRequest) (*apitypes.FileOpResult, error) {
	var out apitypes.FileOpResult
	return &out, c.do(ctx, http.MethodPost, "/files/op", req, &out)
}

// FileDownload returns file bytes.
func (c *Client) FileDownload(ctx context.Context, user, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/files/content?user="+url.QueryEscape(user)+"&path="+url.QueryEscape(path), nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		e := &APIError{Status: res.StatusCode}
		_ = json.Unmarshal(data, e)
		if e.Title == "" {
			e.Title = res.Status
		}
		return nil, e
	}
	return data, err
}

// FileUpload uploads bytes to a path.
func (c *Client) FileUpload(ctx context.Context, user, path string, data []byte) (*apitypes.FileOpResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/files/content?user="+url.QueryEscape(user)+"&path="+url.QueryEscape(path), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		e := &APIError{Status: res.StatusCode}
		_ = json.Unmarshal(body, e)
		if e.Title == "" {
			e.Title = res.Status
		}
		return nil, e
	}
	var out apitypes.FileOpResult
	return &out, json.Unmarshal(body, &out)
}

// DNSProviders lists DNS providers.
func (c *Client) DNSProviders(ctx context.Context) ([]*store.DNSProvider, error) {
	var out []*store.DNSProvider
	return out, c.do(ctx, http.MethodGet, "/dns-providers", nil, &out)
}

// DNSProviderTypes lists supported types.
func (c *Client) DNSProviderTypes(ctx context.Context) (map[string][]string, error) {
	var out map[string][]string
	return out, c.do(ctx, http.MethodGet, "/dns-providers/types", nil, &out)
}

// DNSProviderCreate adds a provider.
func (c *Client) DNSProviderCreate(ctx context.Context, req apitypes.DNSProviderRequest) (*store.DNSProvider, error) {
	var out store.DNSProvider
	return &out, c.do(ctx, http.MethodPost, "/dns-providers", req, &out)
}

// DNSProviderDelete removes a provider.
func (c *Client) DNSProviderDelete(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/dns-providers/"+name, nil, nil)
}

// ImportCertificate installs an existing certificate.
func (c *Client) ImportCertificate(ctx context.Context, req apitypes.ImportCertificateRequest) (*store.Certificate, error) {
	var out store.Certificate
	return &out, c.do(ctx, http.MethodPost, "/certificates/import", req, &out)
}

// AllApps lists every app service (admin).
func (c *Client) AllApps(ctx context.Context) ([]*apitypes.AppStatus, error) {
	var out []*apitypes.AppStatus
	return out, c.do(ctx, http.MethodGet, "/apps", nil, &out)
}

// Apps lists the app services of a user.
func (c *Client) Apps(ctx context.Context, login string) ([]*apitypes.AppStatus, error) {
	var out []*apitypes.AppStatus
	return out, c.do(ctx, http.MethodGet, "/users/"+login+"/apps", nil, &out)
}

// GetApp returns one app service.
func (c *Client) GetApp(ctx context.Context, login, name string) (*apitypes.AppStatus, error) {
	var out apitypes.AppStatus
	return &out, c.do(ctx, http.MethodGet, "/users/"+login+"/apps/"+name, nil, &out)
}

// CreateApp creates and starts an app service.
func (c *Client) CreateApp(ctx context.Context, login string, req apitypes.AppRequest) (*apitypes.AppStatus, error) {
	var out apitypes.AppStatus
	return &out, c.do(ctx, http.MethodPost, "/users/"+login+"/apps", req, &out)
}

// UpdateApp edits an app service.
func (c *Client) UpdateApp(ctx context.Context, login, name string, req apitypes.AppUpdateRequest) (*apitypes.AppStatus, error) {
	var out apitypes.AppStatus
	return &out, c.do(ctx, http.MethodPatch, "/users/"+login+"/apps/"+name, req, &out)
}

// AppAction starts, stops or restarts an app service.
func (c *Client) AppAction(ctx context.Context, login, name, action string) (*apitypes.AppStatus, error) {
	var out apitypes.AppStatus
	return &out, c.do(ctx, http.MethodPost, "/users/"+login+"/apps/"+name+"/"+action, nil, &out)
}

// AppLogs returns the journal tail of an app service.
func (c *Client) AppLogs(ctx context.Context, login, name string, lines int) (*apitypes.LogTail, error) {
	var out apitypes.LogTail
	return &out, c.do(ctx, http.MethodGet, fmt.Sprintf("/users/%s/apps/%s/logs?lines=%d", login, name, lines), nil, &out)
}

// DeleteApp removes an app service.
func (c *Client) DeleteApp(ctx context.Context, login, name string) error {
	return c.do(ctx, http.MethodDelete, "/users/"+login+"/apps/"+name, nil, nil)
}

// RealIP returns the trusted-proxy settings of nginx.
func (c *Client) RealIP(ctx context.Context) (*apitypes.RealIPSettings, error) {
	var out apitypes.RealIPSettings
	return &out, c.do(ctx, http.MethodGet, "/stack/nginx/real-ip", nil, &out)
}

// SetRealIP replaces the trusted-proxy settings of nginx.
func (c *Client) SetRealIP(ctx context.Context, req apitypes.RealIPSettings) (*apitypes.RealIPSettings, error) {
	var out apitypes.RealIPSettings
	return &out, c.do(ctx, http.MethodPut, "/stack/nginx/real-ip", req, &out)
}

// DeleteUser removes a user and everything they own (async).
func (c *Client) DeleteUser(ctx context.Context, login string, purge bool) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	q := ""
	if purge {
		q = "?purge=true"
	}
	return &out, c.do(ctx, http.MethodDelete, "/users/"+login+q, nil, &out)
}

// SiteNginx returns the generated server block and custom include of a site.
func (c *Client) SiteNginx(ctx context.Context, domain string) (*apitypes.SiteNginx, error) {
	var out apitypes.SiteNginx
	return &out, c.do(ctx, http.MethodGet, "/sites/"+domain+"/nginx", nil, &out)
}

// SetSiteNginx replaces the custom include of a site (validated by nginx -t).
func (c *Client) SetSiteNginx(ctx context.Context, domain, custom string) (*apitypes.SiteNginx, error) {
	var out apitypes.SiteNginx
	return &out, c.do(ctx, http.MethodPut, "/sites/"+domain+"/nginx", apitypes.SiteNginxRequest{Custom: custom}, &out)
}

// SitePHP returns the effective PHP settings of a site.
func (c *Client) SitePHP(ctx context.Context, domain string) (*apitypes.SitePHP, error) {
	var out apitypes.SitePHP
	return &out, c.do(ctx, http.MethodGet, "/sites/"+domain+"/php", nil, &out)
}

// Presets lists the CMS presets for new sites.
func (c *Client) Presets(ctx context.Context) ([]apitypes.SitePreset, error) {
	var out []apitypes.SitePreset
	return out, c.do(ctx, http.MethodGet, "/sites/presets", nil, &out)
}

// UpdateStatus reports the panel version and the release found last time.
func (c *Client) UpdateStatus(ctx context.Context) (*apitypes.UpdateStatus, error) {
	var out apitypes.UpdateStatus
	return &out, c.do(ctx, http.MethodGet, "/system/update", nil, &out)
}

// SetUpdateSettings changes where the panel looks for new versions.
func (c *Client) SetUpdateSettings(ctx context.Context, req apitypes.UpdateSettingsRequest) (*apitypes.UpdateStatus, error) {
	var out apitypes.UpdateStatus
	return &out, c.do(ctx, http.MethodPut, "/system/update", req, &out)
}

// CheckUpdate asks the repository for a release right now.
func (c *Client) CheckUpdate(ctx context.Context) (*apitypes.UpdateStatus, error) {
	var out apitypes.UpdateStatus
	return &out, c.do(ctx, http.MethodPost, "/system/update/check", struct{}{}, &out)
}

// ApplyUpdate installs a version; an empty version means the latest one.
func (c *Client) ApplyUpdate(ctx context.Context, version string) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	return &out, c.do(ctx, http.MethodPost, "/system/update/apply", apitypes.UpdateApplyRequest{Version: version}, &out)
}

// PHPExtensions lists the extensions of a branch.
func (c *Client) PHPExtensions(ctx context.Context, version string) (*apitypes.PHPExtensions, error) {
	var out apitypes.PHPExtensions
	return &out, c.do(ctx, http.MethodGet, "/php/versions/"+version+"/extensions", nil, &out)
}

// SetPHPExtension switches one extension on or off for a branch.
func (c *Client) SetPHPExtension(ctx context.Context, version, name string, enabled bool) (*apitypes.PHPExtensions, error) {
	var out apitypes.PHPExtensions
	return &out, c.do(ctx, http.MethodPost, "/php/versions/"+version+"/extensions", apitypes.PHPExtensionRequest{Name: name, Enabled: enabled}, &out)
}

// MailStatus returns the state of the mail server.
func (c *Client) MailStatus(ctx context.Context) (*apitypes.MailStatus, error) {
	var out apitypes.MailStatus
	return &out, c.do(ctx, http.MethodGet, "/mail", nil, &out)
}

// MailInstall installs postfix, dovecot and opendkim.
func (c *Client) MailInstall(ctx context.Context, req apitypes.MailInstallRequest) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	return &out, c.do(ctx, http.MethodPost, "/mail/install", req, &out)
}

// MailApply regenerates the mail configuration.
func (c *Client) MailApply(ctx context.Context) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	return &out, c.do(ctx, http.MethodPost, "/mail/apply", struct{}{}, &out)
}

// MailSettings changes the server-wide mail settings.
func (c *Client) MailSettings(ctx context.Context, req apitypes.MailSettingsRequest) (*apitypes.MailStatus, error) {
	var out apitypes.MailStatus
	return &out, c.do(ctx, http.MethodPut, "/mail/settings", req, &out)
}

// MailDomains lists mail domains.
func (c *Client) MailDomains(ctx context.Context) ([]*store.MailDomain, error) {
	var out []*store.MailDomain
	return out, c.do(ctx, http.MethodGet, "/mail/domains", nil, &out)
}

// CreateMailDomain adds a mail domain.
func (c *Client) CreateMailDomain(ctx context.Context, req apitypes.MailDomainRequest) (*store.MailDomain, error) {
	var out store.MailDomain
	return &out, c.do(ctx, http.MethodPost, "/mail/domains", req, &out)
}

// UpdateMailDomain changes a mail domain.
func (c *Client) UpdateMailDomain(ctx context.Context, name string, req apitypes.MailDomainUpdateRequest) (*store.MailDomain, error) {
	var out store.MailDomain
	return &out, c.do(ctx, http.MethodPatch, "/mail/domains/"+name, req, &out)
}

// DeleteMailDomain removes a mail domain.
func (c *Client) DeleteMailDomain(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/mail/domains/"+name, nil, nil)
}

// MailDKIM generates a new signing key for the domain.
func (c *Client) MailDKIM(ctx context.Context, name string) (*store.MailDomain, error) {
	var out store.MailDomain
	return &out, c.do(ctx, http.MethodPost, "/mail/domains/"+name+"/dkim", struct{}{}, &out)
}

// MailDNS returns the required DNS records and their live state.
func (c *Client) MailDNS(ctx context.Context, name string) (*apitypes.MailDNS, error) {
	var out apitypes.MailDNS
	return &out, c.do(ctx, http.MethodGet, "/mail/domains/"+name+"/dns", nil, &out)
}

// Mailboxes lists mailboxes, optionally of one domain.
func (c *Client) Mailboxes(ctx context.Context, domain string) ([]*store.Mailbox, error) {
	var out []*store.Mailbox
	q := ""
	if domain != "" {
		q = "?domain=" + domain
	}
	return out, c.do(ctx, http.MethodGet, "/mail/mailboxes"+q, nil, &out)
}

// CreateMailbox creates a mailbox.
func (c *Client) CreateMailbox(ctx context.Context, req apitypes.MailboxRequest) (*apitypes.MailboxResponse, error) {
	var out apitypes.MailboxResponse
	return &out, c.do(ctx, http.MethodPost, "/mail/mailboxes", req, &out)
}

// UpdateMailbox changes a mailbox.
func (c *Client) UpdateMailbox(ctx context.Context, address string, req apitypes.MailboxUpdateRequest) (*apitypes.MailboxResponse, error) {
	var out apitypes.MailboxResponse
	return &out, c.do(ctx, http.MethodPatch, "/mail/mailboxes/"+address, req, &out)
}

// DeleteMailbox removes a mailbox.
func (c *Client) DeleteMailbox(ctx context.Context, address string, purge bool) error {
	q := ""
	if purge {
		q = "?purge=true"
	}
	return c.do(ctx, http.MethodDelete, "/mail/mailboxes/"+address+q, nil, nil)
}

// MailAliases lists aliases, optionally of one domain.
func (c *Client) MailAliases(ctx context.Context, domain string) ([]*store.MailAlias, error) {
	var out []*store.MailAlias
	q := ""
	if domain != "" {
		q = "?domain=" + domain
	}
	return out, c.do(ctx, http.MethodGet, "/mail/aliases"+q, nil, &out)
}

// CreateMailAlias creates or replaces an alias.
func (c *Client) CreateMailAlias(ctx context.Context, req apitypes.MailAliasRequest) (*store.MailAlias, error) {
	var out store.MailAlias
	return &out, c.do(ctx, http.MethodPost, "/mail/aliases", req, &out)
}

// DeleteMailAlias removes an alias.
func (c *Client) DeleteMailAlias(ctx context.Context, address string) error {
	return c.do(ctx, http.MethodDelete, "/mail/aliases/"+address, nil, nil)
}

// InstallWebmail installs Roundcube as a site.
func (c *Client) InstallWebmail(ctx context.Context, req apitypes.WebmailRequest) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	return &out, c.do(ctx, http.MethodPost, "/mail/webmail", req, &out)
}

// MigrateGrant opens this panel for one migration and returns the token once.
func (c *Client) MigrateGrant(ctx context.Context, req apitypes.MigrationGrantRequest) (*apitypes.MigrationGrantResponse, error) {
	var out apitypes.MigrationGrantResponse
	return &out, c.do(ctx, http.MethodPost, "/migrate/grant", req, &out)
}

// MigratePlan asks what would arrive from another panel and what stands in the way.
func (c *Client) MigratePlan(ctx context.Context, req apitypes.MigrationSourceRequest) (*apitypes.MigrationPlan, error) {
	var out apitypes.MigrationPlan
	return &out, c.do(ctx, http.MethodPost, "/migrate/plan", req, &out)
}

// MigrateRun takes the account over.
func (c *Client) MigrateRun(ctx context.Context, req apitypes.MigrationSourceRequest) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	return &out, c.do(ctx, http.MethodPost, "/migrate/run", req, &out)
}
