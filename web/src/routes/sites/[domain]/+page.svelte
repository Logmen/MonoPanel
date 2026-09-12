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
  const domain = $derived(page.params.domain!);
  const admin = $derived(auth.me?.role === 'admin');
  let site = $state<any>(null);
  let php = $state<any[]>([]);
  let presets = $state<any[]>([]);
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
      notify(`${cmsForm.cms}: установка запущена`);
    } catch (e: any) { error = e.text || String(e); notify(error, 'err'); } finally { cmsBusy = false; }
  }
  const cmsName = (id: string) => cmsList.find((c) => c.id === id)?.name ?? id;
  const cmsAdminPath = (id: string) => ({ wordpress: 'wp-admin/', joomla: 'administrator/', opencart: 'admin/', bitrix: 'bitrix/admin/' } as Record<string, string>)[id] ?? '';
  async function issueTLS(e: Event) { e.preventDefault(); error = ''; try { const r: any = await api(`/sites/${domain}/tls/issue`, { method: 'POST', json: { staging: tform.staging, dns: tform.dns || undefined } }); job = r.job_id; tlsOpen = false; site.ssl = 'auto'; } catch (e: any) { error = e.text || String(e); notify(error, 'err'); } }
  onMount(() => { load().catch((e) => (error = e.text || String(e))); });
  $effect(() => { if (tab === 'php' && !phpInfo) loadPHP(); if (tab === 'nginx' && !nginx) loadNginx(); if (tab === 'logs' && !log) loadLog(); });

  function baseBody() {
    return {
      aliases: aliases.split(',').map((s) => s.trim()).filter(Boolean), mode: site.mode, backend: site.backend || undefined, php_version: site.php_version || undefined,
      docroot: site.docroot, ssl: site.ssl, http2: site.http2, http3: site.http3, redirect_https: site.redirect_https, redirect_www: site.redirect_www,
      static_by_nginx: site.static_by_nginx, fpm_pm: site.fpm_pm, fpm_max_children: site.fpm_max_children, allow_exec: site.allow_exec, client_max_body: site.client_max_body,
      allow_from: allow.split(/[\s,]+/).map((s) => s.trim()).filter(Boolean), preset: site.preset || ''
    };
  }
  async function save(e: Event) {
    e.preventDefault();
    error = '';
    try {
      const res: any = await api(`/sites/${domain}`, { method: 'PATCH', json: baseBody() });
      job = res.job_id;
      notify('Настройки сохранены, применяю');
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
      notify('PHP-параметры сохранены, пул перезапускается');
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
      notify(custom.trim() ? 'nginx проверен и перезагружен' : 'Свои директивы убраны, nginx перезагружен');
    } catch (e) { nginxError = e instanceof ApiError ? e.text : String(e); }
    finally { nginxBusy = false; }
  }
  function tabKey(e: KeyboardEvent) {
    if (e.key === 'Tab') { e.preventDefault(); const t = e.target as HTMLTextAreaElement; const s = t.selectionStart; custom = custom.slice(0, s) + '    ' + custom.slice(t.selectionEnd); queueMicrotask(() => t.setSelectionRange(s + 4, s + 4)); }
  }
  async function loadLog() { try { log = await api(`/sites/${domain}/logs/${logType}?lines=200`); } catch (e: any) { error = e.text || String(e); } }
  const tabs: [typeof tab, string, string][] = [['settings', 'Настройки', 'settings'], ['cms', 'CMS', 'plus'], ['php', 'PHP', 'code'], ['nginx', 'nginx', 'file'], ['files', 'Файлы', 'box'], ['logs', 'Логи', 'terminal']];
</script>

<PageHead title={domain} mono back="/sites" sub={site ? `владелец ${site.login} · ${site.ip} · ${site.mode === 'proxy' ? 'proxy → ' + site.backend : 'PHP ' + site.php_version + ' · ' + site.mode}` : ''}>
  {#if site}
    <a class="btn btn-sm" href="https://{domain}" target="_blank" rel="noopener"><Icon name="external" size={13} /> открыть</a>
    <span class="tag {site.status === 'active' ? 'tag-ok' : site.status === 'error' ? 'tag-err' : 'tag-warn'}">{site.status}</span>
    <span class="tag {site.certificate_id ? 'tag-ok' : 'tag-muted'}"><Icon name="shield" size={11} /> {site.certificate_id ? 'https' : 'http'}</span>
  {/if}
</PageHead>
{#if error}<p class="text-danger text-sm mb-3 flex items-center gap-1.5"><Icon name="alert" size={15} /> {error}</p>{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
{#if site}
  <div class="tabs rise">
    {#each tabs as [id, title, icon]}<button class="tab {tab === id ? 'tab-on' : ''} inline-flex items-center gap-1.5" onclick={() => (tab = id)}><Icon name={icon} size={14} /> {title}</button>{/each}
  </div>

  {#if tab === 'settings'}
    <form class="card grid md:grid-cols-3 gap-3 rise" onsubmit={save}>
      <div class="md:col-span-3 text-xs text-muted font-mono break-all">docroot /var/www/{site.login}/data/www/{domain}{site.docroot ? '/' + site.docroot : ''}</div>
      <div><label class="label" for="al">Алиасы</label><input id="al" class="input" bind:value={aliases} placeholder="www.example.com, shop.example.com" /></div>
      <div><label class="label" for="mode">Режим</label><select id="mode" class="input" bind:value={site.mode}><option value="fpm">nginx + php-fpm</option><option value="apache">nginx + Apache</option><option value="proxy">proxy → backend</option></select></div>
      {#if site.mode === 'proxy'}<div><label class="label" for="be">Backend</label><input id="be" class="input font-mono" bind:value={site.backend} /></div>{:else}
      <div><label class="label" for="php">PHP</label><select id="php" class="input" bind:value={site.php_version}>{#each php as v}<option value={v.version}>{v.version}</option>{/each}</select></div>{/if}
      {#if site.mode !== 'proxy'}<div><label class="label" for="pr">Пресет CMS</label><select id="pr" class="input" bind:value={site.preset}>{#each presets as p}<option value={p.id}>{p.name}</option>{/each}</select></div>{/if}
      <div><label class="label" for="dr">Подкаталог docroot</label><input id="dr" class="input" bind:value={site.docroot} placeholder="public" /></div>
      <div><label class="label" for="ssl">SSL</label><select id="ssl" class="input" bind:value={site.ssl}><option value="auto">auto (Let's Encrypt)</option><option value="none">none</option></select>
        <div class="text-xs mt-1 flex flex-wrap items-center gap-2">
          {#if cert}<span class="tag {cert.status === 'valid' ? 'tag-ok' : cert.status === 'error' ? 'tag-err' : 'tag-warn'}">{cert.status}</span><span class="text-muted">{cert.issuer || cert.name}{cert.not_after ? ` · до ${new Date(cert.not_after).toLocaleDateString()}` : ''}</span>{:else}<span class="text-muted">сертификата нет</span>{/if}
          <button type="button" class="btn btn-sm" onclick={() => { tform = { staging: false, dns: '' }; tlsOpen = true; }}><Icon name="plus" size={12} /> выпустить</button>
        </div>
        {#if cert?.last_error}<div class="text-xs text-danger mt-1 break-words">{cert.last_error}</div>{/if}
      </div>
      <div><label class="label" for="rw">www</label><select id="rw" class="input" bind:value={site.redirect_www}><option value="none">как есть</option><option value="to_root">www → без www</option><option value="to_www">без www → www</option></select></div>
      <div><label class="label" for="pm">php-fpm pm</label><select id="pm" class="input" bind:value={site.fpm_pm}><option value="ondemand">ondemand</option><option value="dynamic">dynamic</option><option value="static">static</option></select></div>
      <div><label class="label" for="mc">max_children</label><input id="mc" class="input" type="number" min="1" bind:value={site.fpm_max_children} /></div>
      <div><label class="label" for="mb">client_max_body_size</label><input id="mb" class="input font-mono" bind:value={site.client_max_body} /></div>
      <div class="md:col-span-3"><label class="label" for="allow">Доступ только с IP / CIDR (пусто — для всех; ACME-проверка остаётся открытой)</label><input id="allow" class="input font-mono" bind:value={allow} placeholder="203.0.113.0/24, 198.51.100.7" /></div>
      <div class="md:col-span-3 flex flex-wrap gap-x-5 gap-y-2 text-sm">
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.redirect_https} /> HTTP → HTTPS (+HSTS)</label>
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.http2} /> HTTP/2</label>
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.http3} /> HTTP/3</label>
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.static_by_nginx} /> статика через nginx (apache)</label>
        <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={site.allow_exec} /> разрешить exec/system</label>
      </div>
      <div class="md:col-span-3"><button class="btn btn-primary"><Icon name="save" size={14} /> Сохранить и применить</button></div>
    </form>

  {:else if tab === 'cms'}
    <div class="card rise">
      {#if site.cms}
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div>
            <div class="font-medium">{cmsName(site.cms)} {site.cms_version}</div>
            <p class="text-xs text-muted">установлен панелью{#if site.cms_at} {new Date(site.cms_at).toLocaleString('ru-RU')}{/if} · пресет {site.preset || '—'} · файлы и база принадлежат {site.login}</p>
          </div>
          <a class="btn btn-sm" href={(site.certificate_id ? 'https://' : 'http://') + domain + '/' + cmsAdminPath(site.cms)} target="_blank" rel="noopener">открыть админку</a>
        </div>
        <p class="text-sm text-muted mt-3">Поставить другую CMS сюда же можно с флагом «заменить файлы»: docroot будет очищен, прежняя база останется.</p>
      {/if}
      {#if cmsResult}
        <div class="p-3 rounded-lg border border-accent bg-accent-soft text-sm mt-3">
          <div class="font-medium text-accent-ink">{cmsName(cmsResult.cms)}: доступы администратора, показаны один раз</div>
          <div class="font-mono text-xs mt-1 break-all">{cmsResult.admin_url}<br />логин {cmsResult.admin_login}{#if cmsResult.admin_password} · пароль {cmsResult.admin_password}{/if} · {cmsResult.admin_email}<br />база {cmsResult.database}</div>
        </div>
      {/if}
      <form class="grid md:grid-cols-3 gap-3 mt-4 items-end" onsubmit={installCMS}>
        <div class="md:col-span-3 text-sm text-muted">Дистрибутив скачивается у производителя, распаковывается в docroot от имени клиента, база создаётся отдельно, установку делает штатный установщик CMS. Сайт получает соответствующий пресет.</div>
        <div><label class="label" for="cms">CMS</label><select id="cms" class="input" bind:value={cmsForm.cms}>{#each cmsList as c}<option value={c.id}>{c.name}</option>{/each}</select></div>
        <div><label class="label" for="ct">Название сайта</label><input id="ct" class="input" bind:value={cmsForm.title} placeholder={domain} /></div>
        <div><label class="label" for="cl">Логин администратора</label><input id="cl" class="input" bind:value={cmsForm.admin_login} placeholder="admin" /></div>
        <div><label class="label" for="cp">Пароль администратора</label><input id="cp" class="input" type="password" bind:value={cmsForm.admin_password} placeholder="12–20 символов, иначе сгенерируется" /></div>
        <div><label class="label" for="ce">E-mail администратора</label><input id="ce" class="input" bind:value={cmsForm.admin_email} placeholder="e-mail владельца" /></div>
        {#if cmsForm.cms === 'bitrix'}
          <div><label class="label" for="ced">Редакция (пробная)</label><select id="ced" class="input" bind:value={cmsForm.edition}><option value="start">Старт</option><option value="standard">Стандарт</option><option value="small_business">Малый бизнес</option><option value="business">Бизнес</option></select></div>
          <div><label class="label" for="csol">Решение</label><select id="csol" class="input" bind:value={cmsForm.solution}><option value="clean">Чистая установка (Маркетплейс)</option><option value="demo">Демо-сайт из дистрибутива</option><option value="custom">Решение из Маркетплейса по id</option></select></div>
          {#if cmsForm.solution === 'custom'}<div><label class="label" for="csid">Id решения</label><input id="csid" class="input font-mono" bind:value={cmsForm.solutionId} placeholder="vendor.solution" /></div>{/if}
        {/if}
        <label class="flex items-center gap-1.5 text-sm md:col-span-2"><input type="checkbox" bind:checked={cmsForm.force} /> заменить файлы в непустом docroot</label>
        <div class="md:col-span-3 text-xs text-muted">{cmsList.find((c) => c.id === cmsForm.cms)?.notes ?? ''}</div>
        <div class="md:col-span-3"><button class="btn btn-primary" disabled={cmsBusy}><Icon name="plus" size={14} /> {cmsBusy ? 'запускаю…' : 'Установить'}</button></div>
      </form>
    </div>

  {:else if tab === 'php'}
    <div class="grid lg:grid-cols-5 gap-4">
      <div class="card lg:col-span-3 p-0 overflow-hidden rise">
        <div class="px-4 py-3 border-b border-line flex items-center justify-between"><span class="font-medium text-sm">Действующие параметры</span><span class="text-xs text-muted font-mono">{phpInfo ? `PHP ${phpInfo.version} · ${phpInfo.pool_path}` : ''}</span></div>
        {#if phpInfo}
          <table class="tbl">
            <thead><tr><th>Параметр</th><th>Значение</th><th>Источник</th><th></th></tr></thead>
            <tbody>
              {#each phpInfo.values as v, i}
                {@const o = overrides.find((x) => x.key === v.key)}
                <tr class="rise" style="--i:{i}">
                  <td data-label="Параметр" class="font-mono text-xs">{v.key}</td>
                  <td data-label="Значение" class="font-mono text-xs">{o ? o.value : v.value}{#if o && o.value !== v.value}<span class="text-muted"> (было {v.value})</span>{/if}</td>
                  <td data-label="Источник"><span class="tag {o || v.source === 'site' ? 'tag-accent' : v.source === 'preset' ? 'tag-ok' : 'tag-muted'}">{o || v.source === 'site' ? 'сайт' : v.source === 'preset' ? 'пресет' : 'панель'}</span></td>
                  <td data-label="" class="text-right"><button class="btn btn-ghost btn-sm" onclick={() => useDefault(v.key, o ? o.value : v.value)}>изменить</button></td>
                </tr>
              {/each}
            </tbody>
          </table>
        {:else}<div class="p-4 text-sm text-muted">загрузка…</div>{/if}
      </div>
      <div class="lg:col-span-2 space-y-3">
        <div class="card rise" style="--i:1">
          <div class="font-medium text-sm mb-2">Переопределения сайта</div>
          <div class="flex gap-2 mb-2">
            <input class="input font-mono text-xs" list="ini-keys" bind:value={newKey} placeholder="memory_limit" onkeydown={(e) => e.key === 'Enter' && (e.preventDefault(), addOverride())} />
            <input class="input font-mono text-xs w-32" bind:value={newValue} placeholder="512M" onkeydown={(e) => e.key === 'Enter' && (e.preventDefault(), addOverride())} />
            <button class="btn btn-sm" onclick={addOverride}><Icon name="plus" size={13} /></button>
          </div>
          <datalist id="ini-keys">{#each phpInfo?.allowed || [] as k}<option value={k}></option>{/each}</datalist>
          {#if overrides.length}
            <ul class="divide-y divide-line text-xs font-mono">
              {#each overrides as o, i (o.key)}
                <li class="flex items-center gap-2 py-1.5" transition:slide={{ duration: dur(150) }}><span class="flex-1 truncate">{o.key}</span><input class="input w-32 py-0.5 text-xs" bind:value={o.value} /><button class="btn btn-ghost btn-sm text-danger" onclick={() => (overrides = overrides.filter((_, j) => j !== i))} aria-label="убрать"><Icon name="x" size={13} /></button></li>
              {/each}
            </ul>
          {:else}<p class="text-xs text-muted">Пока используются значения панели. Выберите параметр слева или введите ключ.</p>{/if}
          <button class="btn btn-primary mt-3 w-full justify-center" onclick={savePHP}><Icon name="save" size={14} /> Сохранить и перезапустить пул</button>
        </div>
        <p class="text-xs text-muted px-1">Допустимые ключи: {phpInfo?.allowed?.join(', ')}</p>
      </div>
    </div>

  {:else if tab === 'nginx'}
    <div class="space-y-4">
      <div class="card rise">
        <div class="flex flex-wrap items-center justify-between gap-2 mb-2">
          <div><div class="font-medium text-sm">Свои директивы внутри <code class="font-mono">server {'{}'}</code></div><div class="text-xs text-muted font-mono">{nginx?.custom_path || ''}</div></div>
          <div class="flex gap-2">
            {#if admin}<button class="btn btn-primary btn-sm" disabled={nginxBusy} onclick={saveNginx}><Icon name="check" size={13} /> {nginxBusy ? 'проверяю…' : 'Проверить и применить'}</button>{/if}
          </div>
        </div>
        <textarea class="input font-mono text-xs h-64 leading-5 resize-y" bind:value={custom} onkeydown={tabKey} spellcheck="false" readonly={!admin} placeholder={"# например:\nlocation /api/ {\n    proxy_pass http://127.0.0.1:3000;\n}\n"}></textarea>
        {#if nginxError}<div class="mt-2 text-xs text-danger code border-danger/40" transition:slide={{ duration: dur(150) }}>{nginxError}</div>{/if}
        <p class="text-xs text-muted mt-2">Файл проверяется через <code>nginx -t</code>; при ошибке возвращается прежняя версия. {#if !admin}Изменять может администратор.{/if}{#if nginx?.others?.length} Другие include в каталоге: {nginx.others.join(', ')}.{/if}</p>
      </div>
      <div class="card rise" style="--i:1">
        <button class="text-sm font-medium inline-flex items-center gap-1.5" onclick={() => (showGenerated = !showGenerated)}><Icon name="chevron" size={14} class="transition-transform {showGenerated ? 'rotate-90' : ''}" /> Сгенерированный server-блок <span class="text-xs text-muted font-mono font-normal">{nginx?.config_path || ''}</span></button>
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
        {#each ['access', 'error', 'php', 'slow', 'apache-access', 'apache-error'] as t}<button class="btn btn-sm {logType === t ? 'btn-primary' : 'btn-ghost'}" onclick={() => { logType = t; loadLog(); }}>{t}</button>{/each}
        <button class="btn btn-sm ml-auto" onclick={loadLog}><Icon name="refresh" size={13} /></button>
      </div>
      {#if log}<div class="text-xs text-muted font-mono mb-1">{log.path} · {bytes(log.size)}</div><pre class="code max-h-[28rem]">{log.lines.join('\n') || '(пусто)'}</pre>{/if}
    </div>
  {/if}
{/if}

<Modal open={tlsOpen} title="Сертификат для {domain}" onclose={() => (tlsOpen = false)}>
  <form id="site-tls" class="grid gap-3" onsubmit={issueTLS}>
    <p class="text-sm text-muted">Будут покрыты {[domain, ...aliases.split(',').map((s) => s.trim()).filter(Boolean)].join(', ')}. Сайт переключится на HTTPS, как только сертификат выпустится.</p>
    <div><label class="label" for="td">Проверка</label><select id="td" class="input" bind:value={tform.dns}><option value="">HTTP-01: домен должен вести на этот сервер, порт 80 открыт</option>{#each providers as p}<option value={p.name}>DNS-01: {p.name} ({p.type}) — работает и до переключения DNS</option>{/each}</select></div>
    <label class="text-sm flex items-center gap-1"><input type="checkbox" bind:checked={tform.staging} /> staging (тестовый, недоверенный — для проверки настройки)</label>
  </form>
  {#snippet footer()}<button class="btn" onclick={() => (tlsOpen = false)}>Отмена</button><button class="btn btn-primary" form="site-tls">Выпустить</button>{/snippet}
</Modal>
