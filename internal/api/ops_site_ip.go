package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

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
	for _, ip := range append(s.hostIPs(), s.hostIPv6s()...) {
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
			// Right after boot an IPv6 address from router advertisements may
			// simply not be there yet; its default server is not stale.
			if strings.Contains(ip, ":") && justBooted(5*time.Minute) {
				continue
			}
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
	local6 := map[string]bool{}
	for _, ip := range s.hostIPv6s() {
		local6[ip] = true
	}
	for _, site := range sites {
		if site.IP != "" && !local[site.IP] {
			out[site.IP] = append(out[site.IP], site.Domain)
		}
		if site.IPv6 != "" && !local6[site.IPv6] {
			out[site.IPv6] = append(out[site.IPv6], site.Domain)
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
	v6 := false
	for _, ip := range ips {
		parts = append(parts, fmt.Sprintf("%s (%s)", ip, strings.Join(off[ip], ", ")))
		v6 = v6 || strings.Contains(ip, ":")
	}
	to := "<address>"
	if local := s.hostIPs(); len(local) == 1 {
		to = local[0]
	}
	hint := "mp site move-ip " + to
	if v6 {
		hint += "; IPv6: mp site set <domain> --ipv6 <address>|none"
	}
	return "addresses not on this server, nginx cannot bind them: " + strings.Join(parts, "; ") + " — " + hint
}

// checkHostIPv6 refuses an IPv6 address the host does not have.
func (s *Server) checkHostIPv6(ip string) error {
	for _, local := range s.hostIPv6s() {
		if local == ip {
			return nil
		}
	}
	if have := s.hostIPv6s(); len(have) > 0 {
		return fmt.Errorf("IPv6 address %s is not on this server (available: %s)", ip, strings.Join(have, ", "))
	}
	return fmt.Errorf("IPv6 address %s is not on this server (it has no global IPv6 address)", ip)
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
//
// The list is made when the job starts; each site then moves under its lock
// (moveSiteIP), from what it is by then. A site.apply of it takes the same
// lock, so it renders either before the move — on the old address, which
// fails or goes — or after it, on the new one, and never writes the file
// back between the removal and the move.
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
	var moved []*store.Site
	var failed error
	webmail := false
	for _, site := range sites {
		ok, block, err := s.moveSiteIP(ctx, jc, site, p.To)
		webmail = webmail || block
		if err != nil {
			failed = fmt.Errorf("%s: %w", site.Domain, err)
			break
		}
		if ok {
			moved = append(moved, site)
		}
	}
	// What the host lost goes from nginx before any site comes back: a
	// listen left on such an address fails every check, the sites' too.
	if gone := s.pruneDefaultServers(ctx); len(gone) > 0 {
		jc.Logf("removed the default servers of addresses no longer on this server: %s", strings.Join(gone, ", "))
	}
	files, quic, err := s.nginxGlobalFiles(ctx)
	if err == nil {
		web := s.profile.Web()
		// nginx may be down since boot: reload-or-restart brings it up.
		if _, err = s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Validate: [][]string{web.NginxCheckArgv}, Reload: []string{web.NginxService}, Force: true, Origin: "stack:nginx"}); err != nil {
			err = fmt.Errorf("default servers: %w", err)
		} else {
			s.defaultQuic.Store(quicFlag(quic))
		}
	}
	if failed == nil {
		failed = err
	}
	// The sites that moved get their applies whatever failed after them:
	// their configuration is gone, and a second move-ip, which lists what
	// is still on the old address, would not find them.
	jc.Progress(50, "sites")
	for _, site := range moved {
		id, err := s.enqueueSiteApply(ctx, site, jc.RequestedBy)
		if err != nil {
			return errors.Join(failed, err)
		}
		jc.Logf("%s: job #%d applies the configuration", site.Domain, id)
	}
	// The webmail's block on its port went with its site's (moveSiteIP):
	// mail.apply writes it again, on the address the site has by then.
	if webmail {
		job, err := s.jobs.Enqueue(ctx, "mail.apply", struct{}{}, jobs.WithLockKey("mail"), jobs.WithRequestedBy(jc.RequestedBy))
		if err != nil {
			return errors.Join(failed, err)
		}
		jc.Logf("the webmail on its port: job #%d writes its server block again", job.ID)
	}
	if failed != nil {
		return failed
	}
	jc.Progress(100, fmt.Sprintf("sites moved to %s: %d", p.To, len(moved)))
	return nil
}

// moveSiteIP moves one site of the list to the address to, under its lock,
// and tells whether it did. The site is read again under the lock: since the
// job listed it, it may have been removed, its account or itself may be on
// the way out, or an administrator may have put it on another address
// (mp site set) — such a site is not moved and gets no apply from here. One
// being removed loses its server block all the same, as its removal would:
// it listens on an address the host lost, and nginx's check fails while it
// is there. Only the address is written (store.MoveSiteIP): a change made
// to the site meanwhile stays.
//
// The webmail on its port listens on its site's address as well
// (applyWebmailPort), and nginx's check fails on that block just the same:
// it goes with the site's, and the job then queues a mail.apply (webmail),
// which writes it again on the address the site has by then. webmailMu,
// held until the site has moved, keeps a mail change — applyWebmailPort
// reads the site's address under it — from writing the block back on the
// old address in between.
func (s *Server) moveSiteIP(ctx context.Context, jc *jobs.Context, listed *store.Site, to string) (moved, webmail bool, err error) {
	defer s.lockSite(listed.ID)()
	site, err := s.db.GetSite(ctx, listed.ID)
	if errors.Is(err, store.ErrNotFound) {
		jc.Logf("%s: removed meanwhile", listed.Domain)
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if site.IP != listed.IP {
		jc.Logf("%s: on %s now, not %s: left as it is", site.Domain, site.IP, listed.IP)
		return false, false, nil
	}
	user, err := s.db.GetUserByID(ctx, site.UserID)
	if err != nil {
		return false, false, err
	}
	paths := []string{s.layoutFor(site, user).nginxConf}
	if c := s.loadMailConfig(ctx); c.Webmail == site.Domain && c.WebmailPort > 0 {
		s.webmailMu.Lock()
		defer s.webmailMu.Unlock()
		// Asked again under the lock (removeSitePaths says why).
		if c := s.loadMailConfig(ctx); c.Webmail == site.Domain && c.WebmailPort > 0 {
			paths, webmail = append(paths, s.webmailConfPath()), true
		}
	}
	if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: paths}); err != nil {
		return false, webmail, err
	}
	if site.Status == store.SiteDeleting || user.Status == store.UserDeleting {
		jc.Logf("%s: being removed: its server block goes, the site does not move", site.Domain)
		return false, webmail, nil
	}
	ok, err := s.db.MoveSiteIP(context.WithoutCancel(ctx), site.ID, listed.IP, to)
	if err != nil || !ok {
		if err == nil {
			jc.Logf("%s: changed meanwhile (its address, or it is being removed): not moved", site.Domain)
		}
		return false, webmail, err
	}
	jc.Logf("%s: %s → %s", site.Domain, listed.IP, to)
	return true, webmail, nil
}
