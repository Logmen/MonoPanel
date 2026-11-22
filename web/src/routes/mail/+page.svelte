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
  // Установленные ветки PHP для сайта вебпочты. null — ещё не спрашивали:
  // список нужен, только пока вебпочта не установлена, а GET /php/versions
  // может сходить к пакетному менеджеру за матрицей доступности.
  let php = $state<any[] | null>(null);
  const newestPHP = $derived(php?.length ? php[php.length - 1].version : '');
  let error = $state('');
  let loading = $state(true);
  let job = $state<number | null>(null);
  let tab = $state<'domains' | 'boxes' | 'aliases'>('domains');
  // Ящики и алиасы одного домена фильтрует API (?domain=), как
  // mp mail box list --domain: у обычного аккаунта чужой домен — 404.
  let filter = $state('');
  let created = $state<any>(null);
  let dns = $state<any>(null);
  let dnsLoading = $state(false);
  let del = $state<{ kind: string; name: string; note: string } | null>(null);
  let purge = $state(false);
  // Кнопки в строке — иконки без подписи, поэтому каждая сначала объясняет,
  // что именно сейчас произойдёт.
  let ask = $state<Ask | null>(null);
  let boxEdit = $state<{ box: any; name: string; quota_mb: number; active: boolean; password: string } | null>(null);
  let aliasEdit = $state<{ address: string; destinations: string } | null>(null);
  let saving = $state(false);

  let install = $state({ hostname: '', pop3: true });
  let installFilled = false;
  let domainForm = $state({ name: '', user: '', lenient: false, send_only: false, dkim: true });
  let boxForm = $state({ address: '', password: '', name: '', quota_mb: 1024 });
  let aliasForm = $state({ address: '', destinations: '' });
  let webmailForm = $state({ domain: '', user: '', php_version: '', port: 2096 });
  let settings = $state({ hostname: '', max_size_mb: 50, pop3: true, dkim: true, port25: true, rbl: '', webmail_port: 0 });
  let showSettings = $state(false);

  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  const byDomain = (f: string) => (f ? '?domain=' + encodeURIComponent(f) : '');
  // Домен фильтра могли удалить в другой вкладке: на 404 показываем все
  // домены, а не роняем обновление всей страницы. f — фильтр, которому
  // соответствует ответ: пока запрос шёл, соседний мог уже сбросить фильтр,
  // а человек — выбрать другой домен.
  const listed = async (path: string): Promise<{ f: string; list: any[] }> => {
    const f = filter;
    try {
      return { f, list: await api<any[]>(path + byDomain(f)) };
    } catch (e) {
      if (!f || !(e instanceof ApiError) || e.status !== 404) throw e;
      if (filter === f) filter = '';
      return { f: '', list: await api<any[]>(path) };
    }
  };
  // Адреса пересылки вводят через запятую или столбиком — пробелов в адресе не бывает.
  const addressList = (s: string) => s.split(/[\s,;]+/).map((x) => x.trim()).filter(Boolean);

  // Всё разом: статус опрашивает почтовые порты, и ждать его, чтобы потом
  // сходить ещё три раза подряд, — это лишняя секунда на пустом месте.
  async function load() {
    try {
      const [status, d, b, a, u] = await Promise.all([
        api('/mail'), api('/mail/domains'), listed('/mail/mailboxes'), listed('/mail/aliases'),
        admin ? api('/users') : Promise.resolve([])
      ]);
      st = status;
      settings = { hostname: st.hostname ?? '', max_size_mb: st.max_size_mb || 50, pop3: !!st.pop3, dkim: !!st.dkim, port25: !!st.port25, rbl: (st.rbl ?? []).join(', '), webmail_port: st.webmail_port ?? 0 };
      // Форма установки берёт то, что сервер уже помнит (POP3 по умолчанию
      // включён), — но один раз, чтобы перезагрузка не стёрла введённое.
      if (!st.installed && !installFilled) { install = { hostname: st.hostname ?? '', pop3: st.pop3 ?? true }; installFilled = true; }
      domains = d as any[];
      // Список другого фильтра затёр бы тот, что уже показал showDomain.
      if (b.f === filter) boxes = b.list;
      if (a.f === filter) aliases = a.list;
      users = (u as any[]).filter((x) => x.role === 'user' && x.unix_uid);
      if (admin && st.installed && !st.webmail && php === null) loadPHP();
    } catch (e) { fail(e); } finally { loading = false; }
  }
  onMount(load);

  async function loadPHP() {
    try { php = ((await api('/php/versions')) as any).installed.filter((v: any) => v.status === 'installed'); } catch { /* в списке останется «новейшая» — ту выберет сервер */ }
  }
  async function showDomain(name: string) {
    filter = name;
    try {
      const [b, a] = await Promise.all([api('/mail/mailboxes' + byDomain(name)), api('/mail/aliases' + byDomain(name))]);
      if (filter !== name) return; // пока ждали ответа, выбрали другой домен
      boxes = b as any[];
      aliases = a as any[];
    } catch (e) { fail(e); }
  }

  async function doInstall() {
    error = '';
    try { const r: any = await api('/mail/install', { method: 'POST', json: { hostname: install.hostname.trim(), pop3: install.pop3 } }); job = r.job_id; } catch (e) { fail(e); }
  }
  // mp mail apply: в отличие от «Сохранить и применить», перечитывает сервисы
  // и тогда, когда ни один файл не изменился.
  async function applyNow() {
    error = '';
    try { const r: any = await api('/mail/apply', { method: 'POST' }); job = r.job_id; notify(t('mail.applying')); } catch (e) { fail(e); }
  }
  const askApply = (): Ask => ({ title: t('mail.askApplyTitle'), note: t('mail.askApplyNote'), action: t('mail.askApplyAction'), run: applyNow });
  async function saveSettings(e: Event) {
    e.preventDefault(); error = '';
    try {
      const body: any = { hostname: settings.hostname, max_size_mb: Number(settings.max_size_mb), pop3: settings.pop3, dkim: settings.dkim, port25: settings.port25,
        webmail_port: Number(settings.webmail_port) || 0, rbl: addressList(settings.rbl) };
      st = await api('/mail/settings', { method: 'PUT', json: body });
      notify(t('mail.settingsSaved')); showSettings = false; await load();
    } catch (e) { fail(e); }
  }
  async function addDomain(e: Event) {
    e.preventDefault(); error = '';
    try {
      const body: any = { name: domainForm.name.trim(), lenient: domainForm.send_only ? false : domainForm.lenient, send_only: domainForm.send_only };
      // Галочка DKIM есть, только когда подпись включена на сервере: без неё
      // ключ не выпускается в любом случае.
      if (st?.dkim) body.dkim = domainForm.dkim;
      if (admin) body.user = domainForm.user;
      await api('/mail/domains', { method: 'POST', json: body });
      // Владелец остаётся (домены одного аккаунта часто добавляют подряд), а
      // флаги — к значениям по умолчанию: снятая однажды галочка DKIM не
      // должна тихо оставить без подписи и следующий домен.
      domainForm = { ...domainForm, name: '', lenient: false, send_only: false, dkim: true };
      notify(t('mail.domainAdded')); await load();
    } catch (e) { fail(e); }
  }
  async function addBox(e: Event) {
    e.preventDefault(); error = '';
    try {
      const body: any = { address: boxForm.address.trim(), quota_mb: Number(boxForm.quota_mb) || 0, name: boxForm.name.trim() };
      if (boxForm.password) body.password = boxForm.password;
      created = await api('/mail/mailboxes', { method: 'POST', json: body });
      boxForm = { address: '', password: '', name: '', quota_mb: 1024 }; await load();
    } catch (e) { fail(e); }
  }
  async function addAlias(e: Event) {
    e.preventDefault(); error = '';
    try {
      await api('/mail/aliases', { method: 'POST', json: { address: aliasForm.address.trim(), destinations: addressList(aliasForm.destinations) } });
      aliasForm = { address: '', destinations: '' }; notify(t('mail.aliasCreated')); await load();
    } catch (e) { fail(e); }
  }
  // POST /mail/aliases создаёт или заменяет алиас: правка — та же операция с тем же адресом.
  async function saveAlias(e: Event) {
    e.preventDefault(); if (!aliasEdit || saving) return; error = '';
    const address = aliasEdit.address;
    saving = true;
    try {
      await api('/mail/aliases', { method: 'POST', json: { address, destinations: addressList(aliasEdit.destinations) } });
      aliasEdit = null; notify(t('mail.aliasSaved', { address })); await load();
    } catch (e) { fail(e); } finally { saving = false; }
  }
  async function newPassword(address: string) {
    error = '';
    try { const r: any = await api(`/mail/mailboxes/${address}`, { method: 'PATCH', json: {} }); created = { mailbox: { address }, password: r.password, reset: true }; } catch (e) { fail(e); }
  }
  const askPassword = (b: any): Ask => ({
    title: t('mail.askPasswordTitle', { address: b.address }),
    note: t('mail.askPasswordNote'),
    action: t('mail.askPasswordAction'),
    run: () => newPassword(b.address)
  });
  async function toggleBox(b: any) {
    error = '';
    try { await api(`/mail/mailboxes/${b.address}`, { method: 'PATCH', json: { active: !b.active } }); await load(); } catch (e) { fail(e); }
  }
  const askToggleBox = (b: any): Omit<Ask, 'run'> => b.active
    ? { title: t('mail.askBoxOffTitle', { address: b.address }), danger: true, action: t('mail.askBoxOffAction'),
        note: t('mail.askBoxOffNote') }
    : { title: t('mail.askBoxOnTitle', { address: b.address }), action: t('mail.askBoxOnAction'),
        note: t('mail.askBoxOnNote') };
  const editBox = (b: any) => (boxEdit = { box: b, name: b.name ?? '', quota_mb: b.quota_mb, active: b.active, password: '' });
  async function saveBox(e: Event) {
    e.preventDefault(); if (!boxEdit || saving) return; error = '';
    const b = boxEdit.box, body: any = {};
    if (boxEdit.name.trim() !== (b.name ?? '')) body.name = boxEdit.name.trim();
    if (Number(boxEdit.quota_mb) !== b.quota_mb) body.quota_mb = Number(boxEdit.quota_mb);
    if (boxEdit.active !== b.active) body.active = boxEdit.active;
    if (boxEdit.password) body.password = boxEdit.password;
    // Пустой PATCH для API значит «сгенерируй новый пароль» — нечаянно его
    // отправлять нельзя: для этого есть отдельная кнопка с подтверждением.
    if (!Object.keys(body).length) { boxEdit = null; notify(t('mail.nothingChanged')); return; }
    saving = true;
    try {
      await api(`/mail/mailboxes/${b.address}`, { method: 'PATCH', json: body });
      boxEdit = null; notify(t('mail.boxSaved', { address: b.address })); await load();
    } catch (e) { fail(e); } finally { saving = false; }
  }
  async function remove() {
    if (!del) return;
    error = '';
    try {
      if (del.kind === 'domain') await api(`/mail/domains/${del.name}`, { method: 'DELETE' });
      if (del.kind === 'box') await api(`/mail/mailboxes/${del.name}${purge ? '?purge=true' : ''}`, { method: 'DELETE' });
      if (del.kind === 'alias') await api(`/mail/aliases/${del.name}`, { method: 'DELETE' });
      // Фильтр по удалённому домену API отверг бы как несуществующий.
      if (del.kind === 'domain' && del.name === filter) filter = '';
      notify(t('mail.deleted', { name: del.name })); del = null; purge = false; await load();
    } catch (e) { fail(e); }
  }
  // Проверка живая и может идти секунды: окно, закрытое за это время (или
  // открытое уже для другого домена), ответ не должен открыть снова.
  let dnsReq = 0;
  async function showDNS(name: string) {
    const n = ++dnsReq;
    dnsLoading = true; dns = { domain: name, records: [] };
    try {
      const r = await api(`/mail/domains/${name}/dns`);
      if (n === dnsReq && dns) dns = r;
    } catch (e) { if (n === dnsReq) { fail(e); dns = null; } } finally { if (n === dnsReq) dnsLoading = false; }
  }
  async function patchDomain(d: any, body: any) {
    error = '';
    try { await api(`/mail/domains/${d.name}`, { method: 'PATCH', json: body }); await load(); } catch (e) { fail(e); }
  }
  // Домен «только отправка» не может быть приёмником (API это отвергает),
  // поэтому приёмник, переходящий в «только отправку», теряет режим тем же запросом.
  const askSendOnly = (d: any): Ask => d.send_only
    ? { title: t('mail.askReceiveTitle', { name: d.name }), note: t('mail.askReceiveNote'), action: t('mail.askReceiveAction'), run: () => patchDomain(d, { send_only: false }) }
    : { title: t('mail.askSendOnlyTitle', { name: d.name }), note: t('mail.askSendOnlyNote') + (d.lenient ? ' ' + t('mail.askSendOnlyNoteLenient') : ''), action: t('mail.askSendOnlyAction'),
        run: () => patchDomain(d, d.lenient ? { send_only: true, lenient: false } : { send_only: true }) };
  const askLenient = (d: any): Ask => d.lenient
    ? { title: t('mail.askStrictTitle', { name: d.name }), action: t('mail.askStrictAction'),
        note: t('mail.askStrictNote'), run: () => patchDomain(d, { lenient: false }) }
    : { title: t('mail.askLenientTitle', { name: d.name }), action: t('mail.askLenientAction'),
        note: t('mail.askLenientNote'), run: () => patchDomain(d, { lenient: true }) };
  const askDomainActive = (d: any): Ask => d.active
    ? { title: t('mail.askDomainOffTitle', { name: d.name }), danger: true, action: t('mail.askDomainOffAction'),
        note: d.send_only ? t('mail.askDomainOffNoteSendOnly') : t('mail.askDomainOffNote'), run: () => patchDomain(d, { active: false }) }
    : { title: t('mail.askDomainOnTitle', { name: d.name }), action: t('mail.askDomainOnAction'),
        note: d.send_only ? t('mail.askDomainOnNoteSendOnly') : t('mail.askDomainOnNote'), run: () => patchDomain(d, { active: true }) };
  async function rotateDKIM(name: string) {
    error = '';
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
    try {
      const body: any = { domain: webmailForm.domain.trim(), user: webmailForm.user, port: Number(webmailForm.port) || 0 };
      if (webmailForm.php_version) body.php_version = webmailForm.php_version;
      const r: any = await api('/mail/webmail', { method: 'POST', json: body }); job = r.job_id;
    } catch (e) { fail(e); }
  }
  // mp mail webmail <домен> поверх установленной вебпочты — тот же POST:
  // задача оставляет базу, её пароль, ключ DES и добавленные плагины, а
  // старую схему обновляет. Владельца API требует от администратора явно —
  // берём его у сайта вебпочты; порт и версию PHP не шлём, они остаются.
  async function reinstallWebmail() {
    error = '';
    try {
      const site: any = await api(`/sites/${st.webmail}`);
      const r: any = await api('/mail/webmail', { method: 'POST', json: { domain: st.webmail, user: site.login } });
      job = r.job_id;
    } catch (e) { fail(e); }
  }
  const askReinstallWebmail = (): Ask => ({ title: t('mail.askWebmailTitle', { domain: st.webmail }), note: t('mail.askWebmailNote'), action: t('mail.askWebmailAction'), run: reinstallWebmail });
  const mark = (s: string) => ({ ok: 'text-ok', missing: 'text-danger', mismatch: 'text-warn', unknown: 'text-muted' })[s] ?? 'text-muted';
  const markKey: Record<string, MsgKey> = { ok: 'mail.dnsOk', missing: 'mail.dnsMissing', mismatch: 'mail.dnsMismatch', unknown: 'mail.dnsUnknown' };
  const markText = (s: string) => (markKey[s] ? t(markKey[s]) : s);
</script>

<PageHead title={t('mail.title')} sub={loading ? '' : st?.installed ? `${st.hostname} · postfix ${st.versions?.postfix ?? ''} · dovecot ${st.versions?.dovecot ?? ''}` : t('mail.subNotInstalled')}>
  {#if st?.installed && admin}
    <button class="btn" onclick={() => (ask = askApply())} title={t('mail.applyTitle')}><Icon name="refresh" size={15} /> {t('mail.apply')}</button>
    <button class="btn" onclick={() => (showSettings = !showSettings)}><Icon name="settings" size={15} /> {t('mail.settings')}</button>
  {/if}
</PageHead>

{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
<!-- Журнал задачи остаётся и после её конца: и итог применения, и причина сбоя установки нужны человеку, а не только перезагрузке страницы. -->
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}

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
        <label class="flex items-center gap-2 text-sm pb-1.5" title={t('mail.pop3Hint')}><input type="checkbox" bind:checked={install.pop3} /> POP3 (110/995)</label>
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
      <div><label class="label" for="sm">{t('mail.maxSize')}</label><input id="sm" class="input" type="number" min="1" max="512" required bind:value={settings.max_size_mb} /></div>
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
      <div class="flex justify-between text-sm py-0.5"><span>{t('mail.maxSize')}</span><span class="tabular-nums">{st.max_size_mb}</span></div>
    </div>
    <div class="card rise" style="--i:1">
      <div class="text-xs text-muted mb-2">{t('mail.ports')}</div>
      <div class="flex flex-wrap gap-1.5">
        {#each st.ports ?? [] as p}
          <span class="text-xs px-1.5 py-0.5 rounded border {p.managed && p.open ? 'border-ok/40 text-ok' : p.managed ? 'border-danger/40 text-danger' : p.open ? 'border-warn/40 text-warn' : 'border-line text-muted'}"
                title={p.open ? p.owner || t('mail.portListening') : t('mail.portNotListening')}>{p.port} {p.name}{#if p.open && !p.managed}{' · '}{t('mail.portForeign')}{/if}</span>
        {/each}
      </div>
    </div>
    <div class="card rise" style="--i:2">
      <div class="text-xs text-muted mb-2">{t('mail.webmail')}</div>
      {#if st.webmail}
        <a class="text-sm text-accent-ink hover:underline inline-flex items-center gap-1 break-all" href={st.webmail_url} target="_blank">{st.webmail_url.replace('https://', '').replace(/\/$/, '')} <Icon name="external" size={12} /></a>
        <div class="text-xs text-muted mt-1">Roundcube {st.versions?.roundcube}{#if st.webmail_port}{' · '}{t('mail.webmailSite', { site: st.webmail })}{/if}</div>
        {#if admin}<button class="btn btn-sm mt-2" onclick={() => (ask = askReinstallWebmail())} title={t('mail.webmailReinstallTitle')}><Icon name="refresh" size={13} /> {t('mail.webmailReinstall')}</button>{/if}
      {:else if admin}
        <form class="space-y-2" onsubmit={installWebmail}>
          <input class="input font-mono text-sm" bind:value={webmailForm.domain} placeholder="webmail.example.com" aria-label={t('mail.webmailDomain')} required />
          <select class="input text-sm" bind:value={webmailForm.user} aria-label={t('mail.owner')} required><option value="">{t('mail.ownerPlaceholder')}</option>{#each users as u}<option value={u.login}>{u.login}</option>{/each}</select>
          <select class="input text-sm" bind:value={webmailForm.php_version} title={t('mail.webmailPhpTitle')} aria-label={t('mail.webmailPhpTitle')}><option value="">{t('mail.phpNewest')}{newestPHP ? ` (${newestPHP})` : ''}</option>{#each php ?? [] as v}<option value={v.version}>PHP {v.version}</option>{/each}</select>
          <div class="flex flex-wrap gap-2 items-center">
            <label class="flex items-center gap-1.5 text-xs text-muted" title={t('mail.webmailPortTitle')}>{t('mail.webmailPortShort')} <input class="input text-sm w-24" type="number" min="0" max="65535" bind:value={webmailForm.port} /></label>
            <button class="btn btn-primary btn-sm whitespace-nowrap ml-auto">{t('mail.installWebmail')}</button>
          </div>
          {#if php && !php.length}<p class="text-xs text-warn">{t('mail.noPhp')} <a href="/php" class="text-accent-ink hover:underline">{t('mail.toPhp')}</a></p>{/if}
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
      {created.reset ? t('mail.newPassword') : t('mail.mailbox')} <b>{created.mailbox.address}</b>{#if created.password}{' · '}{t('mail.passwordLabel')} <b class="select-all">{created.password}</b>{/if}
      {#if created.imap}<div class="text-xs text-muted mt-1">IMAP: {created.imap} · SMTP: {created.smtp}</div>{/if}
      {#if created.password}<div class="text-xs text-muted">{t('mail.shownOnce')}</div>{/if}
    </div>
  {/if}

  <div class="flex flex-wrap items-center gap-1 mb-3 text-sm">
    {#each [['domains', 'mail.tabDomains', domains.length], ['boxes', 'mail.tabBoxes', boxes.length], ['aliases', 'mail.tabAliases', aliases.length]] as [key, title, n]}
      <button class="px-3 py-1.5 rounded-md transition-colors {tab === key ? 'bg-accent-soft text-accent-ink font-medium' : 'text-muted hover:text-ink'}" onclick={() => (tab = key as any)}>{t(title as MsgKey)} <span class="tabular-nums opacity-60">{n}</span></button>
    {/each}
    {#if tab !== 'domains' && (domains.length > 1 || filter)}
      <select class="input text-sm w-auto ml-auto" value={filter} onchange={(e) => showDomain(e.currentTarget.value)} title={t('mail.filterTitle')} aria-label={t('mail.filterTitle')}>
        <option value="">{t('mail.allDomains')}</option>
        {#each domains as d}<option value={d.name}>{d.name}</option>{/each}
      </select>
    {/if}
  </div>

  {#if tab === 'domains'}
    <form class="card grid md:grid-cols-4 gap-3 items-end mb-3 rise" onsubmit={addDomain}>
      <div><label class="label" for="dn">{t('mail.domain')}</label><input id="dn" class="input font-mono" bind:value={domainForm.name} placeholder="example.com" required /></div>
      {#if admin}<div><label class="label" for="du">{t('mail.owner')}</label><select id="du" class="input" bind:value={domainForm.user} required><option value="">—</option>{#each users as u}<option value={u.login}>{u.login}</option>{/each}</select></div>{/if}
      <div class="flex flex-col gap-1 pb-1">
        <label class="flex items-center gap-2 text-sm" title={t('mail.sendOnlyHint')}>
          <input type="checkbox" bind:checked={domainForm.send_only} /> {t('mail.sendOnly')}
        </label>
        <label class="flex items-center gap-2 text-sm" title={t('mail.lenientHint')}>
          <input type="checkbox" bind:checked={domainForm.lenient} disabled={domainForm.send_only} /> {t('mail.lenient')}
        </label>
        {#if st.dkim}
          <label class="flex items-center gap-2 text-sm" title={t('mail.dkimKeyHint')}>
            <input type="checkbox" bind:checked={domainForm.dkim} /> {t('mail.dkimKey')}
          </label>
        {/if}
      </div>
      <button class="btn btn-primary">{t('mail.addDomain')}</button>
    </form>
    <div class="card overflow-x-auto p-0 rise">
      <table class="tbl"><thead><tr><th>{t('mail.domain')}</th><th>{t('mail.owner')}</th><th>{t('mail.colBoxes')}</th><th>{t('mail.colAliases')}</th><th>DKIM</th><th></th></tr></thead>
        <tbody>
          {#each domains as d, i}
            <tr class="rise" style="--i:{i}">
              <td data-label={t('mail.domain')} class="font-mono font-medium"><span class={d.active ? '' : 'text-muted'}>{d.name}</span>{#if !d.active}<span class="ml-2 text-[10px] px-1.5 py-0.5 rounded border border-line text-muted font-sans">{t('mail.domainOffBadge')}</span>{/if}{#if d.lenient}<span class="ml-2 text-[10px] px-1.5 py-0.5 rounded border border-warn/40 text-warn font-sans">{t('mail.lenientBadge')}</span>{/if}{#if d.send_only}<span class="ml-2 text-[10px] px-1.5 py-0.5 rounded border border-accent/40 text-accent-ink font-sans" title={t('mail.sendOnlyHint')}>{t('mail.sendOnlyBadge')}</span>{/if}</td>
              <td data-label={t('mail.owner')}>{d.login}</td>
              <td data-label={t('mail.colBoxes')} class="tabular-nums">{d.mailboxes}</td>
              <td data-label={t('mail.colAliases')} class="tabular-nums">{d.aliases}</td>
              <td data-label="DKIM" class="font-mono text-xs text-muted">{d.dkim_selector || '—'}</td>
              <td data-label=""><div class="row-actions">
                <button class="btn btn-sm" onclick={() => showDNS(d.name)}><Icon name="globe" size={13} /> DNS</button>
                <button class="btn btn-sm" onclick={() => (ask = askDKIM(d))} title={t('mail.rotateDkimTitle')}><Icon name="key" size={13} /></button>
                <button class="btn btn-sm" onclick={() => (ask = askSendOnly(d))} title={d.send_only ? t('mail.sendOnlyOffTitle') : t('mail.sendOnlyOnTitle')} aria-pressed={d.send_only}><Icon name="mail" size={13} /></button>
                {#if !d.send_only}<button class="btn btn-sm" onclick={() => (ask = askLenient(d))} title={d.lenient ? t('mail.strictTitle') : t('mail.lenientTitle')}><Icon name="shield" size={13} /></button>{/if}
                <button class="btn btn-sm" onclick={() => (ask = askDomainActive(d))} title={d.active ? t('mail.domainOffTitle') : t('mail.domainOnTitle')}><Icon name={d.active ? 'stop' : 'play'} size={13} /></button>
                <button class="btn btn-danger btn-sm" onclick={() => (del = { kind: 'domain', name: d.name, note: t('mail.delDomainNote') })} title={t('common.delete')}><Icon name="trash" size={13} /></button>
              </div></td>
            </tr>
          {/each}
          {#if !domains.length}<Empty text={t('mail.noDomains')} cols={6} />{/if}
        </tbody></table>
    </div>
  {:else if tab === 'boxes'}
    <form class="card grid md:grid-cols-6 gap-3 items-end mb-3 rise" onsubmit={addBox}>
      <div class="md:col-span-2"><label class="label" for="ba">{t('mail.address')}</label><input id="ba" class="input font-mono" bind:value={boxForm.address} placeholder="user@example.com" required /></div>
      <div><label class="label" for="bn">{t('common.name')}</label><input id="bn" class="input" bind:value={boxForm.name} placeholder={t('mail.namePlaceholder')} maxlength="128" /></div>
      <div><label class="label" for="bp">{t('mail.password')}</label><input id="bp" class="input" type="password" bind:value={boxForm.password} placeholder={t('mail.passwordAuto')} title={t('mail.passwordAutoHint')} minlength="8" maxlength="1024" autocomplete="new-password" /></div>
      <div><label class="label" for="bq">{t('mail.quotaMb')}</label><input id="bq" class="input" type="number" min="0" max="1048576" required bind:value={boxForm.quota_mb} /></div>
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
                <button class="btn btn-sm" onclick={() => editBox(b)} title={t('mail.editBox')}><Icon name="pencil" size={13} /></button>
                <button class="btn btn-sm" onclick={() => (ask = askPassword(b))}><Icon name="key" size={13} /> {t('mail.passwordBtn')}</button>
                <button class="btn btn-sm" onclick={() => (ask = { ...askToggleBox(b), run: () => toggleBox(b) })}>{b.active ? t('mail.disableBtn') : t('mail.enableBtn')}</button>
                <button class="btn btn-danger btn-sm" onclick={() => (del = { kind: 'box', name: b.address, note: t('mail.delBoxNote') })} title={t('common.delete')}><Icon name="trash" size={13} /></button>
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
    <p class="text-xs text-muted mb-3">{@html t('mail.catchAllHint')} {t('mail.aliasReplaceHint')}</p>
    <div class="card overflow-x-auto p-0 rise">
      <table class="tbl"><thead><tr><th>{t('mail.address')}</th><th>{t('mail.colDest')}</th><th></th></tr></thead>
        <tbody>
          {#each aliases as a, i}
            <tr class="rise" style="--i:{i}">
              <td data-label={t('mail.address')} class="font-mono font-medium">{a.address}</td>
              <td data-label={t('mail.colDest')} class="font-mono text-xs break-all">{addressList(a.destination).join(', ')}</td>
              <td data-label=""><div class="row-actions">
                <button class="btn btn-sm" onclick={() => (aliasEdit = { address: a.address, destinations: addressList(a.destination).join('\n') })} title={t('mail.editAlias')}><Icon name="pencil" size={13} /></button>
                <button class="btn btn-danger btn-sm" onclick={() => (del = { kind: 'alias', name: a.address, note: t('mail.delAliasNote') })} title={t('common.delete')}><Icon name="trash" size={13} /></button>
              </div></td>
            </tr>
          {/each}
          {#if !aliases.length}<Empty text={t('mail.noAliases')} cols={3} />{/if}
        </tbody></table>
    </div>
  {/if}
{/if}

<Modal open={!!dns} title={t('mail.dnsTitle', { domain: dns?.domain ?? '' })} onclose={() => (dns = null)}>
  {#if dnsLoading}
    <p class="text-muted">{t('mail.dnsChecking')}</p>
  {:else if dns}
    {#if dns.records?.length}<p class={dns.ok ? 'text-ok' : 'text-warn'}>{dns.ok ? t('mail.dnsAllOk') : t('mail.dnsNotOk')}</p>{/if}
    <div class="space-y-3 max-h-[60vh] overflow-y-auto -mx-1 px-1">
      {#each dns.records as r}
        <div class="border border-line rounded-md p-2.5">
          <div class="flex justify-between gap-2 text-xs mb-1">
            <span class="font-mono text-muted">{r.type} · {r.name}{#if !r.required}{' · '}{t('mail.dnsOptional')}{/if}</span>
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

<Modal open={!!boxEdit} title={t('mail.editBoxTitle', { address: boxEdit?.box.address ?? '' })} onclose={() => (boxEdit = null)}>
  {#if boxEdit}
    <form id="box-edit" class="space-y-3" onsubmit={saveBox}>
      <div><label class="label" for="en">{t('common.name')}</label><input id="en" class="input" bind:value={boxEdit.name} placeholder={t('mail.namePlaceholder')} maxlength="128" /></div>
      <div><label class="label" for="eq">{t('mail.quotaMb')}</label><input id="eq" class="input" type="number" min="0" max="1048576" required bind:value={boxEdit.quota_mb} />
        <p class="text-xs text-muted mt-1">{t('mail.quotaHint')}</p></div>
      <div><label class="label" for="ep">{t('mail.newPasswordLabel')}</label><input id="ep" class="input font-mono" type="text" bind:value={boxEdit.password} minlength="8" maxlength="1024" autocomplete="new-password" spellcheck="false" />
        <p class="text-xs text-muted mt-1">{t('mail.newPasswordHint')}</p></div>
      <label class="flex items-start gap-2"><input type="checkbox" class="mt-0.5" bind:checked={boxEdit.active} /><span>{t('mail.boxActiveLabel')}<br /><span class="text-xs text-muted">{t('mail.boxActiveHint')}</span></span></label>
    </form>
  {/if}
  {#snippet footer()}<button class="btn" onclick={() => (boxEdit = null)}>{t('common.cancel')}</button><button class="btn btn-primary" form="box-edit" disabled={saving}>{t('common.save')}</button>{/snippet}
</Modal>

<Modal open={!!aliasEdit} title={t('mail.editAliasTitle', { address: aliasEdit?.address ?? '' })} onclose={() => (aliasEdit = null)}>
  {#if aliasEdit}
    <form id="alias-edit" onsubmit={saveAlias}>
      <label class="label" for="ed">{t('mail.destinations')}</label>
      <textarea id="ed" class="input font-mono text-xs h-28 resize-y" bind:value={aliasEdit.destinations} required spellcheck="false"></textarea>
      <p class="text-xs text-muted mt-1">{t('mail.destinationsHint')}</p>
    </form>
  {/if}
  {#snippet footer()}<button class="btn" onclick={() => (aliasEdit = null)}>{t('common.cancel')}</button><button class="btn btn-primary" form="alias-edit" disabled={saving}>{t('common.save')}</button>{/snippet}
</Modal>

<Modal open={!!del} title={t('mail.deleteTitle', { name: del?.name ?? '' })} onclose={() => { del = null; purge = false; }}>
  <p class="text-muted">{del?.note}</p>
  {#if del?.kind === 'box'}<label class="flex items-center gap-2"><input type="checkbox" bind:checked={purge} /> {t('mail.purge')}</label>{/if}
  {#snippet footer()}<button class="btn" onclick={() => { del = null; purge = false; }}>{t('common.cancel')}</button><button class="btn btn-danger" onclick={remove}>{t('common.delete')}</button>{/snippet}
</Modal>
