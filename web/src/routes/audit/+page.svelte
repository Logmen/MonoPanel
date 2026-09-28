<script lang="ts">
  import { onMount } from 'svelte';
  import { api, when } from '$lib/api';
  import { t, type MsgKey } from '$lib/i18n/index.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  let entries = $state<any[] | null>(null);
  let error = $state('');
  let filter = $state('');
  // Фильтры — префикс действия на сервере, «отказы» — по результату здесь.
  const filters: [string, MsgKey][] = [['', 'audit.filterAll'], ['auth.', 'audit.filterAuth'], ['token.', 'audit.filterTokens'], ['denied', 'audit.filterDenied']];
  async function load() {
    try {
      const action = filter === 'denied' ? '' : filter;
      entries = await api(`/system/audit?limit=200${action ? `&action=${encodeURIComponent(action)}` : ''}`);
    } catch (e: any) { error = e.text || String(e); }
  }
  onMount(() => { load(); const timer = setInterval(load, 10000); return () => clearInterval(timer); });
  $effect(() => { filter; load(); });
  const shown = $derived((entries || []).filter((e) => filter !== 'denied' || e.result !== 'ok'));
  const details = (d: Record<string, unknown> | undefined) => d ? Object.entries(d).map(([k, v]) => `${k}=${v}`).join(' ') : '';
</script>

<PageHead title={t('audit.title')} sub={t('audit.sub')}>
  <div class="flex gap-1">{#each filters as [v, k]}<button class="btn btn-sm {filter === v ? 'btn-primary' : 'btn-ghost'}" onclick={() => (filter = v)}>{t(k)}</button>{/each}</div>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
<div class="card overflow-x-auto p-0 rise">
  {#if !entries}<Skeleton rows={6} />{:else}
  <table class="tbl"><thead><tr><th>{t('audit.colTime')}</th><th>{t('audit.colWho')}</th><th>{t('audit.colAction')}</th><th>{t('audit.colTarget')}</th><th>{t('audit.colIP')}</th><th>{t('audit.colResult')}</th><th>{t('audit.colDetails')}</th></tr></thead>
    <tbody>
      {#each shown as e, i}<tr class="rise" style="--i:{Math.min(i, 12)}"><td data-label={t('audit.colTime')} class="text-xs text-muted whitespace-nowrap">{when(e.ts)}</td><td data-label={t('audit.colWho')}>{e.actor}</td><td data-label={t('audit.colAction')} class="font-mono">{e.action}</td><td data-label={t('audit.colTarget')} class="font-mono text-muted">{e.target || ''}</td><td data-label={t('audit.colIP')} class="font-mono text-muted">{e.ip || ''}</td><td data-label={t('audit.colResult')}><span class="tag {e.result === 'ok' ? 'tag-ok' : 'tag-err'}">{e.result}</span></td><td data-label={t('audit.colDetails')} class="text-xs text-muted max-w-md truncate" title={details(e.details)}>{details(e.details)}</td></tr>{/each}
      {#if !shown.length}<tr><td colspan="7" class="text-muted text-center py-6">{t('audit.empty')}</td></tr>{/if}
    </tbody></table>
  {/if}
</div>
<p class="text-xs text-muted mt-3">{t('audit.file')}</p>
