<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { slide } from 'svelte/transition';
  import { api, when } from '$lib/api';
  import { auth, notify, dur } from '$lib/state.svelte';
  import { t, type MsgKey } from '$lib/i18n/index.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import AccountCard from './AccountCard.svelte';
  import CronPanel from './CronPanel.svelte';
  import AppsPanel from './AppsPanel.svelte';
  import ValkeyPanel from './ValkeyPanel.svelte';
  import SharesPanel from './SharesPanel.svelte';
  import TokensPanel from './TokensPanel.svelte';
  import { errText, genPassword, type Account } from './lib';

  // An administrator gets every account (GET /users) and the cross-account
  // lists; an ordinary account gets only itself (GET /users/{login}) — the
  // same page, called «Account» in the nav, without the admin controls.
  const admin = $derived(auth.me?.role === 'admin');
  const myLogin = $derived(auth.me?.login ?? '');
  let users = $state<Account[] | null>(null);
  let me = $state<Account | null>(null);
  let error = $state('');
  let job = $state<number | null>(null);
  let showForm = $state(false);
  const blankForm = () => ({ login: '', password: '', email: '', role: 'user', shell: false, quota: null as number | null });
  let form = $state(blankForm());
  let formPwShown = $state(false);
  type Tab = 'accounts' | 'cron' | 'apps' | 'valkey';
  let tab = $state<Tab>('accounts');
  const tabs: [Tab, MsgKey, string][] = [['accounts', 'users.tabAccounts', 'users'], ['cron', 'users.tabCron', 'clock'], ['apps', 'users.tabApps', 'box'], ['valkey', 'users.tabValkey', 'db']];
  type Kind = 'account' | 'cron' | 'apps' | 'valkey' | 'shares' | 'tokens';
  let panel = $state<{ login: string; kind: Kind } | null>(null);
  let panelEl: HTMLDivElement | undefined = $state();
  type Section = Exclude<Kind, 'account'>;
  let section = $state<Section>('cron');
  const sections: [Section, MsgKey, string][] = [['cron', 'users.tabCron', 'clock'], ['apps', 'users.appServices', 'box'], ['valkey', 'users.tabValkey', 'db'], ['shares', 'users.shares', 'file'], ['tokens', 'users.tokens', 'code']];
  // Bumped by a refresh button: the open panel reloads without losing its forms.
  let nonce = $state(0);
  let del = $state<Account | null>(null);
  let ask = $state<Ask | null>(null);
  let purge = $state(false);
  let confirmLogin = $state('');
  let pw = $state<{ login: string; value: string; show: boolean; current: string } | null>(null);

  async function load() {
    try {
      if (admin) users = await api('/users');
      else me = await api(`/users/${myLogin}`);
    } catch (e) { error = errText(e); }
  }
  onMount(load);
  const fail = (e: unknown) => { error = errText(e); notify(error, 'err'); };
  async function create(e: Event) {
    e.preventDefault(); error = '';
    const body: any = { login: form.login.trim(), role: form.role, shell: form.shell };
    if (form.password) body.password = form.password;
    if (form.email.trim()) body.email = form.email.trim();
    if (form.quota != null && String(form.quota) !== '') body.quota_mb = Number(form.quota);
    try {
      const res: any = await api('/users', { method: 'POST', json: body });
      job = res.job_id; form = blankForm(); formPwShown = false; showForm = false;
      notify(t('users.created', { login: body.login }));
      await load();
    } catch (e) { fail(e); }
  }
  async function patch(login: string, body: any, done = t('users.updated', { login })) {
    error = '';
    try { const res: any = await api(`/users/${login}`, { method: 'PATCH', json: body }); if (res?.job_id) job = res.job_id; notify(done); await load(); return true; } catch (e) { fail(e); return false; }
  }
  function askPassword(u: Account) { pw = { login: u.login, value: '', show: u.login !== myLogin, current: '' }; }
  async function setPassword() {
    if (!pw) return;
    if (pw.value.length < 8) { notify(t('users.passwordTooShort'), 'err'); return; }
    // Your own password needs the current one (a stolen session must not
    // set a new password and then turn 2FA off with it).
    const body: any = { password: pw.value };
    if (pw.login === myLogin) {
      if (!pw.current) { notify(t('users.currentPasswordNeeded'), 'err'); return; }
      body.current_password = pw.current;
    }
    if (await patch(pw.login, body, t('users.passwordChanged', { login: pw.login }))) pw = null;
  }
  async function remove() {
    if (!del || confirmLogin !== del.login) return;
    try { const res: any = await api(`/users/${del.login}?purge=${purge}`, { method: 'DELETE' }); job = res.job_id; notify(t('users.deleting', { login: del.login })); if (panel?.login === del.login) panel = null; del = null; confirmLogin = ''; } catch (e) { fail(e); }
  }
  const askShell = (u: Account): Ask => u.shell
    ? { title: t('users.askSftpTitle', { login: u.login }), action: t('users.askSftpAction'), note: t('users.askSftpNote'), run: () => patch(u.login, { shell: false }) }
    : { title: t('users.askShellTitle', { login: u.login }), action: t('users.askShellAction'), note: t('users.askShellNote'), run: () => patch(u.login, { shell: true }) };
  const askStatus = (u: Account): Ask => u.status === 'active'
    ? { title: t('users.askSuspendTitle', { login: u.login }), danger: true, action: t('users.askSuspendAction'), note: t('users.askSuspendNote'), run: () => patch(u.login, { status: 'suspended' }) }
    : { title: t('users.askUnsuspendTitle', { login: u.login }), action: t('users.askUnsuspendAction'), note: t('users.askUnsuspendNote'), run: () => patch(u.login, { status: 'active' }) };
  async function openPanel(login: string, kind: Kind) {
    if (panel?.login === login && panel.kind === kind) nonce++;
    else panel = { login, kind };
    await tick();
    panelEl?.scrollIntoView({ behavior: dur(1) ? 'smooth' : 'auto', block: 'nearest' });
  }
  const panelUser = $derived(panel ? (users?.find((u) => u.login === panel?.login) ?? null) : null);
  const kindTitle = (k: Kind): string => ({ account: t('users.panelAccount'), cron: 'Cron', apps: t('users.appServices'), valkey: 'Valkey', shares: t('users.shares'), tokens: t('users.tokens') })[k];
  const tabHint = (k: Tab): string => (k === 'cron' ? t('users.allCronHint') : k === 'apps' ? t('users.allAppsHint') : t('users.allValkeyHint'));
  function refreshPanel() { if (panel?.kind === 'account') load(); else nonce++; }
  function refreshAll() { load(); nonce++; }
</script>

{#if admin}
  <PageHead title={t('users.title')} sub={t('users.sub')}>
    <button class="btn btn-primary" onclick={() => (showForm = !showForm)}><Icon name="plus" size={15} /> {t('users.newUser')}</button>
  </PageHead>
{:else}
  <PageHead title={t('users.accountTitle')} sub={t('users.accountSub')} />
{/if}
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if admin && showForm}
  <form class="card grid sm:grid-cols-2 md:grid-cols-4 lg:grid-cols-7 gap-3 items-end mb-4 rise" onsubmit={create}>
    <div><label class="label" for="nu-l">{t('users.login')}</label><input id="nu-l" class="input font-mono" bind:value={form.login} pattern="[a-z_][a-z0-9_\-]{'{'}0,31{'}'}" required /></div>
    <div class="lg:col-span-2"><label class="label" for="nu-p">{t('users.password')}</label><div class="flex gap-1.5"><input id="nu-p" class="input" type={formPwShown ? 'text' : 'password'} bind:value={form.password} placeholder={t('users.passwordPlaceholder')} minlength="8" autocomplete="new-password" /><button type="button" class="btn" onclick={() => { form.password = genPassword(); formPwShown = true; }} title={t('users.generate')}><Icon name="refresh" size={14} /></button><button type="button" class="btn" onclick={() => (formPwShown = !formPwShown)} title={t('users.showPassword')} aria-pressed={formPwShown}><Icon name="eye" size={14} /></button></div></div>
    <div><label class="label" for="nu-e">E-mail</label><input id="nu-e" class="input" type="email" bind:value={form.email} /></div>
    <div><label class="label" for="nu-r">{t('users.role')}</label><select id="nu-r" class="input" bind:value={form.role}><option value="user">user</option><option value="admin">admin</option></select></div>
    <div><label class="label" for="nu-q" title={t('users.quotaHint')}>{t('users.quota')}</label><input id="nu-q" class="input" type="number" min="0" bind:value={form.quota} placeholder="—" title={t('users.quotaHint')} /></div>
    <div class="flex flex-wrap items-center justify-between gap-2"><label class="text-sm flex items-center gap-1.5"><input type="checkbox" bind:checked={form.shell} /> SSH shell</label><button class="btn btn-primary">{t('common.create')}</button></div>
  </form>
{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => { load(); nonce++; }} /></div>{/if}

{#if admin}
  <div class="tabs rise">
    {#each tabs as [id, key, icon]}<button class="tab {tab === id ? 'tab-on' : ''} inline-flex items-center gap-1.5" onclick={() => (tab = id)}><Icon name={icon} size={14} /> {t(key)}</button>{/each}
  </div>
  {#if tab === 'accounts'}
    <div class="card overflow-x-auto p-0 mb-4 rise">
      {#if !users}<Skeleton rows={5} />{:else}
      <table class="tbl">
        <thead><tr><th>{t('users.login')}</th><th>{t('users.role')}</th><th>{t('common.status')}</th><th>UID</th><th>{t('users.colAccess')}</th><th>E-mail</th><th>{t('users.colCreated')}</th><th></th></tr></thead>
        <tbody>
          {#each users as u, i (u.login)}
            <tr class="rise" style="--i:{i}">
              <td data-label={t('users.login')} class="font-medium">{u.login}{#if u.login === myLogin}<span class="tag tag-muted ml-1">{t('users.you')}</span>{/if}</td><td data-label={t('users.role')} class="text-muted">{u.role}</td>
              <td data-label={t('common.status')}><span class="tag {u.status === 'active' ? 'tag-ok' : u.status === 'deleting' ? 'tag-err' : 'tag-warn'}">{#if u.status === 'deleting'}<span class="dot dot-live"></span>{/if}{u.status}</span></td>
              <td data-label="UID" class="font-mono" title={u.home || ''}>{u.unix_uid ?? '—'}</td>
              <td data-label={t('users.colAccess')}>{#if u.role === 'user'}<span class="tag tag-muted">{u.shell ? 'SSH shell' : 'SFTP-only'}</span>{/if}</td>
              <td data-label="E-mail" class="text-muted">{u.email || '—'}</td><td data-label={t('users.colCreated')} class="text-xs text-muted">{when(u.created_at)}</td>
              <td data-label=""><div class="row-actions">
                <button class="btn btn-sm {panel?.login === u.login && panel.kind === 'account' ? 'btn-primary' : ''}" onclick={() => openPanel(u.login, 'account')} title={t('users.accountBtn')}><Icon name="user" size={13} /></button>
                <button class="btn btn-sm" onclick={() => askPassword(u)} title={t('users.changePassword')}><Icon name="key" size={13} /></button>
                {#if u.role === 'user' && u.status !== 'deleting'}<button class="btn btn-sm" onclick={() => (ask = askShell(u))}>{u.shell ? '→ SFTP-only' : '→ shell'}</button>{/if}
                {#if u.unix_uid != null}
                  <button class="btn btn-sm" onclick={() => openPanel(u.login, 'cron')}><Icon name="clock" size={13} /> cron</button>
                  <button class="btn btn-sm" onclick={() => openPanel(u.login, 'apps')}><Icon name="box" size={13} /> apps</button>
                  <button class="btn btn-sm" onclick={() => openPanel(u.login, 'valkey')}><Icon name="db" size={13} /> valkey</button>
                  <button class="btn btn-sm" onclick={() => openPanel(u.login, 'shares')}><Icon name="file" size={13} /> {t('users.sharesBtn')}</button>
                {/if}
                <button class="btn btn-sm" onclick={() => openPanel(u.login, 'tokens')}><Icon name="code" size={13} /> {t('users.tokensBtn')}</button>
                {#if u.login !== myLogin && (u.status === 'active' || u.status === 'suspended')}<button class="btn btn-sm" onclick={() => (ask = askStatus(u))}>{u.status === 'active' ? t('users.suspend') : t('users.unsuspend')}</button>{/if}
                {#if u.login !== myLogin}<button class="btn btn-danger btn-sm" onclick={() => { del = u; purge = false; confirmLogin = ''; }} title={t('users.delete')}><Icon name="trash" size={13} /></button>{/if}
              </div></td>
            </tr>
          {/each}
        </tbody>
      </table>
      {/if}
    </div>

    {#if panel}
      <div class="card rise" bind:this={panelEl} transition:slide={{ duration: dur(180) }}>
        <div class="flex justify-between items-center gap-2 mb-3"><span class="font-medium">{kindTitle(panel.kind)}: <span class="font-mono">{panel.login}</span></span><div class="flex gap-1"><button class="btn btn-sm" onclick={refreshPanel} title={t('common.refresh')}><Icon name="refresh" size={13} /></button><button class="btn btn-sm" onclick={() => (panel = null)}>{t('users.closePanel')}</button></div></div>
        {#key panel.login}
          {#if panel.kind === 'account'}
            {#if panelUser}<AccountCard user={panelUser} admin self={panelUser.login === myLogin} onchanged={load} onpassword={() => panelUser && askPassword(panelUser)} />{/if}
          {:else if panel.kind === 'cron'}<CronPanel login={panel.login} accounts={users ?? []} {nonce} />
          {:else if panel.kind === 'apps'}<AppsPanel login={panel.login} accounts={users ?? []} {nonce} />
          {:else if panel.kind === 'valkey'}<ValkeyPanel login={panel.login} accounts={users ?? []} {nonce} />
          {:else if panel.kind === 'shares'}{#if panelUser}<SharesPanel account={panelUser} admin {nonce} onjob={(id) => (job = id)} />{/if}
          {:else}<TokensPanel login={panel.login} admin self={panel.login === myLogin} {nonce} />{/if}
        {/key}
      </div>
    {/if}
  {:else}
    <div class="card rise">
      <div class="flex justify-between items-start gap-2 mb-3"><p class="text-xs text-muted">{tabHint(tab)}</p><button class="btn btn-sm" onclick={refreshAll} title={t('common.refresh')}><Icon name="refresh" size={13} /></button></div>
      {#if !users}<Skeleton rows={4} />
      {:else if tab === 'cron'}<CronPanel accounts={users} {nonce} />
      {:else if tab === 'apps'}<AppsPanel accounts={users} {nonce} />
      {:else}<ValkeyPanel accounts={users} {nonce} />{/if}
    </div>
  {/if}
{:else}
  {#if !me}<div class="card rise"><Skeleton rows={4} /></div>
  {:else}
    <div class="card mb-4 rise"><AccountCard user={me} self onchanged={load} onpassword={() => me && askPassword(me)} /></div>
    <div class="tabs rise">
      {#each sections as [id, key, icon]}<button class="tab {section === id ? 'tab-on' : ''} inline-flex items-center gap-1.5" onclick={() => (section = id)}><Icon name={icon} size={14} /> {t(key)}</button>{/each}
    </div>
    <div class="card rise">
      <div class="flex justify-between items-center gap-2 mb-3"><span class="font-medium">{t(sections.find(([id]) => id === section)?.[1] ?? 'users.tabCron')}</span><button class="btn btn-sm" onclick={refreshAll} title={t('common.refresh')}><Icon name="refresh" size={13} /></button></div>
      {#if section === 'cron'}<CronPanel login={me.login} accounts={[me]} {nonce} />
      {:else if section === 'apps'}<AppsPanel login={me.login} accounts={[me]} {nonce} />
      {:else if section === 'valkey'}<ValkeyPanel login={me.login} accounts={[me]} {nonce} />
      {:else if section === 'shares'}<SharesPanel account={me} {nonce} />
      {:else}<TokensPanel login={me.login} self {nonce} />{/if}
    </div>
  {/if}
{/if}

<Confirm bind:ask />

<Modal open={!!pw} title={t('users.newPasswordFor', { login: pw?.login ?? '' })} onclose={() => (pw = null)}>
  <p class="text-muted">{t('users.passwordHint')}</p>
  {#if pw}
    {#if pw.login === myLogin}
      <label class="label" for="pw-cur">{t('users.currentPassword')}</label>
      <input id="pw-cur" class="input font-mono mb-2" type="password" bind:value={pw.current} autocomplete="current-password" />
      <label class="label" for="pw-new">{t('users.newPassword')}</label>
    {/if}
    <div class="flex gap-1.5">
      <input id="pw-new" class="input font-mono" type={pw.show ? 'text' : 'password'} bind:value={pw.value} autocomplete="new-password" aria-label={t('users.password')} onkeydown={(e) => e.key === 'Enter' && setPassword()} />
      <button type="button" class="btn" onclick={() => pw && ((pw.value = genPassword()), (pw.show = true))} title={t('users.generate')}><Icon name="refresh" size={14} /></button>
      <button type="button" class="btn" onclick={() => pw && (pw.show = !pw.show)} title={t('users.showPassword')} aria-pressed={pw.show}><Icon name="eye" size={14} /></button>
    </div>
    <p class="text-xs text-muted">{pw.login === myLogin ? t('users.passwordSessionsSelf') : t('users.passwordSessions')}</p>
  {/if}
  {#snippet footer()}<button class="btn" onclick={() => (pw = null)}>{t('common.cancel')}</button><button class="btn btn-primary" onclick={setPassword}>{t('common.save')}</button>{/snippet}
</Modal>

<Modal open={!!del} title={t('users.deleteUserTitle', { login: del?.login ?? '' })} onclose={() => { del = null; confirmLogin = ''; }}>
  <p>{#if del?.role === 'admin'}{t('users.deleteUserNoteAdmin')}{:else}{t('users.deleteUserNote')}{/if}</p>
  {#if del?.unix_uid}
    <label class="flex items-start gap-2 p-2 rounded-md border {purge ? 'border-danger/50 bg-danger-soft' : 'border-line'} transition-colors"><input type="checkbox" bind:checked={purge} class="mt-0.5" /><span>{t('users.purgeLabel')} <code class="font-mono">{del.home || '/var/www/' + del.login}</code><br /><span class="text-xs text-muted">{t('users.purgeHint')}</span></span></label>
  {/if}
  <div><label class="label" for="cl">{t('users.confirmLogin')}</label><input id="cl" class="input font-mono" bind:value={confirmLogin} placeholder={del?.login} onkeydown={(e) => e.key === 'Enter' && remove()} /></div>
  {#snippet footer()}<button class="btn" onclick={() => (del = null)}>{t('common.cancel')}</button><button class="btn btn-danger" disabled={confirmLogin !== del?.login} onclick={remove}><Icon name="trash" size={14} /> {purge ? t('users.deleteWithFiles') : t('common.delete')}</button>{/snippet}
</Modal>
