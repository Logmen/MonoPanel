<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError, bytes } from '$lib/api';
  import { auth, notify } from '$lib/state.svelte';
  import { t, tn } from '$lib/i18n/index.svelte';
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
  let pwd = $state<{ name: string; value: string } | null>(null);
  let pwdBusy = $state(false);
  let ask = $state<Ask | null>(null);
  // auth: '' — панель решает по сайтам владельца, native / sha2 — явно.
  let form = $state({ name: '', user: '', password: '', auth: '' as '' | 'native' | 'sha2', remote: false });
  const admin = $derived(auth.me?.role === 'admin');
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function load() {
    try { engine = await api('/db/engine'); dbs = await api('/databases'); if (admin) users = ((await api('/users')) as any[]).filter((u) => u.role === 'user'); } catch (e: any) { error = e.text || String(e); }
    try { pma = await api('/db/phpmyadmin'); if (pma && !pmaForm.touched) pmaForm = { port: pma.port || 2083, php_version: '', touched: false }; } catch { pma = null; }
    if (admin && pmaPHP === null) { try { pmaPHP = ((await api('/php/versions')) as any).installed.filter((v: any) => v.status === 'installed'); } catch { pmaPHP = []; } }
  }
  // phpMyAdmin панели: свой порт с именем и сертификатом панели. Кнопка в строке
  // базы просит одноразовую ссылку (временная учётка только на эту базу); окно
  // открывается до запроса, иначе браузер примет его за всплывающее.
  let pma = $state<any>(null);
  let pmaPHP = $state<any[] | null>(null);
  let pmaForm = $state({ port: 2083, php_version: '', touched: false });
  async function pmaInstall(e: Event) {
    e.preventDefault(); error = '';
    const body: Record<string, unknown> = { port: Number(pmaForm.port) || 0 };
    if (pmaForm.php_version) body.php_version = pmaForm.php_version;
    try { const r: any = await api('/db/phpmyadmin', { method: 'POST', json: body }); job = r.job_id; pmaForm.touched = false; } catch (e) { fail(e); }
  }
  async function pmaRemove() { error = ''; try { const r: any = await api('/db/phpmyadmin', { method: 'DELETE' }); job = r.job_id; } catch (e) { fail(e); } }
  const askPMARemove = (): Ask => ({ title: t('databases.pmaRemoveTitle'), note: t('databases.pmaRemoveNote'), action: t('databases.pmaRemove'), run: pmaRemove });
  async function pmaOpen(name: string) {
    error = '';
    const w = window.open('about:blank', '_blank');
    try {
      const r: any = await api(`/databases/${name}/phpmyadmin`, { method: 'POST' });
      if (w) { w.opener = null; w.location.href = r.url; } else window.location.href = r.url;
    } catch (e) { w?.close(); fail(e); }
  }
  onMount(load);
  // Счётчик баз у сервера общий; аккаунту показываем число своих — тех, что в таблице.
  const sub = $derived.by(() => {
    if (!engine?.installed) return 'MySQL 8.4 / Percona Server 8.4';
    const i = engine.instance;
    return [`${i.engine} ${i.version}`, engine.service?.active_state, i.socket, `mysql_native_password: ${i.native_password ? t('common.on') : t('common.off')}`, tn('databases.count', admin ? (engine.databases ?? dbs.length) : dbs.length)].filter(Boolean).join(' · ');
  });
  async function installEngine(c: string) { error = ''; try { const r: any = await api('/stack/install', { method: 'POST', json: { component: c } }); job = r.job_id; } catch (e) { fail(e); } }
  // Полное имя базы и её пользователя — <login>_<name>, MySQL ограничивает его 32 символами.
  const owner = $derived(admin ? form.user : (auth.me?.login ?? ''));
  // Включение mysql_native_password перезапускает MySQL всего сервера, поэтому API
  // делает это только для администратора; аккаунту вариант доступен, когда он уже включён.
  const nativeLocked = $derived(!admin && !!engine?.installed && !engine.instance?.native_password);
  const fullName = $derived(owner && form.name ? `${owner}_${form.name}` : '');
  async function create(e: Event) {
    e.preventDefault(); error = '';
    const body: Record<string, unknown> = { name: form.name };
    if (admin) body.user = form.user;
    if (form.password) body.password = form.password;
    if (form.auth) body.legacy_auth = form.auth === 'native';
    if (form.remote) body.remote = true;
    try { created = await api('/databases', { method: 'POST', json: body }); form = { name: '', user: form.user, password: '', auth: '', remote: false }; showForm = false; await load(); } catch (e) { fail(e); }
  }
  async function drop() { if (!del) return; try { await api(`/databases/${del.name}`, { method: 'DELETE' }); notify(t('databases.deleted', { name: del.name })); del = null; await load(); } catch (e) { fail(e); } }
  // Пустой пароль — панель сгенерирует и покажет его один раз; заданный не показывается.
  async function passwd() {
    if (!pwd) return; const { name, value } = pwd; pwdBusy = true; error = '';
    try {
      const r: any = await api(`/databases/${name}/password`, { method: 'POST', json: value ? { password: value } : {} });
      pwd = null;
      if (r.password) created = { database: { name }, password: r.password, reset: true };
      else notify(t('databases.passwordUpdated', { name }));
    } catch (e) { fail(e); } finally { pwdBusy = false; }
  }
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
  // Перенастройка под хост: панель заново считает свои значения от памяти сервера
  // (и mysql_native_password по установленным PHP), заданные вручную сохраняются.
  const askTune = (): Ask => ({
    title: t('databases.tuneTitle'),
    note: t('databases.tuneNote', { ram: cfg?.ram_mb ?? '?' }),
    action: t('databases.tuneAction'),
    run: () => tune()
  });
  async function tune() {
    cfgBusy = true; error = '';
    try { const st: any = await api('/db/engine/tune', { method: 'POST' }); notify(t('databases.tuned', { state: st.service?.active_state ?? '?' })); await load(); cfg = await api('/db/engine/config'); } catch (e) { fail(e); } finally { cfgBusy = false; }
  }
</script>

<PageHead title={t('databases.title')} {sub}>
  {#if engine?.installed}<button class="btn btn-primary" onclick={() => (showForm = !showForm)}><Icon name="plus" size={15} /> {t('databases.newDb')}</button>{/if}
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if engine && !engine.installed && admin}
  <div class="card mb-4 text-sm rise">
    <div class="flex flex-wrap items-center gap-3"><span>{engine.instance?.status === 'installing' ? t('databases.installing', { engine: engine.instance.engine }) : t('databases.notInstalled')}</span><button class="btn btn-primary" onclick={() => installEngine('percona')}>{t('databases.installPercona')}</button><button class="btn" onclick={() => installEngine('mysql')}>MySQL 8.4</button></div>
    {#if engine.instance?.last_error}<div class="text-xs text-danger mt-2">{t('databases.lastError', { error: engine.instance.last_error })}</div>{/if}
  </div>
{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
{#if engine?.installed}
  {#if showForm}
    <form class="card grid md:grid-cols-3 lg:grid-cols-6 gap-3 items-start mb-4 rise" onsubmit={create}>
      <div><label class="label" for="n">{t('databases.nameSuffix')}</label><input id="n" class="input font-mono" bind:value={form.name} pattern="[a-z0-9_]{'{'}1,24{'}'}" maxlength="24" required />{#if fullName}<p class="text-[11px] mt-1 font-mono {fullName.length > 32 ? 'text-danger' : 'text-muted'}">{fullName.length > 32 ? t('databases.tooLong', { name: fullName }) : fullName}</p>{/if}</div>
      {#if admin}<div><label class="label" for="u">{t('databases.owner')}</label><select id="u" class="input" bind:value={form.user} required><option value="">—</option>{#each users as u}<option value={u.login}>{u.login}</option>{/each}</select></div>{/if}
      <div><label class="label" for="pw">{t('databases.password')}</label><input id="pw" class="input" bind:value={form.password} minlength="8" maxlength="128" autocomplete="off" placeholder={t('databases.generated')} /></div>
      <div class="lg:col-span-2"><label class="label" for="au">{t('databases.auth')}</label><select id="au" class="input" bind:value={form.auth}><option value="">{t('databases.authAuto')}</option><option value="native" disabled={nativeLocked}>{t('databases.authNative')}</option><option value="sha2">{t('databases.authSha2')}</option></select>{#if form.auth === 'native' && !engine.instance.native_password}<p class="text-[11px] mt-1 text-warn">{t('databases.authNativeRestart')}</p>{:else if form.auth === ''}<p class="text-[11px] mt-1 text-muted">{t('databases.authAutoHint')}</p>{/if}{#if nativeLocked}<p class="text-[11px] mt-1 text-muted">{t('databases.authNativeAdmin')}</p>{/if}</div>
      <div class="flex flex-col gap-2 md:pt-5"><button class="btn btn-primary self-start" disabled={fullName.length > 32}>{t('databases.createNamed')}</button></div>
      <!-- Поле видно, но выключено: API пока отклоняет remote (MySQL слушает только localhost). -->
      <div class="md:col-span-3 lg:col-span-6"><label class="flex items-start gap-2 text-sm text-muted cursor-not-allowed"><input type="checkbox" bind:checked={form.remote} class="mt-1" disabled aria-describedby="db-remote-hint" /><span>{t('databases.remote')} <span id="db-remote-hint" class="text-xs">— {t('databases.remoteHint')}</span></span></label></div>
    </form>
  {/if}
  {#if created}<div class="card mb-4 text-sm font-mono rise border-accent/40">{created.reset ? t('databases.newPassword') : t('databases.database')} <b>{created.database.name}</b>{#if created.database.login}{' · '}{t('databases.ownerIs', { login: created.database.login })}{/if}{#each created.database.users ?? [] as u}{' · '}{u.name}@{u.host} ({u.auth_plugin}){/each}{#if created.password}{' · '}{t('databases.passwordIs')} <b class="select-all">{created.password}</b>{/if}{#if created.dsn}<div class="text-xs text-muted mt-1 select-all">{created.dsn}</div>{/if}{#if created.password}<div class="text-xs text-muted">{t('databases.shownOnce')}</div>{/if}</div>{/if}
  <div class="card overflow-x-auto p-0 rise">
    <!-- Сравнение — строкой под именем базы, а не своей колонкой: с ней кнопки строки уезжали за край таблицы уже на 1024px. -->
    <table class="tbl"><thead><tr><th>{t('databases.colDatabase')}</th><th>{t('databases.owner')}</th><th>{t('databases.accounts')}</th><th>{t('databases.size')}</th><th></th></tr></thead>
      <tbody>
        {#each dbs as d, i (d.id)}<tr class="rise" style="--i:{i}"><td data-label={t('databases.colDatabase')}><div class="min-w-0"><div class="font-mono font-medium">{d.name}</div>{#if d.collation}<div class="font-mono text-xs text-muted" title={[t('databases.collation') + ': ' + d.collation, d.charset].filter(Boolean).join(' · ')}>{d.collation}</div>{/if}</div></td><td data-label={t('databases.owner')}>{d.login}</td><td data-label={t('databases.accounts')} class="font-mono text-xs text-muted">{(d.users ?? []).map((u: any) => u.name + '@' + u.host + ' (' + u.auth_plugin + ')').join(', ')}</td><td data-label={t('databases.size')} class="tabular-nums">{bytes(d.size_bytes)}</td><td data-label=""><div class="row-actions">{#if pma?.installed}<button class="btn btn-sm" title={t('databases.pmaOpenTitle')} onclick={() => pmaOpen(d.name)}><Icon name="external" size={13} /> {t('databases.pmaOpen')}</button>{/if}<button class="btn btn-sm" onclick={() => (pwd = { name: d.name, value: '' })}><Icon name="key" size={13} /> {t('databases.passwordBtn')}</button><button class="btn btn-danger btn-sm" title={t('common.delete')} aria-label={t('common.delete')} onclick={() => (del = d)}><Icon name="trash" size={13} /></button></div></td></tr>{/each}
        {#if !dbs.length}<Empty text={t('databases.empty')} cols={5} />{/if}
      </tbody></table>
  </div>
  {#if admin && pma}
    <div class="card mt-4 rise" style="--i:1">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div class="min-w-0">
          <div class="font-medium text-sm">{t('databases.pmaTitle')}</div>
          {#if pma.installed}
            <a class="text-sm text-accent-ink hover:underline inline-flex items-center gap-1 break-all mt-1" href={pma.url} target="_blank" rel="noopener">{pma.url.replace('https://', '').replace(/\/$/, '')} <Icon name="external" size={12} /></a>
            <div class="text-xs text-muted mt-1">{t('databases.pmaState', { version: pma.version, php: pma.php_version })}{#if pma.sessions}{' · '}{t('databases.pmaSessions', { n: pma.sessions })}{/if}{#if pma.version !== pma.pinned}{' · '}<span class="text-warn">{t('databases.pmaUpgrade', { version: pma.pinned })}</span>{/if}</div>
          {:else}
            <div class="text-sm text-muted mt-1">{t('databases.pmaNotInstalled', { version: pma.pinned })}</div>
          {/if}
          <p class="text-xs text-muted mt-1">{t('databases.pmaHint')}</p>
        </div>
        {#if pma.installed}<button class="btn btn-danger btn-sm shrink-0" onclick={() => (ask = askPMARemove())}><Icon name="trash" size={13} /> {t('databases.pmaRemove')}</button>{/if}
      </div>
      <form class="flex flex-wrap items-end gap-2 mt-3" onsubmit={pmaInstall}>
        <label class="flex items-center gap-1.5 text-xs text-muted">{t('databases.pmaPort')} <input class="input text-sm w-24" type="number" min="1024" max="65535" bind:value={pmaForm.port} oninput={() => (pmaForm.touched = true)} /></label>
        <label class="flex items-center gap-1.5 text-xs text-muted">{t('databases.pmaPHP')} <select class="input text-sm" bind:value={pmaForm.php_version} onchange={() => (pmaForm.touched = true)}><option value="">{t('databases.pmaPHPAuto')}</option>{#each pmaPHP ?? [] as v}<option value={v.version}>PHP {v.version}</option>{/each}</select></label>
        <button class="btn btn-primary btn-sm">{pma.installed ? t('databases.pmaApply') : t('databases.pmaInstall')}</button>
      </form>
    </div>
  {:else if pma?.installed}
    <p class="text-xs text-muted mt-3">{t('databases.pmaUserHint')} <a class="text-accent-ink hover:underline" href={pma.url} target="_blank" rel="noopener">{pma.url}</a></p>
  {/if}
  {#if admin && cfg}
    <div class="card p-0 overflow-hidden mt-4 rise" style="--i:1">
      <div class="px-4 py-3 border-b border-line flex flex-wrap items-start justify-between gap-3">
        <div class="min-w-0">
          <div class="font-medium text-sm">{t('databases.cfgTitle')}</div>
          <p class="text-xs text-muted mt-1">{t('databases.cfgHint', { ram: cfg.ram_mb })}</p>
        </div>
        <button class="btn btn-sm shrink-0" disabled={cfgBusy} onclick={() => (ask = askTune())}><Icon name="refresh" size={13} /> {t('databases.tune')}</button>
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
<Modal open={!!pwd} title={t('databases.newPasswordTitle', { name: pwd?.name ?? '' })} onclose={() => (pwd = null)}>
  <p class="text-muted">{t('databases.newPasswordNote')}</p>
  {#if pwd}<form id="db-passwd" onsubmit={(e) => { e.preventDefault(); passwd(); }}><label class="label" for="npw">{t('databases.newPasswordLabel')}</label><input id="npw" class="input" bind:value={pwd.value} minlength="8" maxlength="128" autocomplete="new-password" placeholder={t('databases.newPasswordPh')} /></form>{/if}
  {#snippet footer()}<button class="btn" onclick={() => (pwd = null)} disabled={pwdBusy}>{t('common.cancel')}</button><button class="btn btn-primary" form="db-passwd" disabled={pwdBusy}>{pwdBusy ? t('databases.busy') : pwd?.value ? t('databases.setPassword') : t('databases.generate')}</button>{/snippet}
</Modal>
