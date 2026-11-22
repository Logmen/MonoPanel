<script lang="ts">
  import { untrack } from 'svelte';
  import { slide } from 'svelte/transition';
  import { api, when } from '$lib/api';
  import { notify, dur } from '$lib/state.svelte';
  import { t } from '$lib/i18n/index.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import { errText, envList, realTime, withUnix, type Account } from './lib';

  // App services (systemd units running as the account) of one account, or of
  // every account when login is empty (GET /apps, administrators).
  let { login = '', accounts = [], nonce = 0 }: { login?: string; accounts?: Account[]; nonce?: number } = $props();
  const uid = $props.id();
  const all = $derived(!login);
  const owners = $derived(withUnix(accounts));
  let items = $state<any[] | null>(null);
  let error = $state('');
  let ask = $state<Ask | null>(null);
  const blank = (owner = '') => ({ owner, name: '', command: '', workdir: '', description: '', env_file: '', env: '', restart: 'always', enabled: true });
  let form = $state(blank());
  let busy = $state('');
  let info = $state<any>(null);
  type Edit = { login: string; name: string; command: string; workdir: string; description: string; env_file: string; env: string; restart: string; enabled: boolean };
  let edit = $state<Edit | null>(null);
  let editError = $state('');
  let saving = $state(false);
  // Journal of one app under the table: journalctl -u <unit> -n <lines>, newest at the bottom.
  let logs = $state<{ login: string; name: string } | null>(null);
  let logLines = $state(100);
  let tail = $state<any>(null);
  let logBusy = $state(false);
  let pre: HTMLPreElement | undefined = $state();

  const fail = (e: unknown) => { error = errText(e); notify(error, 'err'); };
  const whose = (a: any): string => a.app.login || login;
  const keyOf = (a: any) => whose(a) + '/' + a.app.name;
  const unitOf = (l: string, name: string) => `monopanel-app-${l}-${name}.service`;
  const homeOf = (l: string) => accounts.find((a) => a.login === l)?.home || `/var/www/${l}`;
  const stateOf = (a: any): string => a.service?.active_state || a.app.status || '';
  const firstLine = (s: string) => s.split('\n')[0];
  const restarts = $derived([['always', t('users.restartAlways')], ['on-failure', t('users.restartOnFailure')], ['no', t('users.restartNo')]] as const);

  async function reload() { try { items = login ? await api(`/users/${login}/apps`) : await api('/apps'); } catch (e) { fail(e); } }
  $effect(() => { void nonce; untrack(reload); });
  $effect(() => { if (all && !form.owner && owners.length) form.owner = owners[0].login; });
  $effect(() => { void tail; if (pre) pre.scrollTop = pre.scrollHeight; });

  async function add(e: Event) {
    e.preventDefault(); error = '';
    const owner = login || form.owner;
    if (!owner) return;
    const body: any = { name: form.name.trim(), command: form.command.trim(), restart: form.restart, enabled: form.enabled };
    if (form.workdir.trim()) body.workdir = form.workdir.trim();
    if (form.description.trim()) body.description = form.description.trim();
    if (form.env_file.trim()) body.env_file = form.env_file.trim();
    const env = envList(form.env);
    if (env.length) body.env = env;
    try {
      await api(`/users/${owner}/apps`, { method: 'POST', json: body });
      notify(t(form.enabled ? 'users.appStarted' : 'users.appCreated', { name: body.name }));
      form = blank(form.owner);
      await reload();
    } catch (e) { fail(e); }
  }
  async function act(a: any, action: 'start' | 'stop' | 'restart') {
    busy = keyOf(a); error = '';
    try { await api(`/users/${whose(a)}/apps/${a.app.name}/${action}`, { method: 'POST' }); await reload(); } catch (e) { fail(e); } finally { busy = ''; }
  }
  async function rm(a: any) {
    error = '';
    try {
      await api(`/users/${whose(a)}/apps/${a.app.name}`, { method: 'DELETE' });
      if (logs?.login === whose(a) && logs.name === a.app.name) logs = null;
      notify(t('users.appDeleted', { name: a.app.name }));
      await reload();
    } catch (e) { fail(e); }
  }
  async function details(a: any) { error = ''; try { info = await api(`/users/${whose(a)}/apps/${a.app.name}`); } catch (e) { fail(e); } }
  function startEdit(app: any, owner: string) {
    editError = '';
    edit = { login: owner, name: app.name, command: app.command, workdir: app.workdir || '', description: app.description || '', env_file: app.env_file || '', env: (app.env || []).join('\n'), restart: app.restart || 'always', enabled: app.enabled };
  }
  async function save(e: Event) {
    e.preventDefault(); if (!edit || saving) return;
    editError = ''; saving = true;
    // PATCH takes every field: an empty workdir means ~/data again, an empty
    // EnvironmentFile and an empty list drop them.
    const body = { command: edit.command.trim(), workdir: edit.workdir.trim(), description: edit.description.trim(), env_file: edit.env_file.trim(), env: envList(edit.env), restart: edit.restart, enabled: edit.enabled };
    try {
      await api(`/users/${edit.login}/apps/${edit.name}`, { method: 'PATCH', json: body });
      notify(t('users.appSaved', { name: edit.name }));
      edit = null;
      await reload();
    } catch (e) { editError = errText(e); } finally { saving = false; }
  }
  async function loadLogs() {
    if (!logs) return;
    const { login: l, name } = logs;
    logBusy = true;
    try { const r = await api(`/users/${l}/apps/${name}/logs?lines=${logLines}`); if (logs?.login === l && logs.name === name) tail = r; } catch (e) { fail(e); } finally { logBusy = false; }
  }
  function openLogs(l: string, name: string) { logs = { login: l, name }; tail = null; info = null; loadLogs(); }
  const askStop = (a: any): Ask => ({ title: t('users.askAppStopTitle', { name: a.app.name }), danger: true, action: t('users.askAppStopAction'), note: t('users.askAppStopNote'), run: () => act(a, 'stop') });
  const askRm = (a: any): Ask => ({ title: t('users.askAppDeleteTitle', { name: a.app.name }), danger: true, action: t('common.delete'), note: t('users.askAppDeleteNote'), run: () => rm(a) });
  const cols = $derived(all ? 5 : 4);
</script>

{#if error}<p class="text-danger text-sm mb-2">{error}</p>{/if}
{#if all && !owners.length}
  <p class="text-sm text-muted mb-3">{t('users.noOwners')}</p>
{:else}
  {@const owner = login || form.owner}
  <form class="grid md:grid-cols-4 gap-2 items-end mb-1" onsubmit={add}>
    {#if all}<div><label class="label" for="{uid}-o">{t('users.owner')}</label><select id="{uid}-o" class="input" bind:value={form.owner} required>{#each owners as a}<option value={a.login}>{a.login}</option>{/each}</select></div>{/if}
    <div><label class="label" for="{uid}-n">{t('common.name')}</label><input id="{uid}-n" class="input font-mono" bind:value={form.name} pattern="[a-z0-9][a-z0-9_\-]{'{'}0,31{'}'}" placeholder="api" required /></div>
    <div class="md:col-span-2"><label class="label" for="{uid}-c">{t('users.commandAbs')}</label><input id="{uid}-c" class="input font-mono" bind:value={form.command} placeholder="{homeOf(owner)}/data/venv/bin/gunicorn --bind 127.0.0.1:5000 app:app" maxlength="1000" required /></div>
    <div><label class="label" for="{uid}-w">{t('users.appWorkdir')}</label><input id="{uid}-w" class="input font-mono" bind:value={form.workdir} placeholder="{homeOf(owner)}/data" /></div>
    <div><label class="label" for="{uid}-d">{t('users.appDescription')}</label><input id="{uid}-d" class="input" bind:value={form.description} maxlength="120" /></div>
    <div><label class="label" for="{uid}-f">EnvironmentFile</label><input id="{uid}-f" class="input font-mono" bind:value={form.env_file} placeholder="{homeOf(owner)}/data/.env" /></div>
    <div><label class="label" for="{uid}-r">{t('users.appRestart')}</label><select id="{uid}-r" class="input" bind:value={form.restart}>{#each restarts as [v, label]}<option value={v}>{label}</option>{/each}</select></div>
    <label class="text-sm flex items-center gap-1.5 pb-2"><input type="checkbox" bind:checked={form.enabled} /> {t('users.appEnabled')}</label>
    <div class={all ? 'md:col-span-2' : 'md:col-span-3'}><label class="label" for="{uid}-e">{t('users.appEnv')}</label><textarea id="{uid}-e" class="input font-mono text-xs h-16 resize-y" bind:value={form.env} placeholder={'PORT=5000\nAPP_ENV=production'} spellcheck="false"></textarea></div>
    <button class="btn btn-primary">{form.enabled ? t('users.start') : t('common.create')}</button>
  </form>
  <p class="text-xs text-muted mb-3">{t('users.appHint')}</p>
{/if}
{#if !items}<Skeleton rows={3} />{:else}
  <table class="tbl">
    <thead><tr>{#if all}<th>{t('users.owner')}</th>{/if}<th>{t('common.name')}</th><th>{t('users.colState')}</th><th>{t('users.command')}</th><th></th></tr></thead>
    <tbody>
      {#each items as a (keyOf(a))}
        {@const st = stateOf(a)}
        <tr>
          {#if all}<td data-label={t('users.owner')} class="font-mono">{whose(a)}</td>{/if}
          <td data-label={t('common.name')}><span class="font-mono">{a.app.name}</span>{#if a.app.description}<div class="text-xs text-muted">{a.app.description}</div>{/if}</td>
          <td data-label={t('users.colState')}><span class="tag {st === 'active' ? 'tag-ok' : st === 'failed' ? 'tag-err' : 'tag-muted'}">{#if st === 'active'}<span class="dot dot-live"></span>{/if}{st || '—'}{a.service ? '/' + a.service.sub_state : ''}</span>{#if !a.app.enabled}<span class="tag tag-muted ml-1">{t('users.autostartOff')}</span>{/if}</td>
          <td data-label={t('users.command')} class="font-mono text-xs max-w-md"><div class="truncate" title={a.app.command}>{a.app.command}</div>{#if a.app.last_error}<div class="text-danger break-words mt-0.5" title={a.app.last_error}>{firstLine(a.app.last_error)}</div>{/if}</td>
          <td data-label=""><div class="row-actions">
            <button class="btn btn-sm" onclick={() => details(a)} title={t('users.appDetails')}><Icon name="eye" size={13} /></button>
            <button class="btn btn-sm" onclick={() => startEdit(a.app, whose(a))} title={t('users.appEdit')}><Icon name="pencil" size={13} /></button>
            <button class="btn btn-sm" onclick={() => openLogs(whose(a), a.app.name)} title={t('users.appLogs')}><Icon name="terminal" size={13} /></button>
            <button class="btn btn-sm" disabled={busy === keyOf(a)} onclick={() => act(a, 'restart')} title={t('users.restart')}><Icon name="refresh" size={13} /></button>
            {#if st === 'active'}<button class="btn btn-sm" disabled={busy === keyOf(a)} onclick={() => (ask = askStop(a))} title={t('users.stop')}><Icon name="stop" size={13} /></button>{:else}<button class="btn btn-sm" disabled={busy === keyOf(a)} onclick={() => act(a, 'start')} title={t('users.start')}><Icon name="play" size={13} /></button>{/if}
            <button class="btn btn-danger btn-sm" onclick={() => (ask = askRm(a))} title={t('users.delete')}><Icon name="trash" size={13} /></button>
          </div></td>
        </tr>
      {/each}
      {#if !items.length}<Empty text={all ? t('users.noAppsAll') : t('users.noApps')} {cols} />{/if}
    </tbody>
  </table>
  <p class="text-xs text-muted mt-2">{t('users.appStopNote')}</p>
{/if}

{#if logs}
  <div class="mt-4 rounded-lg border border-line p-3" transition:slide={{ duration: dur(180) }}>
    <div class="flex flex-wrap items-center gap-2 mb-2">
      <span class="text-sm font-medium">{t('users.appLogTitle')} <span class="font-mono">{logs.login}/{logs.name}</span></span>
      {#if tail?.path}<span class="text-xs text-muted font-mono break-all">{tail.path}</span>{/if}
      <div class="ml-auto flex items-center gap-1.5">
        <label class="text-xs text-muted" for="{uid}-ll">{t('users.appLogLines')}</label>
        <select id="{uid}-ll" class="input w-24 py-1" value={logLines} onchange={(e) => { logLines = Number(e.currentTarget.value); loadLogs(); }}>{#each [50, 100, 200, 500, 1000, 2000] as n}<option value={n}>{n}</option>{/each}</select>
        <button class="btn btn-sm" onclick={loadLogs} disabled={logBusy} title={t('common.refresh')}><Icon name="refresh" size={13} /></button>
        <button class="btn btn-sm" onclick={() => (logs = null)} title={t('common.close')}><Icon name="x" size={13} /></button>
      </div>
    </div>
    <pre bind:this={pre} class="code max-h-[28rem]">{tail ? (tail.lines ?? []).join('\n') || t('users.appLogEmpty') : t('common.loading')}</pre>
  </div>
{/if}

<Confirm bind:ask />

<Modal open={!!info} title={info ? t('users.appDetailsTitle', { name: (info.app.login || login) + '/' + info.app.name }) : ''} onclose={() => (info = null)}>
  {#if info}
    {@const a = info.app}
    {@const s = info.service}
    {@const owner = a.login || login}
    <dl class="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1.5 text-xs">
      <dt class="text-muted">ID</dt><dd class="font-mono">{a.id}</dd>
      <dt class="text-muted">{t('users.appUnit')}</dt><dd class="font-mono break-all">{s?.unit || unitOf(owner, a.name)}</dd>
      <dt class="text-muted">{t('users.colState')}</dt><dd>{s ? `${s.active_state} (${s.sub_state})` : a.status || '—'}{s?.load_state && s.load_state !== 'loaded' ? ' · ' + s.load_state : ''}</dd>
      <dt class="text-muted">{t('users.appUnitFile')}</dt><dd class="font-mono">{s?.unit_file_state || '—'}</dd>
      <dt class="text-muted">{t('users.appSince')}</dt><dd>{realTime(s?.active_since) ? when(s.active_since) : '—'}</dd>
      <dt class="text-muted">{t('users.appAutostart')}</dt><dd>{a.enabled ? t('common.on') : t('common.off')}</dd>
      <dt class="text-muted">{t('users.appRestart')}</dt><dd class="font-mono">{a.restart || 'always'}</dd>
      <dt class="text-muted">{t('users.appDescription')}</dt><dd class="break-words">{a.description || '—'}</dd>
      <dt class="text-muted">{t('users.command')}</dt><dd class="font-mono break-all">{a.command}</dd>
      <dt class="text-muted">{t('users.appWorkdir')}</dt><dd class="font-mono break-all">{a.workdir || '—'}</dd>
      <dt class="text-muted">EnvironmentFile</dt><dd class="font-mono break-all">{a.env_file || '—'}</dd>
      <dt class="text-muted">{t('users.appEnvShort')}</dt><dd class="font-mono break-all">{#each a.env || [] as e}<div>{e}</div>{:else}—{/each}</dd>
      <dt class="text-muted">{t('users.appStoredStatus')}</dt><dd class="font-mono">{a.status || '—'}</dd>
      <dt class="text-muted">{t('users.appLastError')}</dt><dd class="break-words whitespace-pre-wrap {a.last_error ? 'text-danger' : ''}">{a.last_error || '—'}</dd>
      <dt class="text-muted">{t('users.colCreated')}</dt><dd>{when(a.created_at)}</dd>
      <dt class="text-muted">{t('users.colUpdated')}</dt><dd>{when(a.updated_at)}</dd>
    </dl>
  {/if}
  {#snippet footer()}
    {#if info}
      {@const a = info.app}
      <button class="btn" onclick={() => openLogs(a.login || login, a.name)}><Icon name="terminal" size={13} /> {t('users.appLogs')}</button>
      <button class="btn" onclick={() => { const app = info.app; info = null; startEdit(app, app.login || login); }}><Icon name="pencil" size={13} /> {t('common.edit')}</button>
    {/if}
    <button class="btn btn-primary" onclick={() => (info = null)}>{t('common.close')}</button>
  {/snippet}
</Modal>

<Modal open={!!edit} title={edit ? t('users.appEditTitle', { name: edit.login + '/' + edit.name }) : ''} onclose={() => (edit = null)}>
  {#if edit}
    <form id="{uid}-edit" class="grid gap-3" onsubmit={save}>
      <div><label class="label" for="{uid}-ec">{t('users.commandAbs')}</label><input id="{uid}-ec" class="input font-mono" bind:value={edit.command} maxlength="1000" required /></div>
      <div><label class="label" for="{uid}-ew">{t('users.appWorkdir')}</label><input id="{uid}-ew" class="input font-mono" bind:value={edit.workdir} placeholder="{homeOf(edit.login)}/data" /></div>
      <div><label class="label" for="{uid}-ed">{t('users.appDescription')}</label><input id="{uid}-ed" class="input" bind:value={edit.description} maxlength="120" /></div>
      <div><label class="label" for="{uid}-ef">EnvironmentFile</label><input id="{uid}-ef" class="input font-mono" bind:value={edit.env_file} placeholder="{homeOf(edit.login)}/data/.env" /></div>
      <div><label class="label" for="{uid}-ee">{t('users.appEnv')}</label><textarea id="{uid}-ee" class="input font-mono text-xs h-24 resize-y" bind:value={edit.env} placeholder={'PORT=5000\nAPP_ENV=production'} spellcheck="false"></textarea></div>
      <div class="grid grid-cols-2 gap-3 items-end">
        <div><label class="label" for="{uid}-er">{t('users.appRestart')}</label><select id="{uid}-er" class="input" bind:value={edit.restart}>{#each restarts as [v, label]}<option value={v}>{label}</option>{/each}</select></div>
        <label class="text-sm flex items-center gap-1.5 pb-2"><input type="checkbox" bind:checked={edit.enabled} /> {t('users.appEnabled')}</label>
      </div>
      <p class="text-xs text-muted">{edit.enabled ? t('users.appEditNoteOn') : t('users.appEditNoteOff')}</p>
      {#if editError}<p class="text-danger text-xs break-words">{editError}</p>{/if}
    </form>
  {/if}
  {#snippet footer()}<button class="btn" onclick={() => (edit = null)}>{t('common.cancel')}</button><button class="btn btn-primary" form="{uid}-edit" disabled={saving}>{saving ? t('users.saving') : t('common.save')}</button>{/snippet}
</Modal>
