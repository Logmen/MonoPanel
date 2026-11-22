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
	"monopanel/internal/auth"
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
// the removal is refused: the restore goes on. Its start check waits for the
// removal meanwhile (restoreMu) instead of passing between the removal's
// look and a mark: a restore the look had missed would be refused there.
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
	proceed := sync.OnceFunc(func() { close(goOn) })
	t.Cleanup(func() {
		proceed()
		f.s.restoresRead = nil
	})
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
	// runner claims the restore.
	release()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		j, err := f.db.GetJob(f.ctx, ref.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status == store.JobRunning {
			break
		}
		if j.Status != store.JobQueued || time.Now().After(deadline) {
			t.Fatalf("the restore was not claimed: %s %s", j.Status, j.Error)
		}
	}
	// Its start check waits for the removal, between its look and its mark.
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		select {
		case <-atRestic:
			t.Fatal("the restore got past its start check while the removal of the account was between its look for restores and its mark")
		default:
		}
	}
	proceed()
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
	select {
	case <-atRestic:
	case <-time.After(20 * time.Second):
		t.Fatal("the restore did not start once the removal was refused")
	}
	unblock()
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("the restore: %s %s", job.Status, job.Error)
	}
}

// A restore asked for while the removal of an account or a site is between
// its look for in-place restores into the home or the tree and its mark —
// here from inside that look (restoresRead): between its two reads, or right
// after them — waits for the mark (restoreMu), then is refused, and the
// removal goes ahead. Never both: without the lock the restore, checked
// before the mark, was queued as well, to write into a home or a tree whose
// removal had started.
func TestRestoreAndRemovalNeverBothPass(t *testing.T) {
	for _, c := range []struct {
		name, remove, include, refusal string
		// at is the read of the removal's look the restore is asked for
		// after: 1 the queued restores, 2 the running ones.
		at int
	}{
		{"a site, between the reads", "/sites/a.example.com", "/var/www/alex/data/www/a.example.com/public_html",
			"site a.example.com is being removed: an in-place restore of /var/www/alex/data/www/a.example.com/public_html does not write into its tree", 1},
		{"an account, after the reads", "/users/alex", "/var/www/alex/data/www",
			"the account alex is being removed: an in-place restore of /var/www/alex/data/www does not write into its home", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _, _, hold := restoreFixture(t)
			// A restore that gets queued stays queued: the removal's read
			// of the running restores does not find it.
			release := hold()
			defer release()
			// Nor does the removal's job get far before the restore is
			// answered — it acts through the agent first, and the agent
			// waits: the restore is checked against a removal under way,
			// not one over and gone.
			answered := make(chan struct{})
			free := sync.OnceFunc(func() { close(answered) })
			f.agent.SetBefore(func(string, []byte) { <-answered })
			t.Cleanup(func() {
				free()
				f.agent.SetBefore(nil)
			})
			type answer struct {
				status int
				body   string
				err    error
			}
			asked := make(chan answer, 1)
			waited := make(chan bool, 1)
			reads := 0
			f.s.restoresRead = func() {
				if reads++; reads != c.at {
					return
				}
				go func() {
					status, body, err := f.send(http.MethodPost, "/backups/restore", map[string]any{"target": "local", "snapshot": "latest", "include": []string{c.include}, "in_place": true})
					asked <- answer{status, body, err}
				}()
				// A restore past restoreMu answers in milliseconds; one
				// that has not answered in 300 ms waits for the removal.
				select {
				case a := <-asked:
					asked <- a
					waited <- false
				case <-time.After(300 * time.Millisecond):
					waited <- true
				}
			}
			status, raw := f.do(http.MethodDelete, c.remove, nil)
			var a answer
			select {
			case a = <-asked:
			case <-time.After(20 * time.Second):
				t.Fatal("the restore did not answer")
			}
			free()
			if a.err != nil {
				t.Fatal(a.err)
			}
			var w bool
			select {
			case w = <-waited:
			default:
				t.Fatalf("DELETE %s did not look for restores: %d %s", c.remove, status, raw)
			}
			if status == http.StatusAccepted && a.status == http.StatusAccepted {
				t.Fatalf("both went through: DELETE %s %d, and the in-place restore of %s asked for before its mark %d %s", c.remove, status, c.include, a.status, a.body)
			}
			if status != http.StatusAccepted || !w || a.status != http.StatusConflict || !strings.Contains(a.body, c.refusal) {
				t.Fatalf("DELETE %s: %d %s; the restore (waited for the mark: %v): %d %s", c.remove, status, raw, w, a.status, a.body)
			}
			if list, err := f.db.ListJobsFiltered(f.ctx, store.JobFilter{Type: "backup.restore"}); err != nil || len(list) != 0 {
				t.Fatalf("restores queued: %d %v", len(list), err)
			}
			var ref apitypes.JobRef
			if err := json.Unmarshal(raw, &ref); err != nil {
				t.Fatal(err)
			}
			if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
				t.Fatalf("the removal: %s %s\n%s", job.Status, job.Error, job.Log)
			}
		})
	}
}

// callGate holds every agent call that hits it until open; reached is
// closed at the first.
type callGate struct {
	reached, release chan struct{}
	first, rel       sync.Once
}

func newCallGate() *callGate {
	return &callGate{reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *callGate) hit() {
	g.first.Do(func() { close(g.reached) })
	<-g.release
}

func (g *callGate) open() { g.rel.Do(func() { close(g.release) }) }

func (g *callGate) wait(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s: not reached", what)
	}
}

// removalHeld holds a user.delete of alex at its first agent call, the
// removal of a.example.com's configuration; extra sees every agent call
// too.
func (f *siteFixture) removalHeld(extra func(p string, body []byte)) *callGate {
	rm := newCallGate()
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/paths/remove" && strings.Contains(string(body), "a.example.com.conf") {
			rm.hit()
		}
		if extra != nil {
			extra(p, body)
		}
	})
	f.t.Cleanup(func() {
		rm.open()
		f.agent.SetBefore(nil)
	})
	return rm
}

// keepsOut checks, while the removal of alex is under way, that alex is
// still being removed and an in-place restore into the home is refused.
func (f *siteFixture) keepsOut(t *testing.T, how string) {
	t.Helper()
	if u, _ := f.db.GetUserByLogin(f.ctx, "alex"); u.Status != store.UserDeleting {
		t.Errorf("%s: alex is %s during the removal", how, u.Status)
	}
	runs := f.resticRestores()
	status, body := f.restore(true, "/var/www/alex/data/www")
	if status != http.StatusConflict || !strings.Contains(body, "the account alex is being removed") {
		t.Errorf("%s: the in-place restore into the home during the removal: %d %s", how, status, body)
		var ref apitypes.JobRef
		if json.Unmarshal([]byte(body), &ref) == nil && ref.JobID != 0 {
			f.waitJob(ref.JobID)
		}
	}
	if n := f.resticRestores() - runs; n > 0 {
		t.Errorf("%s: restic restored %d times during the removal", how, n)
	}
}

// A user.provision of alex queued before DELETE /users/alex — a change of
// the shell — runs first, under the same key: it provisions nothing for an
// account being removed and leaves the mark, so an in-place restore into
// the home stays refused while the removal runs. One that runs after the
// removal, the account gone, provisions nothing either.
func TestAccountRemovalMarkSurvivesAProvision(t *testing.T) {
	f, alex, _, _ := restoreFixture(t)
	release := f.holdKey("user:alex")
	var prov apitypes.UserWithJob
	f.call(http.MethodPatch, "/users/alex", map[string]any{"shell": false}, http.StatusAccepted, &prov)
	if prov.JobID == 0 {
		t.Fatal("no user.provision queued")
	}
	rm := f.removalHeld(nil)
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex?purge=true", nil, http.StatusAccepted, &del)
	release()
	if j := f.waitJob(prov.JobID); j.Status != store.JobDone || j.Message != "the account alex is being removed: nothing to provision" {
		t.Fatalf("the provision: %s %s\n%s", j.Status, j.Error, j.Log)
	}
	rm.wait(t, "the user.delete")
	f.keepsOut(t, "a user.provision queued before the removal")
	rm.open()
	if j := f.waitJob(del.JobID); j.Status != store.JobDone {
		t.Fatalf("user.delete: %s %s", j.Status, j.Error)
	}
	ensures := 0
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/user/ensure" && strings.Contains(string(c.Body), `"login":"alex"`) {
			ensures++
		}
	}
	late, err := f.s.jobs.Enqueue(f.ctx, "user.provision", userProvisionPayload{UserID: alex.ID, Login: "alex", Shell: true}, jobs.WithLockKey("user:alex"))
	if err != nil {
		t.Fatal(err)
	}
	if j := f.waitJob(late.ID); j.Status != store.JobDone || j.Message != "the account is gone: nothing to provision" {
		t.Fatalf("a provision after the removal: %s %s\n%s", j.Status, j.Error, j.Log)
	}
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/user/ensure" && strings.Contains(string(c.Body), `"login":"alex"`) {
			ensures--
		}
	}
	if ensures != 0 {
		t.Fatal("the unix user of a removed account was made again")
	}
}

// PATCH /users/alex — a password: bcrypt between its read and its write —
// read alex before DELETE /users/alex marked it: its write does not put the
// status it read back over the mark, and it says the account is being
// removed. The refusal changed nothing: not the panel password, not the
// unix one, not the account's sessions.
func TestAccountRemovalMarkSurvivesAStaleProfileWrite(t *testing.T) {
	f, _, _, _ := restoreFixture(t)
	alex := f.alexWithPassword()
	if err := f.db.CreateSession(f.ctx, &store.Session{ID: "alex-session", UserID: alex.ID, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	rm := f.removalHeld(nil)
	deleted := make(chan sent, 1)
	var once sync.Once
	f.s.beforeChangeWrite = func() {
		once.Do(func() { f.sendBeside(deleted, http.MethodDelete, "/users/alex?purge=true", nil) })
	}
	t.Cleanup(func() { f.s.beforeChangeWrite = nil })
	n := len(f.agent.Calls())
	if status, raw := f.do(http.MethodPatch, "/users/alex", map[string]any{"password": "another-password-1"}); status != http.StatusConflict || !strings.Contains(string(raw), "the account alex is being removed") {
		t.Errorf("the PATCH: %d %s", status, raw)
	}
	d := answerOf(t, deleted)
	var del apitypes.JobRef
	if err := json.Unmarshal([]byte(d.body), &del); d.err != nil || d.status != http.StatusAccepted || err != nil {
		t.Fatalf("DELETE /users/alex inside the PATCH: %d %v %s", d.status, d.err, d.body)
	}
	rm.wait(t, "the user.delete")
	got, _ := f.db.GetUserByLogin(f.ctx, "alex")
	if same, _ := auth.VerifyPassword("alex-password", got.PasswordHash); !same {
		t.Error("the refused PATCH changed the panel password")
	}
	for _, c := range f.agent.Calls()[n:] {
		if c.Path == "/v1/user/password" {
			t.Errorf("the refused PATCH set the unix password: %s", c.Body)
		}
	}
	if _, err := f.db.GetSession(f.ctx, "alex-session"); err != nil {
		t.Errorf("the refused PATCH ended alex's session: %v", err)
	}
	f.keepsOut(t, "a PATCH /users/alex that read the account before the mark")
	rm.open()
	if j := f.waitJob(del.JobID); j.Status != store.JobDone {
		t.Fatalf("user.delete: %s %s", j.Status, j.Error)
	}
}

// An account being removed takes no change of its profile: a status would
// put the mark away, a shell would queue a provision that makes the unix
// user again after the removal.
func TestChangeOfAnAccountBeingRemovedIsRefused(t *testing.T) {
	f, _, _, _ := restoreFixture(t)
	rm := f.removalHeld(nil)
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex?purge=true", nil, http.StatusAccepted, &del)
	rm.wait(t, "the user.delete")
	n := len(f.agent.Calls())
	for _, body := range []map[string]any{{"status": "suspended"}, {"status": "active"}, {"shell": false}, {"email": "alex@example.org"}, {"password": "another-password-1"}} {
		if status, raw := f.do(http.MethodPatch, "/users/alex", body); status != http.StatusConflict || !strings.Contains(string(raw), "the account alex is being removed") {
			t.Errorf("PATCH %v during the removal: %d %s", body, status, raw)
		}
	}
	if list, _ := f.db.ListJobsFiltered(f.ctx, store.JobFilter{Type: "user.provision"}); len(list) != 0 {
		t.Errorf("provisions queued: %d", len(list))
	}
	for _, c := range f.agent.Calls()[n:] {
		if c.Path == "/v1/user/password" {
			t.Errorf("the password of an account being removed was changed: %s", c.Body)
		}
	}
	f.keepsOut(t, "PATCH /users/alex during the removal")
	rm.open()
	if j := f.waitJob(del.JobID); j.Status != store.JobDone {
		t.Fatalf("user.delete: %s %s", j.Status, j.Error)
	}
}

// An account whose unix user is not made yet — its provision is still to
// run, and may make the home before the removal purges it — is not removed
// while an in-place restore into that home is queued; and a restore into the
// home of such an account being removed is refused, asked for or starting.
func TestRemovalOfAnAccountNotProvisionedYetWaitsForARestore(t *testing.T) {
	f, _, _, hold := restoreFixture(t)
	f.holdKey("user:carol")
	var created apitypes.UserWithJob
	f.call(http.MethodPost, "/users", map[string]any{"login": "carol", "password": "carol-password-1"}, http.StatusAccepted, &created)
	release := hold()
	defer release()
	var ref apitypes.JobRef
	f.call(http.MethodPost, "/backups/restore", map[string]any{"target": "local", "snapshot": "latest", "include": []string{"/var/www/carol"}, "in_place": true}, http.StatusAccepted, &ref)
	if status, raw := f.do(http.MethodDelete, "/users/carol?purge=true", nil); status != http.StatusConflict || !strings.Contains(string(raw), "an in-place restore into /var/www/carol is queued") {
		t.Fatalf("DELETE /users/carol beside a restore into its home: %d %s", status, raw)
	}
	carol, err := f.db.GetUserByLogin(f.ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if carol.Status == store.UserDeleting || carol.UnixUID != nil {
		t.Fatalf("carol: %s, unix uid %v", carol.Status, carol.UnixUID)
	}
	// Marked now, as by a removal asked for earlier: the restore is refused
	// as it starts, and again when asked for.
	if err := f.db.SetUserStatus(f.ctx, carol.ID, store.UserDeleting); err != nil {
		t.Fatal(err)
	}
	runs := f.resticRestores()
	release()
	if j := f.waitJob(ref.JobID); j.Status != store.JobFailed || !strings.Contains(j.Error, "the account carol is being removed") {
		t.Fatalf("the restore into the home of carol being removed: %s %s", j.Status, j.Error)
	}
	if n := f.resticRestores() - runs; n > 0 {
		t.Fatalf("restic restored %d times", n)
	}
	if status, body := f.restore(true, "/var/www/carol/data"); status != http.StatusConflict || !strings.Contains(body, "the account carol is being removed") {
		t.Fatalf("a restore into the home of carol being removed: %d %s", status, body)
	}
}

// overlapping: an include at, below or above the directory, after cleaning;
// a shared prefix of names is not containment.
func TestOverlappingPaths(t *testing.T) {
	for _, c := range []struct {
		inc, dir string
		want     bool
	}{
		{"/var/www/alex", "/var/www/alex", true},
		{"/var/www/alex/", "/var/www/alex", true},
		{"/var/www/alex", "/var/www/alex/", true},
		{"/var/www/alex/data/www/a.example.com", "/var/www/alex", true},
		{"/var/www", "/var/www/alex", true},
		{"/var", "/var/www/alex/data/www/a.example.com", true},
		{"/", "/var/www/alex", true},
		{"/var/www/alex", "/", true},
		{"//var//www//alex//x", "/var/www/alex", true},
		{"/var/www/alex/./data", "/var/www/alex", true},
		{"/var/www/alex/../bob", "/var/www/alex", false},
		{"/var/www/bob/../alex/data", "/var/www/alex", true},
		{"/var/www/alex/..", "/var/www/alex", true},
		{"/var/www/alexandra", "/var/www/alex", false},
		{"/var/www/alex2", "/var/www/alex", false},
		{"/var/www/al", "/var/www/alex", false},
		{"/var/www/alex/data/www/a.example.com.bak", "/var/www/alex/data/www/a.example.com", false},
		{"/var/www/alex/data/www/b.example.com", "/var/www/alex/data/www/a.example.com", false},
		{"/etc/monopanel", "/var/www/alex", false},
	} {
		if got := overlapping([]string{c.inc}, c.dir) != ""; got != c.want {
			t.Errorf("overlapping(%q, %q) = %v, want %v", c.inc, c.dir, got, c.want)
		}
	}
	if got := overlapping([]string{"/srv/x", "/var/www/alex/y"}, "/var/www/alex"); got != "/var/www/alex/y" {
		t.Errorf("the second include: %q", got)
	}
}
