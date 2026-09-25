<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError, bytes } from '$lib/api';
  import { auth, notify } from '$lib/state.svelte';
  import { t } from '$lib/i18n/index.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Icon from '$lib/components/Icon.svelte';
  let engine = $state<any>(null);
  let dbs = $state<any[]>([]);
  let users = $state<any[]>([]);
  let error = $state('');
  let created = $state<any>(null);
  let job = $state<number | null>(null);
  let showForm = $state(false);
  let del = $state<any>(null);
  let ask = $state<Ask | null>(null);
  let form = $state({ name: '', user: '', password: '' });
  const admin = $derived(auth.me?.role === 'admin');
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function load() {
    try { engine = await api('/db/engine'); dbs = await api('/databases'); if (admin) users = ((await api('/users')) as any[]).filter((u) => u.role === 'user'); } catch (e: any) { error = e.text || String(e); }
  }
  onMount(load);
  async function installEngine(c: string) { error = ''; try { const r: any = await api('/stack/install', { method: 'POST', json: { component: c } }); job = r.job_id; } catch (e) { fail(e); } }
  async function create(e: Event) { e.preventDefault(); error = ''; try { const body: any = { ...form }; if (!admin) delete body.user; if (!body.password) delete body.password; created = await api('/databases', { method: 'POST', json: body }); form.name = ''; showForm = false; await load(); } catch (e) { fail(e); } }
  async function drop() { if (!del) return; try { await api(`/databases/${del.name}`, { method: 'DELETE' }); notify(t('databases.deleted', { name: del.name })); del = null; await load(); } catch (e) { fail(e); } }
  const askPassword = (name: string): Ask => ({
    title: t('databases.newPasswordTitle', { name }),
    note: t('databases.newPasswordNote'),
    action: t('databases.generate'),
    run: () => passwd(name)
  });
  // Параметры сервера MySQL: значения панели под этот хост и заданные вручную.
  let cfg = $state<any>(null);
  let cfgKey = $state('');
  let cfgValue = $state('');
  let cfgBusy = $state(false);
  let cfgKeyInput = $state<HTMLInputElement | null>(null);
  async function loadCfg() { if (!admin || !engine?.installed) return; try { cfg = await api('/db/engine/config'); } catch (e) { fail(e); } }
  $effect(() => { if (admin && engine?.installed && !cfg) loadCfg(); });
  function editCfg(key: string, value: string) { cfgKey = key; cfgValue = value; queueMicrotask(() => cfgKeyInput?.focus()); }
  async function saveCfg(settings: Record<string, string>) {
    cfgBusy = true; error = '';
    try { cfg = await api('/db/engine/config', { method: 'PUT', json: { settings } }); notify(t('databases.cfgSaved')); cfgKey = ''; cfgValue = ''; } catch (e) { fail(e); await loadCfg(); } finally { cfgBusy = false; }
  }
  const askCfg = (settings: Record<string, string>): Ask => ({
    title: t('databases.cfgAskTitle'),
    note: t('databases.cfgAskNote'),
    action: t('databases.cfgAskAction'),
    run: () => saveCfg(settings)
  });
  function submitCfg(e: Event) { e.preventDefault(); const k = cfgKey.trim(); const v = cfgValue.trim(); if (k && v) ask = askCfg({ [k]: v }); }
  async function passwd(name: string) { try { const r: any = await api(`/databases/${name}/password`, { method: 'POST', json: {} }); created = { database: { name }, password: r.password, reset: true }; } catch (e) { fail(e); } }
</script>

<PageHead title={t('databases.title')} sub={engine?.installed ? `${engine.instance.engine} ${engine.instance.version} · ${engine.service?.active_state} · ${engine.instance.socket}` : 'MySQL 8.4 / Percona Server 8.4'}>
  {#if engine?.installed}<button class="btn btn-primary" onclick={() => (showForm = !showForm)}><Icon name="plus" size={15} /> {t('databases.newDb')}</button>{/if}
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if engine && !engine.installed && admin}
  <div class="card mb-4 flex flex-wrap items-center gap-3 text-sm rise"><span>{t('databases.notInstalled')}</span><button class="btn btn-primary" onclick={() => installEngine('percona')}>{t('databases.installPercona')}</button><button class="btn" onclick={() => installEngine('mysql')}>MySQL 8.4</button></div>
{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
{#if engine?.installed}
  {#if showForm}
    <form class="card grid md:grid-cols-5 gap-3 items-end mb-4 rise" onsubmit={create}>
      <div><label class="label" for="n">{t('databases.nameSuffix')}</label><input id="n" class="input font-mono" bind:value={form.name} pattern="[a-z0-9_]{'{'}1,24{'}'}" required /></div>
      {#if admin}<div><label class="label" for="u">{t('databases.owner')}</label><select id="u" class="input" bind:value={form.user} required><option value="">—</option>{#each users as u}<option value={u.login}>{u.login}</option>{/each}</select></div>{/if}
      <div><label class="label" for="pw">{t('databases.password')}</label><input id="pw" class="input" bind:value={form.password} placeholder={t('databases.generated')} /></div>
      <button class="btn btn-primary">{t('databases.createNamed')}</button>
    </form>
  {/if}
  {#if created}<div class="card mb-4 text-sm font-mono rise border-accent/40">{created.reset ? t('databases.newPassword') : t('databases.database')} <b>{created.database.name}</b>{#if created.database.users?.[0]} · {created.database.users[0].name}@localhost{/if}{#if created.password} · {t('databases.passwordIs')} <b class="select-all">{created.password}</b>{/if}{#if created.dsn}<div class="text-xs text-muted mt-1">{created.dsn}</div>{/if}<div class="text-xs text-muted">{t('databases.shownOnce')}</div></div>{/if}
  <div class="card overflow-x-auto p-0 rise">
    <table class="tbl"><thead><tr><th>{t('databases.colDatabase')}</th><th>{t('databases.owner')}</th><th>{t('databases.accounts')}</th><th>{t('databases.size')}</th><th></th></tr></thead>
      <tbody>
        {#each dbs as d, i}<tr class="rise" style="--i:{i}"><td data-label={t('databases.colDatabase')} class="font-mono font-medium">{d.name}</td><td data-label={t('databases.owner')}>{d.login}</td><td data-label={t('databases.accounts')} class="font-mono text-xs text-muted">{d.users.map((u: any) => u.name + '@' + u.host + ' (' + u.auth_plugin + ')').join(', ')}</td><td data-label={t('databases.size')} class="tabular-nums">{bytes(d.size_bytes)}</td><td data-label=""><div class="row-actions"><button class="btn btn-sm" onclick={() => (ask = askPassword(d.name))}><Icon name="key" size={13} /> {t('databases.passwordBtn')}</button><button class="btn btn-danger btn-sm" onclick={() => (del = d)}><Icon name="trash" size={13} /></button></div></td></tr>{/each}
        {#if !dbs.length}<Empty text={t('databases.empty')} cols={5} />{/if}
      </tbody></table>
  </div>
  {#if admin && cfg}
    <div class="card p-0 overflow-hidden mt-4 rise" style="--i:1">
      <div class="px-4 py-3 border-b border-line">
        <div class="font-medium text-sm">{t('databases.cfgTitle')}</div>
        <p class="text-xs text-muted mt-1">{t('databases.cfgHint', { ram: cfg.ram_mb })}</p>
      </div>
      <form class="flex flex-wrap gap-2 px-4 py-3 border-b border-line" onsubmit={submitCfg}>
        <input bind:this={cfgKeyInput} class="input font-mono text-xs flex-1 min-w-40" list="db-cfg-keys" bind:value={cfgKey} placeholder="innodb_buffer_pool_size" aria-label={t('databases.cfgKey')} />
        <input class="input font-mono text-xs w-44" bind:value={cfgValue} placeholder="2G" aria-label={t('databases.cfgValue')} />
        <button class="btn btn-primary btn-sm" disabled={cfgBusy || !cfgKey.trim() || !cfgValue.trim()}><Icon name="save" size={13} /> {t('databases.cfgSave')}</button>
        <datalist id="db-cfg-keys">{#each cfg.allowed as k}<option value={k}></option>{/each}</datalist>
        <p class="basis-full text-xs text-muted m-0">{t('databases.cfgEmptyHint')}</p>
      </form>
      <div class="overflow-x-auto">
        <table class="tbl">
          <thead><tr><th>{t('databases.cfgKey')}</th><th>{t('databases.cfgValue')}</th><th>{t('databases.cfgSource')}</th><th></th></tr></thead>
          <tbody>
            {#each cfg.values as v, i (v.key)}
              <tr class="rise" style="--i:{i}">
                <td data-label={t('databases.cfgKey')} class="font-mono text-xs">{v.key}</td>
                <td data-label={t('databases.cfgValue')} class="font-mono text-xs">{v.value || '—'}</td>
                <td data-label={t('databases.cfgSource')}><span class="tag {v.source === 'custom' ? 'tag-warn' : 'tag-muted'}">{v.source === 'custom' ? t('databases.cfgCustom') : t('databases.cfgPanel')}</span></td>
                <td data-label="" class="text-right whitespace-nowrap">
                  {#if cfg.allowed.includes(v.key)}<button class="btn btn-ghost btn-sm" disabled={cfgBusy} onclick={() => editCfg(v.key, v.value)}>{t('databases.cfgEdit')}</button>{/if}
                  {#if v.source === 'custom'}<button class="btn btn-ghost btn-sm text-danger" disabled={cfgBusy} title={t('databases.cfgReset')} aria-label={t('databases.cfgReset')} onclick={() => (ask = askCfg({ [v.key]: '' }))}><Icon name="x" size={13} /></button>{/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <p class="text-xs text-muted px-4 py-3 border-t border-line">{t('databases.cfgAllowed', { keys: cfg.allowed.join(', ') })}</p>
    </div>
  {/if}
{/if}
<Confirm bind:ask />
<Modal open={!!del} title={t('databases.deleteTitle', { name: del?.name ?? '' })} onclose={() => (del = null)}>
  <p>{t('databases.deleteNote')}</p>
  {#snippet footer()}<button class="btn" onclick={() => (del = null)}>{t('common.cancel')}</button><button class="btn btn-danger" onclick={drop}>{t('common.delete')}</button>{/snippet}
</Modal>
