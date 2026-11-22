<script lang="ts">
  import { onMount } from 'svelte';
  import { slide } from 'svelte/transition';
  import { page } from '$app/state';
  import { api, ApiError } from '$lib/api';
  import { auth, notify, dur } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import { t, tn, type MsgKey } from '$lib/i18n/index.svelte';
  let sites = $state<any[] | null>(null);
  let users = $state<any[]>([]);
  let php = $state<any[]>([]);
  let error = $state('');
  let job = $state<number | null>(null);
  let showForm = $state(false);
  let showMore = $state(false);
  let del = $state<any>(null);
  let ask = $state<Ask | null>(null);
  let purge = $state(false);
  let confirmDomain = $state('');
  let presets = $state<any[]>([]);
  // Имена и описания пресетов приходят с сервера по-английски: известные
  // подменяем по id словарём, незнакомые показываем как есть.
  const presetName = (p: any) => (p.id === '' ? t('sites.presetUniversal') : p.id === 'bitrix' ? t('sites.presetBitrix') : p.name);
  const presetDescs: Record<string, MsgKey> = { '': 'sites.presetDescUniversal', wordpress: 'sites.presetDescWordpress', joomla: 'sites.presetDescJoomla', bitrix: 'sites.presetDescBitrix', opencart: 'sites.presetDescOpencart' };
  const presetDesc = (p: any) => (p.id in presetDescs ? t(presetDescs[p.id]) : p.description);
  // The first row is what most sites need; "more settings" holds every other
  // field of POST /sites. Empty fields are not sent, so the server's defaults
  // apply — and the preset's own (1C-Bitrix sizes the pool by RAM, 256m bodies).
  let form = $state({
    domain: '', user: '', www: true, mode: 'fpm', php_version: '', ssl: 'auto', backend: '', preset: '',
    aliases: '', docroot: '', ip: '', ipv6Mode: 'auto', ipv6: '', redirect_www: 'none', fpm_pm: '', fpm_max_children: '', client_max_body: '',
    rate_limit: 0, allow_from: '', php_ini: '', http2: true, http3: false, redirect_https: true, static_by_nginx: true, allow_exec: false, security_headers: false
  });
  const presetInfo = $derived(presets.find((p) => p.id === form.preset));
  const admin = $derived(auth.me?.role === 'admin');
  const proxy = $derived(form.mode === 'proxy');
  /* Moving every site of an address to another one (admin): after the host's
     address changed nginx cannot bind the old one, and one site at a time does
     not pass the configuration check while the others still name it. */
  let showMove = $state(false);
  let move = $state({ from: '', to: '' });
  let moved = $state<{ to: string; sites: string[] } | null>(null);
  const siteIPs = $derived([...new Set((sites ?? []).map((s) => s.ip).filter(Boolean))].sort() as string[]);
  const onFrom = $derived(move.from.trim() ? (sites ?? []).filter((s) => s.ip === move.from.trim()).map((s) => s.domain) : []);
  async function load() {
    try {
      sites = await api('/sites');
      if (admin) users = ((await api('/users')) as any[]).filter((u) => u.role === 'user' && u.unix_uid);
      php = ((await api('/php/versions')) as any).installed.filter((v: any) => v.status === 'installed');
      presets = await api('/sites/presets');
    } catch (e: any) { error = e.text || String(e); }
  }
  // /sites?move=<address> opens the move form, for links from diagnostics.
  onMount(() => { load(); if (admin && page.url.searchParams.has('move')) { showMove = true; move.to = page.url.searchParams.get('move') ?? ''; } });
  const words = (s: string) => s.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean);
  // php_ini is typed as key=value lines (mp site add --ini key=value); comments are skipped.
  function iniLines(text: string): Record<string, string> {
    const out: Record<string, string> = {};
    for (const raw of text.split('\n')) {
      const l = raw.trim();
      if (!l || l.startsWith(';') || l.startsWith('#')) continue;
      const i = l.indexOf('=');
      if (i < 1) throw new Error(t('sites.phpIniBad', { line: l }));
      out[l.slice(0, i).trim()] = l.slice(i + 1).trim();
    }
    return out;
  }
  function createBody() {
    const f = form;
    const body: any = { domain: f.domain.trim(), www: f.www, mode: f.mode, ssl: f.ssl, http2: f.http2, http3: f.http3, redirect_https: f.redirect_https, security_headers: f.security_headers };
    if (admin) body.user = f.user;
    if (proxy) body.backend = f.backend.trim();
    else {
      if (f.php_version) body.php_version = f.php_version;
      if (f.preset) body.preset = f.preset;
      if (f.fpm_pm) body.fpm_pm = f.fpm_pm;
      if (Number(f.fpm_max_children) > 0) body.fpm_max_children = Number(f.fpm_max_children);
      body.allow_exec = f.allow_exec;
      const ini = iniLines(f.php_ini);
      if (Object.keys(ini).length) body.php_ini = ini;
    }
    if (f.mode === 'apache') body.static_by_nginx = f.static_by_nginx;
    const aliases = words(f.aliases);
    if (aliases.length) body.aliases = aliases;
    // As in the site settings: "/public/" is the subdirectory public.
    const docroot = f.docroot.trim().replace(/^\/+|\/+$/g, '');
    if (docroot) body.docroot = docroot;
    if (f.ip.trim()) body.ip = f.ip.trim();
    if (f.ipv6Mode === 'none') body.ipv6 = 'none';
    else if (f.ipv6Mode === 'addr') {
      // Collapsed "more settings" keep their values but not the browser's check of
      // them: an address picked and left empty must not quietly become "auto".
      if (!f.ipv6.trim()) { showMore = true; throw new Error(t('sites.ipv6Missing')); }
      body.ipv6 = f.ipv6.trim();
    }
    if (f.redirect_www !== 'none') body.redirect_www = f.redirect_www;
    if (f.client_max_body.trim()) body.client_max_body = f.client_max_body.trim();
    if (Number(f.rate_limit) > 0) body.rate_limit = Number(f.rate_limit);
    const allow = words(f.allow_from);
    if (allow.length) body.allow_from = allow;
    return body;
  }
  async function create(e: Event) {
    e.preventDefault();
    error = '';
    try {
      const res: any = await api('/sites', { method: 'POST', json: createBody() });
      job = res.job_id;
      form.domain = '';
      form.aliases = '';
      showForm = false;
      await load();
    } catch (e) { error = e instanceof ApiError ? e.text : e instanceof Error ? e.message : String(e); }
  }
  const askApply = (s: any): Ask => ({
    title: t('sites.askApplyTitle', { domain: s.domain }),
    note: t('sites.askApplyNote', { domain: s.domain }),
    action: t('common.apply'),
    run: () => action(s.domain, 'apply')
  });
  const askSuspend = (s: any): Ask => s.status === 'suspended'
    ? { title: t('sites.askEnableTitle', { domain: s.domain }), action: t('sites.enable'),
        note: t('sites.askEnableNote'),
        run: () => action(s.domain, 'unsuspend') }
    : { title: t('sites.askSuspendTitle', { domain: s.domain }), danger: true, action: t('sites.suspend'),
        note: t('sites.askSuspendNote'),
        run: () => action(s.domain, 'suspend') };
  // The toast names the action in the person's language; an action without
  // a key (none today) falls back to the raw verb rather than a missing key.
  const actionToast: Record<string, MsgKey> = { delete: 'sites.deleting', apply: 'sites.toastApply', suspend: 'sites.toastSuspend', unsuspend: 'sites.toastUnsuspend' };
  async function action(domain: string, act: string) {
    error = '';
    try {
      const res: any = act === 'delete' ? await api(`/sites/${domain}?purge=${purge}`, { method: 'DELETE' }) : await api(`/sites/${domain}/${act}`, { method: 'POST' });
      job = res.job_id;
      del = null;
      notify(actionToast[act] ? t(actionToast[act], { domain }) : `${domain}: ${act}`);
    } catch (e) { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); }
  }
  // Which sites move is the server's answer; the dialog says what the form asks for.
  function askMove(e: Event) {
    e.preventDefault();
    const from = move.from.trim(), to = move.to.trim();
    ask = {
      title: t('sites.askMoveTitle', { to }),
      note: (from ? t('sites.askMoveNoteFrom', { from, to }) : t('sites.askMoveNoteGone', { to })) + ' ' + t('sites.askMoveNote', { to }),
      action: t('sites.moveBtn'),
      run: async () => {
        error = '';
        try {
          const res: any = await api('/sites/move-ip', { method: 'POST', json: from ? { from, to } : { to } });
          job = res.job_id;
          moved = { to, sites: res.sites ?? [] };
          notify(t('sites.moving', { to, list: moved.sites.join(', ') }));
          showMove = false;
        } catch (e) { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); }
      }
    };
  }
</script>

<PageHead title={t('sites.title')} sub={t('sites.sub')}>
  {#if admin}<button class="btn {showMove ? 'btn-primary' : ''}" onclick={() => (showMove = !showMove)}><Icon name="move" size={15} /> {t('sites.moveIp')}</button>{/if}
  <button class="btn btn-primary" onclick={() => (showForm = !showForm)}><Icon name="plus" size={15} /> {t('sites.newSite')}</button>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if showMove && admin}
  <form class="card grid md:grid-cols-5 gap-3 items-end mb-4 rise" onsubmit={askMove}>
    <div class="md:col-span-5"><div class="font-medium text-sm">{t('sites.moveTitle')}</div><p class="text-xs text-muted mt-1">{t('sites.moveIntro')}</p></div>
    <div class="md:col-span-2"><label class="label" for="mf">{t('sites.moveFrom')}</label><input id="mf" class="input font-mono" list="site-ips" bind:value={move.from} placeholder={t('sites.moveFromPh')} /></div>
    <div class="md:col-span-2"><label class="label" for="mt">{t('sites.moveTo')}</label><input id="mt" class="input font-mono" bind:value={move.to} placeholder="203.0.113.10" required /></div>
    <button class="btn btn-primary"><Icon name="move" size={14} /> {t('sites.moveBtn')}</button>
    <p class="md:col-span-5 text-xs text-muted -mt-1">{move.from.trim() ? (onFrom.length ? tn('sites.moveOnFrom', onFrom.length, { ip: move.from.trim(), list: onFrom.join(', ') }) : t('sites.moveNoneOn', { ip: move.from.trim() })) : t('sites.moveGoneHint')}</p>
    <datalist id="site-ips">{#each siteIPs as ip}<option value={ip}></option>{/each}</datalist>
  </form>
{/if}
{#if showForm}
  <form class="card grid md:grid-cols-6 gap-3 items-end mb-4 rise" onsubmit={create}>
    <div class="md:col-span-2"><label class="label" for="d">{t('sites.domain')}</label><input id="d" class="input" bind:value={form.domain} placeholder="example.com" required /></div>
    {#if admin}<div><label class="label" for="u">{t('sites.owner')}</label><select id="u" class="input" bind:value={form.user} required><option value="">—</option>{#each users as u}<option value={u.login}>{u.login}</option>{/each}</select></div>{/if}
    <div><label class="label" for="m">{t('sites.mode')}</label><select id="m" class="input" bind:value={form.mode}><option value="fpm">nginx + php-fpm</option><option value="apache">nginx + Apache</option><option value="proxy">proxy → backend</option></select></div>
    {#if proxy}
      <div><label class="label" for="b">Backend</label><input id="b" class="input font-mono" bind:value={form.backend} placeholder="http://127.0.0.1:3000" required /></div>
    {:else}
      <div><label class="label" for="p">PHP</label><select id="p" class="input" bind:value={form.php_version}><option value="">{t('sites.phpLatest')}</option>{#each php as v}<option value={v.version}>{v.version}</option>{/each}</select></div>
      <div><label class="label" for="pr">{t('sites.preset')}</label><select id="pr" class="input" bind:value={form.preset}>{#each presets as p}<option value={p.id}>{presetName(p)}</option>{/each}</select></div>
    {/if}
    {#if presetInfo?.description && !proxy}<p class="md:col-span-6 text-xs text-muted -mt-1">{presetName(presetInfo)}: {presetDesc(presetInfo)} {t('sites.presetHint')}</p>{/if}
    {#if showMore}
      <div class="md:col-span-6 grid md:grid-cols-4 gap-3 items-end border-t border-line pt-3" transition:slide={{ duration: dur(180) }}>
        <div class="md:col-span-2"><label class="label" for="al">{t('site.aliases')}</label><input id="al" class="input font-mono" bind:value={form.aliases} placeholder="shop.example.com, example.net" /></div>
        <div><label class="label" for="dr">{t('site.docrootSub')}</label><input id="dr" class="input font-mono" bind:value={form.docroot} placeholder="public" /></div>
        <div><label class="label" for="rw">www</label><select id="rw" class="input" bind:value={form.redirect_www}><option value="none">{t('site.wwwAsIs')}</option><option value="to_root">{t('site.wwwToRoot')}</option><option value="to_www">{t('site.wwwToWww')}</option></select></div>
        <div><label class="label" for="ip">{t('site.ip')}</label><input id="ip" class="input font-mono" list="site-ips-new" bind:value={form.ip} placeholder={t('sites.ipPh')} /></div>
        <div><label class="label" for="i6m">IPv6</label><select id="i6m" class="input" bind:value={form.ipv6Mode}><option value="auto">{t('sites.ipv6Auto')}</option><option value="none">{t('sites.ipv6None')}</option><option value="addr">{t('sites.ipv6Addr')}</option></select></div>
        {#if form.ipv6Mode === 'addr'}<div><label class="label" for="i6">{t('sites.ipv6Address')}</label><input id="i6" class="input font-mono" bind:value={form.ipv6} placeholder="2001:db8::10" required /></div>{/if}
        <div><label class="label" for="mb">client_max_body_size</label><input id="mb" class="input font-mono" bind:value={form.client_max_body} pattern={'[0-9]+[kKmMgG]?'} placeholder={t('sites.maxBodyPh')} /></div>
        {#if !proxy}
          <div><label class="label" for="pm">php-fpm pm</label><select id="pm" class="input" bind:value={form.fpm_pm}><option value="">{t('sites.byDefault', { value: 'ondemand' })}</option><option value="ondemand">ondemand</option><option value="dynamic">dynamic</option><option value="static">static</option></select></div>
          <div><label class="label" for="mc">max_children</label><input id="mc" class="input" type="number" min="1" max="512" bind:value={form.fpm_max_children} placeholder={t('sites.maxChildrenPh')} /></div>
        {/if}
        <div><label class="label" for="rl">{t('site.rateLimit')}</label><input id="rl" class="input" type="number" min="0" max="10000" bind:value={form.rate_limit} /></div>
        <div class="md:col-span-4"><label class="label" for="af">{t('site.allowFrom')}</label><input id="af" class="input font-mono" bind:value={form.allow_from} placeholder="203.0.113.0/24, 198.51.100.7" /></div>
        {#if !proxy}<div class="md:col-span-4"><label class="label" for="ini">{t('sites.phpIni')}</label><textarea id="ini" class="input font-mono text-xs h-20 resize-y" bind:value={form.php_ini} spellcheck="false" placeholder={'memory_limit=256M\nupload_max_filesize=64M'}></textarea><p class="text-xs text-muted mt-1">{t('sites.phpIniHint')}</p></div>{/if}
        <div class="md:col-span-4 flex flex-wrap gap-x-5 gap-y-2 text-sm">
          <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={form.redirect_https} /> HTTP → HTTPS (+HSTS)</label>
          <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={form.http2} /> HTTP/2</label>
          <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={form.http3} /> HTTP/3</label>
          {#if form.mode === 'apache'}<label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={form.static_by_nginx} /> {t('site.staticByNginx')}</label>{/if}
          {#if !proxy}<label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={form.allow_exec} /> {t('site.allowExec')}</label>{/if}
          <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={form.security_headers} /> {t('site.securityHeaders')}</label>
        </div>
        <datalist id="site-ips-new">{#each siteIPs as ip}<option value={ip}></option>{/each}</datalist>
      </div>
    {/if}
    <div class="md:col-span-6 flex flex-wrap items-center gap-x-4 gap-y-2 text-sm">
      <label class="flex items-center gap-1.5"><input type="checkbox" bind:checked={form.www} /> www</label>
      <label class="flex items-center gap-1.5"><input type="checkbox" checked={form.ssl === 'auto'} onchange={(e) => (form.ssl = (e.target as HTMLInputElement).checked ? 'auto' : 'none')} /> SSL</label>
      <button type="button" class="btn btn-ghost btn-sm" onclick={() => (showMore = !showMore)} aria-expanded={showMore}><Icon name="chevron" size={13} class="transition-transform {showMore ? 'rotate-90' : ''}" /> {t('sites.moreSettings')}</button>
      <button class="btn btn-primary ml-auto">{t('common.create')}</button>
    </div>
  </form>
{/if}
{#if moved}<div class="card mb-4 text-sm rise border-accent/40 flex items-start gap-2"><Icon name="move" size={15} class="text-accent-ink shrink-0 mt-0.5" /><span class="flex-1 break-words">{t('sites.moving', { to: moved.to, list: moved.sites.join(', ') })}</span><button class="btn btn-ghost btn-sm" onclick={() => (moved = null)} aria-label={t('common.close')}><Icon name="x" size={13} /></button></div>{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
<div class="card overflow-x-auto p-0 rise">
  {#if !sites}<Skeleton rows={5} />{:else}
  <table class="tbl">
    <thead><tr><th>{t('sites.domain')}</th><th>{t('sites.owner')}</th><th>PHP</th><th>{t('sites.mode')}</th><th>IP</th><th>SSL</th><th>{t('common.status')}</th><th></th></tr></thead>
    <tbody>
      {#each sites as s, i}
        <tr class="rise" style="--i:{i}">
          <td data-label={t('sites.domain')}><a class="link font-medium" href="/sites/{s.domain}">{s.domain}</a>{#if s.aliases?.length}<div class="text-xs text-muted">{s.aliases.join(', ')}</div>{/if}{#if s.allow_from?.length}<div class="text-[11px] text-muted font-mono flex items-center gap-1 mt-0.5"><Icon name="lock" size={11} /> {s.allow_from.length} IP</div>{/if}</td>
          <td data-label={t('sites.owner')}>{s.login}</td><td data-label="PHP" class="font-mono">{s.php_version || '—'}</td>
          <td data-label={t('sites.mode')} class="text-muted">{s.mode}{#if s.preset}<span class="tag tag-accent ml-1">{s.preset}</span>{/if}{#if s.cms}<div class="text-xs mt-0.5">{t('sites.cmsLine', { cms: (s.cms + ' ' + (s.cms_version ?? '')).trim() })}</div>{/if}</td>
          <td data-label="IP" class="font-mono text-xs">{s.ip || '—'}{#if s.ipv6}<div class="text-muted break-all">{s.ipv6}</div>{/if}</td>
          <td data-label="SSL"><span class="tag {s.certificate_id ? 'tag-ok' : 'tag-muted'}">{s.certificate_id ? 'https' : s.ssl}</span></td>
          <td data-label={t('common.status')}><span class="tag {s.status === 'active' ? 'tag-ok' : s.status === 'error' ? 'tag-err' : 'tag-warn'}">{s.status}</span>{#if s.last_error}<div class="text-xs text-danger max-w-xs truncate" title={s.last_error}>{s.last_error}</div>{/if}</td>
          <td data-label=""><div class="row-actions">
            <button class="btn btn-sm" onclick={() => (ask = askApply(s))} title={t('sites.applyTitle')}><Icon name="refresh" size={13} /></button>
            <!-- Кнопки строки — иконки, как «применить» и «удалить»: с колонкой IP подписанная «стоп» выталкивала их за край таблицы уже на 1024px; что произойдёт, объясняет окно подтверждения. -->
            {#if admin}{#if s.status === 'suspended'}<button class="btn btn-sm" onclick={() => (ask = askSuspend(s))} title={t('sites.enable')} aria-label={t('sites.enable')}><Icon name="play" size={13} /></button>{:else}<button class="btn btn-sm" onclick={() => (ask = askSuspend(s))} title={t('sites.suspend')} aria-label={t('sites.suspend')}><Icon name="stop" size={13} /></button>{/if}{/if}
            <button class="btn btn-danger btn-sm" onclick={() => { del = s; purge = false; confirmDomain = ''; }} title={t('sites.delBtnTitle')}><Icon name="trash" size={13} /></button>
          </div></td>
        </tr>
      {/each}
      {#if !sites.length}<Empty text={t('sites.empty')} cols={8} />{/if}
    </tbody>
  </table>
  {/if}
</div>
<Confirm bind:ask />
<!-- Deleting with the files cannot be undone: like an account, the domain is typed first. -->
<Modal open={!!del} title={t('sites.delTitle', { domain: del?.domain ?? '' })} onclose={() => (del = null)}>
  <p>{t('sites.delNote')}</p>
  <label class="flex items-start gap-2 p-2 rounded-md border {purge ? 'border-danger/50 bg-danger-soft' : 'border-line'} transition-colors"><input type="checkbox" bind:checked={purge} class="mt-0.5" /><span>{t('sites.delPurge')}<br /><span class="text-xs text-muted">{t('sites.delPurgeDir', { dir: 'data/www/' + (del?.domain ?? ''), login: del?.login ?? '' })}</span></span></label>
  {#if purge}<div><label class="label" for="cd">{t('sites.delConfirm')}</label><input id="cd" class="input font-mono" bind:value={confirmDomain} placeholder={del?.domain} /></div>{/if}
  {#snippet footer()}<button class="btn" onclick={() => (del = null)}>{t('common.cancel')}</button><button class="btn btn-danger" disabled={purge && confirmDomain.trim() !== del?.domain} onclick={() => action(del.domain, 'delete')}><Icon name="trash" size={14} /> {purge ? t('sites.deleteWithFiles') : t('common.delete')}</button>{/snippet}
</Modal>
