package parity

import (
	"slices"
	"strings"
)

// apiOp is an API operation as the router sees it.
type apiOp struct {
	id     string
	method string
	path   string
	segs   []string // param for a path parameter
	admin  bool     // only administrators may call it
}

// param marks a path parameter among an operation's segments; a call's
// dynamic segment is "".
const param = "{}"

func newAPIOp(id, method, p string, admin bool) apiOp {
	o := apiOp{id: id, method: method, path: p, admin: admin}
	for _, s := range strings.Split(p, "/") {
		switch {
		case s == "":
		case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
			o.segs = append(o.segs, param)
		default:
			o.segs = append(o.segs, s)
		}
	}
	return o
}

func (o apiOp) String() string { return o.method + " " + o.path }

// fits says whether a call with these segments can reach the operation. A
// parameter takes any segment; a static segment takes the same text, or a
// dynamic segment when it is dyn ("*" for any static segment): the page
// computes it, e.g. `/firewall/${action}`.
func (o apiOp) fits(segs []string, dyn string) bool {
	if len(o.segs) != len(segs) {
		return false
	}
	for i, s := range o.segs {
		c := segs[i]
		switch {
		case s == param, c == s:
		case c == "" && (dyn == "*" || dyn == s):
		default:
			return false
		}
	}
	return true
}

// route lists the operations a call reaches, as the router picks them: of
// the operations whose segments fit, a static segment beats a parameter at
// the first place they differ (GET /sites/presets is not GET /sites/{domain}).
// An empty method stands for every method.
func route(ops []apiOp, method string, segs []string) []apiOp {
	best := map[string][]apiOp{} // per method
	for _, o := range ops {
		if method != "" && o.method != method || !o.fits(segs, "") {
			continue
		}
		cur := best[o.method]
		switch c := compareStatic(o, cur); {
		case len(cur) == 0 || c > 0:
			best[o.method] = []apiOp{o}
		case c == 0:
			best[o.method] = append(cur, o)
		}
	}
	var out []apiOp
	for _, list := range best {
		out = append(out, list...)
	}
	slices.SortFunc(out, func(a, b apiOp) int { return strings.Compare(a.id, b.id) })
	return out
}

// compareStatic compares o with the current best (all of equal rank):
// positive when o has a static segment where they have a parameter first.
func compareStatic(o apiOp, cur []apiOp) int {
	if len(cur) == 0 {
		return 1
	}
	for i, s := range o.segs {
		a, b := s != param, cur[0].segs[i] != param
		switch {
		case a && !b:
			return 1
		case !a && b:
			return -1
		}
	}
	return 0
}

// reaches says whether a call reaches the operation.
func reaches(ops []apiOp, method string, segs []string, o apiOp) bool {
	if method != "" && method != o.method {
		return false
	}
	for _, r := range route(ops, method, segs) {
		if r.id == o.id {
			return true
		}
	}
	return false
}

// mayReach lists the operations a call could reach whatever its dynamic
// segments turn out to be.
func mayReach(ops []apiOp, method string, segs []string) []apiOp {
	var out []apiOp
	for _, o := range ops {
		if (method == "" || o.method == method) && o.fits(segs, "*") {
			out = append(out, o)
		}
	}
	return out
}

// sortedKeys are a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
