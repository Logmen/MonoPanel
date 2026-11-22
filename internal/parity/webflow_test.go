package parity

import (
	"slices"
	"strings"
	"testing"
)

// declsOf lists a file's declarations as "kind name", sorted, for the
// kinds asked for.
func declsOf(f *webFile, kinds ...declKind) []string {
	fl := flowOf(f)
	var out []string
	for name, list := range fl.byName {
		for _, d := range list {
			if slices.Contains(kinds, d.kind) {
				out = append(out, map[declKind]string{dVar: "var", dFunc: "func", dParam: "param", dLoop: "loop", dProp: "prop", dImport: "import"}[d.kind]+" "+name)
			}
		}
	}
	slices.Sort(out)
	return out
}

func TestFlowDeclarations(t *testing.T) {
	f := parseWebFile("routes/x/+page.svelte", `<script lang="ts">
  import { api, type Thing } from '$lib/api';
  import Panel from './Panel.svelte';
  let { login = '', admin = false, nonce = 0 }: { login?: string; admin?: boolean; nonce?: number } = $props();
  let tested = $state<Record<string, { at?: string; err?: string }>>({});
  const s = site, o = saved, b: Record<string, unknown> = {};
  const [first, second] = pair;
  function load() { for (const k of ['a', 'b'] as const) use(k); }
  const ask = (x: any): Ask => ({ run: () => go(x) });
  const later = async () => { await load(); };
  export function shown(v) { return v; }
</script>
<p>{tested}</p>`)
	got := declsOf(f, dVar, dFunc, dProp, dImport, dLoop)
	want := []string{"func load", "func shown", "import Panel", "import Thing", "import api", "loop k", "prop admin", "prop login", "prop nonce",
		"var ask", "var b", "var first", "var later", "var o", "var s", "var second", "var tested"}
	if !slices.Equal(got, want) {
		t.Errorf("declarations:\n got %q\nwant %q", got, want)
	}
	fl := flowOf(f)
	// The type argument with an object type in it is skipped whole: the
	// initializer is the call, and no declaration named "string" appears.
	d := fl.byName["tested"][0]
	if got := tokensText(d.list.slice(d.init)); got != "$state<Record<string,{at?:string;err?:string}>>({})" {
		t.Errorf("initializer of tested: %s", got)
	}
	if d := fl.byName["b"][0]; tokensText(d.list.slice(d.init)) != "{}" {
		t.Errorf("the third declarator's initializer: %s", tokensText(d.list.slice(d.init)))
	}
	if d := fl.byName["first"][0]; d.hasInit() {
		t.Error("a name out of a pattern holds a part of the value, not the initializer")
	}
	if fl.byName["ask"][0].fn == nil || fl.byName["later"][0].fn == nil || fl.byName["ask"][0].fn.name != "ask" {
		t.Error("an arrow assigned to a name is that name's function")
	}
	if p := fl.byName["x"]; len(p) != 1 || p[0].kind != dParam || p[0].owner != fl.byName["ask"][0].fn {
		t.Errorf("the arrow's parameter behind a return type: %+v", p)
	}
	if imp := fl.byName["Panel"][0]; imp.from != "routes/x/Panel.svelte" {
		t.Errorf("import source: %q", imp.from)
	}
}

func TestFlowResolveAndReturns(t *testing.T) {
	f := parseWebFile("lib/x.ts", `const body = { a: 1 };
function make(flag) {
  const body = { b: 2 };
  if (flag) return body;
  const inner = () => { return { nope: 1 }; };
  return { c: 3 };
}
function use() { return body; }
make(true); make(false);
const cb = make;
`)
	fl := flowOf(f)
	inMake := strings.Index(f.code, "return body;")
	inUse := strings.LastIndex(f.code, "return body;")
	if d := fl.resolve("body", inMake); d == nil || !strings.Contains(tokensText(d.list.slice(d.init)), "b:2") {
		t.Errorf("body inside make is make's own: %v", d)
	}
	if d := fl.resolve("body", inUse); d == nil || !strings.Contains(tokensText(d.list.slice(d.init)), "a:1") {
		t.Errorf("body inside use is the top-level one: %v", d)
	}
	mk := fl.byName["make"][0]
	var rets []string
	for _, r := range mk.fn.returns() {
		rets = append(rets, tokensText(mk.list.slice(r)))
	}
	if !slices.Equal(rets, []string{"body", "{c:3}"}) {
		t.Errorf("returns of make (not of the arrow inside it): %q", rets)
	}
	calls, passed := fl.callsOf(mk)
	if len(calls) != 2 || !passed {
		t.Errorf("make is called twice and handed over once: %d calls, passed=%v", len(calls), passed)
	}
	if a := calls[0].args; len(a) != 1 || tokensText(calls[0].l.slice(a[0])) != "true" {
		t.Errorf("arguments of the first call: %v", a)
	}
}

func TestFlowStatements(t *testing.T) {
	src := `if (a) { x(); } else if (b) y(); else { z(); }
if (!admin) return;
try { p(); } catch (e) { q(); } finally { r(); }
const v = 1
const w = 2`
	l := flowOf(parseWebFile("lib/x.ts", src)).lists[0]
	// The first statement runs to the end of its last else.
	if e := l.stmtEnd(0); !l.tok(e).is("}") || l.tok(e+1).text != "if" {
		t.Errorf("if … else if … else ends at %d (%q)", e, l.tok(e).text)
	}
	k := l.nextStmt(0)
	e := l.stmtEnd(k)
	s := l.after(k + 1)
	if !l.exits(s, l.stmtEnd(s)) || l.tok(e).text != ";" {
		t.Errorf("if (!admin) return; — its statement exits: %d %q", e, l.tok(e).text)
	}
	k = l.nextStmt(k)
	if e := l.stmtEnd(k); !l.tok(e).is("}") || l.tok(e+1).text != "const" {
		t.Errorf("try … catch … finally: ends before %q", l.tok(e+1).text)
	}
	k = l.nextStmt(k)
	if e := l.stmtEnd(k); l.tok(e).text != "1" {
		t.Errorf("a statement ends at a line that starts another one: %q", l.tok(e).text)
	}
}

func TestComponentUsages(t *testing.T) {
	page := parseWebFile("routes/u/+page.svelte", `<script lang="ts">
  import Panel from './Panel.svelte';
  import Modal from '$lib/components/Modal.svelte';
  const admin = $derived(auth.me?.role === 'admin');
</script>
<Panel login={me.login} {admin} bind:open />
<Modal open={!!del} onclose={() => (del = null)}>
  <button onclick={() => x > 1 && go()}>go</button>
  <Modal open={inner}>nested</Modal>
</Modal>
<Panel admin {...rest} />`)
	panel := parseWebFile("routes/u/Panel.svelte", `<script lang="ts">let { login = '', admin = false, open = $bindable(false) } = $props();</script>`)
	modal := parseWebFile("lib/components/Modal.svelte", `<script lang="ts">let { open = false, children } = $props();</script>{#if open}{@render children?.()}{/if}`)
	us := usages(map[string]*webFile{page.path: page, panel.path: panel, modal.path: modal})
	if len(us["routes/u/Panel.svelte"]) != 2 || len(us["lib/components/Modal.svelte"]) != 2 {
		t.Fatalf("usages: %v", us)
	}
	first := us["routes/u/Panel.svelte"][0]
	if a := first.attr("login"); !a.given || a.value == nil || tokensText(a.value.toks) != "me.login" {
		t.Errorf("login={me.login}: %+v", a)
	}
	if a := first.attr("admin"); !a.given || a.value == nil || tokensText(a.value.toks) != "admin" {
		t.Errorf("{admin}: %+v", a)
	}
	if a := first.attr("open"); !a.given || !a.bound {
		t.Errorf("bind:open: %+v", a)
	}
	if a := first.attr("nonce"); a.given {
		t.Errorf("nonce is not given: %+v", a)
	}
	second := us["routes/u/Panel.svelte"][1]
	if a := second.attr("admin"); !a.given || !a.bare || !a.spread {
		t.Errorf("admin alone and a spread: %+v", a)
	}
	outer := us["lib/components/Modal.svelte"][0]
	end := closeTag(page, outer)
	if end < 0 || !strings.HasPrefix(page.code[end:], "</Modal>\n<Panel") {
		t.Errorf("the outer Modal closes after the nested one (and the > of => does not end its tag): %q", page.code[max(end, 0):])
	}
	if !strings.Contains(page.code[outer.pos:outer.end], "onclose") || strings.Contains(page.code[outer.pos:outer.end], "button") {
		t.Errorf("the tag's end: %q", page.code[outer.pos:outer.end])
	}
}
