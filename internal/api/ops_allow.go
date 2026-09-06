package api

import (
	"fmt"
	"net"
	"strings"
)

// normalizeAllowFrom validates an IP allow-list (single addresses or CIDRs)
// and returns it in canonical form without duplicates.
func normalizeAllowFrom(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		var canon string
		if strings.Contains(v, "/") {
			_, ipnet, err := net.ParseCIDR(v)
			if err != nil {
				return nil, fmt.Errorf("allow_from: %q is not an IP or CIDR", raw)
			}
			canon = ipnet.String()
		} else {
			ip := net.ParseIP(v)
			if ip == nil {
				return nil, fmt.Errorf("allow_from: %q is not an IP or CIDR", raw)
			}
			canon = ip.String()
		}
		if !seen[canon] {
			seen[canon] = true
			out = append(out, canon)
		}
	}
	if len(out) > 64 {
		return nil, fmt.Errorf("allow_from: at most 64 entries")
	}
	return out, nil
}
