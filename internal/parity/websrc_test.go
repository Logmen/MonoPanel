package parity

import (
	"slices"
	"strings"
	"testing"
)

// callsOf lists a file's calls as "METHOD /path" with {} for computed
// segments, in order.
func callsOf(f *webFile) []string {
	var out []string
	for _, c := range f.calls {
		m := c.method
		if m == "" {
			m = "?"
		}
		out = append(out, m+" "+strings.ReplaceAll(segsString(c.segs), "{…}", "{}"))
	}
	return out
}

func TestWebCallsInScript(t *testing.T) {
	src := `import { api, apiText, apiPutRaw, ApiError } from '$lib/api';
const a = await api('/sites');
await api(` + "`/sites/${encodeURIComponent(domain)}/apply`" + `, { method: 'POST' });
await api('/sites/' + d + '/apply', { method: "DELETE" });
const s = await api<Site[]>('/sites/presets');
const r = await api<{ job_id: number }>('/migrate/run', { method: 'POST', json: req });
const l = await api(kind === 'cron' ? ` + "`/users/${login}/cron`" + ` : kind === 'shares' ? ` + "`/users/${login}/shares`" + ` : ` + "`/users/${login}/apps`" + `);
await api(` + "`/tokens?user=${encodeURIComponent(login)}`" + `);
await api(` + "`/mail/mailboxes/${del.name}${purge ? '?purge=true' : ''}`" + `, { method: 'DELETE' });
await api('/system/metrics?range=' + range);
await api(` + "`/system/audit?limit=200${action ? `&action=${encodeURIComponent(action)}` : ''}`" + `);
const text = await apiText(` + "`/files/content?${q(p)}`" + `);
await apiPutRaw(` + "`/files/content?${q(p)}`" + `, text);
const res = await fetch('/api/v1/system/console', { method: 'POST', body: JSON.stringify({ args, lang }) });
const es = new EventSource(` + "`/api/v1/jobs/${id}/events`" + `);
window.location.href = ` + "`/api/v1/files/content?${q(join(cwd, e.name))}`" + `;
await api(path, { method: m });
await api('/x', { ...init, method: 'POST' });
await api(` + "`/jobs${query}`" + `);
await api('/y', { method: editing ? 'PATCH' : 'POST', json: body });
await api('/z', { json: method });
await api('/w', { json: { a: 1 }, method: a ? 'put' : b ? 'DELETE' : 'PUT' });
await api('/v', { method: a ? m : 'POST' });
await api('/u', { method });
`
	f := parseWebFile("lib/x.ts", src)
	want := []string{
		"GET /sites",
		"POST /sites/{}/apply",
		"DELETE /sites/{}/apply",
		"GET /sites/presets",
		"POST /migrate/run",
		"GET /users/{}/cron", "GET /users/{}/shares", "GET /users/{}/apps",
		"GET /tokens",
		"DELETE /mail/mailboxes/{}",
		"GET /system/metrics",
		"GET /system/audit",
		"GET /files/content",
		"PUT /files/content",
		"POST /system/console",
		"GET /jobs/{}/events",
		"GET /files/content",
		"? /x",
		"GET /{}", "GET /jobs",
		"PATCH /y", "POST /y", // both branches of the choice
		"GET /z", // method is a value there, not the key
		"PUT /w", "DELETE /w",
		"? /v", // a branch the code computes
		"? /u",
	}
	if got := callsOf(f); !slices.Equal(got, want) {
		t.Errorf("calls:\n got %q\nwant %q", got, want)
	}
	if len(f.vague) != 1 || f.vague[0].method != "" || f.vague[0].text != "path" {
		t.Errorf("vague calls: %+v", f.vague)
	}
}

func TestWebIgnoresCommentsWrappersAndRegexps(t *testing.T) {
	src := `// await api('/commented', { method: 'POST' });
/* api('/blocked') */
export async function api<T = unknown>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch('/api/v1' + path, { ...init, credentials: 'same-origin' });
  return res.json();
}
const parts = text.split(/[\s,]+/).map((s) => s.replace(/'/g, ''));
const q = "it's"; const r = 'say "hi"';
await api('/after', { method: 'PUT' });
const n = total / 2; await api('/divided');
`
	f := parseWebFile("lib/api.ts", src)
	if got, want := callsOf(f), []string{"PUT /after", "GET /divided"}; !slices.Equal(got, want) {
		t.Errorf("calls: got %q, want %q", got, want)
	}
	if strings.Contains(f.code, "commented") || strings.Contains(f.code, "blocked") || !strings.Contains(f.code, "/after") {
		t.Errorf("comments are not blanked in the code:\n%s", f.code)
	}
}

func TestWebSvelte(t *testing.T) {
	src := `<script lang="ts">
  import { api } from '$lib/api';
  import LogView from '$lib/components/LogView.svelte';
  let form = $state({ name: '', legacy_auth: false });
  async function act(a: string) { await api(` + "`/firewall/${a}`" + `, { method: 'POST' }); }
  async function save() {
    const body: any = { name: form.name };
    if (form.remote) body.remote = true;
    await api('/databases', { method: 'POST', json: body });
  }
</script>

<!-- api('/in-a-comment', { method: 'DELETE' }) -->
<p>Don't stop: it's {count} o'clock</p>
{#if open}
  <button onclick={() => act('apply')}>{t('firewall.apply')}</button>
  <button onclick={() => api('/mail/apply', { method: 'POST', json: {} })}>x</button>
{/if}
<a href="/api/v1/docs" target="_blank">docs</a>
<a href="/api/v1/files/content?{q(e.name)}" download>{t('databases.remote_hint')}</a>
<a href="/stack">stack</a>
<LogView src={` + "`/system/logs/${sel.unit}`" + `} bind:lines />
<input bind:checked={form.legacy_auth} placeholder="?limit=" />
<style>
  .x { content: "api('/styled')"; }
</style>
`
	f := parseWebFile("routes/x/+page.svelte", src)
	if got, want := callsOf(f), []string{"POST /firewall/{}", "POST /databases", "POST /mail/apply", "GET /docs", "GET /files/content"}; !slices.Equal(got, want) {
		t.Errorf("calls: got %q, want %q", got, want)
	}
	if !f.hasString("apply") || f.hasString("in-a-comment") {
		t.Error("strings of the markup")
	}
	for _, w := range []string{"legacy_auth", "remote", "name", "limit"} {
		if !f.hasWord(w) {
			t.Errorf("%q is named in the file, but not among its words", w)
		}
	}
	for _, w := range []string{"api", "LogView", "act", "remote_hint"} {
		if f.hasWord(w) {
			t.Errorf("%q is not a field name, but among the words", w)
		}
	}
	var logs bool
	for _, p := range f.paths {
		if segsString(p.segs) == "/system/logs/{…}" {
			logs = true
		}
	}
	if !logs {
		t.Errorf("the path handed to LogView is not among the paths: %v", f.paths)
	}
}

func TestRoute(t *testing.T) {
	ops := []apiOp{
		newAPIOp("sites-list", "GET", "/sites", false),
		newAPIOp("sites-get", "GET", "/sites/{domain}", false),
		newAPIOp("sites-delete", "DELETE", "/sites/{domain}", false),
		newAPIOp("sites-presets", "GET", "/sites/presets", false),
		newAPIOp("sites-apply", "POST", "/sites/{domain}/apply", false),
		newAPIOp("sites-fix", "POST", "/sites/{domain}/fix", false),
		newAPIOp("firewall-apply", "POST", "/firewall/apply", true),
		newAPIOp("firewall-ban", "POST", "/firewall/ban", true),
	}
	ids := func(list []apiOp) []string {
		var out []string
		for _, o := range list {
			out = append(out, o.id)
		}
		return out
	}
	cases := []struct {
		method string
		segs   []string
		want   []string
	}{
		{"GET", []string{"sites", "presets"}, []string{"sites-presets"}}, // a static segment beats {domain}
		{"GET", []string{"sites", ""}, []string{"sites-get"}},
		{"GET", []string{"sites", "example.com"}, []string{"sites-get"}},
		{"", []string{"sites", ""}, []string{"sites-delete", "sites-get"}},
		{"POST", []string{"sites", "", ""}, nil}, // a computed action reaches nothing for sure
		{"POST", []string{"firewall", ""}, nil},
		{"PATCH", []string{"sites", ""}, nil},
	}
	for _, c := range cases {
		if got := ids(route(ops, c.method, c.segs)); !slices.Equal(got, c.want) {
			t.Errorf("route %s %v = %v, want %v", c.method, c.segs, got, c.want)
		}
	}
	if got := ids(mayReach(ops, "POST", []string{"firewall", ""})); !slices.Equal(got, []string{"firewall-apply", "firewall-ban"}) {
		t.Errorf("mayReach POST /firewall/{} = %v", got)
	}
	if !ops[4].fits([]string{"sites", "", ""}, "apply") || ops[5].fits([]string{"sites", "", ""}, "apply") {
		t.Error("fits with a computed segment")
	}
}

func TestExample(t *testing.T) {
	if got := example(newAPIOp("x", "PATCH", "/users/{login}/apps/{name}", false)); got != "api(`/users/${login}/apps/${name}`, { method: 'PATCH', … })" {
		t.Errorf("example: %s", got)
	}
}

// TestFinds checks how a manifest location is matched: by a call routed to
// the operation, by a computed segment with its value in the file (Dyn), by
// the path handed to a component (Loose) and by a pattern.
func TestFinds(t *testing.T) {
	w := &world{ops: []apiOp{
		newAPIOp("fw-apply", "POST", "/firewall/apply", true),
		newAPIOp("fw-ban", "POST", "/firewall/ban", true),
		newAPIOp("logs", "GET", "/system/logs/{unit}", true),
		newAPIOp("app-get", "GET", "/users/{login}/apps/{name}", false),
		newAPIOp("app-del", "DELETE", "/users/{login}/apps/{name}", false),
		newAPIOp("cert-get", "GET", "/certificates/{id}", true),
		newAPIOp("cert-import", "POST", "/certificates/import", true),
	}}
	page := parseWebFile("routes/fw/+page.svelte", `<script lang="ts">
  async function act(a: string) { await api(`+"`/firewall/${a}`"+`, { method: 'POST' }); }
  const del = (l, n) => api(`+"`/users/${l}/apps/${n}`"+`, { method: 'DELETE' });
  await api('/certificates/import', { method: 'POST', json: x });
</script>
<button onclick={() => act('apply')}>go</button>
<LogView src={`+"`/system/logs/${unit}`"+`} />`)
	files := []*webFile{page}
	byID := func(id string) apiOp {
		for _, o := range w.ops {
			if o.id == id {
				return o
			}
		}
		t.Fatalf("no op %s", id)
		return apiOp{}
	}
	if msg := w.finds(byID("fw-apply"), Web{Dyn: "apply"}, files); msg != "" {
		t.Errorf("Dyn with the value in the file: %s", msg)
	}
	if msg := w.finds(byID("fw-ban"), Web{Dyn: "ban"}, files); !strings.Contains(msg, "may be it") {
		t.Errorf("Dyn without the value in the file must fail and point at the computed call: %q", msg)
	}
	if msg := w.finds(byID("fw-apply"), Web{}, files); msg == "" {
		t.Error("a computed segment must not count without Dyn")
	}
	if msg := w.finds(byID("logs"), Web{Loose: true}, files); msg != "" {
		t.Errorf("Loose with the path handed to a component: %s", msg)
	}
	if msg := w.finds(byID("logs"), Web{}, files); msg == "" {
		t.Error("a path handed to a component is no call without Loose")
	}
	msg := w.finds(byID("app-get"), Web{}, files)
	if !strings.Contains(msg, "another method") || !strings.Contains(msg, "DELETE /users/{…}/apps/{…}") {
		t.Errorf("a GET missing next to a DELETE of the same path: %q", msg)
	}
	if msg := w.finds(byID("cert-get"), Web{}, files); strings.Contains(msg, "another method") {
		t.Errorf("POST /certificates/import is another path, not /certificates/{id}: %q", msg)
	}
	if msg := w.finds(byID("cert-get"), Web{Pattern: `LogView src=`}, files); msg != "" {
		t.Errorf("Pattern: %s", msg)
	}
	if msg := w.finds(byID("cert-get"), Web{Pattern: `nowhere`}, files); msg == "" {
		t.Error("a Pattern that does not match must fail")
	}
	if o := op("x-pattern", webPattern(`a`, "routes/x/")); o.Web[0].Pattern != "a" || !strings.HasPrefix(o.at, "websrc_test.go:") {
		t.Errorf("webPattern: %+v", o)
	}
}

// FuzzParseWebFile: whatever a source file holds, half-written code
// included, reading it must not panic.
func FuzzParseWebFile(f *testing.F) {
	for _, s := range []string{
		"", "`", "`${", "`${`", "'", "\"", "/*", "/", "//", "{", "}", "{/", "<script>", "<script", "<!--", "<style>",
		"api(", "api('/x'", "api<", "api<T>(`/a/${b}`", "fetch('/api/v1/x', {", "new EventSource(`/api/v1/${", "import { a",
		"<a href=\"/api/v1/{x", "<p>it's {a ? '/b' : `/c/${d}`}</p>", "x = /[/]/; api('/y')", "/\\", "a = /\\", "<script lang=\"ts\">api('/z', { method",
		"api('/x', { method: a ? 'B' :", "api('/x', { method: ? : })", "api('/x', { method: (a ? 'B' : 'C') })", "api('/x', { method: })", "api('/x', {,method",
		"const x = $state<Record<string, { a?: string; b: number }>>({});", "function f(a, {b, c = 1}, ...d) { return { ...a, [b]: c }; }",
		"if (!admin) return; api('/x', { json: body })", "{#if admin}<A x={y} {z} bind:w />{:else if b}{/if}", "{#if}{:else}{/each}{/if}",
		"const q = new URLSearchParams({ a: 1 }); q.set('b', 2); api('/y?' + q);", "<script>let { p = $bindable() } = $props();</script>{#if p}{@render children()}{/if}",
		"for (const k of ['a', 'b']) body[k] = 1; api('/z', { method: 'POST', json: body });", "a ? b : c ? d : e", "x => y => z", "async (a): Promise<void> => {}",
		"const body = { a: 1 }, f = () => api('/w', { method: 'PUT', json: { ...body, b } });", "<Modal open={!!del}><Modal open={x}></Modal>",
	} {
		f.Add("x.svelte", s)
		f.Add("x.ts", s)
	}
	f.Fuzz(func(t *testing.T, name, src string) {
		w := parseWebFile(name, src)
		if len(w.code) != len(src) {
			t.Fatalf("the code must keep the file's offsets: %d bytes for %d", len(w.code), len(src))
		}
		// Nor must following its values: the structure, the regions, what
		// each call sends, what each name refers to.
		fl := flowOf(w)
		scanRegions(w)
		for _, c := range append(w.calls, w.vague...) {
			bodyOf(w, c)
			queryOf(w, c)
		}
		for _, list := range fl.byName {
			for _, d := range list {
				fl.refs(d)
				if d.fn != nil {
					d.fn.returns()
				}
			}
		}
		if nav := (map[string]*webFile{"lib/components/Nav.svelte": w, name: w}); name != "" {
			if g, _ := newRoles(nav); g != nil {
				for _, c := range w.calls {
					g.guarded(w, c.pos)
				}
			}
		}
	})
}

// TestWebTypesAreNoFields: the names in imports and type declarations (a
// type that mirrors a response) are not fields a form sends; the code after
// them is still read.
func TestWebTypesAreNoFields(t *testing.T) {
	f := parseWebFile("routes/u/lib.ts", `import { api, type Thing } from '$lib/api';
export type Account = { id: number; quota_mb?: number; tags: Map<string, Array<number>> }
export interface Site extends Base<X> { rate_limit: number; nested: { docroot: string } }
type Mode = 'a' | 'b'
const form = { name: '' };
type Fn = (in_place: boolean) => void;
await api('/after-types', { method: 'POST', json: { keep_daily: 1 } });
`)
	for _, w := range []string{"quota_mb", "rate_limit", "docroot", "in_place", "Thing", "Account"} {
		if f.hasWord(w) {
			t.Errorf("%q is declared in a type or imported, not sent", w)
		}
	}
	for _, w := range []string{"name", "keep_daily", "form"} {
		if !f.hasWord(w) {
			t.Errorf("%q is in the code after the types, but not among the words", w)
		}
	}
	if got := callsOf(f); !slices.Equal(got, []string{"POST /after-types"}) {
		t.Errorf("calls after the types: %q", got)
	}
}
