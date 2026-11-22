<script lang="ts">
  import { untrack } from 'svelte';
  import { api, ApiError, bytes } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import { t, tn, dateLocale } from '$lib/i18n/index.svelte';
  import Icon from './Icon.svelte';
  import Skeleton from './Skeleton.svelte';

  // Хвост лога или журнала. src — путь API, который на ?lines=N отвечает
  // {path, size?, lines}: /system/logs/{unit}, /users/{login}/apps/{name}/logs,
  // /sites/{domain}/logs/{type}. Строки идут как у journalctl — новые внизу,
  // поэтому вид держится у нижнего края, пока человек сам не прокрутит вверх.
  // max — потолок lines у этого пути в API (5000 у журнала и логов сайта,
  // 2000 у логов app-сервиса). Перечитать снаружи: bind:this и reload().
  type Tail = { path: string; size?: number; lines: string[] };
  let { src, lines = $bindable(200), max = 5000, title = '', empty = '' }: { src: string; lines?: number; max?: number; title?: string; empty?: string } = $props();

  const presets = [50, 100, 200, 500, 1000, 2000, 5000];
  const count = $derived(Math.min(Math.max(1, Math.trunc(lines) || 1), max));
  const choices = $derived([...new Set([...presets.filter((n) => n <= max), count])].sort((a, b) => a - b));
  // "Other…" opens a field for any count from 1 to max, as `mp logs -n` takes.
  let custom = $state(false);
  const focus = (el: HTMLInputElement) => {
    el.focus();
    el.select();
  };
  function commit(el: HTMLInputElement) {
    if (!custom) return; // Esc already closed it; the blur of the removed field must not apply the number
    const n = Math.trunc(el.valueAsNumber);
    if (n >= 1) lines = Math.min(n, max);
    custom = false;
  }
  let tail = $state<Tail | null>(null);
  let error = $state('');
  let loading = $state(false);
  let at = $state<Date | null>(null);
  let filter = $state('');
  let live = $state(false);
  let pre: HTMLPreElement | undefined = $state();
  let stick = true; // follow the newest line until the reader scrolls up
  let seq = 0; // only the answer to the latest request is shown
  let shownSrc = '';
  const canCopy = typeof navigator !== 'undefined' && !!navigator.clipboard;

  async function fetchTail() {
    const url = src;
    const my = ++seq;
    loading = true;
    try {
      const res = await api<Tail>(url + (url.includes('?') ? '&' : '?') + 'lines=' + count);
      if (my !== seq) return;
      tail = { ...res, lines: res.lines ?? [] };
      error = '';
      at = new Date();
    } catch (e) {
      if (my !== seq) return;
      error = e instanceof ApiError ? e.text : String(e);
    } finally {
      if (my === seq) loading = false;
    }
  }

  /** Read the log again, e.g. after an action that writes to it. */
  export function reload(): Promise<void> {
    return fetchTail();
  }

  // Another source starts from scratch; another line count keeps the view.
  $effect(() => {
    src;
    count;
    untrack(() => {
      if (src !== shownSrc) {
        shownSrc = src;
        tail = null;
        error = '';
        filter = '';
        stick = true;
      }
      fetchTail();
    });
  });

  // Live mode re-reads the tail while the tab is visible; a slow answer is not stacked.
  $effect(() => {
    if (!live) return;
    const id = setInterval(() => {
      if (!loading && document.visibilityState === 'visible') fetchTail();
    }, 5000);
    return () => clearInterval(id);
  });

  const needle = $derived(filter.trim().toLowerCase());
  const shown = $derived(needle ? (tail?.lines ?? []).filter((l) => l.toLowerCase().includes(needle)) : (tail?.lines ?? []));
  $effect(() => {
    shown;
    if (pre && stick) pre.scrollTop = pre.scrollHeight;
  });
  function onScroll() {
    if (pre) stick = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 24;
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(shown.join('\n'));
      notify(t('logView.copied'));
    } catch {
      notify(t('logView.copyFailed'), 'err');
    }
  }
</script>

<div class="min-w-0">
  <div class="flex flex-wrap items-center gap-2 mb-2">
    {#if title}<span class="font-medium">{title}</span>{/if}
    {#if tail}<span class="text-xs text-muted font-mono truncate min-w-0" title={tail.path}>{tail.path}{tail.size ? ' · ' + bytes(tail.size) : ''}</span>{/if}
    <div class="ml-auto flex flex-wrap items-center gap-2">
      <input class="input w-36 py-1 text-xs" type="search" bind:value={filter} placeholder={t('logView.filter')} aria-label={t('logView.filterLabel')} title={t('logView.filterLabel')} />
      {#if custom}
        <input class="input w-24 py-1 text-xs" type="number" min="1" {max} step="1" value={count} use:focus onblur={(e) => commit(e.currentTarget)} onkeydown={(e) => { if (e.key === 'Enter') { e.preventDefault(); commit(e.currentTarget); } else if (e.key === 'Escape') { e.stopPropagation(); custom = false; } }} aria-label={t('logView.linesLabel')} title={t('logView.linesCustomHint', { max })} />
      {:else}
        <select class="input w-auto py-1 text-xs" value={count} onchange={(e) => { if (e.currentTarget.value === 'custom') custom = true; else lines = Number(e.currentTarget.value); }} aria-label={t('logView.linesLabel')} title={t('logView.linesLabel')}>
          {#each choices as n}<option value={n}>{tn('logView.lines', n)}</option>{/each}
          <option value="custom">{t('logView.linesOther')}</option>
        </select>
      {/if}
      <label class="text-xs flex items-center gap-1.5 cursor-pointer" title={t('logView.liveHint')}><input type="checkbox" bind:checked={live} /> {t('logView.live')}</label>
      {#if canCopy}<button class="btn btn-sm" onclick={copy} disabled={!shown.length} title={t('logView.copyHint')}>{t('logView.copy')}</button>{/if}
      <button class="btn btn-sm" onclick={fetchTail} disabled={loading} title={t('common.refresh')} aria-label={t('common.refresh')}><Icon name="refresh" size={13} class={loading ? 'animate-spin' : ''} /></button>
    </div>
  </div>
  {#if error}<p class="text-danger text-xs mb-2 break-words">{error}</p>{/if}
  {#if tail}
    <pre bind:this={pre} onscroll={onScroll} role="log" class="code break-words max-h-[min(32rem,70vh)] transition-opacity {loading ? 'opacity-70' : ''}">{shown.length ? shown.join('\n') : needle ? t('logView.noMatch') : empty || t('logView.empty')}</pre>
    <div class="flex flex-wrap justify-between gap-2 text-[11px] text-muted font-mono mt-1">
      <span>{needle ? t('logView.shown', { shown: shown.length, total: tn('logView.lines', tail.lines.length) }) : tn('logView.lines', tail.lines.length)}</span>
      {#if at}<span>{t('logView.updated', { time: at.toLocaleTimeString(dateLocale()) })}</span>{/if}
    </div>
  {:else if loading}
    <Skeleton rows={6} />
  {/if}
</div>
