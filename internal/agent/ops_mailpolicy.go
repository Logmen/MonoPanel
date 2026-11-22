package agent

import (
	"context"
	"os"
	"time"

	"monopanel/internal/mailpolicy"
	"monopanel/internal/osprofile"
)

// mailPolicyProbe asks the panel's mail policy what postfix asks it first,
// the milter protocol's option negotiation, where postfix reaches it: the
// socket of Debian and Ubuntu, postfix's own, or the loopback port of EL —
// once it has made sure that what listens there is the panel's (PID 1's
// socket on EL, not an account's process that took the port). It also reads
// main.cf: postfix asks the policy only where main.cf names it first. The
// doctor's "mail policy" line, and the API's check before main.cf is told
// to wait for the policy. The address is the OS family's, never the
// caller's.
func (s *Server) mailPolicyProbe(ctx context.Context, _ *struct{}) (*MailPolicyProbeResponse, error) {
	l := mailpolicy.ListenerFor(s.profile != nil && s.profile.Family() == osprofile.FamilyDebian)
	resp := &MailPolicyProbeResponse{Listener: l.Address}
	if b, err := os.ReadFile(s.mainCF()); err != nil {
		resp.ConfigError = err.Error()
	} else {
		resp.Configured, resp.ConfigError = mailpolicy.MainCFAsks(string(b), l)
	}
	// A milter that does not answer is the answer, not a failure of the
	// call: the doctor says why.
	err := mailpolicy.CheckListener(s.procRoot(), l, uidOrNone(mailpolicy.User), uidOrNone("postfix"))
	if err == nil {
		err = mailpolicy.Probe(ctx, l, 3*time.Second)
	}
	resp.OK = err == nil
	if !resp.OK {
		resp.Error = err.Error()
	}
	return resp, nil
}

// mainCF is postfix's main.cf (another one in tests).
func (s *Server) mainCF() string {
	if s.mainCFPath != "" {
		return s.mainCFPath
	}
	return mailpolicy.MainCFFile
}

// procRoot is where proc/net is read: "/" but in tests.
func (s *Server) procRoot() string {
	if s.procRootPath != "" {
		return s.procRootPath
	}
	return "/"
}

// uidOrNone is a login's uid, -1 when there is none.
func uidOrNone(login string) int {
	uid, err := lookupUID(login)
	if err != nil {
		return -1
	}
	return uid
}
