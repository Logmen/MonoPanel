package api

import (
	"strings"
	"testing"
	"time"
)

// A configuration check that hangs (nginx -t waiting for DNS while the
// network is down) takes its own limit, not the doctor's whole budget: it is
// a warning that says why, and the checks after it still run.
func TestDoctorConfigCheckHasItsOwnLimit(t *testing.T) {
	f := newSiteFixture(t)
	f.s.doctorCheckLimit = 200 * time.Millisecond
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/config/apply" && strings.Contains(string(body), `"origin":"doctor"`) && strings.Contains(string(body), "nginx") {
			time.Sleep(time.Second)
		}
	})
	t.Cleanup(func() { f.agent.SetBefore(nil) })
	d := f.s.doctor(f.ctx)
	nginx, later := false, 0
	for _, c := range d.Checks {
		switch {
		case c.Name == "config nginx":
			nginx = c.Status == "warn" && strings.Contains(c.Detail, "did not finish in 200ms")
		case nginx && !strings.Contains(c.Detail, "timed out"):
			later++
		}
	}
	if !nginx || later == 0 {
		t.Errorf("nginx %v, %d checks after it: %+v", nginx, later, d.Checks)
	}
}
