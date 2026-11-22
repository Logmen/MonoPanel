package parity

import (
	"slices"
	"strings"
	"testing"
)

// sentBy reads what the calls of a file send, by the path they call:
// "POST /x" → the body's fields; "GET /x?" → the query's keys. Open
// readings end in "+open".
func sentBy(t *testing.T, f *webFile) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, c := range f.calls {
		key := c.method + " " + segsString(c.segs)
		var k keys
		if b, has := bodyOf(f, c); has {
			k = b
		} else {
			k = queryOf(f, c)
			key += "?"
		}
		v := strings.Join(k.names(), ",")
		if len(k.open) > 0 {
			v += "+open"
		}
		if prev, dup := out[key]; dup {
			v = prev + "|" + v
		}
		out[key] = v
	}
	return out
}

func TestBodyKeys(t *testing.T) {
	f := parseWebFile("routes/x/+page.svelte", `<script lang="ts">
  import { api } from '$lib/api';
  import { build } from './lib';
  let form = $state({ name: '', quota: 0, never_sent: '' });
  let rule = $state({ kind: 'allow', port: '' });
  // Only the keys of what goes into the call count: form's own keys do not.
  async function literal() { await api('/literal', { method: 'POST', json: { name: form.name, quota_mb: form.quota } }); }
  function createBody() {
    const f = form;
    const body: Record<string, unknown> = { domain: f.name };
    if (admin) body.user = f.name;
    for (const k of ['http2', 'http3'] as const) body[k] = true;
    Object.assign(body, { extra: 1 });
    return body;
  }
  async function viaFunction() { await api('/function', { method: 'POST', json: createBody() }); }
  async function viaState() { await api('/state', { method: 'POST', json: rule }); }
  async function put(body: Record<string, unknown>, done: string) { await api('/param', { method: 'PUT', json: { always: 1, ...body } }); }
  const a = () => put({ repo: 'x', channel: 'stable' }, 'saved');
  const b = () => put({ clear_token: true }, 'forgotten');
  async function choice(version: string) { await api('/choice', { method: 'POST', json: version ? { version } : {} }); }
  const derived = $derived.by(() => { const d: any = {}; if (x) d.hostname = 1; return d; });
  async function spread(restart: boolean) { await api('/spread', { method: 'PUT', json: restart ? { restart: true } : { ...derived, ...(again ? { restart: true } : {}) } }); }
  async function imported() { await api('/imported', { method: 'POST', json: build() }); }
  async function console() { await fetch('/api/v1/console', { method: 'POST', body: JSON.stringify({ args, lang }) }); }
  async function computed(key: string) { await api('/computed', { method: 'PUT', json: { [key]: 1 } }); }
</script>`)
	got := sentBy(t, f)
	want := map[string]string{
		"POST /literal":  "name,quota_mb",
		"POST /function": "domain,extra,http2,http3,user",
		"POST /state":    "kind,port",
		"PUT /param":     "always,channel,clear_token,repo",
		"POST /choice":   "version",
		"PUT /spread":    "hostname,restart",
		"POST /imported": "+open",
		"POST /console":  "args,lang",
		"PUT /computed":  "+open",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s sends %q, want %q", k, got[k], v)
		}
	}
	// Where a field is set: the key's token, for the role check.
	for _, c := range f.calls {
		if segsString(c.segs) == "/function" {
			k, _ := bodyOf(f, c)
			at := k.at["user"]
			if len(at) != 1 || !strings.HasPrefix(f.code[at[0]:], "user = f.name") {
				t.Errorf("user is set at %v", at)
			}
		}
	}
}

func TestQueryKeys(t *testing.T) {
	f := parseWebFile("routes/x/+page.svelte", `<script lang="ts">
  import { api } from '$lib/api';
  let { login = '' } = $props();
  const byDomain = (d: string) => (d ? '?domain=' + encodeURIComponent(d) : '');
  const q = (p: string) => `+"`user=${encodeURIComponent(login)}&path=${encodeURIComponent(p)}`"+`;
  async function a() { await api('/a' + byDomain(name)); }
  async function b(status: string) {
    const params = new URLSearchParams({ limit: String(limit) });
    if (status) params.set('status', status);
    await api('/b?' + params);
  }
  async function c(id: string) { await api(`+"`/c/${login}/x?lines=${n}`"+`); }
  async function d() { await api(`+"`/d?${q(cwd)}`"+`); }
  async function e(name: string) { await api(`+"`/e/${name}${purge ? '?purge=true' : ''}`"+`, { method: 'DELETE' }); }
  async function f2() { await api('/f?range=' + range); }
</script>`)
	got := sentBy(t, f)
	want := map[string]string{
		"GET /a?":        "domain",
		"GET /{…}?":      "domain", // the scanner's other reading of '/a' + byDomain(…): a computed segment
		"GET /b?":        "limit,status",
		"GET /c/{…}/x?":  "lines", // ${login} is a segment: the prop adds no key, and does not open the reading
		"GET /d?":        "path,user",
		"DELETE /e/{…}?": "purge",
		"GET /f?":        "range",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: query %q, want %q", k, got[k], v)
		}
	}
	if !slices.Equal(sortedKeys(got), sortedKeys(want)) {
		t.Errorf("calls: %v", sortedKeys(got))
	}
}
