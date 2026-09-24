<script lang="ts">
  import { onMount } from 'svelte';
  import { slide } from 'svelte/transition';
  import { api, ApiError, when } from '$lib/api';
  import { auth, notify, dur } from '$lib/state.svelte';
  import { t } from '$lib/i18n/index.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Icon from '$lib/components/Icon.svelte';
  let users = $state<any[] | null>(null);
  let error = $state('');
  let job = $state<number | null>(null);
  let showForm = $state(false);
  let form = $state({ login: '', password: '', email: '', role: 'user', shell: false });
  type PanelKind = 'cron' | 'apps' | 'valkey';
  let panel = $state<{ login: string; kind: PanelKind; items: any[]; engine?: string } | null>(null);
  let cronForm = $state({ schedule: '*/5 * * * *', command: '' });
  let appForm = $state({ name: '', command: '', workdir: '', env_file: '' });
  // Valkey of an account: a cache and a PHP sessions instance, memory in MB.
  const vkPurposes = ['cache', 'sessions'] as const;
  type VkPurpose = (typeof vkPurposes)[number];
  let vkMem = $state<Record<VkPurpose, number>>({ cache: 128, sessions: 64 });
  let vkBusy = $state('');
  let del = $state<any>(null);
  let ask = $state<Ask | null>(null);
  let purge = $state(false);
  let confirmLogin = $state('');
  let pw = $state<{ login: string; value: string } | null>(null);
  async function load() { try { users = await api('/users'); } catch (e: any) { error = e.text || String(e); } }
  onMount(load);
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function create(e: Event) {
    e.preventDefault(); error = '';
    try {
      const body: any = { ...form }; if (!body.password) delete body.password; if (!body.email) delete body.email;
      const res: any = await api('/users', { method: 'POST', json: body });
      job = res.job_id; form = { login: '', password: '', email: '', role: 'user', shell: false }; showForm = false; await load();
    } catch (e) { fail(e); }
  }
  async function patch(login: string, body: any) {
    error = '';
    try { const res: any = await api(`/users/${login}`, { method: 'PATCH', json: body }); if (res.job_id) job = res.job_id; notify(t('users.updated', { login })); await load(); } catch (e) { fail(e); }
  }
  async function setPassword() { if (!pw || pw.value.length < 8) { notify(t('users.passwordTooShort'), 'err'); return; } await patch(pw.login, { password: pw.value }); pw = null; }
  async function remove() {
    if (!del || confirmLogin !== del.login) return;
    try { const res: any = await api(`/users/${del.login}?purge=${purge}`, { method: 'DELETE' }); job = res.job_id; notify(t('users.deleting', { login: del.login })); del = null; confirmLogin = ''; } catch (e) { fail(e); }
  }
  const askShell = (u: any): Ask => u.shell
    ? { title: t('users.askSftpTitle', { login: u.login }), action: t('users.askSftpAction'),
        note: t('users.askSftpNote'),
        run: () => patch(u.login, { shell: false }) }
    : { title: t('users.askShellTitle', { login: u.login }), action: t('users.askShellAction'),
        note: t('users.askShellNote'),
        run: () => patch(u.login, { shell: true }) };
  const askStatus = (u: any): Ask => u.status === 'active'
    ? { title: t('users.askSuspendTitle', { login: u.login }), danger: true, action: t('users.askSuspendAction'),
        note: t('users.askSuspendNote'),
        run: () => patch(u.login, { status: 'suspended' }) }
    : { title: t('users.askUnsuspendTitle', { login: u.login }), action: t('users.askUnsuspendAction'),
        note: t('users.askUnsuspendNote'),
        run: () => patch(u.login, { status: 'active' }) };
  const askCron = (j: any): Ask => ({
    title: t('users.askCronTitle'),
    note: t('users.askCronNote', { schedule: j.schedule }),
    danger: true, action: t('common.delete'),
    run: () => rmCron(j.id)
  });
  const askApp = (a: any, act: string): Ask => act === 'delete'
    ? { title: t('users.askAppDeleteTitle', { name: a.app.name }), danger: true, action: t('common.delete'),
        note: t('users.askAppDeleteNote'),
        run: () => appAction(a.app.name, 'delete') }
    : { title: t('users.askAppStopTitle', { name: a.app.name }), danger: true, action: t('users.askAppStopAction'),
        note: t('users.askAppStopNote'),
        run: () => appAction(a.app.name, 'stop') };
  async function openPanel(login: string, kind: PanelKind) {
    try {
      if (kind === 'valkey') {
        const r: any = await api(`/users/${login}/valkey`);
        if (panel?.login !== login || panel.kind !== 'valkey') vkMem = { cache: 128, sessions: 64 };
        for (const v of r.instances) vkMem[v.instance.purpose as VkPurpose] = v.instance.memory_mb;
        panel = { login, kind, items: r.instances, engine: r.engine };
      } else panel = { login, kind, items: await api(kind === 'cron' ? `/users/${login}/cron` : `/users/${login}/apps`) };
    } catch (e) { fail(e); }
  }
  async function refreshPanel() { if (panel) await openPanel(panel.login, panel.kind); }
  async function addCron(e: Event) { e.preventDefault(); if (!panel) return; try { await api(`/users/${panel.login}/cron`, { method: 'POST', json: cronForm }); cronForm.command = ''; await refreshPanel(); } catch (e) { fail(e); } }
  async function rmCron(id: number) { if (!panel) return; try { await api(`/users/${panel.login}/cron/${id}`, { method: 'DELETE' }); await refreshPanel(); } catch (e) { fail(e); } }
  async function addApp(e: Event) { e.preventDefault(); if (!panel) return; try { const body: any = { ...appForm }; if (!body.workdir) delete body.workdir; if (!body.env_file) delete body.env_file; await api(`/users/${panel.login}/apps`, { method: 'POST', json: body }); appForm = { name: '', command: '', workdir: '', env_file: '' }; await refreshPanel(); notify(t('users.appStarted')); } catch (e) { fail(e); } }
  const vkOf = (purpose: VkPurpose) => panel?.items.find((v) => v.instance.purpose === purpose);
  const vkTitle = (purpose: VkPurpose) => (purpose === 'cache' ? t('users.vkCache') : t('users.vkSessions'));
  async function vkPut(e: Event, purpose: VkPurpose) {
    e.preventDefault(); if (!panel) return;
    const had = !!vkOf(purpose); vkBusy = purpose;
    try {
      await api(`/users/${panel.login}/valkey/${purpose}`, { method: 'PUT', json: { memory_mb: vkMem[purpose] } });
      notify(t(had ? 'users.vkSaved' : 'users.vkStarted', { name: vkTitle(purpose) }));
      await refreshPanel();
    } catch (e) { fail(e); } finally { vkBusy = ''; }
  }
  async function vkRestart(purpose: VkPurpose) {
    if (!panel) return; vkBusy = purpose;
    try { await api(`/users/${panel.login}/valkey/${purpose}/restart`, { method: 'POST' }); notify(t('users.vkRestarted', { name: vkTitle(purpose) })); await refreshPanel(); } catch (e) { fail(e); } finally { vkBusy = ''; }
  }
  const askValkey = (purpose: VkPurpose): Ask => ({
    title: purpose === 'cache' ? t('users.askVkCacheTitle', { login: panel?.login ?? '' }) : t('users.askVkSessionsTitle', { login: panel?.login ?? '' }),
    note: purpose === 'cache' ? t('users.askVkCacheNote') : t('users.askVkSessionsNote'),
    danger: true, action: t('common.delete'),
    run: async () => { if (!panel) return; try { await api(`/users/${panel.login}/valkey/${purpose}`, { method: 'DELETE' }); await refreshPanel(); } catch (e) { fail(e); } }
  });
  async function appAction(name: string, act: string) { if (!panel) return; try { if (act === 'delete') await api(`/users/${panel.login}/apps/${name}`, { method: 'DELETE' }); else await api(`/users/${panel.login}/apps/${name}/${act}`, { method: 'POST' }); await refreshPanel(); } catch (e) { fail(e); } }
</script>

<PageHead title={t('users.title')} sub={t('users.sub')}>
  <button class="btn btn-primary" onclick={() => (showForm = !showForm)}><Icon name="plus" size={15} /> {t('users.newUser')}</button>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if showForm}
  <form class="card grid md:grid-cols-6 gap-3 items-end mb-4 rise" onsubmit={create}>
    <div><label class="label" for="l">{t('users.login')}</label><input id="l" class="input font-mono" bind:value={form.login} pattern="[a-z_][a-z0-9_-]{'{'}0,31{'}'}" required /></div>
    <div><label class="label" for="p">{t('users.password')}</label><input id="p" class="input" type="password" bind:value={form.password} placeholder={t('users.passwordPlaceholder')} /></div>
    <div><label class="label" for="e">E-mail</label><input id="e" class="input" type="email" bind:value={form.email} /></div>
    <div><label class="label" for="r">{t('users.role')}</label><select id="r" class="input" bind:value={form.role}><option value="user">user</option><option value="admin">admin</option></select></div>
    <label class="text-sm flex items-center gap-1.5"><input type="checkbox" bind:checked={form.shell} /> SSH shell</label>
    <button class="btn btn-primary">{t('common.create')}</button>
  </form>
{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
<div class="card overflow-x-auto p-0 mb-4 rise">
  {#if !users}<Skeleton rows={5} />{:else}
  <table class="tbl">
    <thead><tr><th>{t('users.login')}</th><th>{t('users.role')}</th><th>{t('common.status')}</th><th>UID</th><th>{t('users.colAccess')}</th><th>E-mail</th><th>{t('users.colCreated')}</th><th></th></tr></thead>
    <tbody>
      {#each users as u, i}
        <tr class="rise" style="--i:{i}">
          <td data-label={t('users.login')} class="font-medium">{u.login}{#if u.login === auth.me?.login}<span class="tag tag-muted ml-1">{t('users.you')}</span>{/if}</td><td data-label={t('users.role')} class="text-muted">{u.role}</td>
          <td><span class="tag {u.status === 'active' ? 'tag-ok' : u.status === 'deleting' ? 'tag-err' : 'tag-warn'}">{#if u.status === 'deleting'}<span class="dot dot-live"></span>{/if}{u.status}</span></td>
          <td data-label="UID" class="font-mono">{u.unix_uid ?? '—'}</td>
          <td>{#if u.role === 'user'}<span class="tag tag-muted">{u.shell ? 'SSH shell' : 'SFTP-only'}</span>{/if}</td>
          <td data-label="E-mail" class="text-muted">{u.email || '—'}</td><td data-label={t('users.colCreated')} class="text-xs text-muted">{when(u.created_at)}</td>
          <td><div class="row-actions">
            <button class="btn btn-sm" onclick={() => (pw = { login: u.login, value: '' })} title={t('users.changePassword')}><Icon name="key" size={13} /></button>
            {#if u.role === 'user'}
              <button class="btn btn-sm" onclick={() => (ask = askShell(u))}>{u.shell ? '→ SFTP-only' : '→ shell'}</button>
              <button class="btn btn-sm" onclick={() => openPanel(u.login, 'cron')}><Icon name="clock" size={13} /> cron</button>
              <button class="btn btn-sm" onclick={() => openPanel(u.login, 'apps')}><Icon name="box" size={13} /> apps</button>
              <button class="btn btn-sm" onclick={() => openPanel(u.login, 'valkey')}><Icon name="db" size={13} /> valkey</button>
              <button class="btn btn-sm" onclick={() => (ask = askStatus(u))}>{u.status === 'active' ? t('users.suspend') : t('users.unsuspend')}</button>
            {/if}
            {#if u.login !== auth.me?.login}<button class="btn btn-danger btn-sm" onclick={() => { del = u; purge = false; confirmLogin = ''; }} title={t('users.delete')}><Icon name="trash" size={13} /></button>{/if}
          </div></td>
        </tr>
      {/each}
    </tbody>
  </table>
  {/if}
</div>

{#if panel}
  <div class="card rise" transition:slide={{ duration: dur(180) }}>
    <div class="flex justify-between items-center mb-3"><span class="font-medium">{panel.kind === 'cron' ? 'Cron' : panel.kind === 'valkey' ? 'Valkey' : t('users.appServices')}: <span class="font-mono">{panel.login}</span></span><div class="flex gap-1"><button class="btn btn-sm" onclick={refreshPanel}><Icon name="refresh" size={13} /></button><button class="btn btn-sm" onclick={() => (panel = null)}>{t('users.closePanel')}</button></div></div>
    {#if panel.kind === 'cron'}
      <form class="grid md:grid-cols-4 gap-2 items-end mb-3" onsubmit={addCron}>
        <div><label class="label" for="cs">{t('users.schedule')}</label><input id="cs" class="input font-mono" bind:value={cronForm.schedule} /></div>
        <div class="md:col-span-2"><label class="label" for="cc">{t('users.command')}</label><input id="cc" class="input font-mono" bind:value={cronForm.command} placeholder="php ~/data/www/site/cron.php" required /></div>
        <button class="btn btn-primary">{t('common.add')}</button>
      </form>
      <table class="tbl"><thead><tr><th>ID</th><th>{t('users.schedule')}</th><th>{t('users.command')}</th><th></th></tr></thead><tbody>
        {#each panel.items as j}<tr><td data-label="ID">{j.id}</td><td data-label={t('users.schedule')} class="font-mono">{j.schedule}{#if !j.enabled} <span class="tag tag-muted">off</span>{/if}</td><td data-label={t('users.command')} class="font-mono text-xs">{j.command}</td><td data-label="" class="text-right"><button class="btn btn-danger btn-sm" onclick={() => (ask = askCron(j))}><Icon name="trash" size={13} /></button></td></tr>{/each}
        {#if !panel.items.length}<tr><td colspan="4" class="text-muted text-center py-4">{t('users.noCron')}</td></tr>{/if}
      </tbody></table>
    {:else if panel.kind === 'valkey'}
      {#if !panel.engine}
        <p class="text-sm text-muted">{t('users.vkNotInstalled')} <a href="/stack" class="text-accent-ink hover:underline">{t('users.vkToStack')}</a></p>
      {:else}
        <div class="grid md:grid-cols-2 gap-3">
          {#each vkPurposes as purpose}
            {@const v = vkOf(purpose)}
            {@const st = v ? v.service?.active_state || v.instance.status || 'unknown' : ''}
            <form class="rounded-lg border border-line p-3 flex flex-col gap-2" onsubmit={(e) => vkPut(e, purpose)}>
              <div class="flex items-start justify-between gap-2">
                <div><div class="font-medium">{vkTitle(purpose)}</div><div class="text-xs text-muted">{purpose === 'cache' ? t('users.vkCacheHint') : t('users.vkSessionsHint')}</div></div>
                {#if v}<span class="tag {st === 'active' ? 'tag-ok' : st === 'failed' ? 'tag-err' : 'tag-muted'}">{#if st === 'active'}<span class="dot dot-live"></span>{/if}{st}</span>{:else}<span class="tag tag-muted">{t('users.vkNone')}</span>{/if}
              </div>
              {#if v}<div class="text-xs font-mono break-all">{v.socket}</div>{/if}
              {#if v?.instance.last_error}<div class="text-xs text-danger break-words">{v.instance.last_error}</div>{/if}
              <div class="flex items-end gap-2 mt-auto">
                <div class="flex-1"><label class="label" for="vk-{purpose}">{t('users.vkMemory')}</label><input id="vk-{purpose}" class="input" type="number" min="16" max="8192" required bind:value={vkMem[purpose]} /></div>
                <button class="btn btn-primary" disabled={vkBusy === purpose}>{v ? t('common.save') : t('common.create')}</button>
                {#if v}
                  <button type="button" class="btn" disabled={vkBusy === purpose} onclick={() => vkRestart(purpose)} title={t('users.restart')}><Icon name="refresh" size={13} /></button>
                  <button type="button" class="btn btn-danger" onclick={() => (ask = askValkey(purpose))} title={t('users.delete')}><Icon name="trash" size={13} /></button>
                {/if}
              </div>
              {#if v}<p class="text-xs text-muted break-words">{purpose === 'cache' ? t('users.vkCacheUse', { socket: v.socket }) : t('users.vkSessionsUse')}</p>{/if}
            </form>
          {/each}
        </div>
        <p class="text-xs text-muted mt-3">{t('users.vkIsolation', { login: panel.login, engine: panel.engine === 'valkey' ? 'Valkey' : 'Redis' })}</p>
      {/if}
    {:else}
      <form class="grid md:grid-cols-5 gap-2 items-end mb-3" onsubmit={addApp}>
        <div><label class="label" for="an">{t('common.name')}</label><input id="an" class="input font-mono" bind:value={appForm.name} pattern="[a-z0-9][a-z0-9_-]{'{'}0,31{'}'}" required /></div>
        <div class="md:col-span-2"><label class="label" for="ac">{t('users.commandAbs')}</label><input id="ac" class="input font-mono" bind:value={appForm.command} placeholder="/var/www/{panel.login}/data/venv/bin/gunicorn --bind 127.0.0.1:5000 app:app" required /></div>
        <div><label class="label" for="aw">Workdir</label><input id="aw" class="input font-mono" bind:value={appForm.workdir} placeholder="~/data" /></div>
        <button class="btn btn-primary">{t('users.start')}</button>
      </form>
      <table class="tbl"><thead><tr><th>{t('common.name')}</th><th>{t('users.colState')}</th><th>{t('users.command')}</th><th></th></tr></thead><tbody>
        {#each panel.items as a}
          {@const st = a.service?.active_state || a.app.status}
          <tr><td data-label={t('common.name')} class="font-mono">{a.app.name}</td><td data-label={t('users.colState')}><span class="tag {st === 'active' ? 'tag-ok' : st === 'failed' ? 'tag-err' : 'tag-muted'}">{#if st === 'active'}<span class="dot dot-live"></span>{/if}{st}{a.service ? '/' + a.service.sub_state : ''}</span>{#if !a.app.enabled}<span class="tag tag-muted ml-1">{t('users.autostartOff')}</span>{/if}</td><td data-label={t('users.command')} class="font-mono text-xs max-w-md truncate" title={a.app.command}>{a.app.command}</td>
          <td><div class="row-actions"><button class="btn btn-sm" onclick={() => appAction(a.app.name, 'restart')} title={t('users.restart')}><Icon name="refresh" size={13} /></button>{#if st === 'active'}<button class="btn btn-sm" onclick={() => (ask = askApp(a, 'stop'))} title={t('users.stop')}><Icon name="stop" size={13} /></button>{:else}<button class="btn btn-sm" onclick={() => appAction(a.app.name, 'start')}><Icon name="play" size={13} /></button>{/if}<button class="btn btn-danger btn-sm" onclick={() => (ask = askApp(a, 'delete'))} title={t('users.delete')}><Icon name="trash" size={13} /></button></div></td></tr>
        {/each}
        {#if !panel.items.length}<tr><td colspan="4" class="text-muted text-center py-4">{t('users.noApps')}</td></tr>{/if}
      </tbody></table>
    {/if}
  </div>
{/if}

<Confirm bind:ask />

<Modal open={!!pw} title={t('users.newPasswordFor', { login: pw?.login ?? '' })} onclose={() => (pw = null)}>
  <p class="text-muted">{t('users.passwordHint')}</p>
  {#if pw}<input class="input font-mono" type="text" bind:value={pw.value} autocomplete="new-password" onkeydown={(e) => e.key === 'Enter' && setPassword()} />{/if}
  {#snippet footer()}<button class="btn" onclick={() => (pw = null)}>{t('common.cancel')}</button><button class="btn btn-primary" onclick={setPassword}>{t('common.save')}</button>{/snippet}
</Modal>

<Modal open={!!del} title={t('users.deleteUserTitle', { login: del?.login ?? '' })} onclose={() => { del = null; confirmLogin = ''; }}>
  <p>{#if del?.role === 'admin'}{t('users.deleteUserNoteAdmin')}{:else}{t('users.deleteUserNote')}{/if}</p>
  {#if del?.unix_uid}
    <label class="flex items-start gap-2 p-2 rounded-md border {purge ? 'border-danger/50 bg-danger-soft' : 'border-line'} transition-colors"><input type="checkbox" bind:checked={purge} class="mt-0.5" /><span>{t('users.purgeLabel')} <code class="font-mono">/var/www/{del.login}</code><br /><span class="text-xs text-muted">{t('users.purgeHint')}</span></span></label>
  {/if}
  <div><label class="label" for="cl">{t('users.confirmLogin')}</label><input id="cl" class="input font-mono" bind:value={confirmLogin} placeholder={del?.login} onkeydown={(e) => e.key === 'Enter' && remove()} /></div>
  {#snippet footer()}<button class="btn" onclick={() => (del = null)}>{t('common.cancel')}</button><button class="btn btn-danger" disabled={confirmLogin !== del?.login} onclick={remove}><Icon name="trash" size={14} /> {purge ? t('users.deleteWithFiles') : t('common.delete')}</button>{/snippet}
</Modal>
