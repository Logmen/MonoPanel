<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError } from '$lib/api';
  import { auth, notify } from '$lib/state.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import { t, type MsgKey } from '$lib/i18n/index.svelte';

  const admin = $derived(auth.me?.role === 'admin');
  let st = $state<any>(null);
  let domains = $state<any[]>([]);
  let boxes = $state<any[]>([]);
  let aliases = $state<any[]>([]);
  let users = $state<any[]>([]);
  let error = $state('');
  let loading = $state(true);
  let job = $state<number | null>(null);
  let tab = $state<'domains' | 'boxes' | 'aliases'>('domains');
  let created = $state<any>(null);
  let dns = $state<any>(null);
  let dnsLoading = $state(false);
  let del = $state<{ kind: string; name: string; note: string } | null>(null);
  let purge = $state(false);
  // Кнопки в строке — иконки без подписи, поэтому каждая сначала объясняет,
  // что именно сейчас произойдёт.
  let ask = $state<Ask | null>(null);

  let install = $state({ hostname: '' });
  let domainForm = $state({ name: '', user: '', lenient: false });
  let boxForm = $state({ address: '', password: '', name: '', quota_mb: 1024 });
  let aliasForm = $state({ address: '', destinations: '' });
  let webmailForm = $state({ domain: '', user: '', port: 2096 });
  let settings = $state({ hostname: '', max_size_mb: 50, pop3: true, dkim: true, port25: true, rbl: '', webmail_port: 0 });
  let showSettings = $state(false);

  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };

  // Всё разом: статус опрашивает почтовые порты, и ждать его, чтобы потом
  // сходить ещё три раза подряд, — это лишняя секунда на пустом месте.
  async function load() {
    try {
      const [status, d, b, a, u] = await Promise.all([
        api('/mail'), api('/mail/domains'), api('/mail/mailboxes'), api('/mail/aliases'),
        admin ? api('/users') : Promise.resolve([])
      ]);
      st = status;
      settings = { hostname: st.hostname ?? '', max_size_mb: st.max_size_mb || 50, pop3: !!st.pop3, dkim: !!st.dkim, port25: !!st.port25, rbl: (st.rbl ?? []).join(', '), webmail_port: st.webmail_port ?? 0 };
      domains = d as any[];
      boxes = b as any[];
      aliases = a as any[];
      users = (u as any[]).filter((x) => x.role === 'user' && x.unix_uid);
    } catch (e) { fail(e); } finally { loading = false; }
  }
  onMount(load);

  async function doInstall() {
    error = '';
    try { const r: any = await api('/mail/install', { method: 'POST', json: install }); job = r.job_id; } catch (e) { fail(e); }
  }
  async function saveSettings(e: Event) {
    e.preventDefault(); error = '';
    try {
      const body: any = { hostname: settings.hostname, max_size_mb: settings.max_size_mb, pop3: settings.pop3, dkim: settings.dkim, port25: settings.port25,
        webmail_port: Number(settings.webmail_port) || 0, rbl: settings.rbl.split(',').map((s) => s.trim()).filter(Boolean) };
      st = await api('/mail/settings', { method: 'PUT', json: body });
      notify(t('mail.settingsSaved')); showSettings = false; await load();
    } catch (e) { fail(e); }
  }
  async function addDomain(e: Event) {
    e.preventDefault(); error = '';
    try {
      const body: any = { name: domainForm.name.trim(), lenient: domainForm.lenient };
      if (admin) body.user = domainForm.user;
      await api('/mail/domains', { method: 'POST', json: body });
      domainForm.name = ''; notify(t('mail.domainAdded')); await load();
    } catch (e) { fail(e); }
  }
  async function addBox(e: Event) {
    e.preventDefault(); error = '';
    try {
      const body: any = { address: boxForm.address.trim(), quota_mb: boxForm.quota_mb, name: boxForm.name };
      if (boxForm.password) body.password = boxForm.password;
      created = await api('/mail/mailboxes', { method: 'POST', json: body });
      boxForm = { address: '', password: '', name: '', quota_mb: 1024 }; await load();
    } catch (e) { fail(e); }
  }
  async function addAlias(e: Event) {
    e.preventDefault(); error = '';
    try {
      await api('/mail/aliases', { method: 'POST', json: { address: aliasForm.address.trim(), destinations: aliasForm.destinations.split(',').map((s) => s.trim()).filter(Boolean) } });
      aliasForm = { address: '', destinations: '' }; notify(t('mail.aliasCreated')); await load();
    } catch (e) { fail(e); }
  }
  async function newPassword(address: string) {
    try { const r: any = await api(`/mail/mailboxes/${address}`, { method: 'PATCH', json: {} }); created = { mailbox: { address }, password: r.password, reset: true }; } catch (e) { fail(e); }
  }
  const askPassword = (b: any): Ask => ({
    title: t('mail.askPasswordTitle', { address: b.address }),
    note: t('mail.askPasswordNote'),
    action: t('mail.askPasswordAction'),
    run: () => newPassword(b.address)
  });
  async function toggleBox(b: any) {
    try { await api(`/mail/mailboxes/${b.address}`, { method: 'PATCH', json: { active: !b.active } }); await load(); } catch (e) { fail(e); }
  }
  const askToggleBox = (b: any): Omit<Ask, 'run'> => b.active
    ? { title: t('mail.askBoxOffTitle', { address: b.address }), danger: true, action: t('mail.askBoxOffAction'),
        note: t('mail.askBoxOffNote') }
    : { title: t('mail.askBoxOnTitle', { address: b.address }), action: t('mail.askBoxOnAction'),
        note: t('mail.askBoxOnNote') };
  async function remove() {
    if (!del) return;
    try {
      if (del.kind === 'domain') await api(`/mail/domains/${del.name}`, { method: 'DELETE' });
      if (del.kind === 'box') await api(`/mail/mailboxes/${del.name}${purge ? '?purge=true' : ''}`, { method: 'DELETE' });
      if (del.kind === 'alias') await api(`/mail/aliases/${del.name}`, { method: 'DELETE' });
      notify(t('mail.deleted', { name: del.name })); del = null; purge = false; await load();
    } catch (e) { fail(e); }
  }
  async function showDNS(name: string) {
    dnsLoading = true; dns = { domain: name, records: [] };
    try { dns = await api(`/mail/domains/${name}/dns`); } catch (e) { fail(e); dns = null; } finally { dnsLoading = false; }
  }
  async function toggleLenient(d: any) {
    try { await api(`/mail/domains/${d.name}`, { method: 'PATCH', json: { lenient: !d.lenient } }); await load(); } catch (e) { fail(e); }
  }
  const askLenient = (d: any): Omit<Ask, 'run'> => d.lenient
    ? { title: t('mail.askStrictTitle', { name: d.name }), action: t('mail.askStrictAction'),
        note: t('mail.askStrictNote') }
    : { title: t('mail.askLenientTitle', { name: d.name }), action: t('mail.askLenientAction'),
        note: t('mail.askLenientNote') };
  async function rotateDKIM(name: string) {
    try { await api(`/mail/domains/${name}/dkim`, { method: 'POST', json: {} }); notify(t('mail.dkimRotated')); await load(); await showDNS(name); } catch (e) { fail(e); }
  }
  const askDKIM = (d: any): Ask => ({
    title: t('mail.askDkimTitle', { name: d.name }),
    note: d.dkim_selector ? t('mail.askDkimNoteRotate') : t('mail.askDkimNoteNew'),
    action: t('mail.askDkimAction'),
    run: () => rotateDKIM(d.name)
  });
  async function installWebmail(e: Event) {
    e.preventDefault(); error = '';
    try { const r: any = await api('/mail/webmail', { method: 'POST', json: { ...webmailForm, port: Number(webmailForm.port) || 0 } }); job = r.job_id; } catch (e) { fail(e); }
  }
  const mark = (s: string) => ({ ok: 'text-ok', missing: 'text-danger', mismatch: 'text-warn', unknown: 'text-muted' })[s] ?? 'text-muted';
  const markKey: Record<string, MsgKey> = { ok: 'mail.dnsOk', missing: 'mail.dnsMissing', mismatch: 'mail.dnsMismatch', unknown: 'mail.dnsUnknown' };
  const markText = (s: string) => (markKey[s] ? t(markKey[s]) : s);
</script>

<PageHead title={t('mail.title')} sub={loading ? '' : st?.installed ? `${st.hostname} · postfix ${st.versions?.postfix ?? ''} · dovecot ${st.versions?.dovecot ?? ''}` : t('mail.subNotInstalled')}>
  {#if st?.installed && admin}
    <button class="btn" onclick={() => (showSettings = !showSettings)}><Icon name="settings" size={15} /> {t('mail.settings')}</button>
  {/if}
</PageHead>

{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => { job = null; load(); }} /></div>{/if}

{#if loading}
  <div class="card rise"><Skeleton rows={5} /></div>
{:else if st && !st.installed}
  <div class="card mb-4 rise">
    <div class="text-sm mb-3">{@html t('mail.notInstalled')}</div>
    {#if admin}
      <form class="flex flex-wrap gap-3 items-end" onsubmit={(e) => { e.preventDefault(); doInstall(); }}>
        <div class="grow max-w-sm"><label class="label" for="h">{t('mail.hostnameLabel')}</label>
          <input id="h" class="input font-mono" bind:value={install.hostname} placeholder="mail.example.com" />
          <p class="text-xs text-muted mt-1">{t('mail.hostnameHint')}</p></div>
        <button class="btn btn-primary">{t('mail.install')}</button>
      </form>
    {:else}
      <div class="text-sm text-muted">{t('mail.askAdmin')}</div>
    {/if}
  </div>
{/if}

{#if st?.installed && !loading}
  {#if showSettings && admin}
    <form class="card mb-4 grid md:grid-cols-3 gap-3 items-end rise" onsubmit={saveSettings}>
      <div><label class="label" for="sh">{t('mail.serverName')}</label><input id="sh" class="input font-mono" bind:value={settings.hostname} /></div>
      <div><label class="label" for="sm">{t('mail.maxSize')}</label><input id="sm" class="input" type="number" min="1" max="512" bind:value={settings.max_size_mb} /></div>
      <div><label class="label" for="sr">{t('mail.rbl')}</label><input id="sr" class="input font-mono" bind:value={settings.rbl} placeholder="zen.spamhaus.org" /></div>
      <div><label class="label" for="sw">{t('mail.webmailPort')}</label><input id="sw" class="input" type="number" min="0" max="65535" bind:value={settings.webmail_port} />
        <p class="text-xs text-muted mt-1">{t('mail.webmailPortHint')}</p></div>
      <label class="flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={settings.pop3} /> POP3 (110/995)</label>
      <label class="flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={settings.dkim} /> {t('mail.signDkim')}</label>
      <label class="flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={settings.port25} /> {t('mail.port25')}</label>
      <div class="md:col-span-3 flex gap-2"><button class="btn btn-primary">{t('mail.saveApply')}</button><button type="button" class="btn" onclick={() => (showSettings = false)}>{t('common.cancel')}</button></div>
    </form>
  {/if}

  <div class="grid gap-3 md:grid-cols-3 mb-4">
    <div class="card rise">
      <div class="text-xs text-muted mb-2">{t('mail.services')}</div>
      {#each st.services ?? [] as s}
        <div class="flex justify-between text-sm py-0.5"><span class="font-mono">{s.unit.replace('.service', '')}</span>
          <span class={s.active_state === 'active' ? 'text-ok' : 'text-danger'}>{s.active_state}</span></div>
      {/each}
      <div class="flex justify-between text-sm py-0.5 border-t border-line mt-2 pt-2"><span>TLS</span>
        <span class={st.tls === 'acme' || st.tls === 'custom' ? 'text-ok' : 'text-warn'}>{st.tls}{st.cert_until ? ' ' + t('mail.certUntil', { date: st.cert_until.slice(0, 10) }) : ''}</span></div>
    </div>
    <div class="card rise" style="--i:1">
      <div class="text-xs text-muted mb-2">{t('mail.ports')}</div>
      <div class="flex flex-wrap gap-1.5">
        {#each st.ports ?? [] as p}
          <span class="text-xs px-1.5 py-0.5 rounded border {p.managed && p.open ? 'border-ok/40 text-ok' : p.managed ? 'border-danger/40 text-danger' : p.open ? 'border-warn/40 text-warn' : 'border-line text-muted'}"
                title={p.open ? p.owner || t('mail.portListening') : t('mail.portNotListening')}>{p.port} {p.name}{#if p.open && !p.managed} · {t('mail.portForeign')}{/if}</span>
        {/each}
      </div>
    </div>
    <div class="card rise" style="--i:2">
      <div class="text-xs text-muted mb-2">{t('mail.webmail')}</div>
      {#if st.webmail}
        <a class="text-sm text-accent-ink hover:underline inline-flex items-center gap-1 break-all" href={st.webmail_url} target="_blank">{st.webmail_url.replace('https://', '').replace(/\/$/, '')} <Icon name="external" size={12} /></a>
        <div class="text-xs text-muted mt-1">Roundcube {st.versions?.roundcube}{#if st.webmail_port} · {t('mail.webmailSite', { site: st.webmail })}{/if}</div>
      {:else if admin}
        <form class="space-y-2" onsubmit={installWebmail}>
          <input class="input font-mono text-sm" bind:value={webmailForm.domain} placeholder="webmail.example.com" required />
          <div class="flex gap-2">
            <select class="input text-sm" bind:value={webmailForm.user} required><option value="">{t('mail.ownerPlaceholder')}</option>{#each users as u}<option value={u.login}>{u.login}</option>{/each}</select>
            <input class="input text-sm w-24" type="number" min="0" max="65535" bind:value={webmailForm.port} title={t('mail.webmailPortTitle')} />
            <button class="btn btn-primary btn-sm whitespace-nowrap">{t('mail.installWebmail')}</button>
          </div>
          <p class="text-xs text-muted">{t('mail.webmailPortNote', { hostname: st.hostname })}</p>
        </form>
      {:else}
        <div class="text-sm text-muted">{t('mail.webmailNotInstalled')}</div>
      {/if}
    </div>
  </div>

  {#if st.warnings?.length}
    <div class="card mb-4 border-warn/40 rise">
      {#each st.warnings as w}<div class="text-sm flex gap-2 items-start"><Icon name="alert" size={15} class="text-warn shrink-0 mt-0.5" /><span>{w}</span></div>{/each}
    </div>
  {/if}

  {#if created}
    <div class="card mb-4 text-sm font-mono rise border-accent/40">
      {created.reset ? t('mail.newPassword') : t('mail.mailbox')} <b>{created.mailbox.address}</b>{#if created.password} · {t('mail.passwordLabel')} <b class="select-all">{created.password}</b>{/if}
      {#if created.imap}<div class="text-xs text-muted mt-1">IMAP: {created.imap} · SMTP: {created.smtp}</div>{/if}
      <div class="text-xs text-muted">{t('mail.shownOnce')}</div>
    </div>
  {/if}

  <div class="flex gap-1 mb-3 text-sm">
    {#each [['domains', 'mail.tabDomains', domains.length], ['boxes', 'mail.tabBoxes', boxes.length], ['aliases', 'mail.tabAliases', aliases.length]] as [key, title, n]}
      <button class="px-3 py-1.5 rounded-md transition-colors {tab === key ? 'bg-accent-soft text-accent-ink font-medium' : 'text-muted hover:text-ink'}" onclick={() => (tab = key as any)}>{t(title as MsgKey)} <span class="tabular-nums opacity-60">{n}</span></button>
    {/each}
  </div>

  {#if tab === 'domains'}
    <form class="card grid md:grid-cols-4 gap-3 items-end mb-3 rise" onsubmit={addDomain}>
      <div><label class="label" for="dn">{t('mail.domain')}</label><input id="dn" class="input font-mono" bind:value={domainForm.name} placeholder="example.com" required /></div>
      {#if admin}<div><label class="label" for="du">{t('mail.owner')}</label><select id="du" class="input" bind:value={domainForm.user} required><option value="">—</option>{#each users as u}<option value={u.login}>{u.login}</option>{/each}</select></div>{/if}
      <label class="flex items-center gap-2 text-sm pb-2" title={t('mail.lenientHint')}>
        <input type="checkbox" bind:checked={domainForm.lenient} /> {t('mail.lenient')}
      </label>
      <button class="btn btn-primary">{t('mail.addDomain')}</button>
    </form>
    <div class="card overflow-x-auto p-0 rise">
      <table class="tbl"><thead><tr><th>{t('mail.domain')}</th><th>{t('mail.owner')}</th><th>{t('mail.colBoxes')}</th><th>{t('mail.colAliases')}</th><th>DKIM</th><th></th></tr></thead>
        <tbody>
          {#each domains as d, i}
            <tr class="rise" style="--i:{i}">
              <td data-label={t('mail.domain')} class="font-mono font-medium">{d.name}{#if d.lenient}<span class="ml-2 text-[10px] px-1.5 py-0.5 rounded border border-warn/40 text-warn font-sans">{t('mail.lenientBadge')}</span>{/if}</td>
              <td data-label={t('mail.owner')}>{d.login}</td>
              <td data-label={t('mail.colBoxes')} class="tabular-nums">{d.mailboxes}</td>
              <td data-label={t('mail.colAliases')} class="tabular-nums">{d.aliases}</td>
              <td data-label="DKIM" class="font-mono text-xs text-muted">{d.dkim_selector || '—'}</td>
              <td data-label=""><div class="row-actions">
                <button class="btn btn-sm" onclick={() => showDNS(d.name)}><Icon name="globe" size={13} /> DNS</button>
                <button class="btn btn-sm" onclick={() => (ask = askDKIM(d))} title={t('mail.rotateDkimTitle')}><Icon name="key" size={13} /></button>
                <button class="btn btn-sm" onclick={() => (ask = { ...askLenient(d), run: () => toggleLenient(d) })} title={d.lenient ? t('mail.strictTitle') : t('mail.lenientTitle')}><Icon name="shield" size={13} /></button>
                <button class="btn btn-danger btn-sm" onclick={() => (del = { kind: 'domain', name: d.name, note: t('mail.delDomainNote') })}><Icon name="trash" size={13} /></button>
              </div></td>
            </tr>
          {/each}
          {#if !domains.length}<Empty text={t('mail.noDomains')} cols={6} />{/if}
        </tbody></table>
    </div>
  {:else if tab === 'boxes'}
    <form class="card grid md:grid-cols-5 gap-3 items-end mb-3 rise" onsubmit={addBox}>
      <div class="md:col-span-2"><label class="label" for="ba">{t('mail.address')}</label><input id="ba" class="input font-mono" bind:value={boxForm.address} placeholder="user@example.com" required /></div>
      <div><label class="label" for="bn">{t('common.name')}</label><input id="bn" class="input" bind:value={boxForm.name} placeholder={t('mail.namePlaceholder')} /></div>
      <div><label class="label" for="bq">{t('mail.quotaMb')}</label><input id="bq" class="input" type="number" min="0" bind:value={boxForm.quota_mb} /></div>
      <button class="btn btn-primary">{t('mail.createBox')}</button>
    </form>
    <div class="card overflow-x-auto p-0 rise">
      <table class="tbl"><thead><tr><th>{t('mail.address')}</th><th>{t('common.name')}</th><th>{t('mail.quota')}</th><th>{t('mail.state')}</th><th></th></tr></thead>
        <tbody>
          {#each boxes as b, i}
            <tr class="rise" style="--i:{i}">
              <td data-label={t('mail.address')} class="font-mono font-medium">{b.address}</td>
              <td data-label={t('common.name')}>{b.name || '—'}</td>
              <td data-label={t('mail.quota')} class="tabular-nums">{b.quota_mb ? t('mail.mb', { n: b.quota_mb }) : t('mail.noLimit')}</td>
              <td data-label={t('mail.state')}><span class={b.active ? 'text-ok' : 'text-muted'}>{b.active ? t('mail.boxActive') : t('mail.boxDisabled')}</span></td>
              <td data-label=""><div class="row-actions">
                <button class="btn btn-sm" onclick={() => (ask = askPassword(b))}><Icon name="key" size={13} /> {t('mail.passwordBtn')}</button>
                <button class="btn btn-sm" onclick={() => (ask = { ...askToggleBox(b), run: () => toggleBox(b) })}>{b.active ? t('mail.disableBtn') : t('mail.enableBtn')}</button>
                <button class="btn btn-danger btn-sm" onclick={() => (del = { kind: 'box', name: b.address, note: t('mail.delBoxNote') })}><Icon name="trash" size={13} /></button>
              </div></td>
            </tr>
          {/each}
          {#if !boxes.length}<Empty text={t('mail.noBoxes')} cols={5} />{/if}
        </tbody></table>
    </div>
  {:else}
    <form class="card grid md:grid-cols-4 gap-3 items-end mb-3 rise" onsubmit={addAlias}>
      <div><label class="label" for="aa">{t('mail.address')}</label><input id="aa" class="input font-mono" bind:value={aliasForm.address} placeholder="info@example.com" required /></div>
      <div class="md:col-span-2"><label class="label" for="ad">{t('mail.destinations')}</label><input id="ad" class="input font-mono" bind:value={aliasForm.destinations} placeholder="user@example.com, second@example.com" required /></div>
      <button class="btn btn-primary">{t('mail.createAlias')}</button>
    </form>
    <p class="text-xs text-muted mb-3">{@html t('mail.catchAllHint')}</p>
    <div class="card overflow-x-auto p-0 rise">
      <table class="tbl"><thead><tr><th>{t('mail.address')}</th><th>{t('mail.colDest')}</th><th></th></tr></thead>
        <tbody>
          {#each aliases as a, i}
            <tr class="rise" style="--i:{i}">
              <td data-label={t('mail.address')} class="font-mono font-medium">{a.address}</td>
              <td data-label={t('mail.colDest')} class="font-mono text-xs">{a.destination}</td>
              <td data-label=""><div class="row-actions"><button class="btn btn-danger btn-sm" onclick={() => (del = { kind: 'alias', name: a.address, note: t('mail.delAliasNote') })}><Icon name="trash" size={13} /></button></div></td>
            </tr>
          {/each}
          {#if !aliases.length}<Empty text={t('mail.noAliases')} cols={3} />{/if}
        </tbody></table>
    </div>
  {/if}
{/if}

<Modal open={!!dns} title={t('mail.dnsTitle', { domain: dns?.domain ?? '' })}>
  {#if dnsLoading}
    <p class="text-muted">{t('mail.dnsChecking')}</p>
  {:else if dns}
    <div class="space-y-3 max-h-[60vh] overflow-y-auto -mx-1 px-1">
      {#each dns.records as r}
        <div class="border border-line rounded-md p-2.5">
          <div class="flex justify-between gap-2 text-xs mb-1">
            <span class="font-mono text-muted">{r.type} · {r.name}</span>
            <span class={mark(r.status)}>{markText(r.status)}</span>
          </div>
          <div class="font-mono text-xs break-all select-all">{r.value}</div>
          {#if r.found && r.status !== 'ok'}<div class="text-xs text-muted mt-1 break-all">{t('mail.dnsNow', { found: r.found })}</div>{/if}
          {#if r.note}<div class="text-xs text-muted mt-1">{r.note}</div>{/if}
        </div>
      {/each}
    </div>
  {/if}
  {#snippet footer()}<button class="btn" onclick={() => (dns = null)}>{t('common.close')}</button>{/snippet}
</Modal>

<Confirm bind:ask />

<Modal open={!!del} title={t('mail.deleteTitle', { name: del?.name ?? '' })} onclose={() => { del = null; purge = false; }}>
  <p class="text-muted">{del?.note}</p>
  {#if del?.kind === 'box'}<label class="flex items-center gap-2"><input type="checkbox" bind:checked={purge} /> {t('mail.purge')}</label>{/if}
  {#snippet footer()}<button class="btn" onclick={() => { del = null; purge = false; }}>{t('common.cancel')}</button><button class="btn btn-danger" onclick={remove}>{t('common.delete')}</button>{/snippet}
</Modal>
