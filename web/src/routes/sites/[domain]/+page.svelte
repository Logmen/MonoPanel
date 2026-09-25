<script lang="ts">
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { slide } from 'svelte/transition';
  import { api, ApiError, bytes } from '$lib/api';
  import { auth, notify, dur } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import FileManager from '$lib/components/FileManager.svelte';
  import { t, dateLocale, type MsgKey } from '$lib/i18n/index.svelte';
  const domain = $derived(page.params.domain!);
  const admin = $derived(auth.me?.role === 'admin');
  let site = $state<any>(null);
  let php = $state<any[]>([]);
  let presets = $state<any[]>([]);
  // Имена пресетов приходят с сервера по-русски; два не латинских подменяем по id.
  const presetName = (p: any) => (p.id === '' ? t('sites.presetUniversal') : p.id === 'bitrix' ? t('sites.presetBitrix') : p.name);
  // Каталог CMS приходит с сервера по-английски: пояснения к известным — из словаря.
  const cmsNotes: Record<string, MsgKey> = { wordpress: 'site.cmsNoteWordpress', joomla: 'site.cmsNoteJoomla', opencart: 'site.cmsNoteOpencart', bitrix: 'site.cmsNoteBitrix' };
  const cmsLabel = (c: any) => (c.id === 'bitrix' ? t('sites.presetBitrix') : c.name);
  const cmsNote = (c: any) => (c.id in cmsNotes ? t(cmsNotes[c.id]) : (c.notes ?? ''));
  let tab = $state<'settings' | 'php' | 'nginx' | 'files' | 'logs' | 'cms'>('settings');
  let logType = $state('access');
  let log = $state<any>(null);
  let job = $state<number | null>(null);
  // Certificate of this site and the order dialog (HTTP-01 or DNS-01).
  let certs = $state<any[]>([]);
  let providers = $state<any[]>([]);
  let tlsOpen = $state(false);
  /* CMS tab: the catalogue, the install form, the credentials shown once */
  let cmsList = $state<any[]>([]);
  let cmsForm = $state({ cms: 'wordpress', title: '', admin_login: '', admin_password: '', admin_email: '', edition: 'start', solution: 'clean', solutionId: '', force: false });
  let cmsResult = $state<any>(null);
  let cmsBusy = $state(false);
  let tform = $state({ staging: false, dns: '' });
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

  async function load() {
    site = await api(`/sites/${domain}`);
    aliases = (site.aliases || []).join(', ');
    allow = (site.allow_from || []).join(', ');
    overrides = Object.entries(site.php_ini || {}).map(([key, value]) => ({ key, value: String(value) }));
    php = ((await api('/php/versions')) as any).installed.filter((v: any) => v.status === 'installed');
    presets = await api('/sites/presets');
    cmsList = await api('/cms');
    const [c, d] = await Promise.all([api('/certificates'), api('/dns-providers')]);
    certs = c as any[];
    providers = d as any[];
  }
  const cert = $derived(site ? (certs.find((c) => c.id === site.certificate_id) ?? certs.find((c) => c.name === site.domain) ?? null) : null);
  // Installing a CMS is a job; the administrator password comes back once
  // in the response and never appears in the job log.
  async function installCMS(e: Event) {
    e.preventDefault(); error = ''; cmsBusy = true;
    try {
      const body: any = { cms: cmsForm.cms, force: cmsForm.force };
      for (const k of ['title', 'admin_login', 'admin_password', 'admin_email'] as const) if (cmsForm[k]) body[k] = cmsForm[k];
      if (cmsForm.cms === 'bitrix') { body.edition = cmsForm.edition; body.solution = cmsForm.solution === 'custom' ? cmsForm.solutionId : cmsForm.solution; }
      cmsResult = await api(`/sites/${domain}/cms`, { method: 'POST', json: body });
      job = cmsResult.job_id;
      notify(t('site.cmsInstallStarted', { cms: cmsForm.cms }));
    } catch (e: any) { error = e.text || String(e); notify(error, 'err'); } finally { cmsBusy = false; }
  }
  const cmsName = (id: string) => {
    const c = cmsList.find((c) => c.id === id);
    return c ? cmsLabel(c) : id;
  };
  const cmsAdminPath = (id: string) => ({ wordpress: 'wp-admin/', joomla: 'administrator/', opencart: 'admin/', bitrix: 'bitrix/admin/' } as Record<string, string>)[id] ?? '';
  async function issueTLS(e: Event) { e.preventDefault(); error = ''; try { const r: any = await api(`/sites/${domain}/tls/issue`, { method: 'POST', json: { staging: tform.staging, dns: tform.dns || undefined } }); job = r.job_id; tlsOpen = false; site.ssl = 'auto'; } catch (e: any) { error = e.text || String(e); notify(error, 'err'); } }
  onMount(() => { load().catch((e) => (error = e.text || String(e))); });
  $effect(() => { if (tab === 'php' && !phpInfo) loadPHP(); if (tab === 'nginx' && !nginx) loadNginx(); if (tab === 'logs' && !log) loadLog(); });

  function baseBody() {
    return {
      aliases: aliases.split(',').map((s) => s.trim()).filter(Boolean), mode: site.mode, backend: site.backend || undefined, php_version: site.php_version || undefined,
      docroot: site.docroot, ssl: site.ssl, http2: site.http2, http3: site.http3, redirect_https: site.redirect_https, redirect_www: site.redirect_www,
      static_by_nginx: site.static_by_nginx, fpm_pm: site.fpm_pm, fpm_max_children: site.fpm_max_children, allow_exec: site.allow_exec, client_max_body: site.client_max_body,
      allow_from: allow.split(/[\s,]+/).map((s) => s.trim()).filter(Boolean), preset: site.preset || '',
      session_store: site.mode === 'proxy' ? undefined : site.session_store || 'files'
    };
  }
  async function save(e: Event) {
    e.preventDefault();
    error = '';
    try {
      const res: any = await api(`/sites/${domain}`, { method: 'PATCH', json: baseBody() });
      job = res.job_id;
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
    for (const k of Object.keys(site.php_ini || {})) if (!(k in php_ini)) php_ini[k] = '';
    try {
      const res: any = await api(`/sites/${domain}`, { method: 'PATCH', json: { php_ini } });
      job = res.job_id;
      phpInfo = null;
      notify(t('site.phpSaved'));
      await load();
      await loadPHP();
    } catch (e) { error = e instanceof ApiError ? e.text : String(e); }
  }
  async function loadNginx() { try { nginx = await api(`/sites/${domain}/nginx`); custom = nginx.custom; } catch (e: any) { error = e.text || String(e); } }
  async function saveNginx() {
    nginxBusy = true; nginxError = '';
    try {
      nginx = await api(`/sites/${domain}/nginx`, { method: 'PUT', json: { custom } });
      custom = nginx.custom;
      notify(custom.trim() ? t('site.nginxReloaded') : t('site.nginxCustomRemoved'));
    } catch (e) { nginxError = e instanceof ApiError ? e.text : String(e); }
    finally { nginxBusy = false; }
  }
  function tabKey(e: KeyboardEvent) {
    if (e.key === 'Tab') { e.preventDefault(); const ta = e.target as HTMLTextAreaElement; const s = ta.selectionStart; custom = custom.slice(0, s) + '    ' + custom.slice(ta.selectionEnd); queueMicrotask(() => ta.setSelectionRange(s + 4, s + 4)); }
  }
  async function loadLog() { try { log = await api(`/sites/${domain}/logs/${logType}?lines=200`); } catch (e: any) { error = e.text || String(e); } }
  const tabs: [typeof tab, MsgKey, string][] = [['settings', 'site.tabSettings', 'settings'], ['cms', 'site.tabCms', 'plus'], ['php', 'site.tabPhp', 'code'], ['nginx', 'site.tabNginx', 'file'], ['files', 'site.tabFiles', 'box'], ['logs', 'site.tabLogs', 'terminal']];
</script>

<PageHead title={domain} mono back="/sites" sub={site ? t('site.sub', { login: site.login, ip: site.ip, stack: site.mode === 'proxy' ? 'proxy → ' + site.backend : 'PHP ' + site.php_version + ' · ' + site.mode }) : ''}>
  {#if site}
    <a class="btn btn-sm" href="https://{domain}" target="_blank" rel="noopener"><Icon name="external" size={13} /> {t('site.open')}</a>
    <span class="tag {site.status === 'active' ? 'tag-ok' : site.status === 'error' ? 'tag-err' : 'tag-warn'}">{site.status}</span>
    <span class="tag {site.certificate_id ? 'tag-ok' : 'tag-muted'}"><Icon name="shield" size={11} /> {site.certificate_id ? 'https' : 'http'}</span>
  {/if}
</PageHead>
{#if error}<p class="text-danger text-sm mb-3 flex items-center gap-1.5"><Icon name="alert" size={15} /> {error}</p>{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
{#if site}
  <div class="tabs rise">
    {#each tabs as [id, title, icon]}<button class="tab {tab === id ? 'tab-on' : ''} inline-flex items-center gap-1.5" onclick={() => (tab = id)}><Icon name={icon} size={14} /> {t(title)}</button>{/each}
  </div>

  {#if tab === 'settings'}
    <form class="card grid md:grid-cols-3 gap-3 rise" onsubmit={save}>
      <div class="md:col-span-3 text-xs text-muted font-mono break-all">docroot /var/www/{site.login}/data/www/{domain}{site.docroot ? '/' + site.docroot : ''}</div>
      <div><label class="label" for="al">{t('site.aliases')}</label><input id="al" class="input" bind:value={aliases} placeholder="www.example.com, shop.example.com" /></div>
      <div><label class="label" for="mode">{t('site.mode')}</label><select id="mode" class="input" bind:value={site.mode}><option value="fpm">nginx + php-fpm</option><option value="apache">nginx + Apache</option><option value="proxy">proxy → backend</option></select></div>
      {#if site.mode === 'proxy'}<div><label class="label" for="be">Backend</label><input id="be" class="input font-mono" bind:value={site.backend} /></div>{:else}
      <div><label class="label" for="php">PHP</label><select id="php" class="input" bind:value={site.php_version}>{#each php as v}<option value={v.version}>{v.version}</option>{/each}</select></div>{/if}
      {#if site.mode !== 'proxy'}<div><label class="label" for="pr">{t('site.preset')}</label><select id="pr" class="input" bind:value={site.preset}>{#each presets as p}<option value={p.id}>{presetName(p)}</option>{/each}</select></div>
      <div><label class="label" for="ss">{t('site.sessions')}</label><select id="ss" class="input" bind:value={site.session_store}><option value="">{t('site.sessionsFiles')}</option><option value="valkey">{t('site.sessionsValkey')}</option></select>
        {#if site.session_store === 'valkey'}<div class="text-xs text-muted mt-1">{t('site.sessionsHint', { login: site.login, version: site.php_version })}</div>{/if}</div>{/if}
      <div><label class="label" for="dr">{t('site.docrootSub')}</label><input id="dr" class="input" bind:value={site.docroot} placeholder="public" /></div>
      <div><label class="label" for="ssl">SSL</label><select id="ssl" class="input" bind:value={site.ssl}><option value="auto">auto (Let's Encrypt)</option><option value="none">none</option></select>
        <div class="text-xs mt-1 flex flex-wrap items-center gap-2">
          {#if cert}<span class="tag {cert.status === 'valid' ? 'tag-ok' : cert.status === 'error' ? 'tag-err' : 'tag-warn'}">{cert.status}</span><span class="text-muted">{cert.issuer || cert.name}{cert.not_after ? ' · ' + t('site.certUntil', { date: new Date(cert.not_after).toLocaleDateString(dateLocale()) }) : ''}</span>{:else}<span class="text-muted">{t('site.noCert')}</span>{/if}
          <button type="button" class="btn btn-sm" onclick={() => { tform = { staging: false, dns: '' }; tlsOpen = true; }}><Icon name="plus" size={12} /> {t('site.issue')}</button>
        </div>
        {#if cert?.last_error}<div class="text-xs text-danger mt-1 break-words">{cert.last_error}</div>{/if}
      </div>
      <div><label class="label" for="rw">www</label><select id="rw" class="input" bind:value={site.redirect_www}><option value="none">{t('site.wwwAsIs')}</option><option value="to_root">{t('site.wwwToRoot')}</option><option value="to_www">{t('site.wwwToWww')}</option></select></div>
      <div><label class="label" for="pm">php-fpm pm</label><select id="pm" class="input" bind:value={site.fpm_pm}><option value="ondemand">ondemand</option><option value="dynamic">dynamic</option><option value="static">static</option></select></div>
      <div><label class="label" for="mc">max_children</label><input id="mc" class="input" type="number" min="1" bind:value={site.fpm_max_children} /></div>
      <div><label class="label" for="mb">client_max_body_size</label><input id="mb" class="input font-mono" bind:value={site.client_max_body} /></div>
      <div class="md:col-span-3"><label class="label" for="allow">{t('site.allowFrom')}</label><input id="allow" class="input font-mono" bind:value={allow} placeholder="203.0.113.0/24, 198.51.100.7" /></div>
      <div class="md:col-span-3 flex flex-wrap gap-x-5 gap-y-2 text-sm">
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.redirect_https} /> HTTP → HTTPS (+HSTS)</label>
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.http2} /> HTTP/2</label>
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.http3} /> HTTP/3</label>
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.static_by_nginx} /> {t('site.staticByNginx')}</label>
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.allow_exec} /> {t('site.allowExec')}</label>
      </div>
      <div class="md:col-span-3"><button class="btn btn-primary"><Icon name="save" size={14} /> {t('site.saveApply')}</button></div>
    </form>

  {:else if tab === 'cms'}
    <div class="card rise">
      {#if site.cms}
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div>
            <div class="font-medium">{cmsName(site.cms)} {site.cms_version}</div>
            <p class="text-xs text-muted">{site.cms_at ? t('site.cmsInstalledAt', { date: new Date(site.cms_at).toLocaleString(dateLocale()) }) : t('site.cmsInstalled')} · {t('site.cmsPreset', { preset: site.preset || '—' })} · {t('site.cmsOwner', { login: site.login })}</p>
          </div>
          <a class="btn btn-sm" href={(site.certificate_id ? 'https://' : 'http://') + domain + '/' + cmsAdminPath(site.cms)} target="_blank" rel="noopener">{t('site.openAdmin')}</a>
        </div>
        <p class="text-sm text-muted mt-3">{t('site.cmsReplaceHint')}</p>
      {/if}
      {#if cmsResult}
        <div class="p-3 rounded-lg border border-accent bg-accent-soft text-sm mt-3">
          <div class="font-medium text-accent-ink">{t('site.cmsCreds', { cms: cmsName(cmsResult.cms) })}</div>
          <div class="font-mono text-xs mt-1 break-all">{cmsResult.admin_url}<br />{t('site.cmsLogin', { login: cmsResult.admin_login })}{#if cmsResult.admin_password} · {t('site.cmsPassword', { password: cmsResult.admin_password })}{/if} · {cmsResult.admin_email}<br />{t('site.cmsDatabase', { db: cmsResult.database })}</div>
        </div>
      {/if}
      <form class="grid md:grid-cols-3 gap-3 mt-4 items-end" onsubmit={installCMS}>
        <div class="md:col-span-3 text-sm text-muted">{t('site.cmsIntro')}</div>
        <div><label class="label" for="cms">CMS</label><select id="cms" class="input" bind:value={cmsForm.cms}>{#each cmsList as c}<option value={c.id}>{cmsLabel(c)}</option>{/each}</select></div>
        <div><label class="label" for="ct">{t('site.cmsTitle')}</label><input id="ct" class="input" bind:value={cmsForm.title} placeholder={domain} /></div>
        <div><label class="label" for="cl">{t('site.cmsAdminLogin')}</label><input id="cl" class="input" bind:value={cmsForm.admin_login} placeholder="admin" /></div>
        <div><label class="label" for="cp">{t('site.cmsAdminPassword')}</label><input id="cp" class="input" type="password" bind:value={cmsForm.admin_password} placeholder={t('site.cmsPasswordPh')} /></div>
        <div><label class="label" for="ce">{t('site.cmsAdminEmail')}</label><input id="ce" class="input" bind:value={cmsForm.admin_email} placeholder={t('site.cmsEmailPh')} /></div>
        {#if cmsForm.cms === 'bitrix'}
          <div><label class="label" for="ced">{t('site.bitrixEdition')}</label><select id="ced" class="input" bind:value={cmsForm.edition}><option value="start">{t('site.bitrixStart')}</option><option value="standard">{t('site.bitrixStandard')}</option><option value="small_business">{t('site.bitrixSmallBusiness')}</option><option value="business">{t('site.bitrixBusiness')}</option></select></div>
          <div><label class="label" for="csol">{t('site.bitrixSolution')}</label><select id="csol" class="input" bind:value={cmsForm.solution}><option value="clean">{t('site.bitrixClean')}</option><option value="demo">{t('site.bitrixDemo')}</option><option value="custom">{t('site.bitrixCustom')}</option></select></div>
          {#if cmsForm.solution === 'custom'}<div><label class="label" for="csid">{t('site.bitrixSolutionId')}</label><input id="csid" class="input font-mono" bind:value={cmsForm.solutionId} placeholder="vendor.solution" /></div>{/if}
        {/if}
        <label class="flex items-center gap-1.5 text-sm md:col-span-2"><input type="checkbox" bind:checked={cmsForm.force} /> {t('site.cmsForce')}</label>
        <div class="md:col-span-3 text-xs text-muted">{cmsNote(cmsList.find((c) => c.id === cmsForm.cms) ?? {})}</div>
        <div class="md:col-span-3"><button class="btn btn-primary" disabled={cmsBusy}><Icon name="plus" size={14} /> {cmsBusy ? t('site.cmsStarting') : t('site.cmsInstall')}</button></div>
      </form>
    </div>

  {:else if tab === 'php'}
    <div class="grid lg:grid-cols-5 gap-4">
      <div class="card lg:col-span-3 p-0 overflow-hidden rise">
        <div class="px-4 py-3 border-b border-line flex items-center justify-between"><span class="font-medium text-sm">{t('site.phpEffective')}</span><span class="text-xs text-muted font-mono">{phpInfo ? `PHP ${phpInfo.version} · ${phpInfo.pool_path}` : ''}</span></div>
        {#if phpInfo}
          <table class="tbl">
            <thead><tr><th>{t('site.phpKey')}</th><th>{t('site.phpValue')}</th><th>{t('site.phpSource')}</th><th></th></tr></thead>
            <tbody>
              {#each phpInfo.values as v, i}
                {@const o = overrides.find((x) => x.key === v.key)}
                <tr class="rise" style="--i:{i}">
                  <td data-label={t('site.phpKey')} class="font-mono text-xs">{v.key}</td>
                  <td data-label={t('site.phpValue')} class="font-mono text-xs">{o ? o.value : v.value}{#if o && o.value !== v.value}<span class="text-muted"> {t('site.phpWas', { value: v.value })}</span>{/if}</td>
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
            {#if admin}<button class="btn btn-primary btn-sm" disabled={nginxBusy} onclick={saveNginx}><Icon name="check" size={13} /> {nginxBusy ? t('site.nginxChecking') : t('site.nginxCheckApply')}</button>{/if}
          </div>
        </div>
        <textarea class="input font-mono text-xs h-64 leading-5 resize-y" bind:value={custom} onkeydown={tabKey} spellcheck="false" readonly={!admin} placeholder={t('site.nginxPh')}></textarea>
        {#if nginxError}<div class="mt-2 text-xs text-danger code border-danger/40" transition:slide={{ duration: dur(150) }}>{nginxError}</div>{/if}
        <p class="text-xs text-muted mt-2">{t('site.nginxCheckedVia')} <code>nginx -t</code>{t('site.nginxRollback')} {#if !admin}{t('site.nginxAdminOnly')}{/if}{#if nginx?.others?.length} {t('site.nginxOthers', { list: nginx.others.join(', ') })}{/if}</p>
      </div>
      <div class="card rise" style="--i:1">
        <button class="text-sm font-medium inline-flex items-center gap-1.5" onclick={() => (showGenerated = !showGenerated)}><Icon name="chevron" size={14} class="transition-transform {showGenerated ? 'rotate-90' : ''}" /> {t('site.nginxGenerated')} <span class="text-xs text-muted font-mono font-normal">{nginx?.config_path || ''}</span></button>
        {#if showGenerated}<pre class="code mt-3 max-h-[32rem]" transition:slide={{ duration: dur(200) }}>{nginx?.generated || ''}</pre>{/if}
      </div>
    </div>

  {:else if tab === 'files'}
    <div class="rise">
      <FileManager user={site.login} start={'/data/www/' + domain + (site.docroot ? '/' + site.docroot : '')} />
    </div>

  {:else if tab === 'logs'}
    <div class="card rise">
      <div class="flex flex-wrap items-center gap-1.5 mb-3 text-sm">
        {#each ['access', 'error', 'php', 'slow', 'apache-access', 'apache-error'] as kind}<button class="btn btn-sm {logType === kind ? 'btn-primary' : 'btn-ghost'}" onclick={() => { logType = kind; loadLog(); }}>{kind}</button>{/each}
        <button class="btn btn-sm ml-auto" onclick={loadLog}><Icon name="refresh" size={13} /></button>
      </div>
      {#if log}<div class="text-xs text-muted font-mono mb-1">{log.path} · {bytes(log.size)}</div><pre class="code max-h-[28rem]">{log.lines.join('\n') || t('site.logEmpty')}</pre>{/if}
    </div>
  {/if}
{/if}

<Modal open={tlsOpen} title={t('site.tlsTitle', { domain })} onclose={() => (tlsOpen = false)}>
  <form id="site-tls" class="grid gap-3" onsubmit={issueTLS}>
    <p class="text-sm text-muted">{t('site.tlsCovers', { list: [domain, ...aliases.split(',').map((s) => s.trim()).filter(Boolean)].join(', ') })}</p>
    <div><label class="label" for="td">{t('site.tlsChallenge')}</label><select id="td" class="input" bind:value={tform.dns}><option value="">{t('site.tlsHttp01')}</option>{#each providers as p}<option value={p.name}>{t('site.tlsDns01', { name: p.name, type: p.type })}</option>{/each}</select></div>
    <label class="text-sm flex items-center gap-1"><input type="checkbox" bind:checked={tform.staging} /> {t('site.tlsStaging')}</label>
  </form>
  {#snippet footer()}<button class="btn" onclick={() => (tlsOpen = false)}>{t('common.cancel')}</button><button class="btn btn-primary" form="site-tls">{t('site.tlsIssue')}</button>{/snippet}
</Modal>
