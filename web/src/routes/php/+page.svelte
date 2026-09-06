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

  // Расширения ветки: php-fpm — один мастер на версию, поэтому включение и
  // выключение действуют на все сайты этой ветки сразу.
  let openExt = $state<string | null>(null);
  let exts = $state<Record<string, any[]>>({});
  let extBusy = $state('');

  async function toggleExt(version: string) {
    if (openExt === version) { openExt = null; return; }
    openExt = version;
    if (exts[version]) return;
    try {
      const r: any = await api(`/php/versions/${version}/extensions`);
      exts = { ...exts, [version]: r.extensions };
    } catch (e) {
      error = e instanceof ApiError ? e.text : String(e);
      notify(error, 'err');
      openExt = null;
    }
  }

  async function setExt(version: string, name: string, enabled: boolean, critical: boolean) {
    if (!enabled && critical && !confirm(`${name} нужен типовому сайту. Выключить для всех сайтов на PHP ${version}?`)) return;
    extBusy = version + name;
    try {
      const r: any = await api(`/php/versions/${version}/extensions`, { method: 'POST', json: { name, enabled } });
      exts = { ...exts, [version]: r.extensions };
      notify(`${name}: ${enabled ? 'включено' : 'выключено'}, php-fpm ${version} перезапущен`);
    } catch (e) {
      error = e instanceof ApiError ? e.text : String(e);
      notify(error, 'err');
    } finally {
      extBusy = '';
    }
  }
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
          <td data-label="Расширения" class="text-xs text-muted max-w-md">
            {#if p?.status === 'installed'}
              <button class="inline-flex items-center gap-1 hover:text-ink transition-colors" onclick={() => toggleExt(a.version)}>
                <Icon name="chevron" size={13} class="transition-transform {openExt === a.version ? 'rotate-90' : ''}" />
                {exts[a.version] ? exts[a.version].filter((e) => e.enabled).length + ' из ' + exts[a.version].length : (p.extensions?.length ?? 0) + ' пакетов'}
              </button>
            {/if}
          </td>
          <td data-label="" class="text-right">{#if p?.status === 'installed'}<button class="btn btn-danger btn-sm" onclick={() => remove(a.version)}><Icon name="trash" size={13} /></button>{:else if a.available}<button class="btn btn-sm" onclick={() => install(a.version)}><Icon name="plus" size={13} /> установить</button>{/if}</td>
        </tr>
        {#if openExt === a.version}
          <tr>
            <td data-label="" colspan="6" class="bg-surface-2">
              {#if !exts[a.version]}
                <p class="text-sm text-muted py-2">читаем список…</p>
              {:else}
                <p class="text-xs text-muted mb-2">Действует на все сайты ветки {a.version}: php-fpm один на версию. После переключения он перезапускается.</p>
                <div class="flex flex-wrap gap-1.5">
                  {#each exts[a.version] as e}
                    <button
                      class="tag {e.enabled ? 'tag-ok' : 'tag-muted'} cursor-pointer transition-opacity {extBusy === a.version + e.name ? 'opacity-50' : ''}"
                      disabled={!!extBusy}
                      title={e.critical ? 'нужен типовому сайту' : e.enabled ? 'выключить' : 'включить'}
                      onclick={() => setExt(a.version, e.name, !e.enabled, e.critical)}
                    >
                      <Icon name={e.enabled ? 'check' : 'x'} size={11} />
                      {e.name}{#if e.critical}<span class="opacity-60">*</span>{/if}
                    </button>
                  {/each}
                </div>
                <p class="text-[11px] text-muted mt-2">* без этого расширения типовой сайт перестанет работать</p>
              {/if}
            </td>
          </tr>
        {/if}
      {/each}
    </tbody>
  </table>
  {/if}
</div>
