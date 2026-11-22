package demo

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// offline is the demo's internet: the panel's own downloads get what they
// expect from the repositories it knows (a signing key, a release package,
// packages.sury.org's release for the pretend host's Ubuntu), everything else
// is told plainly that there is no network. The loopback is the container
// itself and stays reachable.
type offline struct{}

var loopback = http.DefaultTransport.(*http.Transport).Clone()

const demoKey = "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQINBGDemoKeyForTheMonoPanelDemoItSignsNothingAndNothingIsVerifiedAgainstIt\n=demo\n-----END PGP PUBLIC KEY BLOCK-----\n"

func (offline) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL
	if ip := net.ParseIP(u.Hostname()); u.Hostname() == "localhost" || ip != nil && ip.IsLoopback() {
		return loopback.RoundTrip(req)
	}
	body := ""
	switch {
	case u.Host == "nginx.org" && strings.HasPrefix(u.Path, "/keys/"),
		u.Host == "repo.mysql.com" && strings.Contains(u.Path, "GPG-KEY"):
		body = demoKey
	case u.Host == "packages.sury.org" && strings.HasSuffix(u.Path, ".gpg"):
		body = "\x99\x02\x0ddemo-keyring"
	case u.Host == "packages.sury.org" && u.Path == "/php/dists/noble/Release":
		body = "Origin: deb.sury.org\nSuite: noble\nCodename: noble\nArchitectures: amd64 arm64 armhf\nComponents: main\n"
	case u.Host == "repo.percona.com" && strings.HasSuffix(u.Path, ".deb"):
		body = "!<arch>\ndebian-binary   0           0     0     100644  4         `\n2.0\n"
	default:
		return nil, fmt.Errorf("%s: the demo has no internet access", u.Host)
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": []string{"application/octet-stream"}},
		Body:          io.NopCloser(bytes.NewReader([]byte(body))),
		ContentLength: int64(len(body)), Request: req,
	}, nil
}
