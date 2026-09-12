<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { auth, notify } from '$lib/state.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Icon from '$lib/components/Icon.svelte';
  // The console runs mp on the server with this account's rights and streams
  // the output; the same binary and the same commands as over ssh.
  let line = $state('');
  let output = $state('');
  let running = $state(false);
  let history = $state<string[]>([]);
  let cursor = $state(-1);
  let pre: HTMLPreElement | undefined = $state();
  let ctrl: AbortController | null = null;
  const quick = ['doctor', 'status', 'site list', 'job list', 'cms list', 'selinux', 'db list', 'php list', 'stack list'];
  onMount(() => { try { history = JSON.parse(localStorage.getItem('mp.console.history') || '[]'); } catch { /* fresh browser */ } });
  // shell-like split: quotes group words, nothing else is interpreted
  function split(s: string): string[] {
    const out: string[] = []; let cur = ''; let q = ''; let has = false;
    for (const ch of s) {
      if (q) { if (ch === q) q = ''; else cur += ch; continue; }
      if (ch === '"' || ch === "'") { q = ch; has = true; continue; }
      if (/\s/.test(ch)) { if (cur || has) { out.push(cur); cur = ''; has = false; } continue; }
      cur += ch;
    }
    if (cur || has) out.push(cur);
    return out;
  }
  async function run(cmd?: string) {
    const text = (cmd ?? line).trim().replace(/^mp\s+/, '');
    if (!text || running) return;
    let args = split(text);
    if (!args.length) return;
    line = ''; cursor = -1;
    history = [text, ...history.filter((h) => h !== text)].slice(0, 100);
    try { localStorage.setItem('mp.console.history', JSON.stringify(history)); } catch { /* ignore */ }
    running = true; ctrl = new AbortController();
    try {
      const res = await fetch('/api/v1/system/console', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ args }), signal: ctrl.signal });
      if (!res.ok || !res.body) { const t = await res.text(); let msg = t; try { msg = JSON.parse(t).detail || t; } catch { /* plain */ } output += `$ mp ${text}\n${msg}\n\n`; notify(msg, 'err'); return; }
      const reader = res.body.getReader(); const dec = new TextDecoder();
      for (;;) { const { value, done } = await reader.read(); if (done) break; output += dec.decode(value, { stream: true }); await tick(); if (pre) pre.scrollTop = pre.scrollHeight; }
      output += '\n';
    } catch (e: any) { if (e?.name !== 'AbortError') { output += `console: ${e?.message || e}\n`; } else { output += '[прервано]\n'; } }
    finally { running = false; ctrl = null; await tick(); if (pre) pre.scrollTop = pre.scrollHeight; }
  }
  function key(e: KeyboardEvent) {
    if (e.key === 'Enter') { e.preventDefault(); run(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); if (cursor + 1 < history.length) { cursor += 1; line = history[cursor]; } }
    else if (e.key === 'ArrowDown') { e.preventDefault(); if (cursor > 0) { cursor -= 1; line = history[cursor]; } else { cursor = -1; line = ''; } }
  }
</script>

<PageHead title="Консоль" sub="команды mp от имени {auth.me?.login ?? 'администратора'}: тот же бинарник и те же права, что по ssh; вывод приходит по мере выполнения">
  <button class="btn btn-sm" onclick={() => (output = '')} disabled={!output}><Icon name="trash" size={13} /> очистить</button>
</PageHead>
<div class="card rise">
  <div class="flex flex-wrap gap-1.5 mb-3">
    {#each quick as q}<button class="btn btn-sm font-mono" onclick={() => run(q)} disabled={running}>mp {q}</button>{/each}
  </div>
  <pre bind:this={pre} class="font-mono text-xs whitespace-pre-wrap break-words bg-ground rounded-lg p-3 h-[28rem] overflow-y-auto border border-line">{output || 'Введите команду, например doctor, или нажмите кнопку выше.'}</pre>
  <form class="flex gap-2 mt-3 items-center" onsubmit={(e) => { e.preventDefault(); run(); }}>
    <span class="font-mono text-sm text-muted shrink-0">$ mp</span>
    <!-- svelte-ignore a11y_autofocus -->
    <input class="input font-mono flex-1" bind:value={line} onkeydown={key} placeholder="site list, doctor, cms install example.com wordpress …" autocomplete="off" spellcheck="false" autofocus disabled={running} />
    {#if running}<button type="button" class="btn btn-danger btn-sm" onclick={() => ctrl?.abort()}><Icon name="stop" size={13} /> прервать</button>{:else}<button class="btn btn-primary btn-sm"><Icon name="play" size={13} /> выполнить</button>{/if}
  </form>
  <p class="text-xs text-muted mt-2">Служебные команды (api, agent, helper, fsop, setup) из консоли недоступны; --server и --token подставляет сама панель, каждая команда получает одноразовый токен.</p>
</div>
