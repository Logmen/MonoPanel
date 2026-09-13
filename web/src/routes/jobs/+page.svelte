<script lang="ts">
  import { onMount } from 'svelte';
  import { api, when } from '$lib/api';
  import { t, type MsgKey } from '$lib/i18n/index.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  let jobs = $state<any[] | null>(null);
  let open = $state<number | null>(null);
  let error = $state('');
  let filter = $state('');
  async function load() { try { jobs = await api('/jobs?limit=100'); } catch (e: any) { error = e.text || String(e); } }
  onMount(() => { load(); const timer = setInterval(load, 5000); return () => clearInterval(timer); });
  const shown = $derived((jobs || []).filter((j) => !filter || j.status === filter));
  const filters: [string, MsgKey][] = [['', 'jobs.filterAll'], ['running', 'jobs.filterRunning'], ['failed', 'jobs.filterFailed'], ['done', 'jobs.filterDone']];
</script>

<PageHead title={t('jobs.title')} sub={t('jobs.sub')}>
  <div class="flex gap-1">{#each filters as [v, k]}<button class="btn btn-sm {filter === v ? 'btn-primary' : 'btn-ghost'}" onclick={() => (filter = v)}>{t(k)}</button>{/each}</div>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if open}<div class="mb-4"><JobLog jobId={open} /></div>{/if}
<div class="card overflow-x-auto p-0 rise">
  {#if !jobs}<Skeleton rows={6} />{:else}
  <table class="tbl"><thead><tr><th>ID</th><th>{t('jobs.colType')}</th><th>{t('common.status')}</th><th class="w-28">{t('jobs.colProgress')}</th><th>{t('jobs.colWho')}</th><th>{t('jobs.colCreated')}</th><th>{t('jobs.colMessage')}</th></tr></thead>
    <tbody>
      {#each shown as j, i}<tr class="cursor-pointer rise {open === j.id ? 'bg-accent-soft/40' : ''}" style="--i:{Math.min(i, 12)}" onclick={() => (open = j.id)}><td data-label="ID" class="font-mono text-muted">{j.id}</td><td data-label={t('jobs.colType')} class="font-mono">{j.type}</td><td data-label={t('common.status')}><span class="tag {j.status === 'done' ? 'tag-ok' : j.status === 'failed' ? 'tag-err' : j.status === 'running' ? 'tag-accent' : 'tag-warn'}">{#if j.status === 'running'}<span class="dot dot-live"></span>{/if}{j.status}</span></td><td data-label={t('jobs.colProgress')}><div class="progress {j.status === 'running' ? 'running' : ''}"><i style="width:{j.progress}%"></i></div></td><td data-label={t('jobs.colWho')} class="text-muted">{j.requested_by}</td><td data-label={t('jobs.colCreated')} class="text-xs text-muted whitespace-nowrap">{when(j.created_at)}</td><td data-label={t('jobs.colMessage')} class="text-xs max-w-md truncate {j.error ? 'text-danger' : 'text-muted'}" title={j.error || j.message}>{j.error || j.message}</td></tr>{/each}
      {#if !shown.length}<tr><td colspan="7" class="text-muted text-center py-6">{t('jobs.empty')}</td></tr>{/if}
    </tbody></table>
  {/if}
</div>
