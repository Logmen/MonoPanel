package parity

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// roleWorld is a made-up web UI for the role check: a page ordinary
// accounts open (/users), a page they do not (/admin), components, and the
// extra files given.
func roleWorld(t *testing.T, page string, extra ...map[string]string) *world {
	t.Helper()
	files := map[string]string{
		"lib/components/Nav.svelte": `<script lang="ts">
  const items = $derived(all.filter(([href]) => admin || ['/', '/users'].includes(href)));
</script>`,
		"lib/components/Modal.svelte": `<script lang="ts">
  let { open = $bindable(false), title = '', children, footer } = $props();
</script>
{#if open}<div>{title}{@render children?.()}{#if footer}{@render footer()}{/if}</div>{/if}`,
		"routes/users/Panel.svelte": `<script lang="ts">
  import { api } from '$lib/api';
  let { login = '', admin = false } = $props();
  // Every account's jobs without a login (administrators), one's own with it.
  async function reload() { login ? await api('/users/' + login + '/cron') : await api('/cron'); }
  $effect(() => { reload(); });
  async function reset() { await api('/totp', { method: 'POST' }); }
</script>
{#if admin}<button onclick={reset}>reset</button>{/if}`,
		"lib/admin.ts": `import { api } from './api';
export async function allUsers() { return api('/users'); }
export const settings = () => api('/settings');`,
		"routes/admin/+page.svelte": `<script lang="ts">
  import { api } from '$lib/api';
  onMount(() => api('/settings'));
</script>`,
		"routes/users/+page.svelte": page,
	}
	for _, more := range extra {
		for name, src := range more {
			files[name] = src
		}
	}
	sources := map[string]*webFile{}
	for name, src := range files {
		sources[name] = parseWebFile(name, src)
	}
	ops := []apiOp{
		newAPIOp("users-list", "GET", "/users", true),
		newAPIOp("users-get", "GET", "/users/{login}", false),
		newAPIOp("users-delete", "DELETE", "/users/{login}", true),
		newAPIOp("cron-list", "GET", "/users/{login}/cron", false),
		newAPIOp("cron-all", "GET", "/cron", true),
		newAPIOp("doctor", "GET", "/doctor", true),
		newAPIOp("settings", "GET", "/settings", true),
		newAPIOp("hooks", "GET", "/webhooks", true),
		newAPIOp("tls", "POST", "/tls", true),
		newAPIOp("apps", "GET", "/apps", true),
		newAPIOp("totp", "POST", "/totp", true),
		newAPIOp("and", "GET", "/and", true),
		newAPIOp("site-apply", "POST", "/sites/{domain}/apply", false),
		newAPIOp("site-suspend", "POST", "/sites/{domain}/suspend", true),
		newAPIOp("site-create", "POST", "/sites", false),
	}
	inputs := map[string][]input{"site-create": {{name: "domain", in: "body", typ: "string"}, {name: "user", in: "body", typ: "string"}}}
	man := []Op{op("site-create", cli("site add"), web("routes/users/"), adminField("an ordinary account's site is its own", "user"))}
	return newWorld(ops, map[string]string{}, inputs, &cobra.Command{Use: "mp"}, sources, man)
}

const guardedPage = `<script lang="ts">
  import { api } from '$lib/api';
  import { auth } from '$lib/state.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Panel from './Panel.svelte';
  import { allUsers, settings as loadSettings } from '$lib/admin';
  const admin = $derived(auth.me?.role === 'admin');
  let del = $state<any>(null);
  let open = $state(false);
  let count = $state(0);
  async function load() { if (admin) await api('/users'); else await api('/users/me'); }
  async function early() { if (!admin || busy) return; await api('/doctor'); }
  async function viaButton() { await api('/webhooks'); }
  const askRm = (u: string) => ({ title: u, run: () => rm(u) });
  async function rm(u: string) { await api('/users/' + u, { method: 'DELETE' }); }
  async function issue(e: Event) { e.preventDefault(); await api('/tls', { method: 'POST' }); }
  async function act(d: string, a: string) { await api(` + "`/sites/${d}/${a}`" + `, { method: 'POST' }); }
  function create() {
    const body: any = { domain: 'x' };
    if (admin) body.user = owner;
    return api('/sites', { method: 'POST', json: body });
  }
  onMount(() => { load(); early(); });
  const all = admin ? api('/cron') : Promise.resolve([]);
  const users = admin ? allUsers() : [];
  $effect(() => { if (admin) loadSettings(); });
  $effect(() => { admin && api('/and'); });
</script>
{#if admin}
  <button onclick={viaButton}>hooks</button>
  <button onclick={() => (ask = askRm('a'))}>rm</button>
  <button onclick={() => { del = 'a'; open = true; }}>open</button>
  <button onclick={() => act('x', 'suspend')}>suspend</button>
{/if}
<button onclick={() => count++}>+</button>
<button onclick={() => act('x', 'apply')}>apply</button>
<button onclick={create}>create</button>
<Modal open={!!del} title="delete"><button onclick={() => rm(del)}>ok</button></Modal>
<Modal bind:open={open}><form onsubmit={issue}></form></Modal>
{#if admin}<Panel admin />{:else}<Panel login={me.login} />{/if}`

func TestRolesGuardedPage(t *testing.T) {
	w := roleWorld(t, guardedPage)
	if got := w.webRoleProblems(); len(got) > 0 {
		t.Errorf("every administrators' call of the page is guarded, but:\n%s", strings.Join(got, "\n"))
	}
	g, why := newRoles(w.web)
	if g == nil {
		t.Fatal(why)
	}
	if !slices.Equal(g.pages, []string{"/", "/users"}) || g.shared("routes/admin/+page.svelte") || !g.shared("routes/users/Panel.svelte") || !g.shared("lib/x.ts") || !g.shared("routes/+layout.svelte") {
		t.Errorf("pages of ordinary accounts: %v", g.pages)
	}
	if g.gate("lib/components/Modal.svelte") != "open" {
		t.Errorf("Modal renders its content under open: %q", g.gate("lib/components/Modal.svelte"))
	}
	var state []string
	for d := range g.state {
		if d.list.file.path == "routes/users/+page.svelte" {
			state = append(state, d.name)
		}
	}
	slices.Sort(state)
	if !slices.Equal(state, []string{"del", "open"}) {
		t.Errorf("filled in guarded places only: %v (count is filled by count++ for anyone)", state)
	}
}

// Each change takes a guard away (or adds a way in) and must be reported.
func TestRolesUnguarded(t *testing.T) {
	cases := []struct{ old, new, want string }{
		{"if (admin) await api('/users'); else", "if (busy) await api('/users'); else", "users-list"},
		{"if (!admin || busy) return;", "if (busy) return;", "doctor"},
		{"onMount(() => { load(); early(); });", "onMount(() => { load(); early(); viaButton(); });", "hooks"},
		{"const all = admin ? api('/cron') : Promise.resolve([]);", "const all = count ? api('/cron') : Promise.resolve([]);", "cron-all"},
		{"$effect(() => { admin && api('/and'); });", "$effect(() => { count && api('/and'); });", "and"},
		{"<button onclick={() => count++}>+</button>", "<button onclick={() => (ask = askRm('b'))}>+</button>", "users-delete"},
		{"<button onclick={() => count++}>+</button>", "<button onclick={() => (del = 'b')}>+</button>", "users-delete"},
		{"<button onclick={() => count++}>+</button>", "<button onclick={() => { open = true; }}>+</button>", "tls"},
		{"<button onclick={() => act('x', 'apply')}>apply</button>", "<button onclick={() => act('x', 'suspend')}>apply</button>", "the value 'suspend'"},
		{"{#if admin}<Panel admin />{:else}<Panel login={me.login} />{/if}", "{#if admin}<Panel admin />{:else}<Panel />{/if}", "cron-all"},
		{"{#if admin}<Panel admin />{:else}<Panel login={me.login} />{/if}", "<Panel admin login={me.login} />", "totp"},
		{"if (admin) body.user = owner;", "body.user = owner;", "sets body user of POST /sites"},
		{"const users = admin ? allUsers() : [];", "const users = allUsers();", "users-list"},
		{"$effect(() => { if (admin) loadSettings(); });", "$effect(() => { loadSettings(); });", "web/src/lib/admin.ts:3"},
		// The role of a row (or of the account a dialog is about) is not the
		// role of whoever looks at the page.
		{"if (admin) await api('/users'); else", "if (row.role === 'admin') await api('/users'); else", "users-list"},
		{"const admin = $derived(auth.me?.role === 'admin');", "const admin = $derived(me?.role === 'admin');", "users-list"},
		// A name that starts empty may be empty when the component shows: it
		// does not rule out Panel's way to GET /cron without a login.
		{"{#if admin}<Panel admin />{:else}<Panel login={me.login} />{/if}", "{#if admin}<Panel admin />{:else}<Panel login={count} />{/if}", "cron-all"},
		// A destructuring assignment fills a variable as well.
		{"<button onclick={() => count++}>+</button>", "<button onclick={() => ([del] = ['b'])}>+</button>", "users-delete"},
	}
	for _, c := range cases {
		if !strings.Contains(guardedPage, c.old) {
			t.Fatalf("not in the page: %q", c.old)
		}
		w := roleWorld(t, strings.Replace(guardedPage, c.old, c.new, 1))
		got := strings.Join(w.webRoleProblems(), "\n")
		if !strings.Contains(got, c.want) {
			t.Errorf("%q → %q: no problem mentions %q; got:\n%s", c.old, c.new, c.want, got)
		}
	}
}

func TestRolesConditions(t *testing.T) {
	f := parseWebFile("routes/u/+page.svelte", `<script lang="ts">
  import { auth } from '$lib/state.svelte';
  const admin = $derived(auth.me?.role === 'admin');
  const isAdmin = auth.me?.role == 'admin';
  const inverted = $derived(auth.me?.role === 'admin' ? false : true);
  let del = $state(null);
  function own(auth: any) { if (auth.me.role === 'admin') x(); }
</script>
{#if isAdmin}<p id="i">i</p>{/if}
{#if inverted}<p>j</p>{/if}
{#if !admin}<p>a</p>{:else if x}<p id="b">b</p>{:else}<p id="c">c</p>{/if}
{#if a}<p>d</p>{:else if admin}<p id="e">e</p>{/if}
{#if auth.me?.role !== 'admin' || other}<p>f</p>{:else}<p id="g">g</p>{/if}
{#if del?.unix_uid && y}<p id="h">h</p>{/if}
{#if u.role === 'admin'}<p>k</p>{/if}`)
	nav := parseWebFile("lib/components/Nav.svelte", `<script>const x = ['/u'].includes(h);</script>`)
	g, why := newRoles(map[string]*webFile{f.path: f, nav.path: nav})
	if g == nil {
		t.Fatal(why)
	}
	for id, want := range map[string]bool{"b": true, "c": true, "e": true, "g": true, "h": true, "i": true} {
		at := strings.Index(f.code, `id="`+id+`"`)
		if got := g.guarded(f, at); got != want {
			t.Errorf("<p id=%q>: guarded %v, want %v", id, got, want)
		}
	}
	for _, text := range []string{"<p>a</p>", "<p>d</p>", "<p>f</p>", "<p>j</p>", "<p>k</p>", "x();"} {
		if g.guarded(f, strings.Index(f.code, text)) {
			t.Errorf("%s is not guarded (a row's role, a parameter named auth)", text)
		}
	}
}

// fills: where a name gets a value that may be something.
func TestFills(t *testing.T) {
	l := flowOf(parseWebFile("lib/x.ts", `a = 1; b = null; c++; [d, e] = pair; ({ f, g: h } = obj); [{ i }] = list; for (j of xs) {}
x[k] = 1; m.n = 2; o === 1; [p]; q = { r }; s(t); u[v] = 1; w = ''; y += 1; return [z];`)).lists[0]
	want := map[string]bool{"a": true, "b": false, "c": true, "d": true, "e": true, "f": true, "h": true, "i": true, "j": true,
		"x": false, "k": false, "m": false, "o": false, "p": false, "r": false, "t": false, "v": false, "w": false, "y": true, "z": false}
	seen := map[string]bool{}
	for i, tk := range l.toks {
		if v, ok := want[tk.text]; ok && tk.kind == tIdent && !l.member(i) {
			seen[tk.text] = true
			if got := fills(l, i); got != v {
				t.Errorf("%s: fills %v, want %v", tk.text, got, v)
			}
		}
	}
	if len(seen) != len(want) {
		t.Errorf("names checked: %v", sortedKeys(seen))
	}
}

// Functions that call each other are a way in when any of them is: a cycle
// entered from an ordinary account's button is no guard, however the
// working-out goes round it.
func TestRolesCycles(t *testing.T) {
	const cycle = `<script lang="ts">
  import { api } from '$lib/api';
  import { auth } from '$lib/state.svelte';
  const admin = $derived(auth.me?.role === 'admin');
  async function a() { await api('/users'); c(); }
  function b() { a(); }
  async function c() { await api('/doctor'); b(); }
  function d() { e(); }
  function e() { if (count) d(); api('/webhooks'); }
</script>
BUTTONS`
	ours := func(list []string) []string {
		var out []string
		for _, s := range list {
			if strings.Contains(s, "routes/users/+page.svelte") {
				out = append(out, s)
			}
		}
		return out
	}
	open := roleWorld(t, strings.Replace(cycle, "BUTTONS", `<button onclick={b}>b</button>{#if admin}<button onclick={d}>d</button>{/if}`, 1))
	got := strings.Join(ours(open.webRoleProblems()), "\n")
	for _, want := range []string{"users-list", "doctor"} {
		if !strings.Contains(got, want) {
			t.Errorf("a → c → b → a is entered from an unguarded button, but no problem mentions %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "hooks") {
		t.Errorf("d ⇄ e is entered from a guarded button only:\n%s", got)
	}
	closed := roleWorld(t, strings.Replace(cycle, "BUTTONS", `{#if admin}<button onclick={b}>b</button><button onclick={d}>d</button>{/if}`, 1))
	if got := ours(closed.webRoleProblems()); len(got) > 0 {
		t.Errorf("both cycles are entered from guarded buttons only:\n%s", strings.Join(got, "\n"))
	}
}

// A component's admin prop is the role only where every page sets it from
// the role: a default of true makes every page that leaves it out an
// administrator's.
func TestRolesAdminDefault(t *testing.T) {
	card := func(def string) map[string]string {
		return map[string]string{"routes/users/Card.svelte": `<script lang="ts">
  import { api } from '$lib/api';
  let { admin = ` + def + ` } = $props();
  async function reset() { await api('/totp', { method: 'POST' }); }
</script>
{#if admin}<button onclick={reset}>reset</button>{/if}`}
	}
	const page = `<script lang="ts">import Card from './Card.svelte';</script><Card />`
	for def, want := range map[string]bool{"true": true, "false": false} {
		got := strings.Join(roleWorld(t, page, card(def)).webRoleProblems(), "\n")
		if strings.Contains(got, "Card.svelte") != want {
			t.Errorf("admin = %s by default, a page leaves it out: reported %v, want %v:\n%s", def, !want, want, got)
		}
	}
}

// A request built where the reading cannot follow it may set a field only
// administrators may set: that is reported unless the whole call is theirs.
func TestRolesOpenRequest(t *testing.T) {
	const page = `<script lang="ts">
  import { api } from '$lib/api';
  import { auth } from '$lib/state.svelte';
  import { siteBody } from './lib';
  const admin = $derived(auth.me?.role === 'admin');
  async function create() { await api('/sites', { method: 'POST', json: siteBody() }); }
</script>
BUTTON`
	got := strings.Join(roleWorld(t, strings.Replace(page, "BUTTON", `<button onclick={create}>+</button>`, 1)).webRoleProblems(), "\n")
	if !strings.Contains(got, "site-create") || !strings.Contains(got, "cannot tell whether body user") || !strings.Contains(got, "siteBody(…)") {
		t.Errorf("an imported body of POST /sites on a shared page: %s", got)
	}
	got = strings.Join(roleWorld(t, strings.Replace(page, "BUTTON", `{#if admin}<button onclick={create}>+</button>{/if}`, 1)).webRoleProblems(), "\n")
	if strings.Contains(got, "site-create") {
		t.Errorf("the whole call is for administrators: %s", got)
	}
}
