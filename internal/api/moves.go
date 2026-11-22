package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"monopanel/internal/apitypes"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

// The one-time moves of an upgrade from 0.8.19 — the sites' logs into the
// root-owned tree (moveSiteLogs), the sites in Apache mode onto their
// accounts' own Apaches (refreshApacheInstances) — run as jobs, and a job can
// fail: the site's PHP branch is gone, nginx -t fails for the whole host at
// that moment because of another file, the agent restarts under it. What the
// panel remembers is what finished, never what it queued: a move that did not
// finish is queued again at the next start, and once an hour after the
// start-up refresh ran, each time later than the last — retryFirst after the
// first failure, four times as long after each further one, retryMax at most
// — so that a site that keeps failing costs a failed job a day, not one at
// every start. The doctor names what has not moved, and why.

const (
	retryFirst = 10 * time.Minute
	retryMax   = 24 * time.Hour
)

// retryDelay is the wait before the next try after the n-th failure.
func retryDelay(n int) time.Duration {
	d := retryFirst
	for i := 1; i < n && d < retryMax; i++ {
		d *= 4
	}
	return min(d, retryMax)
}

// retry is the bookkeeping of a move that runs as a job until it succeeds:
// the job queued last, the failures, when the next job may be queued and why
// the last one failed.
type retry struct {
	Job   int64     `json:"job,omitempty"`
	Tries int       `json:"tries,omitempty"`
	Next  time.Time `json:"next,omitzero"`
	Error string    `json:"error,omitempty"`
}

// settle looks at the job queued last and tells whether it still waits or
// runs. One that failed — or was cancelled, or is gone — is a failure: the
// next try waits retryDelay from when it ended. One that is done leaves the
// verdict to the caller, which knows whether the move finished.
func (r *retry) settle(ctx context.Context, db *store.DB) (running bool) {
	if r.Job == 0 {
		return false
	}
	j, err := db.GetJob(ctx, r.Job)
	switch {
	case errors.Is(err, store.ErrNotFound):
		r.fail(time.Now(), fmt.Sprintf("job %d is gone", r.Job))
	case err != nil:
		// Not known now: rather no second job beside one that may run.
		return true
	case j.Status == store.JobQueued || j.Status == store.JobRunning:
		return true
	case j.Status == store.JobDone:
		r.Job = 0
	default:
		at := time.Now()
		if j.FinishedAt != nil {
			at = *j.FinishedAt
		}
		msg := j.Error
		if msg == "" {
			msg = "job " + j.Status
		}
		r.fail(at, fmt.Sprintf("job %d: %s", j.ID, msg))
	}
	return false
}

func (r *retry) fail(at time.Time, msg string) {
	r.Job = 0
	r.Tries++
	r.Error = msg
	r.Next = at.Add(retryDelay(r.Tries))
}

// due tells whether a job may be queued now. A next try further away than
// the longest wait is one a clock set wrong at that failure put there — a
// host that booted with its clock years ahead, set right by NTP later — and
// counts as due: it would otherwise wait those years.
func (r *retry) due(now time.Time) bool {
	return r.Job == 0 && (!now.Before(r.Next) || r.Next.Sub(now) > retryMax)
}

// describe is the state of the move in a few words, for the doctor, with
// the job queued last looked at (on a copy: the doctor changes nothing).
func (r retry) describe(ctx context.Context, db *store.DB) string {
	r.settle(ctx, db)
	switch {
	case r.Job != 0:
		return fmt.Sprintf("queued or running as job %d", r.Job)
	case r.Tries > 0:
		return fmt.Sprintf("failed %d time(s), last %s; next try after %s", r.Tries, oneLine(r.Error, 240), r.Next.UTC().Format("2006-01-02 15:04 MST"))
	}
	return "not tried yet"
}

// oneLine is s on one line, cut at about n bytes (never inside a character).
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n] + "…"
	}
	return s
}

// oneLineEnds is s on one line, cut to about n bytes by leaving out its
// middle: a third from its start, the rest from its end. A package
// manager's output says what went wrong last (E: …, dpkg: error processing
// …), after pages of what went well, and the panel's note on what it did
// then follows it; the start says what failed.
func oneLineEnds(s string, n int) string {
	s = oneLine(s, len(s))
	if len(s) <= n {
		return s
	}
	from := len(s) - (n - n/3)
	for from < len(s) && !utf8.RuneStart(s[from]) {
		from++
	}
	return oneLine(s, n/3) + " " + s[from:]
}

// settingSiteMoves holds the sites that have not moved (siteMoves), as JSON.
const settingSiteMoves = "sites.moves"

// siteMoves are the sites whose live configuration an older panel rendered:
// their servers write the logs into the account's data/logs — nginx, Apache
// and the php-fpm master open them there as root, in a directory the account
// owns — and in Apache mode the shared Apache of 0.8.19 serves them. The
// list is made once, at the first start of this version (and again should
// log_dir change), of every site that had such a configuration; a site
// leaves it with the first site.apply that succeeds, whoever queued it
// (applySite). A site made since was never rendered that way and never
// enters it.
type siteMoves struct {
	// Logs is the log tree the sites move to (siteLogsRoot); empty until the
	// list was made.
	Logs string `json:"logs"`
	// Sites are those that have not moved, by ID.
	Sites map[int64]*siteMove `json:"sites,omitempty"`
	// Webmail is the webmail published on a port, while its server block
	// has not moved (mail.apply).
	Webmail *retry `json:"webmail,omitempty"`
}

// siteMove is one site that has not moved.
type siteMove struct {
	Domain string `json:"domain"`
	// Made: the move made the site's logs in the tree (moveSiteLogs, before
	// it queued the site). The jails list them from then on, empty as they
	// stay until the site moves, so that the move itself leaves the jails as
	// they are: were each site to join them as it moves, every move would
	// restart fail2ban, and a busy host's moves, one site after another,
	// would restart it more often than systemd lets a unit start — which
	// leaves it stopped, and the panel restarts it only when the jails
	// change again.
	Made bool `json:"made,omitempty"`
	retry
}

// siteMovesState reads the moves. What cannot be read counts as a list not
// made yet: it is made anew, and applying a site once more does no harm.
func (s *Server) siteMovesState(ctx context.Context) *siteMoves {
	m := &siteMoves{}
	raw, err := s.db.GetSetting(ctx, settingSiteMoves)
	if err != nil || raw == "" {
		return m
	}
	if err := json.Unmarshal([]byte(raw), m); err != nil {
		s.log.Warn("site moves: unreadable, made anew", "setting", settingSiteMoves, "err", err)
		return &siteMoves{}
	}
	return m
}

// changeSiteMoves changes the moves under movesMu and stores them when fn
// says it changed them.
func (s *Server) changeSiteMoves(ctx context.Context, fn func(m *siteMoves) bool) error {
	s.movesMu.Lock()
	defer s.movesMu.Unlock()
	m := s.siteMovesState(ctx)
	if !fn(m) {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.db.SetSetting(context.WithoutCancel(ctx), settingSiteMoves, string(b))
}

// siteMoved takes a site out of the moves once a site.apply rendered its
// configuration, logs in the tree root and, in Apache mode, its account's
// Apache, and tells whether the site was there.
func (s *Server) siteMoved(ctx context.Context, id int64, root string) bool {
	moved := false
	if err := s.changeSiteMoves(ctx, func(m *siteMoves) bool {
		if _, ok := m.Sites[id]; !ok || m.Logs != root {
			return false
		}
		delete(m.Sites, id)
		moved = true
		return true
	}); err != nil {
		s.log.Warn("site moves: the site moved, but this is not remembered; it is applied once more", "site", id, "err", err)
	}
	return moved
}

// webmailMoved takes the webmail on its port out of the moves — its server
// block logs into the tree, or there is none any more — and tells whether it
// was there.
func (s *Server) webmailMoved(ctx context.Context) bool {
	moved := false
	if err := s.changeSiteMoves(ctx, func(m *siteMoves) bool {
		if m.Webmail == nil {
			return false
		}
		m.Webmail, moved = nil, true
		return true
	}); err != nil {
		s.log.Warn("site moves: the webmail moved, but this is not remembered", "err", err)
	}
	return moved
}

// siteUnmoved tells whether the site's live configuration may still be an
// older panel's: listed in the moves, or no list made yet.
func (s *Server) siteUnmoved(ctx context.Context, id int64) bool {
	m := s.siteMovesState(ctx)
	if m.Logs == "" {
		return true
	}
	_, ok := m.Sites[id]
	return ok
}

// siteLogsPending tells whether the site still logs into its account's
// data/logs: listed among the moves once the list was made for the log tree
// it has (siteUnmoved counts a list not made yet as well, where the site
// logs as it did, for the moves; the clients' log tail reads the tree then).
func (s *Server) siteLogsPending(ctx context.Context, id int64) bool {
	m := s.siteMovesState(ctx)
	_, ok := m.Sites[id]
	return ok && m.Logs == s.siteLogsRoot()
}

// lockSite takes a site for one apply and returns the release. Two jobs of
// one site never run at once (they share its job lock), but the move of the
// sites to their accounts' Apaches applies them inside a job of its own: the
// lock keeps it and a site.apply of the same site from rendering and
// applying side by side, each from what it read of the site. The jobs that
// change a site under another key hold it as well, each for its step on
// the site: the removal of its configuration (user.delete for each of the
// account's sites, removeSiteConfig), sites.move-ip (moveSiteIP) and the
// webmail's work in its tree. So does a CMS install, from its docroot to
// its record (jobSiteCMS), so that its account's removal waits for it, and
// the one request that writes into the site's nginx directory itself, PUT
// /sites/{domain}/nginx, for its write. A job waiting for it holds a worker
// (that request, its answer): it is never held across a longer run than
// that install's (a restore).
func (s *Server) lockSite(id int64) func() {
	m, _ := s.siteLocks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// siteLogsLabelled tells whether the sites may switch to the log tree. On a
// host with SELinux (enforcing or permissive) the tree's label rule comes
// first (refreshSELinuxSiteLogs): nginx and php-fpm run in httpd_t and cannot
// open the var_log_t a directory made under /var/log gets without it, and a
// php-fpm reload that cannot open its logs takes the branch's sites down.
func (s *Server) siteLogsLabelled(ctx context.Context) bool {
	if s.profile.MAC() != "selinux" || !s.selinuxStatus().Supported {
		return true
	}
	v, _ := s.db.GetSetting(ctx, settingSELinuxSiteLogs)
	return v == s.selinuxSiteLogsContext()[0]
}

// siteLogsCheck is the doctor's word on the move of the site logs: the sites
// that still log into their accounts' data/logs, and why. The rotation and
// fail2ban do not cover those logs, and the agent would not let them: root
// rotating or reading files in a directory the account controls is what the
// move ends (it refuses logrotate stanzas outside the tree, and fail2ban
// log paths outside /var/log and the tree). The cure is the move itself.
func (s *Server) siteLogsCheck(ctx context.Context) *apitypes.Check {
	// The generic profile has no start-up refresh, and so no move.
	if s.profile.Family() == osprofile.FamilyUnknown {
		return nil
	}
	root := s.siteLogsRoot()
	m := s.siteMovesState(ctx)
	if m.Logs != root {
		sites, err := s.db.ListSites(ctx, 0)
		if err != nil || len(sites) == 0 {
			return nil
		}
		c := check("site logs", "warn", "the move of the sites' logs to "+root+" has not run yet: the start-up refresh runs it once the agent answers (journalctl -u monopanel-api)")
		return &c
	}
	if len(m.Sites) == 0 && m.Webmail == nil {
		c := check("site logs", "ok", "every site logs into "+root)
		return &c
	}
	labelled := s.siteLogsLabelled(ctx)
	ids := make([]int64, 0, len(m.Sites))
	for id := range m.Sites {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b int64) int { return strings.Compare(m.Sites[a].Domain, m.Sites[b].Domain) })
	var parts []string
	for i, id := range ids {
		if i == 10 {
			parts = append(parts, fmt.Sprintf("and %d more", len(ids)-10))
			break
		}
		mv := m.Sites[id]
		why := mv.describe(ctx, s.db)
		if !labelled {
			why = "waits for the SELinux label rule of " + root
		}
		parts = append(parts, mv.Domain+" ("+why+")")
	}
	if m.Webmail != nil {
		why := m.Webmail.describe(ctx, s.db)
		if !labelled {
			why = "waits for the SELinux label rule of " + root
		}
		parts = append(parts, "the webmail on its port ("+why+")")
	}
	n := len(ids)
	if m.Webmail != nil {
		n++
	}
	c := check("site logs", "warn", fmt.Sprintf("still logging into their accounts' data/logs as an older panel set them up (%d): %s — root opens those logs in a directory the account owns, and neither the rotation nor fail2ban covers them. Fix the cause and run mp site apply <domain>; the panel also tries again by itself", n, strings.Join(parts, "; ")))
	return &c
}
