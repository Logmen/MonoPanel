package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/agent/confcheck"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

// nginx alone talks to an account's Apache. Any local process can connect to
// the Apache's address, and so would skip what nginx enforces for the
// account's sites — their allow lists (mp site add --allow), their rate
// limits — and name the client address itself, which mod_remoteip takes
// from 127.0.0.1, where every local connection comes from. Each account's
// Apache therefore has a secret, 32 random bytes the panel makes when the
// Apache is first set up and keeps in its database, encrypted like the
// other secrets of the panel: nginx sends it with every request
// (/etc/nginx/monopanel/apache/<login>.conf), the Apache refuses whatever
// does not carry it (backend.conf, which its httpd.conf includes). Both
// files are root's, 0600, and hold exactly what confcheck allows. An account
// that learns its own secret — its .htaccess can copy request headers into
// a response — gains nothing: the secret opens that account's Apache alone.
//
// The secret goes to nginx before the Apache demands it, never the other way
// round: an Apache that demanded a secret nginx did not send yet would answer
// the account's sites with 403 until nginx's reload. A new Apache demands it
// from its first start, before nginx is sent there at all. One set up before
// the secret (an httpd.conf, no backend.conf) is "pending": while it runs, a
// site's apply writes only its virtual host there, and nginx sends the
// secret from that apply on; once every site of the account went through,
// the Apache demands it (demandApacheBackends: the start-up job
// apache.backend, mp stack install apache). A site is down for no reload. A
// pending Apache that does not run gets its check with the first site
// applied, which starts it — it would start at the next boot otherwise,
// enabled, with an httpd.conf that lets everything in.

const (
	// settingApacheBackends is "yes" once no account's Apache runs without
	// the check (jobApacheBackend).
	settingApacheBackends = "stack.apache.backends"
	// apacheBackendKey prefixes the setting that keeps an account's secret,
	// by user id: a later account of the same login gets a secret of its own.
	apacheBackendKey = "apache.backend."
)

func apacheBackendSetting(u *store.User) string {
	return apacheBackendKey + strconv.FormatInt(u.ID, 10)
}

// apacheSecret is the backend secret of the account's Apache. With create,
// an account that has none — or one the secret key no longer opens — gets
// a new one; without, it is "" then. A caller that creates holds lockApache:
// two jobs of the account's sites would otherwise make two.
func (s *Server) apacheSecret(ctx context.Context, u *store.User, create bool) (string, error) {
	if s.secrets == nil {
		return "", fmt.Errorf("the secret key %s is unavailable: the backend secret of the Apache of %s is kept encrypted with it", s.cfg.SecretKeyFile, u.Login)
	}
	enc, err := s.db.GetSetting(ctx, apacheBackendSetting(u))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if err == nil {
		if secret, err := s.secrets.Decrypt(enc); err == nil && confcheck.ValidApacheBackendSecret(secret) {
			return secret, nil
		}
		s.log.Warn("apache: the backend secret of an account cannot be read; it gets a new one", "login", u.Login)
	}
	if !create {
		return "", nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(b)
	if enc, err = s.secrets.Encrypt(secret); err != nil {
		return "", err
	}
	if err := s.db.SetSetting(context.WithoutCancel(ctx), apacheBackendSetting(u), enc); err != nil {
		return "", err
	}
	return secret, nil
}

// apacheBackendFile is the check of the account's Apache (the secret in
// inst), root's alone.
func apacheBackendFile(inst *apacheInstance) agent.FileSpec {
	return agent.FileSpec{Path: inst.backend, Content: confcheck.ApacheBackendConf(inst.secret), Mode: 0o600, Owner: "root", Group: "root"}
}

// nginxBackendFile is the header nginx sends the account's Apache, root's
// alone: nginx's master reads it, as root.
func nginxBackendFile(login, secret string) agent.FileSpec {
	return agent.FileSpec{Path: confcheck.NginxApacheBackendFile(login), Content: confcheck.NginxApacheBackendConf(secret), Mode: 0o600, Owner: "root", Group: "root"}
}

// nginxBackendInclude is how a site's server block names the account's
// file: relative to nginx's directory, as its other includes.
func (s *Server) nginxBackendInclude(login string) string {
	return strings.TrimPrefix(confcheck.NginxApacheBackendFile(login), s.profile.Web().NginxConfDir+"/")
}

// apacheDemands tells whether the account's Apache has its check: the panel
// writes backend.conf with the httpd.conf that includes it, and that
// httpd.conf does not start without it.
func (s *Server) apacheDemands(ctx context.Context, inst *apacheInstance) (bool, error) {
	st, err := s.agent.Stat(ctx, inst.backend)
	if err != nil {
		return false, err
	}
	return len(st.Entries) == 1 && st.Entries[0].Exists, nil
}

// apacheBackendPending tells whether the account's Apache was set up before
// the backend secret: an httpd.conf, and no check next to it. Running, it
// lets in whatever comes; stopped, it would at its next start (it stays
// enabled, and boots).
func (s *Server) apacheBackendPending(ctx context.Context, inst *apacheInstance) (bool, error) {
	st, err := s.agent.Stat(ctx, inst.conf, inst.backend)
	if err != nil {
		return false, err
	}
	if len(st.Entries) != 2 {
		return false, fmt.Errorf("the agent described %d of the files %s and %s", len(st.Entries), inst.conf, inst.backend)
	}
	return st.Entries[0].Exists && !st.Entries[1].Exists, nil
}

// demandApacheBackends makes the accounts' Apaches set up before the backend
// secret demand it: each account's sites in Apache mode are applied again
// first — nginx sends the secret from then on, which a running Apache does
// not ask for yet — and only when every one of them went through does the
// Apache demand it. One that does not run gets its check, and starts, with
// the first of its sites. done are the sites applied in this run already.
func (s *Server) demandApacheBackends(ctx context.Context, jc *jobs.Context, done map[int64]bool) error {
	users, err := s.apacheAccounts(ctx)
	if err != nil {
		return err
	}
	var failed []string
	for _, u := range users {
		pending, err := s.apacheBackendPending(ctx, apacheInstanceOf(u.Login))
		if err != nil {
			failed = append(failed, u.Login+": "+err.Error())
			continue
		}
		if !pending {
			continue
		}
		sites, err := s.apacheSitesOf(ctx, u.ID, 0)
		if err != nil {
			return err
		}
		ok := true
		for _, site := range sites {
			if done[site.ID] {
				continue
			}
			jc.Logf("%s: nginx sends the backend secret of the Apache of %s", site.Domain, u.Login)
			if err := s.applySite(ctx, jc, sitePayload{SiteID: site.ID}); err != nil {
				failed = append(failed, site.Domain+": "+err.Error())
				ok = false
			}
		}
		if !ok {
			continue
		}
		if err := s.demandApacheBackend(ctx, jc.Logf, u); err != nil {
			failed = append(failed, u.Login+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("accounts' Apaches that still answer any local process past nginx: %s; fix them, then run mp stack install apache", strings.Join(failed, "; "))
	}
	return nil
}

// demandApacheBackend gives an Apache set up before the backend secret its
// check — httpd.conf with the include and backend.conf, checked with
// apache2 -t, a graceful reload — once nginx sends the secret for every site
// of the account (the caller applied them).
func (s *Server) demandApacheBackend(ctx context.Context, logf func(string, ...any), u *store.User) error {
	defer s.lockApache(u.Login)()
	inst, err := s.apacheInstanceFor(u)
	if err != nil {
		return err
	}
	// Under the lock again: a site's apply may have started it meanwhile.
	if pending, err := s.apacheBackendPending(ctx, inst); err != nil || !pending {
		return err
	}
	if inst.secret, err = s.apacheSecret(ctx, u, false); err != nil {
		return err
	}
	if inst.secret == "" {
		return errors.New("its sites were not applied with the backend secret yet")
	}
	files, err := s.apacheMainFiles(inst, u)
	if err != nil {
		return err
	}
	if err := s.ensureApacheRunDir(ctx, inst); err != nil {
		return err
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		Files: append(files, apacheBackendFile(inst)), Validate: [][]string{inst.check}, Reload: []string{inst.unit}, Origin: "apache:" + u.Login,
	}); err != nil {
		return err
	}
	logf("Apache of %s: answers what nginx sends alone from now on (%s demands the backend secret)", u.Login, inst.unit)
	return nil
}

// dropApacheBackend takes the backend secret of an account whose Apache
// retired away: nginx's file — checked with nginx -t, so a site's server
// block that still includes it (one whose apply failed half-way) keeps it,
// and the secret, rather than break nginx's next reload — and then the
// secret itself. The caller holds lockApache.
func (s *Server) dropApacheBackend(ctx context.Context, logf func(string, ...any), u *store.User) {
	file := confcheck.NginxApacheBackendFile(u.Login)
	st, err := s.agent.Stat(ctx, file)
	if err != nil {
		logf("warning: the backend secret of the Apache of %s: %v", u.Login, err)
		return
	}
	if len(st.Entries) == 1 && st.Entries[0].Exists {
		if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{file}, Validate: [][]string{s.profile.Web().NginxCheckArgv}}); err != nil {
			if secret, _ := s.apacheSecret(ctx, u, false); secret != "" {
				if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{nginxBackendFile(u.Login, secret)}, Origin: "apache:" + u.Login}); err != nil {
					logf("warning: %s could not be put back: %v", file, err)
				}
			}
			logf("warning: %s stays, nginx -t fails without it (a site's server block still includes it?): %v", file, err)
			return
		}
		logf("Apache of %s: nginx's backend secret removed (%s)", u.Login, file)
	}
	if err := s.db.DeleteSetting(context.WithoutCancel(ctx), apacheBackendSetting(u)); err != nil {
		logf("warning: the backend secret of the Apache of %s stays in the database: %v", u.Login, err)
	}
}

// probeApache asks the Apache something, carrying the account's secret like
// every request nginx sends: OPTIONS *, which Apache answers itself, before
// any site's configuration or code is involved.
func (s *Server) probeApache(ctx context.Context, inst *apacheInstance) error {
	tr := &http.Transport{DialContext: s.apacheDial, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	cl := &http.Client{Transport: tr, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, "http://"+inst.addr, nil)
	if err != nil {
		return err
	}
	req.URL = &url.URL{Scheme: "http", Host: inst.addr, Opaque: "*"}
	req.Host = "localhost"
	req.Header.Set(confcheck.ApacheBackendHeader, inst.secret)
	res, err := cl.Do(req)
	if err != nil {
		return err
	}
	return res.Body.Close()
}

// apacheBackendDoctor names the accounts' Apaches that run without their
// check.
func (s *Server) apacheBackendDoctor(ctx context.Context) []apitypes.Check {
	users, err := s.apacheAccounts(ctx)
	if err != nil {
		return nil
	}
	var out []apitypes.Check
	for _, u := range users {
		inst := apacheInstanceOf(u.Login)
		if pending, err := s.apacheBackendPending(ctx, inst); err == nil && pending {
			out = append(out, check("backend "+inst.unit, "warn", "it answers any local process on its address (or will once it starts), past what nginx enforces for the sites of "+u.Login+": set up before nginx's backend secret, it does not demand it yet (the next start of the panel or mp stack install apache sets it up)"))
		}
	}
	return out
}

// ---------------------------------------------------------- upgrade ----

// refreshApacheBackends has the accounts' Apaches that ran before the
// backend secret demand it, once: the apache.backend job. A host without
// Apache has none.
func (s *Server) refreshApacheBackends(ctx context.Context) {
	if v, _ := s.db.GetSetting(ctx, settingApacheBackends); v == "yes" {
		return
	}
	if !s.apacheInstalled(ctx) || s.profile.Family() != osprofile.FamilyDebian {
		if err := s.db.SetSetting(ctx, settingApacheBackends, "yes"); err != nil {
			s.log.Warn("apache backends", "err", err)
		}
		return
	}
	if _, err := s.jobs.Enqueue(ctx, "apache.backend", struct{}{}, jobs.WithLockKey("stack"), jobs.WithRequestedBy("system")); err != nil {
		s.log.Warn("apache: cannot queue the backend secrets of the accounts' Apaches; it runs at the next start", "err", err)
	}
}

// jobApacheBackend is refreshApacheBackends' job.
func (s *Server) jobApacheBackend(ctx context.Context, jc *jobs.Context) error {
	if v, _ := s.db.GetSetting(ctx, settingApacheBackends); v == "yes" {
		jc.Progress(100, "the accounts' Apaches demand their backend secret already")
		return nil
	}
	if err := s.demandApacheBackends(ctx, jc, nil); err != nil {
		return err
	}
	if err := s.db.SetSetting(context.WithoutCancel(ctx), settingApacheBackends, "yes"); err != nil {
		return err
	}
	jc.Progress(100, "the accounts' Apaches answer what nginx sends alone")
	return nil
}
