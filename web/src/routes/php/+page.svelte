<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Icon from '$lib/components/Icon.svelte';
  let data = $state<any>(null);
  let job = $state<number | null>(null);
  let error = $state('');
  async function load() { try { data = await api('/php/versions'); } catch (e: any) { error = e.text || String(e); } }
  onMount(load);
  async function install(v: string) { error = ''; try { const r: any = await api('/php/versions', { method: 'POST', json: { version: v } }); job = r.job_id; } catch (e) { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); } }
  async function remove(v: string) { if (!confirm('Удалить PHP ' + v + '?')) return; error = ''; try { const r: any = await api(`/php/versions/${v}`, { method: 'DELETE' }); job = r.job_id; } catch (e) { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); } }
  const installed = $derived(Object.fromEntries((data?.installed || []).map((p: any) => [p.version, p])));
</script>

<PageHead title="PHP" sub="несколько веток параллельно, у каждой свой php-fpm; версия выбирается на сайт (Sury на Debian/Ubuntu, Remi на EL)" />
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
<div class="card overflow-x-auto p-0 rise">
  {#if !data}<Skeleton rows={6} />{:else}
  <table class="tbl">
    <thead><tr><th>Ветка</th><th>Upstream</th><th>Состояние</th><th>Пакет</th><th>Расширения</th><th></th></tr></thead>
    <tbody>
      {#each data.available as a, i}
        {@const p = installed[a.version]}
        <tr class="rise" style="--i:{i}">
          <td data-label="Ветка" class="font-mono font-medium">{a.version}</td>
          <td data-label="Upstream"><span class="tag {a.support === 'active' ? 'tag-ok' : a.support === 'security' ? 'tag-warn' : 'tag-muted'}">{a.support}</span></td>
          <td data-label="Состояние">{#if p}<span class="tag {p.status === 'installed' ? 'tag-ok' : p.status === 'error' ? 'tag-err' : 'tag-warn'}">{p.status}</span>{#if p.last_error}<div class="text-xs text-danger">{p.last_error}</div>{/if}{:else if !a.available}<span class="text-xs text-muted">{a.note}</span>{:else}<span class="text-muted">—</span>{/if}</td>
          <td data-label="Пакет" class="font-mono text-xs text-muted">{p?.package_version || ''}</td>
          <td data-label="Расширения" class="text-xs text-muted max-w-md">{p?.extensions?.length ? p.extensions.length + ': ' + p.extensions.join(', ') : ''}</td>
          <td data-label="" class="text-right">{#if p?.status === 'installed'}<button class="btn btn-danger btn-sm" onclick={() => remove(a.version)}><Icon name="trash" size={13} /></button>{:else if a.available}<button class="btn btn-sm" onclick={() => install(a.version)}><Icon name="plus" size={13} /> установить</button>{/if}</td>
        </tr>
      {/each}
    </tbody>
  </table>
  {/if}
</div>
