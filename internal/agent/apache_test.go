package agent

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// /v1/apache/ctl switches something of the distribution's Apache off and
// nothing on: what a2enmod, a2enconf or a2ensite enable loads into the root
// parent of apache2.service, which serves none of the panel's sites.
func TestApacheCtlSwitchesOffOnly(t *testing.T) {
	deb := policyServer(t, debian13)
	for _, action := range []string{"enmod", "enconf", "ensite"} {
		for _, name := range []string{"status", "cgid", "lua", "monopanel", "000-default"} {
			_, err := deb.apacheCtl(t.Context(), &ApacheCtlRequest{Action: action, Name: name})
			var ae *Error
			if !errors.As(err, &ae) || ae.Status != http.StatusForbidden || !strings.Contains(ae.Message, "is not allowed") || !strings.Contains(ae.Message, "apache2.service") {
				t.Errorf("%s %s: %v", action, name, err)
			}
		}
	}
	for _, action := range []string{"dismod", "disconf", "dissite"} {
		if tool, err := CheckApacheCtl(action, "monopanel"); err != nil || tool != "a2"+action {
			t.Errorf("%s: %q %v", action, tool, err)
		}
	}
	for _, c := range [][2]string{{"disconf", "../x"}, {"disconf", "-f"}, {"disconf", "a b"}, {"query", "monopanel"}, {"", "monopanel"}} {
		var ae *Error
		if _, err := CheckApacheCtl(c[0], c[1]); !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
			t.Errorf("%s %q: %v", c[0], c[1], err)
		}
	}
	el := policyServer(t, alma9)
	var ae *Error
	if _, err := el.apacheCtl(t.Context(), &ApacheCtlRequest{Action: "disconf", Name: "monopanel"}); !errors.As(err, &ae) || ae.Status != http.StatusNotImplemented {
		t.Errorf("EL: %v", err)
	}
}

// dirs/ensure makes the runtime directory of an account's Apache, which
// apache2 -t wants to see before the unit ever ran: root's, like anything
// it makes outside the web root, and nothing next to it.
func TestApacheRunDirsAreRoots(t *testing.T) {
	s := policyServer(t, debian13)
	for _, p := range []string{"/run/monopanel-apache", "/run/monopanel-apache/alex"} {
		if !s.dirAllowed(p) {
			t.Errorf("%s refused", p)
		}
		if err := s.checkDirSpec(DirSpec{Path: p, Mode: 0o755, Owner: "root", Group: "root"}); err != nil {
			t.Errorf("%s root's: %v", p, err)
		}
		for _, d := range []DirSpec{{Path: p, Mode: 0o755, Owner: "alex"}, {Path: p, Mode: 0o775, Owner: "root"}, {Path: p, Mode: 0o755, Group: "alex"}} {
			if err := s.checkDirSpec(d); err == nil {
				t.Errorf("%+v passed", d)
			}
		}
	}
	for _, p := range []string{"/run", "/run/monopanel-apachex", "/run/apache2", "/run/systemd/x"} {
		if s.dirAllowed(p) {
			t.Errorf("%s allowed", p)
		}
	}
}
