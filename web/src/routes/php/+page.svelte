<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import { t, tn } from '$lib/i18n/index.svelte';
  let data = $state<any>(null);
  let job = $state<number | null>(null);
  let error = $state('');
  async function load() { try { data = await api('/php/versions'); } catch (e: any) { error = e.text || String(e); } }
  onMount(load);
  async function install(v: string) { error = ''; try { const r: any = await api('/php/versions', { method: 'POST', json: { version: v } }); job = r.job_id; } catch (e) { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); } }
  let ask = $state<Ask | null>(null);
  const askRemove = (v: string): Ask => ({
    title: t('php.askRemoveTitle', { version: v }),
    note: t('php.askRemoveNote', { version: v }),
    danger: true, action: t('php.remove'),
    run: () => remove(v)
  });
  async function remove(v: string) { error = ''; try { const r: any = await api(`/php/versions/${v}`, { method: 'DELETE' }); job = r.job_id; } catch (e) { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); } }
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

  // Выключение спрашивают всегда: мастер php-fpm один на версию, поэтому
  // расширение уходит сразу у всех сайтов ветки. Включение — нет: оно ничего
  // не ломает, а перезапуск php-fpm виден в тосте.
  const askExt = (version: string, e: any): Ask => ({
    title: t('php.askExtTitle', { name: e.name, version }),
    note: e.critical
      ? t('php.askExtNoteCritical', { name: e.name, version })
      : t('php.askExtNote', { version }),
    danger: true, action: t('php.disable'),
    run: () => setExt(version, e.name, false)
  });

  // After an offered extension was installed the branch has one package more.
  // Глобальный слой php.ini: что наследует каждый сайт, если ни пресет, ни
  // сам сайт не задают ключ.
  let gphp = $state<any>(null);
  let gKey = $state('');
  let gValue = $state('');
  let gBusy = $state(false);
  let gKeyInput = $state<HTMLInputElement | null>(null);
  async function loadGlobal() { try { gphp = await api('/php/settings'); } catch (e: any) { error = e.text || String(e); } }
  onMount(loadGlobal);
  function editGlobal(key: string, value: string) { gKey = key; gValue = value; queueMicrotask(() => gKeyInput?.focus()); }
  async function saveGlobal(ini: Record<string, string>) {
    gBusy = true; error = '';
    try {
      const r: any = await api('/php/settings', { method: 'PUT', json: { php_ini: ini } });
      gphp = r.settings;
      notify(tn('php.globalSaved', r.jobs.length));
      if (r.jobs.length) job = r.jobs[r.jobs.length - 1];
      gKey = ''; gValue = '';
    } catch (e) {
      error = e instanceof ApiError ? e.text : String(e);
      notify(error, 'err');
    } finally {
      gBusy = false;
    }
  }
  function submitGlobal(e: Event) { e.preventDefault(); const k = gKey.trim(); if (k && gValue.trim()) saveGlobal({ [k]: gValue.trim() }); }

  const installedNow = (version: string, name: string) => { const e = exts[version]?.find((x) => x.name === name); if (e && !e.installed) load(); };
  async function setExt(version: string, name: string, enabled: boolean) {
    extBusy = version + name;
    try {
      const r: any = await api(`/php/versions/${version}/extensions`, { method: 'POST', json: { name, enabled } });
      exts = { ...exts, [version]: r.extensions };
      notify(enabled ? t('php.extEnabledToast', { name, version }) : t('php.extDisabledToast', { name, version }));
      installedNow(version, name);
    } catch (e) {
      error = e instanceof ApiError ? e.text : String(e);
      notify(error, 'err');
    } finally {
      extBusy = '';
    }
  }
</script>

<PageHead title="PHP" sub={t('php.sub')} />
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
<div class="card overflow-x-auto p-0 rise">
  {#if !data}<Skeleton rows={6} />{:else}
  <table class="tbl">
    <thead><tr><th>{t('php.colVersion')}</th><th>Upstream</th><th>{t('php.colState')}</th><th>{t('php.colPackage')}</th><th>{t('php.colExtensions')}</th><th></th></tr></thead>
    <tbody>
      {#each data.available as a, i}
        {@const p = installed[a.version]}
        <tr class="rise" style="--i:{i}">
          <td data-label={t('php.colVersion')} class="font-mono font-medium">{a.version}</td>
          <td data-label="Upstream"><span class="tag {a.support === 'active' ? 'tag-ok' : a.support === 'security' ? 'tag-warn' : 'tag-muted'}">{a.support}</span></td>
          <td data-label={t('php.colState')}>{#if p}<span class="tag {p.status === 'installed' ? 'tag-ok' : p.status === 'error' ? 'tag-err' : 'tag-warn'}">{p.status}</span>{#if p.last_error}<div class="text-xs text-danger">{p.last_error}</div>{/if}{:else if !a.available}<span class="text-xs text-muted">{a.note}</span>{:else}<span class="text-muted">—</span>{/if}</td>
          <td data-label={t('php.colPackage')} class="font-mono text-xs text-muted">{p?.package_version || ''}</td>
          <td data-label={t('php.colExtensions')} class="text-xs text-muted max-w-md">
            {#if p?.status === 'installed'}
              <button class="inline-flex items-center gap-1 hover:text-ink transition-colors" onclick={() => toggleExt(a.version)}>
                <Icon name="chevron" size={13} class="transition-transform {openExt === a.version ? 'rotate-90' : ''}" />
                {exts[a.version] ? t('php.extOf', { enabled: exts[a.version].filter((e) => e.enabled).length, installed: exts[a.version].filter((e) => e.installed).length }) : tn('php.packages', p.extensions?.length ?? 0)}
              </button>
            {/if}
          </td>
          <td data-label="" class="text-right">{#if p?.status === 'installed'}<button class="btn btn-danger btn-sm" onclick={() => (ask = askRemove(a.version))}><Icon name="trash" size={13} /></button>{:else if a.available}<button class="btn btn-sm" onclick={() => install(a.version)}><Icon name="plus" size={13} /> {t('php.btnInstall')}</button>{/if}</td>
        </tr>
        {#if openExt === a.version}
          <tr>
            <td data-label="" colspan="6" class="bg-surface-2">
              {#if !exts[a.version]}
                <p class="text-sm text-muted py-2">{t('php.loadingList')}</p>
              {:else}
                <p class="text-xs text-muted mb-2">{t('php.extHint', { version: a.version })}</p>
                <div class="flex flex-wrap gap-1.5">
                  {#each exts[a.version] as e}
                    <button
                      class="tag {e.enabled ? 'tag-ok' : 'tag-muted'} cursor-pointer transition-opacity {extBusy === a.version + e.name ? 'opacity-50' : ''} {e.installed ? '' : 'border-dashed opacity-70'}"
                      disabled={!!extBusy}
                      title={!e.installed ? t('php.extInstallTitle', { package: e.package }) : e.critical ? t('php.extCriticalTitle') : e.enabled ? t('php.extDisableTitle') : t('php.extEnableTitle')}
                      onclick={() => (e.enabled ? (ask = askExt(a.version, e)) : setExt(a.version, e.name, true))}
                    >
                      <Icon name={e.enabled ? 'check' : e.installed ? 'x' : 'plus'} size={11} />
                      {e.name}{#if e.critical}<span class="opacity-60">*</span>{/if}
                    </button>
                  {/each}
                </div>
                <p class="text-[11px] text-muted mt-2">{t('php.extCriticalFoot')}</p>
              {/if}
            </td>
          </tr>
        {/if}
      {/each}
    </tbody>
  </table>
  {/if}
</div>

<div class="card p-0 overflow-hidden mt-4 rise" style="--i:1">
  <div class="px-4 py-3 border-b border-line">
    <div class="font-medium text-sm">{t('php.globalTitle')}</div>
    <p class="text-xs text-muted mt-1">{t('php.globalHint')}</p>
  </div>
  {#if !gphp}<Skeleton rows={4} />{:else}
    <form class="flex flex-wrap gap-2 px-4 py-3 border-b border-line" onsubmit={submitGlobal}>
      <input bind:this={gKeyInput} class="input font-mono text-xs flex-1 min-w-40" list="global-ini-keys" bind:value={gKey} placeholder="memory_limit" aria-label={t('php.globalKey')} />
      <input class="input font-mono text-xs w-40" bind:value={gValue} placeholder="512M" aria-label={t('php.globalValue')} />
      <button class="btn btn-primary btn-sm" disabled={gBusy || !gKey.trim() || !gValue.trim()}><Icon name="save" size={13} /> {t('php.globalSave')}</button>
      <datalist id="global-ini-keys">{#each gphp.allowed as k}<option value={k}></option>{/each}</datalist>
    </form>
    <table class="tbl">
      <thead><tr><th>{t('php.globalKey')}</th><th>{t('php.globalValue')}</th><th>{t('php.globalSource')}</th><th></th></tr></thead>
      <tbody>
        {#each gphp.values as v, i (v.key)}
          <tr class="rise" style="--i:{i}">
            <td data-label={t('php.globalKey')} class="font-mono text-xs">{v.key}</td>
            <td data-label={t('php.globalValue')} class="font-mono text-xs">{v.value}</td>
            <td data-label={t('php.globalSource')}><span class="tag {v.source === 'global' ? 'tag-warn' : 'tag-muted'}">{v.source === 'global' ? t('php.srcGlobal') : t('php.srcPanel')}</span></td>
            <td data-label="" class="text-right whitespace-nowrap">
              <button class="btn btn-ghost btn-sm" onclick={() => editGlobal(v.key, v.value)}>{t('php.globalEdit')}</button>
              {#if v.source === 'global'}<button class="btn btn-ghost btn-sm text-danger" disabled={gBusy} title={t('php.globalReset')} aria-label={t('php.globalReset')} onclick={() => saveGlobal({ [v.key]: '' })}><Icon name="x" size={13} /></button>{/if}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
    <p class="text-xs text-muted px-4 py-3 border-t border-line">{t('php.globalAllowed', { keys: gphp.allowed.join(', ') })}</p>
  {/if}
</div>

<Confirm bind:ask />
