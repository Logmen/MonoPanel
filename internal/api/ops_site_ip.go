package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

// The host's address can change under the panel (cloud-init, a new VM from
// an image). Sites keep their address in the state and nginx listens on it,
// so after such a change nginx cannot bind the old one and refuses to start;
// the configuration check fails the same way, which blocks moving the sites
// one by one. Here: the default servers of gone addresses are dropped, the
// sites on them are reported, and sites.move-ip moves them all at once.

// defaultServersDir holds one ip-<address>.conf per local address.
func (s *Server) defaultServersDir() string {
	return path.Join(s.profile.Web().NginxConfDir, "monopanel", "http.d")
}

// pruneDefaultServers removes the default servers of addresses the host no
// longer has. With no address at all (network not up yet) it keeps them.
func (s *Server) pruneDefaultServers(ctx context.Context) []string {
	local := map[string]bool{}
	for _, ip := range s.hostIPs() {
		local[ip] = true
	}
	if len(local) == 0 {
		return nil
	}
	dir := s.defaultServersDir()
	list, err := s.agent.ListDir(ctx, dir)
	if err != nil {
		return nil
	}
	var stale []string
	for _, e := range list.Entries {
		ip := strings.TrimSuffix(strings.TrimPrefix(e.Name, "ip-"), ".conf")
		if e.Name == "ip-"+ip+".conf" && net.ParseIP(ip) != nil && !local[ip] {
			stale = append(stale, path.Join(dir, e.Name))
		}
	}
	if len(stale) == 0 {
		return nil
	}
	res, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: stale})
	if err != nil {
		s.log.Warn("stale default servers", "err", err)
		return nil
	}
	return res.Removed
}

// sitesOffHost groups the sites that listen on an address this host does
// not have, by that address.
func (s *Server) sitesOffHost(ctx context.Context) map[string][]string {
	local := map[string]bool{}
	for _, ip := range s.hostIPs() {
		local[ip] = true
	}
	out := map[string][]string{}
	if len(local) == 0 {
		return out
	}
	sites, err := s.db.ListSites(ctx, 0)
	if err != nil {
		return out
	}
	for _, site := range sites {
		if site.IP != "" && !local[site.IP] {
			out[site.IP] = append(out[site.IP], site.Domain)
		}
	}
	return out
}

// offHostHint says what the doctor and the startup log say about them.
func (s *Server) offHostHint(off map[string][]string) string {
	ips := make([]string, 0, len(off))
	for ip := range off {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	var parts []string
	for _, ip := range ips {
		parts = append(parts, fmt.Sprintf("%s (%s)", ip, strings.Join(off[ip], ", ")))
	}
	to := "<address>"
	if local := s.hostIPs(); len(local) == 1 {
		to = local[0]
	}
	return "addresses not on this server, nginx cannot bind them: " + strings.Join(parts, "; ") + " — mp site move-ip " + to
}

// checkHostIP refuses an address the host does not have: nginx would not
// start with it.
func (s *Server) checkHostIP(ip string) error {
	for _, local := range s.hostIPs() {
		if local == ip {
			return nil
		}
	}
	return fmt.Errorf("address %s is not on this server (available: %s)", ip, strings.Join(s.hostIPs(), ", "))
}

type sitesMoveIPPayload struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// sitesToMove lists the sites on from, or on any gone address when from is empty.
func (s *Server) sitesToMove(ctx context.Context, from string) ([]*store.Site, error) {
	all, err := s.db.ListSites(ctx, 0)
	if err != nil {
		return nil, err
	}
	gone := s.sitesOffHost(ctx)
	var out []*store.Site
	for _, site := range all {
		if from != "" && site.IP == from || from == "" && gone[site.IP] != nil {
			out = append(out, site)
		}
	}
	return out, nil
}

type siteMoveIPInput struct {
	Body apitypes.SiteMoveIPRequest
}

type siteMoveIPOutput struct {
	Status int
	Body   apitypes.SiteMoveIPResponse
}

func (s *Server) registerSiteIP() {
	huma.Register(s.api, huma.Operation{
		OperationID: "sites-move-ip", Method: http.MethodPost, Path: "/sites/move-ip", Summary: "Move every site of an address to another address of this host (async)", Tags: []string{"sites"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *siteMoveIPInput) (*siteMoveIPOutput, error) {
		p := principalFrom(ctx)
		b := in.Body
		if net.ParseIP(b.To).To4() == nil || b.From != "" && net.ParseIP(b.From) == nil {
			return nil, huma.Error422UnprocessableEntity("an IPv4 address is required")
		}
		if err := s.checkHostIP(b.To); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		sites, err := s.sitesToMove(ctx, b.From)
		if err != nil {
			return nil, err
		}
		if len(sites) == 0 {
			if b.From == "" {
				return nil, huma.Error422UnprocessableEntity("every site already listens on an address of this server")
			}
			return nil, huma.Error422UnprocessableEntity("no sites on " + b.From)
		}
		job, err := s.jobs.Enqueue(ctx, "sites.move-ip", sitesMoveIPPayload(b), jobs.WithLockKey("sites:ip"), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		out := apitypes.SiteMoveIPResponse{JobID: job.ID}
		for _, site := range sites {
			out.Sites = append(out.Sites, site.Domain)
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "sites.move-ip", Target: b.To, IP: requestInfo(ctx).IP, Details: map[string]any{"from": b.From, "sites": out.Sites}})
		return &siteMoveIPOutput{Status: http.StatusAccepted, Body: out}, nil
	})
}

// jobSitesMoveIP drops the nginx files of the moving sites first — they are
// unreachable anyway, nginx cannot bind their address — so that the check
// passes for the default servers and then for each site as it comes back on
// the new address.
func (s *Server) jobSitesMoveIP(ctx context.Context, jc *jobs.Context) error {
	var p sitesMoveIPPayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	if err := s.checkHostIP(p.To); err != nil {
		return err
	}
	sites, err := s.sitesToMove(ctx, p.From)
	if err != nil {
		return err
	}
	if len(sites) == 0 {
		jc.Progress(100, "nothing to move")
		return nil
	}
	jc.Progress(10, "nginx configuration")
	var confs []string
	for _, site := range sites {
		user, err := s.db.GetUserByID(ctx, site.UserID)
		if err != nil {
			return err
		}
		confs = append(confs, s.layoutFor(site, user).nginxConf)
	}
	if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: confs}); err != nil {
		return err
	}
	for _, site := range sites {
		jc.Logf("%s: %s → %s", site.Domain, site.IP, p.To)
		site.IP = p.To
		if site.Status == store.SiteError {
			site.Status = store.SitePending
		}
		if err := s.db.UpdateSite(ctx, site); err != nil {
			return err
		}
	}
	if gone := s.pruneDefaultServers(ctx); len(gone) > 0 {
		jc.Logf("removed the default servers of addresses no longer on this server: %s", strings.Join(gone, ", "))
	}
	files, err := s.nginxGlobalFiles()
	if err != nil {
		return err
	}
	web := s.profile.Web()
	// nginx may be down since boot: reload-or-restart brings it up.
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Validate: [][]string{web.NginxCheckArgv}, Reload: []string{web.NginxService}, Force: true, Origin: "stack:nginx"}); err != nil {
		return fmt.Errorf("default servers: %w", err)
	}
	jc.Progress(50, "sites")
	for _, site := range sites {
		id, err := s.enqueueSiteApply(ctx, site, jc.RequestedBy)
		if err != nil {
			return err
		}
		jc.Logf("%s: job #%d applies the configuration", site.Domain, id)
	}
	jc.Progress(100, fmt.Sprintf("sites moved to %s: %d", p.To, len(sites)))
	return nil
}
