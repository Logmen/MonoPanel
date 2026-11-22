package mailpolicy

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// GuardTable is the guard's nftables table (GuardUnit).
const GuardTable = "monopanel_mailguard"

// GuardRuleset is what `nft -f -` loads for the guard on EL, where the
// policy and opendkim listen on loopback ports (TCPAddress, DKIMPort) that
// every local process can reach: an output filter in a table of its own —
// beside firewalld's, the panel's firewall's or any other, which it neither
// touches nor needs — that resets a connection to either port on a loopback
// address from any process but root's and the users allowed (postfix's).
// Without it an account speaks the milter protocol to opendkim itself,
// claims to be postfix with a local client, and gets any domain of the
// SigningTable signed; or holds the policy's connections. Loaded again, the
// ruleset replaces itself in one transaction.
func GuardRuleset(allowed ...int) string {
	uids := []string{"0"}
	for _, u := range allowed {
		if u > 0 {
			uids = append(uids, strconv.Itoa(u))
		}
	}
	_, port, _ := net.SplitHostPort(TCPAddress)
	return fmt.Sprintf(`table inet %[1]s
delete table inet %[1]s
table inet %[1]s {
	chain output {
		type filter hook output priority 0; policy accept;
		ip daddr 127.0.0.0/8 tcp dport { %[2]s, %[3]d } meta skuid != { %[4]s } counter reject with tcp reset
	}
}
`, GuardTable, port, DKIMPort, strings.Join(uids, ", "))
}
