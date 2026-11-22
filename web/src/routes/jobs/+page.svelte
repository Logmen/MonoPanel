<script lang="ts">
  import { tick } from 'svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api, ApiError, when } from '$lib/api';
  import { t, type MsgKey } from '$lib/i18n/index.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Icon from '$lib/components/Icon.svelte';

  // Список фильтрует API — статус и сколько последних (до 500), как
  // mp job list --status --limit; задача за пределами списка открывается по
  // номеру (mp job show / mp job wait). Открытая задача живёт в адресе
  // (/jobs?id=42), так на неё можно сослаться с других страниц.
  const statuses: [string, MsgKey][] = [['', 'jobs.filterAll'], ['running', 'jobs.filterRunning'], ['queued', 'jobs.filterQueued'], ['failed', 'jobs.filterFailed'], ['done', 'jobs.filterDone'], ['cancelled', 'jobs.filterCancelled']];
  const limits = [50, 100, 200, 500];
  const wanted = page.url.searchParams.get('status') ?? '';
  let status = $state(statuses.some(([v]) => v === wanted) ? wanted : '');
  let limit = $state(100);
  let jobs = $state<any[] | null>(null);
  let error = $state('');
  let idInput = $state<number | null>(null);
  let box = $state<HTMLDivElement | null>(null);

  // Ответ на старый фильтр, пришедший после нового, не должен его перезаписать.
  let seq = 0;
  async function load() {
    const n = ++seq;
    const q = new URLSearchParams({ limit: String(limit) });
    if (status) q.set('status', status);
    try {
      const list = await api<any[]>('/jobs?' + q);
      if (n !== seq) return;
      jobs = list;
      error = '';
      if (openId && !detailError && (!detail || live(detail.status))) loadDetail(openId);
    } catch (e) {
      if (n === seq) error = e instanceof ApiError ? e.text : String(e);
    }
  }
  $effect(() => {
    status;
    limit;
    load();
    const timer = setInterval(load, 5000);
    return () => clearInterval(timer);
  });

  // GET /jobs/{id} — задача целиком, с журналом (mp job show). Пока она в
  // очереди или идёт, журнал ведёт JobLog по SSE (mp job wait); у завершённой
  // он берётся отсюда: поток завершённой задачи отдаёт снимок и закрывается.
  const openId = $derived(Number(page.url.searchParams.get('id')) || 0);
  let detail = $state<any>(null);
  let detailError = $state('');
  const live = (s: string) => s === 'queued' || s === 'running';
  async function loadDetail(id: number) {
    try {
      const j = await api<any>(`/jobs/${id}`);
      if (id === openId) { detail = j; detailError = ''; }
    } catch (e) {
      if (id === openId) { detail = null; detailError = e instanceof ApiError ? e.text : String(e); }
    }
  }
  $effect(() => {
    const id = openId;
    detail = null;
    detailError = '';
    if (id) loadDetail(id);
  });
  async function show(id: number) {
    await goto(id ? `/jobs?id=${id}` : '/jobs', { replaceState: true, noScroll: true, keepFocus: true });
    if (!id) return;
    await tick();
    box?.scrollIntoView({ block: 'nearest', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
  }
  function openTyped(e: Event) {
    e.preventDefault();
    const n = Math.trunc(Number(idInput));
    if (n > 0) { show(n); idInput = null; }
  }

  const tagOf = (s: string) => (s === 'done' ? 'tag-ok' : s === 'failed' ? 'tag-err' : s === 'running' ? 'tag-accent' : s === 'cancelled' ? 'tag-muted' : 'tag-warn');
  function took(a?: string | null, b?: string | null): string {
    if (!a || !b) return '';
    const sec = Math.max(0, Math.round((new Date(b).getTime() - new Date(a).getTime()) / 1000));
    if (isNaN(sec)) return '';
    if (sec < 60) return t('jobs.durSec', { s: sec });
    if (sec < 3600) return t('jobs.durMin', { m: Math.floor(sec / 60), s: sec % 60 });
    return t('jobs.durHour', { h: Math.floor(sec / 3600), m: Math.floor((sec % 3600) / 60) });
  }
  const payload = (p: unknown) => (p === undefined || p === null ? '—' : JSON.stringify(p, null, 2));
  // Журнал завершённой задачи открывается на последних строках, как живой.
  let logPre = $state<HTMLPreElement | null>(null);
  $effect(() => { detail?.log; if (logPre) logPre.scrollTop = logPre.scrollHeight; });
</script>

<PageHead title={t('jobs.title')} sub={t('jobs.sub')}>
  <div class="flex flex-wrap gap-1">{#each statuses as [v, k]}<button class="btn btn-sm {status === v ? 'btn-primary' : 'btn-ghost'}" aria-pressed={status === v} onclick={() => (status = v)}>{t(k)}</button>{/each}</div>
  <select class="input w-auto py-1 text-xs" bind:value={limit} aria-label={t('jobs.limitLabel')} title={t('jobs.limitLabel')}>{#each limits as n}<option value={n}>{t('jobs.limitOption', { n })}</option>{/each}</select>
  <form class="flex gap-1" onsubmit={openTyped}>
    <input class="input w-24 py-1 text-xs" type="number" min="1" step="1" bind:value={idInput} placeholder={t('jobs.idPlaceholder')} aria-label={t('jobs.openLabel')} />
    <button class="btn btn-sm" disabled={!idInput}>{t('jobs.open')}</button>
  </form>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if openId}
  <div class="grid lg:grid-cols-5 gap-4 mb-4 scroll-mt-4" bind:this={box}>
    {#if !detailError}<div class="lg:col-span-3 min-w-0">
      {#if detail && live(detail.status)}
        {#key openId}<JobLog jobId={openId} onfinish={() => loadDetail(openId)} />{/key}
      {:else if detail}
        <div class="card font-mono text-xs rise">
          <div class="flex items-center gap-3">
            <span class="text-muted">{t('jobs.log')}</span>
            <div class="flex-1 progress"><i style="width:{Math.max(2, detail.progress)}%"></i></div>
            <span class="tabular-nums w-9 text-right text-muted">{detail.progress}%</span>
            <span class="tag {tagOf(detail.status)}">{detail.status}</span>
          </div>
          {#if detail.message}<div class="text-muted mt-2 break-words">{detail.message}</div>{/if}
          {#if detail.log}<pre bind:this={logPre} class="code mt-2 max-h-96">{detail.log}</pre>{:else}<p class="text-muted mt-2">{t('jobs.noLog')}</p>{/if}
          {#if detail.error}<div class="text-danger mt-2 break-words whitespace-pre-wrap">{detail.error}</div>{/if}
        </div>
      {:else}<div class="card p-0"><Skeleton rows={4} /></div>{/if}
    </div>{/if}
    <div class="card {detailError ? 'lg:col-span-5' : 'lg:col-span-2'} min-w-0 text-sm rise">
      <div class="flex items-center gap-2 mb-3">
        <span class="font-medium">{t('jobs.detailTitle', { id: openId })}</span>
        {#if detail}<span class="tag {tagOf(detail.status)}">{#if detail.status === 'running'}<span class="dot dot-live"></span>{/if}{detail.status}</span>{/if}
        <button class="btn btn-ghost btn-sm ml-auto" onclick={() => show(0)} title={t('common.close')} aria-label={t('common.close')}><Icon name="x" size={14} /></button>
      </div>
      {#if detailError}<p class="text-danger">{detailError}</p>
      {:else if !detail}<Skeleton rows={4} />
      {:else}
        <dl class="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1.5 text-xs">
          <dt class="text-muted">{t('jobs.colType')}</dt><dd class="font-mono break-all">{detail.type}</dd>
          <dt class="text-muted">{t('jobs.object')}</dt><dd class="font-mono break-all">{detail.lock_key || '—'}</dd>
          <dt class="text-muted">{t('jobs.colWho')}</dt><dd class="break-all">{detail.requested_by || '—'}</dd>
          <dt class="text-muted">{t('jobs.colCreated')}</dt><dd>{when(detail.created_at)}</dd>
          <dt class="text-muted">{t('jobs.started')}</dt><dd>{when(detail.started_at)}</dd>
          <dt class="text-muted">{t('jobs.finished')}</dt><dd>{when(detail.finished_at)}{#if took(detail.started_at, detail.finished_at)}<span class="text-muted">{' · ' + t('jobs.took', { d: took(detail.started_at, detail.finished_at) })}</span>{/if}</dd>
          {#if detail.worker}<dt class="text-muted">{t('jobs.worker')}</dt><dd class="font-mono break-all">{detail.worker}</dd>{/if}
          {#if detail.idempotency_key}<dt class="text-muted">{t('jobs.idempotency')}</dt><dd class="font-mono break-all">{detail.idempotency_key}</dd>{/if}
        </dl>
        <div class="label mt-4">{t('jobs.payload')}</div>
        <pre class="code max-h-64">{payload(detail.payload)}</pre>
        <p class="text-[11px] text-muted mt-1">{t('jobs.payloadHint')}</p>
      {/if}
    </div>
  </div>
{/if}
<div class="card overflow-x-auto p-0 rise">
  {#if !jobs}<Skeleton rows={6} />{:else}
  <table class="tbl"><thead><tr><th>ID</th><th>{t('jobs.colType')}</th><th>{t('common.status')}</th><th class="w-28">{t('jobs.colProgress')}</th><th>{t('jobs.colWho')}</th><th>{t('jobs.colCreated')}</th><th>{t('jobs.colMessage')}</th></tr></thead>
    <tbody>
      {#each jobs as j, i (j.id)}<tr class="cursor-pointer rise {openId === j.id ? 'bg-accent-soft/40' : ''}" style="--i:{Math.min(i, 12)}" onclick={() => show(j.id)}><td data-label="ID" class="font-mono text-muted">{j.id}</td><td data-label={t('jobs.colType')} class="font-mono">{j.type}</td><td data-label={t('common.status')}><span class="tag {tagOf(j.status)}">{#if j.status === 'running'}<span class="dot dot-live"></span>{/if}{j.status}</span></td><td data-label={t('jobs.colProgress')}><div class="progress {j.status === 'running' ? 'running' : ''}"><i style="width:{j.progress}%"></i></div></td><td data-label={t('jobs.colWho')} class="text-muted">{j.requested_by}</td><td data-label={t('jobs.colCreated')} class="text-xs text-muted whitespace-nowrap">{when(j.created_at)}</td><td data-label={t('jobs.colMessage')} class="text-xs max-w-md truncate {j.error ? 'text-danger' : 'text-muted'}" title={j.error || j.message}>{j.error || j.message}</td></tr>{/each}
      {#if !jobs.length}<tr><td colspan="7" class="text-muted text-center py-6">{status ? t('jobs.emptyStatus') : t('jobs.empty')}</td></tr>{/if}
    </tbody></table>
  {/if}
</div>
{#if jobs?.length}<p class="text-xs text-muted mt-3">{jobs.length >= limit ? t('jobs.shownLimit', { n: jobs.length }) : t('jobs.shown', { n: jobs.length })}</p>{/if}
