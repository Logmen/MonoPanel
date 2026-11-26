<script lang="ts">
  import { onMount, untrack } from 'svelte';
  import { page } from '$app/state';
  import { slide } from 'svelte/transition';
  import { api, ApiError, bytes } from '$lib/api';
  import { auth, notify, dur } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import FileManager from '$lib/components/FileManager.svelte';
  import { t, dateLocale, type MsgKey } from '$lib/i18n/index.svelte';
  const domain = $derived(page.params.domain!);
  const admin = $derived(auth.me?.role === 'admin');
  // saved is the record as the server has it (header, tabs, certificate
  // names); site is the settings form: the same record with the edits.
  let saved = $state.raw<any>(null);
  let site = $state<any>(null);
  let php = $state<any[]>([]);
  let presets = $state<any[]>([]);
  // Имена пресетов приходят с сервера по-русски; два не латинских подменяем по id.
  const presetName = (p: any) => (p.id === '' ? t('sites.presetUniversal') : p.id === 'bitrix' ? t('sites.presetBitrix') : p.name);
  // Каталог CMS приходит с сервера по-английски: пояснения и источники известных — из словаря.
  const cmsNotes: Record<string, MsgKey> = { wordpress: 'site.cmsNoteWordpress', joomla: 'site.cmsNoteJoomla', opencart: 'site.cmsNoteOpencart', bitrix: 'site.cmsNoteBitrix' };
  const cmsLabel = (c: any) => (c.id === 'bitrix' ? t('sites.presetBitrix') : c.name);
  const cmsNote = (c: any) => (c.id in cmsNotes ? t(cmsNotes[c.id]) : (c.notes ?? ''));
  const cmsSources: Record<string, MsgKey> = { wordpress: 'site.cmsSourceWordpress', joomla: 'site.cmsSourceJoomla', opencart: 'site.cmsSourceOpencart', bitrix: 'site.cmsSourceBitrix' };
  const cmsSource = (c: any) => (c.id in cmsSources ? t(cmsSources[c.id]) : (c.source ?? ''));
  const editionKeys: Record<string, MsgKey> = { start: 'site.bitrixStart', standard: 'site.bitrixStandard', small_business: 'site.bitrixSmallBusiness', business: 'site.bitrixBusiness' };
  const editionLabel = (id: string) => (id in editionKeys ? t(editionKeys[id]) : id);
  let tab = $state<'settings' | 'php' | 'nginx' | 'files' | 'logs' | 'cms'>('settings');
  let logType = $state('access');
  let logLines = $state(200);
  let log = $state<any>(null);
  let logError = $state('');
  let logBusy = $state(false);
  let job = $state<number | null>(null);
  let ask = $state<Ask | null>(null);
  // The record as `mp site show` prints it, fetched when the dialog opens.
  let record = $state<any>(null);
  let recordOpen = $state(false);
  // Certificate of this site and the order dialog (HTTP-01 or DNS-01); both
  // lists and the order itself are the administrator's.
  let certs = $state<any[]>([]);
  let providers = $state<any[]>([]);
  let tlsOpen = $state(false);
  /* CMS tab: the catalogue, the install form, the credentials shown once */
  let cmsList = $state<any[]>([]);
  let cmsForm = $state({ cms: 'wordpress', title: '', admin_login: '', admin_password: '', admin_email: '', edition: 'start', solution: 'clean', solutionId: '', force: false });
  let cmsResult = $state<any>(null);
  let cmsBusy = $state(false);
  const cmsInfo = $derived(cmsList.find((c) => c.id === cmsForm.cms));
  let tform = $state({ staging: false, dns: '', email: '' });
  let error = $state('');
  let aliases = $state('');
  let allow = $state('');
  /* PHP tab */
  let phpInfo = $state<any>(null);
  let overrides = $state<{ key: string; value: string }[]>([]);
  let newKey = $state('');
  let newValue = $state('');
  /* nginx tab */
  let nginx = $state<any>(null);
  let custom = $state('');
  let showGenerated = $state(false);
  let nginxBusy = $state(false);
  let nginxError = $state('');

  // The certificate given by hand (PATCH certificate): its id, or auto.
  let certPick = $state('auto');
  function take(s: any) {
    saved = s;
    site = structuredClone(s);
    certPick = s.cert_pinned && s.certificate_id ? String(s.certificate_id) : 'auto';
    aliases = (s.aliases || []).join(', ');
    allow = (s.allow_from || []).join(', ');
    overrides = Object.entries(s.php_ini || {}).map(([key, value]) => ({ key, value: String(value) }));
  }
  async function loadSite() {
    try {
      take(await api(`/sites/${domain}`));
      if (admin) certs = await api('/certificates');
    } catch (e: any) { error = e.text || String(e); }
  }
  // Lists for the selects; they do not change when a job of this site ends.
  async function loadRefs() {
    try {
      const [v, p, c, d] = await Promise.all([api('/php/versions'), api('/sites/presets'), api('/cms'), admin ? api('/dns-providers') : Promise.resolve([])]);
      php = (v as any).installed.filter((x: any) => x.status === 'installed');
      presets = p as any[];
      cmsList = c as any[];
      providers = d as any[];
    } catch (e: any) { error = e.text || String(e); }
  }
  const cert = $derived(saved ? (certs.find((c) => c.id === saved.certificate_id) ?? certs.find((c) => c.name === saved.domain) ?? null) : null);
  // A name covers the host as browsers read it: the same name, or *. with
  // exactly one label in place of the star (acme.NameCovers).
  const nameCovers = (n: string, host: string) => {
    n = n.toLowerCase().replace(/\.$/, '');
    if (n === host) return !n.includes('*');
    if (!n.startsWith('*.') || n.slice(1).includes('*')) return false;
    const rest = n.slice(1);
    return host.endsWith(rest) && host.length > rest.length && !host.slice(0, -rest.length).includes('.');
  };
  // The certificates the site may be given by hand: valid ones that cover it.
  const fitting = $derived(saved ? certs.filter((c) => c.status === 'valid' && (c.names ?? []).some((n: string) => nameCovers(n, saved.domain))) : []);
  // Installing a CMS is a job; the administrator password comes back once
  // in the response and never appears in the job log.
  async function installCMS(e: Event) {
    e.preventDefault(); error = ''; cmsBusy = true;
    try {
      const body: any = { cms: cmsForm.cms, force: cmsForm.force };
      for (const k of ['title', 'admin_login', 'admin_password', 'admin_email'] as const) if (cmsForm[k]) body[k] = cmsForm[k];
      // Editions and solutions exist where the catalogue lists editions (1C-Bitrix); the API refuses them elsewhere.
      if (cmsInfo?.editions?.length) { body.edition = cmsForm.edition; body.solution = cmsForm.solution === 'custom' ? cmsForm.solutionId.trim() : cmsForm.solution; }
      cmsResult = await api(`/sites/${domain}/cms`, { method: 'POST', json: body });
      job = cmsResult.job_id;
      cmsForm.admin_password = '';
      notify(t('site.cmsInstallStarted', { cms: cmsForm.cms }));
    } catch (e: any) { error = e.text || String(e); notify(error, 'err'); } finally { cmsBusy = false; }
  }
  const cmsName = (id: string) => {
    const c = cmsList.find((c) => c.id === id);
    return c ? cmsLabel(c) : id;
  };
  const cmsAdminPath = (id: string) => ({ wordpress: 'wp-admin/', joomla: 'administrator/', opencart: 'admin/', bitrix: 'bitrix/admin/' } as Record<string, string>)[id] ?? '';
  async function issueTLS(e: Event) {
    e.preventDefault(); error = '';
    try {
      const r: any = await api(`/sites/${domain}/tls/issue`, { method: 'POST', json: { staging: tform.staging, dns: tform.dns || undefined, email: tform.email.trim() || undefined } });
      job = r.job_id; tlsOpen = false;
      // The order turns SSL on for the site: the certificate job re-applies only auto-SSL sites.
      saved = { ...saved, ssl: 'auto' }; site.ssl = 'auto';
    } catch (e: any) { error = e.text || String(e); notify(error, 'err'); }
  }
  /* apply, fix, suspend: the same jobs as the buttons on the Sites list and `mp site …` */
  const actionToast: Record<string, MsgKey> = { apply: 'sites.toastApply', suspend: 'sites.toastSuspend', unsuspend: 'sites.toastUnsuspend', fix: 'site.toastFix' };
  async function siteAction(act: string) {
    error = '';
    try {
      const r: any = await api(`/sites/${domain}/${act}`, { method: 'POST' });
      job = r.job_id;
      if (r.site) saved = { ...saved, status: r.site.status };
      notify(t(actionToast[act], { domain }));
    } catch (e) { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); }
  }
  const askApply = (): Ask => ({ title: t('sites.askApplyTitle', { domain }), note: t('sites.askApplyNote', { domain }), action: t('common.apply'), run: () => siteAction('apply') });
  const askFix = (): Ask => ({ title: t('site.askFixTitle', { domain }), note: t('site.askFixNote', { login: saved.login }), action: t('site.askFixAction'), run: () => siteAction('fix') });
  const askSuspend = (): Ask => saved.status === 'suspended'
    ? { title: t('sites.askEnableTitle', { domain }), action: t('sites.enable'), note: t('sites.askEnableNote'), run: () => siteAction('unsuspend') }
    : { title: t('sites.askSuspendTitle', { domain }), danger: true, action: t('sites.suspend'), note: t('sites.askSuspendNote'), run: () => siteAction('suspend') };
  async function openRecord() {
    record = null; recordOpen = true;
    // "$schema" is the API's link to the response schema, not a field of the site.
    try { const r: any = await api(`/sites/${domain}`); record = Object.fromEntries(Object.entries(r).filter(([k]) => !k.startsWith('$'))); } catch (e: any) { recordOpen = false; error = e.text || String(e); }
  }
  onMount(() => { loadSite(); loadRefs(); });
  // A tab loads its data the first time it opens; only the tab is a dependency,
  // the loads themselves (and the buttons that repeat them) change nothing here.
  $effect(() => { const cur = tab; untrack(() => { if (cur === 'php' && !phpInfo) loadPHP(); if (cur === 'nginx' && !nginx) loadNginx(); if (cur === 'logs' && !log && !logBusy) loadLog(); }); });

  const words = (s: string) => s.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean);
  const sameSet = (a: string[], b: string[]) => { const x = [...new Set(a)].sort(), y = [...new Set(b)].sort(); return x.length === y.length && x.every((v, i) => v === y[i]); };
  // Only what the person changed goes to the server, as with `mp site set`
  // and only the flags given: an untouched value cannot trip its own check
  // (an IPv6 address the host has since lost would refuse every save).
  function changes(): Record<string, unknown> {
    const s = site, o = saved, b: Record<string, unknown> = {};
    const al = words(aliases.toLowerCase());
    if (!sameSet(al, o.aliases ?? [])) b.aliases = al;
    const af = words(allow);
    if (!sameSet(af, o.allow_from ?? [])) b.allow_from = af;
    // Empty means "keep" for these in PATCH, so an emptied field is not sent.
    for (const k of ['mode', 'backend', 'php_version', 'ssl', 'redirect_www', 'fpm_pm', 'client_max_body', 'ip']) {
      const v = String(s[k] ?? '').trim();
      if (v && v !== String(o[k] ?? '')) b[k] = v;
    }
    const dr = String(s.docroot ?? '').trim().replace(/^\/+|\/+$/g, '');
    if (dr !== (o.docroot ?? '')) b.docroot = dr;
    const v6 = String(s.ipv6 ?? '').trim();
    if (v6 !== (o.ipv6 ?? '') && !(v6 === 'none' && !o.ipv6)) b.ipv6 = v6 || 'none';
    for (const k of ['http2', 'http3', 'redirect_https', 'static_by_nginx', 'allow_exec', 'security_headers']) if (!!s[k] !== !!o[k]) b[k] = !!s[k];
    const mc = Number(s.fpm_max_children);
    if (mc > 0 && mc !== o.fpm_max_children) b.fpm_max_children = mc;
    const rl = Number(s.rate_limit) || 0;
    if (rl !== (o.rate_limit || 0)) b.rate_limit = rl;
    if ((s.preset ?? '') !== (o.preset ?? '')) b.preset = s.preset ?? '';
    if ((s.session_store ?? '') !== (o.session_store ?? '')) b.session_store = s.session_store || 'files';
    if (admin && certPick !== (o.cert_pinned && o.certificate_id ? String(o.certificate_id) : 'auto')) b.certificate = certPick;
    if (admin && !!s.default_tls !== !!o.default_tls) b.default_tls = !!s.default_tls;
    return b;
  }
  async function save(e: Event) {
    e.preventDefault();
    error = '';
    try {
      const res: any = await api(`/sites/${domain}`, { method: 'PATCH', json: changes() });
      job = res.job_id;
      if (res.site) take(res.site);
      notify(t('site.settingsSaved'));
    } catch (e) { error = e instanceof ApiError ? e.text : String(e); }
  }
  async function loadPHP() { try { phpInfo = await api(`/sites/${domain}/php`); } catch (e: any) { error = e.text || String(e); } }
  function addOverride() {
    const k = newKey.trim();
    if (!k) return;
    const i = overrides.findIndex((o) => o.key === k);
    if (i >= 0) overrides[i].value = newValue; else overrides = [...overrides, { key: k, value: newValue }];
    newKey = ''; newValue = '';
  }
  function useDefault(key: string, value: string) { newKey = key; newValue = value; }
  async function savePHP() {
    error = '';
    const php_ini: Record<string, string> = {};
    for (const o of overrides) if (o.key.trim()) php_ini[o.key.trim()] = o.value.trim();
    for (const k of Object.keys(saved.php_ini || {})) if (!(k in php_ini)) php_ini[k] = '';
    try {
      const res: any = await api(`/sites/${domain}`, { method: 'PATCH', json: { php_ini } });
      job = res.job_id;
      notify(t('site.phpSaved'));
      if (res.site) take(res.site);
      await loadPHP();
    } catch (e) { error = e instanceof ApiError ? e.text : String(e); }
  }
  async function loadNginx() { try { nginx = await api(`/sites/${domain}/nginx`); custom = nginx.custom; } catch (e: any) { error = e.text || String(e); } }
  // An empty text removes the file (mp site nginx --clear).
  async function saveNginx(text: string) {
    nginxBusy = true; nginxError = '';
    try {
      nginx = await api(`/sites/${domain}/nginx`, { method: 'PUT', json: { custom: text } });
      custom = nginx.custom;
      notify(custom.trim() ? t('site.nginxReloaded') : t('site.nginxCustomRemoved'));
    } catch (e) { nginxError = e instanceof ApiError ? e.text : String(e); }
    finally { nginxBusy = false; }
  }
  const askNginxClear = (): Ask => ({ title: t('site.askNginxClearTitle', { domain }), note: t('site.askNginxClearNote', { path: nginx?.custom_path ?? '' }), action: t('common.delete'), danger: true, run: () => saveNginx('') });
  function tabKey(e: KeyboardEvent) {
    if (e.key === 'Tab') { e.preventDefault(); const ta = e.target as HTMLTextAreaElement; const s = ta.selectionStart; custom = custom.slice(0, s) + '    ' + custom.slice(ta.selectionEnd); queueMicrotask(() => ta.setSelectionRange(s + 4, s + 4)); }
  }
  async function loadLog() {
    logBusy = true; logError = '';
    try { log = await api(`/sites/${domain}/logs/${logType}?lines=${logLines}`); } catch (e: any) { log = null; logError = e.text || String(e); } finally { logBusy = false; }
  }
  const logTypes = ['access', 'error', 'php', 'slow', 'apache-access', 'apache-error'];
  const tabs: [typeof tab, MsgKey, string][] = [['settings', 'site.tabSettings', 'settings'], ['cms', 'site.tabCms', 'plus'], ['php', 'site.tabPhp', 'code'], ['nginx', 'site.tabNginx', 'file'], ['files', 'site.tabFiles', 'box'], ['logs', 'site.tabLogs', 'terminal']];
</script>

<PageHead title={domain} mono back="/sites" sub={saved ? t('site.sub', { login: saved.login, ip: saved.ipv6 ? `${saved.ip}, ${saved.ipv6}` : saved.ip, stack: saved.mode === 'proxy' ? 'proxy → ' + saved.backend : 'PHP ' + saved.php_version + ' · ' + saved.mode }) : ''}>
  {#if saved}
    <a class="btn btn-sm" href="https://{domain}" target="_blank" rel="noopener"><Icon name="external" size={13} /> {t('site.open')}</a>
    <button class="btn btn-sm" onclick={() => (ask = askApply())} title={t('sites.applyTitle')}><Icon name="refresh" size={13} /> {t('site.applyBtn')}</button>
    <button class="btn btn-sm" onclick={() => (ask = askFix())} title={t('site.fixTitle')}><Icon name="lock" size={13} /> {t('site.fixBtn')}</button>
    {#if admin}{#if saved.status === 'suspended'}<button class="btn btn-sm" onclick={() => (ask = askSuspend())}><Icon name="play" size={13} /> {t('sites.enableBtn')}</button>{:else}<button class="btn btn-sm" onclick={() => (ask = askSuspend())}><Icon name="stop" size={13} /> {t('sites.stopBtn')}</button>{/if}{/if}
    <button class="btn btn-sm" onclick={openRecord} title={t('site.recordHint')}><Icon name="code" size={13} /> JSON</button>
    <span class="tag {saved.status === 'active' ? 'tag-ok' : saved.status === 'error' ? 'tag-err' : 'tag-warn'}">{saved.status}</span>
    <span class="tag {saved.certificate_id ? 'tag-ok' : 'tag-muted'}"><Icon name="shield" size={11} /> {saved.certificate_id ? 'https' : 'http'}</span>
  {/if}
</PageHead>
{#if error}<p class="text-danger text-sm mb-3 flex items-center gap-1.5"><Icon name="alert" size={15} /> {error}</p>{/if}
{#if saved?.last_error}<div class="card mb-4 border-danger/40 text-sm flex items-start gap-2 rise"><Icon name="alert" size={15} class="text-danger shrink-0 mt-0.5" /><span class="min-w-0 break-words">{t('site.lastError', { error: saved.last_error })}</span></div>{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => loadSite()} /></div>{/if}
{#if site}
  <div class="tabs rise">
    {#each tabs as [id, title, icon]}<button class="tab {tab === id ? 'tab-on' : ''} inline-flex items-center gap-1.5" onclick={() => (tab = id)}><Icon name={icon} size={14} /> {t(title)}</button>{/each}
  </div>

  {#if tab === 'settings'}
    <form class="card grid md:grid-cols-3 gap-3 rise" onsubmit={save}>
      <div class="md:col-span-3 text-xs text-muted font-mono break-all">docroot /var/www/{site.login}/data/www/{domain}{site.docroot ? '/' + site.docroot : ''}</div>
      <div><label class="label" for="al">{t('site.aliases')}</label><input id="al" class="input" bind:value={aliases} placeholder="www.example.com, shop.example.com" /></div>
      <div><label class="label" for="mode">{t('site.mode')}</label><select id="mode" class="input" bind:value={site.mode}><option value="fpm">nginx + php-fpm</option><option value="apache">nginx + Apache</option><option value="proxy">proxy → backend</option></select></div>
      {#if site.mode === 'proxy'}<div><label class="label" for="be">Backend</label><input id="be" class="input font-mono" bind:value={site.backend} placeholder="http://127.0.0.1:3000" required /></div>{:else}
      <div><label class="label" for="php">PHP</label><select id="php" class="input" bind:value={site.php_version}>{#if !saved.php_version}<option value="">{t('sites.phpLatest')}</option>{/if}{#each php as v}<option value={v.version}>{v.version}</option>{/each}</select></div>{/if}
      {#if site.mode !== 'proxy'}<div><label class="label" for="pr">{t('site.preset')}</label><select id="pr" class="input" bind:value={site.preset}>{#each presets as p}<option value={p.id}>{presetName(p)}</option>{/each}</select></div>
      <div><label class="label" for="ss">{t('site.sessions')}</label><select id="ss" class="input" bind:value={site.session_store}><option value="">{t('site.sessionsFiles')}</option><option value="valkey">{t('site.sessionsValkey')}</option></select>
        {#if site.session_store === 'valkey'}<div class="text-xs text-muted mt-1">{t('site.sessionsHint', { login: site.login, version: site.php_version })}</div>{/if}</div>{/if}
      <div><label class="label" for="dr">{t('site.docrootSub')}</label><input id="dr" class="input" bind:value={site.docroot} placeholder="public" /></div>
      <div><label class="label" for="ssl">SSL</label><select id="ssl" class="input" bind:value={site.ssl}><option value="auto">auto (Let's Encrypt)</option><option value="none">none</option></select>
        <div class="text-xs mt-1 flex flex-wrap items-center gap-2">
          {#if cert}<span class="tag {cert.status === 'valid' ? 'tag-ok' : cert.status === 'error' ? 'tag-err' : 'tag-warn'}">{cert.status}</span><span class="text-muted">{cert.issuer || cert.name}{cert.not_after ? ' · ' + t('site.certUntil', { date: new Date(cert.not_after).toLocaleDateString(dateLocale()) }) : ''}</span>{:else if saved.certificate_id}<span class="text-muted">{t('site.certInstalled')}</span>{:else}<span class="text-muted">{t('site.noCert')}</span>{/if}
          {#if admin}<button type="button" class="btn btn-sm" onclick={() => { tform = { staging: false, dns: '', email: '' }; tlsOpen = true; }}><Icon name="plus" size={12} /> {t('site.issue')}</button>{/if}
        </div>
        {#if cert?.last_error}<div class="text-xs text-danger mt-1 break-words">{cert.last_error}</div>{/if}
      </div>
      {#if admin}<div><label class="label" for="cp">{t('site.certPick')}</label><select id="cp" class="input" bind:value={certPick}><option value="auto">{t('site.certAuto')}</option>{#each fitting as c}<option value={String(c.id)}>{c.name} ({c.names.join(', ')})</option>{/each}</select>
        <div class="text-xs text-muted mt-1">{t('site.certPickHint')}</div></div>{/if}
      <div><label class="label" for="rw">www</label><select id="rw" class="input" bind:value={site.redirect_www}><option value="none">{t('site.wwwAsIs')}</option><option value="to_root">{t('site.wwwToRoot')}</option><option value="to_www">{t('site.wwwToWww')}</option></select></div>
      <div><label class="label" for="pm">php-fpm pm</label><select id="pm" class="input" bind:value={site.fpm_pm}><option value="ondemand">ondemand</option><option value="dynamic">dynamic</option><option value="static">static</option></select></div>
      <div><label class="label" for="mc">max_children</label><input id="mc" class="input" type="number" min="1" max="512" required bind:value={site.fpm_max_children} /></div>
      <div><label class="label" for="mb">client_max_body_size</label><input id="mb" class="input font-mono" required pattern={'[0-9]+[kKmMgG]?'} bind:value={site.client_max_body} /></div>
      <div><label class="label" for="ip4">{t('site.ip')}</label><input id="ip4" class="input font-mono" required={!!saved.ip} bind:value={site.ip} placeholder="203.0.113.10" />
        {#if admin}<div class="text-xs text-muted mt-1">{t('site.ipHint')}</div>{/if}</div>
      <div><label class="label" for="ip6">{t('site.ipv6')}</label><input id="ip6" class="input font-mono" bind:value={site.ipv6} placeholder="2001:db8::10" /></div>
      <div><label class="label" for="rl">{t('site.rateLimit')}</label><input id="rl" class="input" type="number" min="0" max="10000" bind:value={site.rate_limit} /></div>
      <div class="md:col-span-3"><label class="label" for="allow">{t('site.allowFrom')}</label><input id="allow" class="input font-mono" bind:value={allow} placeholder="203.0.113.0/24, 198.51.100.7" /></div>
      <div class="md:col-span-3 flex flex-wrap gap-x-5 gap-y-2 text-sm">
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.redirect_https} /> HTTP → HTTPS (+HSTS)</label>
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.http2} /> HTTP/2</label>
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.http3} /> HTTP/3</label>
        {#if admin}<label class="flex items-center gap-1.5" title={t('site.defaultTLSHint')}><input type="checkbox" bind:checked={site.default_tls} /> {t('site.defaultTLS')}</label>{/if}
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.static_by_nginx} /> {t('site.staticByNginx')}</label>
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.allow_exec} /> {t('site.allowExec')}</label>
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.security_headers} /> {t('site.securityHeaders')}</label>
      </div>
      <div class="md:col-span-3"><button class="btn btn-primary"><Icon name="save" size={14} /> {t('site.saveApply')}</button></div>
    </form>

  {:else if tab === 'cms'}
    <div class="card rise">
      {#if saved.cms}
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div>
            <div class="font-medium">{cmsName(saved.cms)} {saved.cms_version}</div>
            <p class="text-xs text-muted">{saved.cms_at ? t('site.cmsInstalledAt', { date: new Date(saved.cms_at).toLocaleString(dateLocale()) }) : t('site.cmsInstalled')} · {t('site.cmsPreset', { preset: saved.preset || '—' })} · {t('site.cmsOwner', { login: saved.login })}{#if saved.cms_database}{' · '}{t('site.cmsDatabase', { db: saved.cms_database })}{/if}</p>
          </div>
          <a class="btn btn-sm" href={(saved.certificate_id ? 'https://' : 'http://') + domain + '/' + cmsAdminPath(saved.cms)} target="_blank" rel="noopener">{t('site.openAdmin')}</a>
        </div>
        <p class="text-sm text-muted mt-3">{t('site.cmsReplaceHint')}</p>
      {/if}
      {#if cmsResult}
        <div class="p-3 rounded-lg border border-accent bg-accent-soft text-sm mt-3">
          <div class="font-medium text-accent-ink">{t('site.cmsCreds', { cms: cmsName(cmsResult.cms) })}</div>
          <div class="font-mono text-xs mt-1 break-all">{cmsResult.admin_url}<br />{t('site.cmsLogin', { login: cmsResult.admin_login })}{#if cmsResult.admin_password}{' · '}{t('site.cmsPassword', { password: cmsResult.admin_password })}{/if} · {cmsResult.admin_email}<br />{t('site.cmsDatabase', { db: cmsResult.database })}</div>
        </div>
      {/if}
      <form class="grid md:grid-cols-3 gap-3 mt-4 items-end" onsubmit={installCMS}>
        <div class="md:col-span-3 text-sm text-muted">{t('site.cmsIntro')}</div>
        <div><label class="label" for="cms">CMS</label><select id="cms" class="input" bind:value={cmsForm.cms}>{#each cmsList as c}<option value={c.id}>{cmsLabel(c)}</option>{/each}</select></div>
        <div><label class="label" for="ct">{t('site.cmsTitle')}</label><input id="ct" class="input" bind:value={cmsForm.title} placeholder={domain} /></div>
        <div><label class="label" for="cl">{t('site.cmsAdminLogin')}</label><input id="cl" class="input" bind:value={cmsForm.admin_login} placeholder="admin" /></div>
        <div><label class="label" for="cp">{t('site.cmsAdminPassword')}</label><input id="cp" class="input" type="password" autocomplete="new-password" bind:value={cmsForm.admin_password} placeholder={t('site.cmsPasswordPh')} /></div>
        <div><label class="label" for="ce">{t('site.cmsAdminEmail')}</label><input id="ce" class="input" bind:value={cmsForm.admin_email} placeholder={t('site.cmsEmailPh')} /></div>
        {#if cmsInfo?.editions?.length}
          <div><label class="label" for="ced">{t('site.bitrixEdition')}</label><select id="ced" class="input" bind:value={cmsForm.edition}>{#each cmsInfo.editions as ed}<option value={ed}>{editionLabel(ed)}</option>{/each}</select></div>
          <div><label class="label" for="csol">{t('site.bitrixSolution')}</label><select id="csol" class="input" bind:value={cmsForm.solution}><option value="clean">{t('site.bitrixClean')}</option><option value="demo">{t('site.bitrixDemo')}</option><option value="custom">{t('site.bitrixCustom')}</option></select></div>
          {#if cmsForm.solution === 'custom'}<div><label class="label" for="csid">{t('site.bitrixSolutionId')}</label><input id="csid" class="input font-mono" bind:value={cmsForm.solutionId} placeholder="vendor.solution" required /></div>{/if}
        {/if}
        <label class="flex items-center gap-1.5 text-sm md:col-span-2"><input type="checkbox" bind:checked={cmsForm.force} /> {t('site.cmsForce')}</label>
        <div class="md:col-span-3 text-xs text-muted">{cmsNote(cmsInfo ?? {})}{#if cmsInfo?.source}<div class="mt-1">{t('site.cmsSource', { source: cmsSource(cmsInfo) })} · {t('site.cmsPreset', { preset: cmsInfo.preset })}</div>{/if}</div>
        <div class="md:col-span-3"><button class="btn btn-primary" disabled={cmsBusy}><Icon name="plus" size={14} /> {cmsBusy ? t('site.cmsStarting') : t('site.cmsInstall')}</button></div>
      </form>
    </div>

  {:else if tab === 'php'}
    <div class="grid lg:grid-cols-5 gap-4">
      <div class="card lg:col-span-3 p-0 overflow-hidden rise">
        <div class="px-4 py-3 border-b border-line flex flex-wrap items-center justify-between gap-x-3"><span class="font-medium text-sm">{t('site.phpEffective')}</span><span class="text-xs text-muted font-mono min-w-0 [overflow-wrap:anywhere]" title={phpInfo?.socket ?? ''}>{phpInfo ? `PHP ${phpInfo.version} · ${phpInfo.pool_path}` : ''}</span></div>
        {#if phpInfo}
          <!-- Keys and values may wrap: the card takes 3/5 of the row from 1024px and clips
               what sticks out, and with redis.session.locking_enabled on one line the table
               was wider than the card up to ~1180px, the "edit" buttons cut off. A key breaks
               after its dots and underscores (<wbr>, so a copied key stays clean), anything
               longer anywhere; the span keeps it one flex item in the phone layout. -->
          <table class="tbl">
            <thead><tr><th>{t('site.phpKey')}</th><th>{t('site.phpValue')}</th><th>{t('site.phpSource')}</th><th></th></tr></thead>
            <tbody>
              {#each phpInfo.values as v, i}
                {@const o = overrides.find((x) => x.key === v.key)}
                <tr class="rise" style="--i:{i}">
                  <td data-label={t('site.phpKey')} class="font-mono text-xs [overflow-wrap:anywhere]"><span>{#each v.key.split(/(?<=[._])/) as part, j}{#if j}<wbr />{/if}{part}{/each}</span></td>
                  <td data-label={t('site.phpValue')} class="font-mono text-xs [overflow-wrap:anywhere]">{o ? o.value : v.value}{#if o && o.value !== v.value}<span class="text-muted"> {t('site.phpWas', { value: v.value })}</span>{/if}</td>
                  <td data-label={t('site.phpSource')}><span class="tag {o || v.source === 'site' ? 'tag-accent' : v.source === 'preset' ? 'tag-ok' : v.source === 'global' ? 'tag-warn' : 'tag-muted'}">{o || v.source === 'site' ? t('site.srcSite') : v.source === 'preset' ? t('site.srcPreset') : v.source === 'global' ? t('site.srcGlobal') : t('site.srcPanel')}</span></td>
                  <td data-label="" class="text-right"><button class="btn btn-ghost btn-sm" onclick={() => useDefault(v.key, o ? o.value : v.value)}>{t('site.phpEdit')}</button></td>
                </tr>
              {/each}
            </tbody>
          </table>
        {:else}<div class="p-4 text-sm text-muted">{t('common.loading')}</div>{/if}
      </div>
      <div class="lg:col-span-2 space-y-3">
        <div class="card rise" style="--i:1">
          <div class="font-medium text-sm mb-2">{t('site.phpOverrides')}</div>
          <div class="flex gap-2 mb-2">
            <input class="input font-mono text-xs" list="ini-keys" bind:value={newKey} placeholder="memory_limit" onkeydown={(e) => e.key === 'Enter' && (e.preventDefault(), addOverride())} />
            <input class="input font-mono text-xs w-32" bind:value={newValue} placeholder="512M" onkeydown={(e) => e.key === 'Enter' && (e.preventDefault(), addOverride())} />
            <button class="btn btn-sm" onclick={addOverride}><Icon name="plus" size={13} /></button>
          </div>
          <datalist id="ini-keys">{#each phpInfo?.allowed || [] as k}<option value={k}></option>{/each}</datalist>
          {#if overrides.length}
            <ul class="divide-y divide-line text-xs font-mono">
              {#each overrides as o, i (o.key)}
                <li class="flex items-center gap-2 py-1.5" transition:slide={{ duration: dur(150) }}><span class="flex-1 truncate">{o.key}</span><input class="input w-32 py-0.5 text-xs" bind:value={o.value} /><button class="btn btn-ghost btn-sm text-danger" onclick={() => (overrides = overrides.filter((_, j) => j !== i))} aria-label={t('site.phpRemove')}><Icon name="x" size={13} /></button></li>
              {/each}
            </ul>
          {:else}<p class="text-xs text-muted">{t('site.phpNoOverrides')}</p>{/if}
          <button class="btn btn-primary mt-3 w-full justify-center" onclick={savePHP}><Icon name="save" size={14} /> {t('site.phpSave')}</button>
        </div>
        <p class="text-xs text-muted px-1">{t('site.phpLayers')}{#if admin}{' '}<a href="/php" class="text-accent-ink hover:underline">PHP →</a>{/if}</p>
        <p class="text-xs text-muted px-1">{t('site.phpAllowed', { keys: phpInfo?.allowed?.join(', ') ?? '' })}</p>
      </div>
    </div>

  {:else if tab === 'nginx'}
    <div class="space-y-4">
      <div class="card rise">
        <div class="flex flex-wrap items-center justify-between gap-2 mb-2">
          <div><div class="font-medium text-sm">{t('site.nginxCustomIn')} <code class="font-mono">server {'{}'}</code></div><div class="text-xs text-muted font-mono">{nginx?.custom_path || ''}</div></div>
          <div class="flex gap-2">
            {#if admin && nginx?.custom?.trim()}<button class="btn btn-danger btn-sm" disabled={nginxBusy} onclick={() => (ask = askNginxClear())}><Icon name="trash" size={13} /> {t('site.nginxClear')}</button>{/if}
            {#if admin}<button class="btn btn-primary btn-sm" disabled={nginxBusy} onclick={() => saveNginx(custom)}><Icon name="check" size={13} /> {nginxBusy ? t('site.nginxChecking') : t('site.nginxCheckApply')}</button>{/if}
          </div>
        </div>
        <textarea class="input font-mono text-xs h-64 leading-5 resize-y" bind:value={custom} onkeydown={tabKey} spellcheck="false" readonly={!admin} placeholder={t('site.nginxPh')}></textarea>
        {#if nginxError}<div class="mt-2 text-xs text-danger code border-danger/40" transition:slide={{ duration: dur(150) }}>{nginxError}</div>{/if}
        <p class="text-xs text-muted mt-2">{t('site.nginxCheckedVia')} <code>nginx -t</code>{t('site.nginxRollback')} {#if !admin}{t('site.nginxAdminOnly')}{/if}{#if nginx?.include_dir}{' '}{t('site.nginxIncludeDir', { dir: nginx.include_dir })}{/if}{#if nginx?.others?.length}{' '}{t('site.nginxOthers', { list: nginx.others.join(', ') })}{/if}</p>
      </div>
      <div class="card rise" style="--i:1">
        <button class="text-sm font-medium inline-flex items-center gap-1.5" onclick={() => (showGenerated = !showGenerated)}><Icon name="chevron" size={14} class="transition-transform {showGenerated ? 'rotate-90' : ''}" /> {t('site.nginxGenerated')} <span class="text-xs text-muted font-mono font-normal">{nginx?.config_path || ''}</span></button>
        {#if showGenerated}<pre class="code mt-3 max-h-[32rem]" transition:slide={{ duration: dur(200) }}>{nginx?.generated || ''}</pre>{/if}
      </div>
    </div>

  {:else if tab === 'files'}
    <div class="rise">
      <FileManager user={saved.login} start={'/data/www/' + domain + (saved.docroot ? '/' + saved.docroot : '')} />
    </div>

  {:else if tab === 'logs'}
    <div class="card rise">
      <div class="flex flex-wrap items-center gap-1.5 mb-3 text-sm">
        {#each logTypes as kind}<button class="btn btn-sm {logType === kind ? 'btn-primary' : 'btn-ghost'}" onclick={() => { logType = kind; loadLog(); }}>{kind}</button>{/each}
        <div class="ml-auto flex items-center gap-1.5">
          <label class="text-xs text-muted" for="ll">{t('site.logLines')}</label>
          <select id="ll" class="input py-1 text-xs w-24" value={logLines} onchange={(e) => { logLines = Number((e.target as HTMLSelectElement).value); loadLog(); }}>{#each [50, 100, 200, 500, 1000, 2000, 5000] as n}<option value={n}>{n}</option>{/each}</select>
          <button class="btn btn-sm" onclick={loadLog} disabled={logBusy} title={t('common.refresh')}><Icon name="refresh" size={13} /></button>
        </div>
      </div>
      {#if logError}<p class="text-xs text-danger mb-2 break-words">{logError}</p>{/if}
      {#if log}<div class="text-xs text-muted font-mono mb-1">{log.path} · {bytes(log.size)}</div><pre class="code max-h-[28rem]">{log.lines.join('\n') || t('site.logEmpty')}</pre>{/if}
    </div>
  {/if}
{/if}

<Confirm bind:ask />

<Modal open={tlsOpen} title={t('site.tlsTitle', { domain })} onclose={() => (tlsOpen = false)}>
  <form id="site-tls" class="grid gap-3" onsubmit={issueTLS}>
    <p class="text-sm text-muted">{t('site.tlsCovers', { list: [domain, ...(saved?.aliases ?? [])].join(', ') })}</p>
    <div><label class="label" for="td">{t('site.tlsChallenge')}</label><select id="td" class="input" bind:value={tform.dns}><option value="">{t('site.tlsHttp01')}</option>{#each providers as p}<option value={p.name}>{t('site.tlsDns01', { name: p.name, type: p.type })}</option>{/each}</select></div>
    <div><label class="label" for="te">{t('site.tlsEmail')}</label><input id="te" class="input" type="email" bind:value={tform.email} placeholder={t('site.tlsEmailPh')} /></div>
    <label class="text-sm flex items-center gap-1"><input type="checkbox" bind:checked={tform.staging} /> {t('site.tlsStaging')}</label>
  </form>
  {#snippet footer()}<button class="btn" onclick={() => (tlsOpen = false)}>{t('common.cancel')}</button><button class="btn btn-primary" form="site-tls">{t('site.tlsIssue')}</button>{/snippet}
</Modal>

<Modal open={recordOpen} title={t('site.recordTitle', { domain })} onclose={() => (recordOpen = false)}>
  <p class="text-xs text-muted">{t('site.recordHint')}</p>
  {#if record}<pre class="code max-h-[60vh] select-all">{JSON.stringify(record, null, 2)}</pre>{:else}<p class="text-muted">{t('common.loading')}</p>{/if}
  {#snippet footer()}<button class="btn" onclick={() => (recordOpen = false)}>{t('common.close')}</button>{/snippet}
</Modal>
