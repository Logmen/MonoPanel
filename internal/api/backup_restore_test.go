package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

// restoreFixture is a panel with a backup target "local", the sites
// a.example.com of alex and b.example.com of bob, and hold: a job under
// the target's lock key that keeps the target's next jobs queued until the
// returned release is called.
func restoreFixture(t *testing.T) (f *siteFixture, alex, bob *store.User, hold func() (release func())) {
	t.Helper()
	f = newSiteFixture(t)
	f.call(http.MethodPost, "/backups/targets", map[string]any{"name": "local", "type": "local", "repository": "/srv/backup"}, http.StatusCreated, nil)
	bob = f.userWithPassword("bob", "bob-password", 1501)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.createSite(map[string]any{"domain": "b.example.com", "user": "bob", "php_version": "8.4", "ssl": "none"})
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan chan struct{}, 1)
	f.s.jobs.Register("test.hold", func(context.Context, *jobs.Context) error {
		<-<-gate
		return nil
	})
	hold = func() func() {
		t.Helper()
		ch := make(chan struct{})
		gate <- ch
		j, err := f.s.jobs.Enqueue(f.ctx, "test.hold", struct{}{}, jobs.WithLockKey("backup:local"))
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			if got, _ := f.db.GetJob(f.ctx, j.ID); got != nil && got.Status == store.JobRunning {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the hold did not start")
			}
			time.Sleep(5 * time.Millisecond)
		}
		var once bool
		return func() {
			if !once {
				once = true
				close(ch)
				f.waitJob(j.ID)
			}
		}
	}
	return f, alex, bob, hold
}

// restore asks for a restore and answers the status and the body.
func (f *siteFixture) restore(inPlace bool, include ...string) (int, string) {
	f.t.Helper()
	status, raw := f.do(http.MethodPost, "/backups/restore", map[string]any{"target": "local", "snapshot": "latest", "include": include, "in_place": inPlace})
	return status, string(raw)
}

// send is do for a goroutine of the test: it fails nothing and answers the
// error instead.
func (f *siteFixture) send(method, path string, body any) (int, string, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, "", err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(f.ctx, method, f.ts.URL+"/api/v1"+path, rdr)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(f.cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	return res.StatusCode, string(raw), err
}

// resticRestores counts the restic restore runs the agent got.
func (f *siteFixture) resticRestores() int {
	n := 0
	for _, tool := range f.agent.Tools() {
		if tool.Name == "restic" && len(tool.Args) > 0 && tool.Args[0] == "restore" {
			n++
		}
	}
	return n
}

// An in-place restore does not write into the home of an account being
// removed nor into the tree of a site being removed — a path above them
// counts, and a restore aside or into someone else's home does not — when it
// is asked for, nor when it starts after waiting for its target's other jobs.
func TestInPlaceRestoreKeepsOutOfWhatIsBeingRemoved(t *testing.T) {
	f, alex, _, hold := restoreFixture(t)
	if err := f.db.SetUserStatus(f.ctx, alex.ID, store.UserDeleting); err != nil {
		t.Fatal(err)
	}
	for _, inc := range []string{"/var/www/alex/data/www/a.example.com", "/var/www/alex", "/var/www/"} {
		status, body := f.restore(true, inc)
		if status != http.StatusConflict || !strings.Contains(body, "the account alex is being removed: an in-place restore of "+inc+" does not write into its home /var/www/alex meanwhile") {
			t.Fatalf("in place into %s while alex goes: %d %s", inc, status, body)
		}
	}
	for _, c := range []struct {
		inPlace bool
		inc     string
	}{{false, "/var/www/alex"}, {true, "/var/www/bob/data/www/b.example.com"}, {true, "/var/www/alexandra"}} {
		var ref apitypes.JobRef
		f.call(http.MethodPost, "/backups/restore", map[string]any{"target": "local", "snapshot": "latest", "include": []string{c.inc}, "in_place": c.inPlace}, http.StatusAccepted, &ref)
		if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
			t.Fatalf("restore of %s (in place %v): %s %s", c.inc, c.inPlace, job.Status, job.Error)
		}
	}
	if err := f.db.SetUserStatus(f.ctx, alex.ID, store.UserActive); err != nil {
		t.Fatal(err)
	}
	a, _ := f.db.GetSiteByDomain(f.ctx, "a.example.com")
	if err := f.db.SetSiteStatus(f.ctx, a.ID, store.SiteDeleting, ""); err != nil {
		t.Fatal(err)
	}
	if status, body := f.restore(true, "/var/www/alex/data/www/a.example.com/public_html"); status != http.StatusConflict || !strings.Contains(body, "site a.example.com is being removed") {
		t.Fatalf("in place into a site being removed: %d %s", status, body)
	}
	var ref apitypes.JobRef
	f.call(http.MethodPost, "/backups/restore", map[string]any{"target": "local", "snapshot": "latest", "include": []string{"/var/www/alex/data/www/other.example.com"}, "in_place": true}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("in place beside a site being removed: %s %s", job.Status, job.Error)
	}
	if err := f.db.SetSiteStatus(f.ctx, a.ID, store.SiteActive, ""); err != nil {
		t.Fatal(err)
	}

	// Asked for while nothing went, started once the account was going.
	release := hold()
	f.call(http.MethodPost, "/backups/restore", map[string]any{"target": "local", "snapshot": "latest", "include": []string{"/var/www/alex"}, "in_place": true}, http.StatusAccepted, &ref)
	if err := f.db.SetUserStatus(f.ctx, alex.ID, store.UserDeleting); err != nil {
		t.Fatal(err)
	}
	runs := f.resticRestores()
	release()
	job := f.waitJob(ref.JobID)
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "the account alex is being removed") {
		t.Fatalf("a restore that starts once the account goes: %s %s", job.Status, job.Error)
	}
	if f.resticRestores() != runs {
		t.Fatal("restic restored into the home of an account being removed")
	}
}

// The removal of an account or a site is refused while an in-place restore
// into its home or tree is queued or runs — not one into another account's
// home — and goes through once the restore is over.
func TestRemovalWaitsForAnInPlaceRestore(t *testing.T) {
	f, _, bob, hold := restoreFixture(t)
	release := hold()
	defer release()
	var ref apitypes.JobRef
	f.call(http.MethodPost, "/backups/restore", map[string]any{"target": "local", "snapshot": "latest", "include": []string{"/var/www/alex/data/www"}, "in_place": true}, http.StatusAccepted, &ref)
	for _, c := range []struct{ path, want string }{
		{"/users/alex", "an in-place restore into /var/www/alex is queued (job "},
		{"/sites/a.example.com", "an in-place restore into /var/www/alex/data/www/a.example.com is queued (job "},
	} {
		status, raw := f.do(http.MethodDelete, c.path, nil)
		if status != http.StatusConflict || !strings.Contains(string(raw), c.want) {
			t.Fatalf("DELETE %s beside the restore: %d %s", c.path, status, raw)
		}
	}
	if u, _ := f.db.GetUserByLogin(f.ctx, "alex"); u.Status == store.UserDeleting {
		t.Fatal("a refused removal marked the account")
	}
	if a, _ := f.db.GetSiteByDomain(f.ctx, "a.example.com"); a.Status == store.SiteDeleting {
		t.Fatal("a refused removal marked the site")
	}
	// bob's home is not restored into.
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/"+bob.Login, nil, http.StatusAccepted, &del)
	if job := f.waitJob(del.JobID); job.Status != store.JobDone {
		t.Fatalf("bob's removal: %s %s", job.Status, job.Error)
	}
	release()
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("the restore: %s %s", job.Status, job.Error)
	}
	f.call(http.MethodDelete, "/sites/a.example.com", nil, http.StatusAccepted, &del)
	if job := f.waitJob(del.JobID); job.Status != store.JobDone {
		t.Fatalf("the site's removal after the restore: %s %s", job.Status, job.Error)
	}
	f.call(http.MethodDelete, "/users/alex", nil, http.StatusAccepted, &del)
	if job := f.waitJob(del.JobID); job.Status != store.JobDone {
		t.Fatalf("the account's removal after the restore: %s %s", job.Status, job.Error)
	}
}

// The removal of an account looks for the in-place restores queued and the
// ones running in two reads. A restore the runner claims between the two —
// queued at the first, running by the second — is found all the same, and
// the removal is refused while it writes into the home.
func TestRemovalFindsARestoreClaimedWhileItLooks(t *testing.T) {
	f, _, _, hold := restoreFixture(t)
	release := hold()
	defer release()
	// restic is held once the restore runs: it stays running.
	atRestic, free := make(chan struct{}), make(chan struct{})
	var held, freed sync.Once
	unblock := func() { freed.Do(func() { close(free) }) }
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/tool" && strings.Contains(string(body), `"name":"restic"`) && strings.Contains(string(body), `"args":["restore"`) {
			held.Do(func() {
				close(atRestic)
				<-free
			})
		}
	})
	t.Cleanup(func() {
		unblock()
		f.agent.SetBefore(nil)
	})
	var ref apitypes.JobRef
	f.call(http.MethodPost, "/backups/restore", map[string]any{"target": "local", "snapshot": "latest", "include": []string{"/var/www/alex/data/www"}, "in_place": true}, http.StatusAccepted, &ref)

	firstRead, goOn := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.s.restoresRead = func() {
		once.Do(func() {
			close(firstRead)
			<-goOn
		})
	}
	t.Cleanup(func() { f.s.restoresRead = nil })
	type answer struct {
		status int
		body   string
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		status, body, err := f.send(http.MethodDelete, "/users/alex", nil)
		answered <- answer{status, body, err}
	}()
	select {
	case <-firstRead:
	case <-time.After(20 * time.Second):
		t.Fatal("the removal did not look for restores")
	}
	// Between the removal's two reads the target's other job ends, and the
	// runner claims the restore, which starts writing.
	release()
	select {
	case <-atRestic:
	case <-time.After(20 * time.Second):
		close(goOn)
		t.Fatal("the restore did not start")
	}
	close(goOn)
	var a answer
	select {
	case a = <-answered:
	case <-time.After(20 * time.Second):
		t.Fatal("DELETE /users/alex did not answer")
	}
	if a.err != nil {
		t.Fatal(a.err)
	}
	if a.status != http.StatusConflict || !strings.Contains(a.body, "an in-place restore into /var/www/alex is ") {
		t.Fatalf("the removal missed the restore the runner claimed while it looked: %d %s", a.status, a.body)
	}
	if u, _ := f.db.GetUserByLogin(f.ctx, "alex"); u.Status == store.UserDeleting {
		t.Fatal("a refused removal marked the account")
	}
	unblock()
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("the restore: %s %s", job.Status, job.Error)
	}
}
