package api

import (
	"context"
	"errors"
	"testing"

	"monopanel/internal/config"
)

// A panel on an internal name has no public record: the doctor's line used to
// warn NXDOMAIN for good. This host's resolver decides now — an address of
// this host or a private one makes the name internal (ok, and Let's Encrypt
// cannot issue for it); a loopback entry of /etc/hosts, a public address
// elsewhere or nothing at all keeps the warning.
func TestDoctorDNSInternalName(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.Web.Hostname = "panel.corp.lan" })
	f.s.SetHostIPs(func() []string { return []string{"203.0.113.10"} })
	f.s.SetHostIPv6s(func() []string { return []string{"2001:db8::10"} })
	f.s.SetLookup(func(context.Context, string) ([]string, error) { return nil, errNXDOMAIN })
	local := map[string][]string{}
	f.s.localLookup = func(_ context.Context, name string) ([]string, error) {
		if a, ok := local[name]; ok {
			return a, nil
		}
		return nil, errors.New("no such host")
	}
	const nx = "warn: NXDOMAIN (no such name in public DNS)"
	for _, c := range []struct {
		name  string
		addrs []string
		want  string
	}{
		{"unknown here too", nil, nx},
		{"a private address", []string{"10.20.0.5"}, "ok: internal name (not in public DNS): panel.corp.lan resolves to 10.20.0.5 here; Let's Encrypt cannot issue for it"},
		{"this host's public address", []string{"203.0.113.10", "2001:db8::10"}, "ok: internal name (not in public DNS): panel.corp.lan resolves to 203.0.113.10, 2001:db8::10 here; Let's Encrypt cannot issue for it"},
		{"a ULA", []string{"fd00::5"}, "ok: internal name (not in public DNS): panel.corp.lan resolves to fd00::5 here; Let's Encrypt cannot issue for it"},
		{"Debian's /etc/hosts line", []string{"127.0.1.1"}, nx + "; this host resolves it to 127.0.1.1"},
		{"another host", []string{"198.51.100.7"}, nx + "; this host resolves it to 198.51.100.7"},
	} {
		if c.addrs == nil {
			delete(local, "panel.corp.lan")
		} else {
			local["panel.corp.lan"] = c.addrs
		}
		if got := f.doctorCheck("dns panel.corp.lan"); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}

	// Another failure of public DNS is no question for the local resolver.
	local["panel.corp.lan"] = []string{"10.20.0.5"}
	f.s.SetLookup(func(context.Context, string) ([]string, error) { return nil, errors.New("i/o timeout") })
	if got := f.doctorCheck("dns panel.corp.lan"); got != "warn: i/o timeout" {
		t.Errorf("a timeout: %q", got)
	}
}
