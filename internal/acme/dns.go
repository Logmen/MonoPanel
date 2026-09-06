package acme

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/go-acme/lego/v4/providers/dns/desec"
	"github.com/go-acme/lego/v4/providers/dns/digitalocean"
	"github.com/go-acme/lego/v4/providers/dns/gandiv5"
	"github.com/go-acme/lego/v4/providers/dns/hetzner"
	"github.com/go-acme/lego/v4/providers/dns/namecheap"
	"github.com/go-acme/lego/v4/providers/dns/rfc2136"
)

// DNSProviderTypes lists the lego DNS providers exposed in the panel with the
// environment variables each expects (see lego docs for optional ones).
var DNSProviderTypes = map[string][]string{
	"cloudflare":   {"CLOUDFLARE_DNS_API_TOKEN"},
	"hetzner":      {"HETZNER_API_KEY"},
	"digitalocean": {"DO_AUTH_TOKEN"},
	"gandiv5":      {"GANDIV5_PERSONAL_ACCESS_TOKEN"},
	"desec":        {"DESEC_TOKEN"},
	"namecheap":    {"NAMECHEAP_API_USER", "NAMECHEAP_API_KEY"},
	"rfc2136":      {"RFC2136_NAMESERVER", "RFC2136_TSIG_KEY", "RFC2136_TSIG_SECRET", "RFC2136_TSIG_ALGORITHM"},
}

// dnsMu serialises DNS-01 orders: lego providers read credentials from the
// process environment.
var dnsMu sync.Mutex

// dnsProvider builds a lego provider from credentials; the returned release
// func restores the environment and must be deferred.
func dnsProvider(typ string, creds map[string]string) (challenge.Provider, func(), error) {
	expected, ok := DNSProviderTypes[typ]
	if !ok {
		return nil, nil, fmt.Errorf("unsupported DNS provider %q", typ)
	}
	for _, k := range expected {
		if creds[k] == "" && k != "RFC2136_TSIG_ALGORITHM" {
			return nil, nil, errors.New("missing credential " + k)
		}
	}
	dnsMu.Lock()
	prev := map[string]*string{}
	for k, v := range creds {
		if old, ok := os.LookupEnv(k); ok {
			o := old
			prev[k] = &o
		} else {
			prev[k] = nil
		}
		os.Setenv(k, v)
	}
	release := func() {
		for k, old := range prev {
			if old == nil {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, *old)
			}
		}
		dnsMu.Unlock()
	}
	var p challenge.Provider
	var err error
	switch typ {
	case "cloudflare":
		p, err = cloudflare.NewDNSProvider()
	case "hetzner":
		p, err = hetzner.NewDNSProvider()
	case "digitalocean":
		p, err = digitalocean.NewDNSProvider()
	case "gandiv5":
		p, err = gandiv5.NewDNSProvider()
	case "desec":
		p, err = desec.NewDNSProvider()
	case "namecheap":
		p, err = namecheap.NewDNSProvider()
	case "rfc2136":
		p, err = rfc2136.NewDNSProvider()
	}
	if err != nil {
		release()
		return nil, nil, err
	}
	return p, release, nil
}
