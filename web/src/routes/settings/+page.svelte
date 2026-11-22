<script lang="ts">
  import { onMount } from 'svelte';
  import QRCode from 'qrcode';
  import { api, ApiError, when } from '$lib/api';
  import { auth, notify, theme, setTheme, type ThemeMode } from '$lib/state.svelte';
  import { t, lang, setLang, dateLocale, type LangMode, type MsgKey } from '$lib/i18n/index.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  let totp = $state<any>(null);
  let setup = $state<any>(null);
  let qr = $state('');
  let code = $state('');
  let totpOff = $state(false);
  let totpPw = $state('');
  let tokens = $state<any[]>([]);
  let tokenForm = $state<{ name: string; scopes: string; expires: number } | null>(null);
  let newToken = $state<any>(null);
  let hooks = $state<any[]>([]);
  let hook = $state({ url: '', events: 'job.failed,cert.issue.*', secret: '' });
  let newHook = $state<any>(null);
  let testing = $state('');
  let tested = $state<Record<string, { at?: string; err?: string }>>({});
  let realip = $state<any>(null);
  let realipFrom = $state('');
  let upd = $state<any>(null);
  let updForm = $state({ repo: '', api: '', channel: 'stable', token: '', check_hours: 24, auto_apply: false });
  let updVersion = $state('');
  let updJob = $state<number | null>(null);
  let updating = $state('');
  let sel = $state<any>(null);
  let pcfg = $state<any>(null);
  let pform = $state({ hostname: '', listen: '', log_level: 'info', log_format: 'text', jobs_workers: 1, restart: false });
  let restarting = $state<{ url: string; sure: boolean; undo: string; late: boolean } | null>(null);
  let tpl = $state<any>(null);
  let tplAll = $state(false);
  let error = $state('');
  let ask = $state<Ask | null>(null);
  const admin = $derived(auth.me?.role === 'admin');
  const errText = (e: unknown) => (e instanceof ApiError ? e.text : String(e));
  const fail = (e: unknown) => { error = errText(e); notify(error, 'err'); };
  const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
  const commaList = (s: string) => s.split(',').map((x) => x.trim()).filter(Boolean);
  const date = (s: string) => new Date(s).toLocaleDateString(dateLocale());
  // Каждая карточка грузится сама: один упавший запрос (агент недоступен, API без
  // /system/config) не должен оставлять пустой всю страницу.
  async function part(f: () => Promise<unknown>) { try { await f(); } catch (e) { error = errText(e); } }
  const loadTotp = () => part(async () => { totp = await api('/auth/totp'); });
  const loadTokens = () => part(async () => { tokens = await api('/tokens'); });
  const loadHooks = () => part(async () => { hooks = await api('/webhooks'); });
  const loadRealIP = () => part(async () => { realip = await api('/stack/nginx/real-ip'); realipFrom = (realip.from || []).join(', '); });
  const loadUpdate = () => part(async () => setUpdate(await api('/system/update')));
  const loadSELinux = () => part(async () => { sel = await api('/system/selinux'); });
  const loadPanel = () => part(async () => setPanel(await api('/system/config')));
  const loadTemplates = () => part(async () => { tpl = await api('/system/templates'); });
  function load() { const all = [loadTotp(), loadTokens()]; if (admin) all.push(loadPanel(), loadUpdate(), loadHooks(), loadRealIP(), loadSELinux(), loadTemplates()); return Promise.all(all); }
  onMount(load);

  /* ── 2FA ─────────────────────────────────────────────────────────────── */
  async function startTotp() { error = ''; try { setup = await api('/auth/totp/setup', { method: 'POST' }); qr = await QRCode.toDataURL(setup.url, { width: 180, margin: 1 }); } catch (e) { fail(e); } }
  async function enableTotp(e: Event) { e.preventDefault(); error = ''; try { await api('/auth/totp/enable', { method: 'POST', json: { code: code.trim() } }); setup = null; code = ''; notify(t('settings.totpEnabled')); await loadTotp(); } catch (e) { fail(e); } }
  // Пароль — в маскированном поле окна, а не в window.prompt(): prompt показывает его открытым текстом.
  async function disableTotp(e: Event) { e.preventDefault(); error = ''; try { await api('/auth/totp/disable', { method: 'POST', json: { password: totpPw } }); totpOff = false; totpPw = ''; notify(t('settings.totpDisabled')); await loadTotp(); } catch (e) { fail(e); } }

  /* ── API tokens (own; tokens of another account live on the Users page) ── */
  function toggleTokenForm() { tokenForm = tokenForm ? null : { name: 'api', scopes: '', expires: 0 }; }
  async function createToken(e: Event) {
    e.preventDefault(); if (!tokenForm) return; error = '';
    const body: Record<string, unknown> = { name: tokenForm.name.trim() };
    const scopes = commaList(tokenForm.scopes); if (scopes.length) body.scopes = scopes;
    if (Number(tokenForm.expires) > 0) body.expires_in_days = Number(tokenForm.expires);
    try { newToken = await api('/tokens', { method: 'POST', json: body }); tokenForm = null; await loadTokens(); } catch (e) { fail(e); }
  }
  const expired = (tk: any) => !!tk.expires_at && new Date(tk.expires_at).getTime() < Date.now();
  const askRevoke = (tk: any): Ask => ({
    title: t('settings.tokenRevokeTitle', { name: tk.name }),
    note: t('settings.tokenRevokeNote'),
    danger: true, action: t('settings.tokenRevokeAction'),
    run: () => revoke(tk.id)
  });
  async function revoke(id: number) { try { await api(`/tokens/${id}`, { method: 'DELETE' }); if (newToken?.record?.id === id) newToken = null; await loadTokens(); } catch (e) { fail(e); } }

  /* ── webhooks ────────────────────────────────────────────────────────── */
  async function addHook(e: Event) {
    e.preventDefault(); error = '';
    const body: Record<string, unknown> = { url: hook.url.trim(), events: commaList(hook.events) };
    if (hook.secret) body.secret = hook.secret;
    try { newHook = await api('/webhooks', { method: 'POST', json: body }); hook = { url: '', events: hook.events, secret: '' }; await loadHooks(); } catch (e) { fail(e); }
  }
  // Проверка — настоящая доставка: до трёх попыток с паузами, поэтому может
  // занять до минуты; ответ (доставлено или ошибка приёмника) остаётся у строки.
  async function testHook(h: any) {
    testing = h.id;
    try { await api(`/webhooks/${h.id}/test`, { method: 'POST' }); tested[h.id] = { at: new Date().toISOString() }; notify(t('settings.hookDeliveredToast', { url: h.url })); }
    catch (e) { tested[h.id] = { err: errText(e) }; notify(errText(e), 'err'); }
    finally { testing = ''; }
  }
  const askHook = (h: any): Ask => ({
    title: t('settings.hookDeleteTitle'),
    note: t('settings.hookDeleteNote', { url: h.url, events: (h.events ?? []).join(', ') }),
    danger: true, action: t('common.delete'),
    run: () => rmHook(h.id)
  });
  async function rmHook(id: string) { try { await api(`/webhooks/${id}`, { method: 'DELETE' }); if (newHook?.id === id) newHook = null; await loadHooks(); } catch (e) { fail(e); } }

  /* ── nginx real_ip ───────────────────────────────────────────────────── */
  async function saveRealIP() { try { realip = await api('/stack/nginx/real-ip', { method: 'PUT', json: { cloudflare: realip.cloudflare, from: realipFrom.split(/[\s,]+/).filter(Boolean) } }); realipFrom = (realip.from || []).join(', '); notify(t('settings.realIpSaved')); } catch (e) { fail(e); } }

  /* ── SELinux ─────────────────────────────────────────────────────────── */
  // Переключение SELinux: permissive только через окно с объяснением цены, обратно — сразу.
  async function setSELinux(mode: string) { try { sel = await api('/system/selinux', { method: 'PUT', json: { mode } }); notify('SELinux: ' + sel.mode, mode === 'permissive' ? 'err' : undefined); } catch (e) { fail(e); } }
  const askPermissive = (): Ask => ({
    title: t('settings.selinuxPermissiveTitle'),
    note: sel?.warning,
    danger: true, action: t('settings.selinuxPermissiveAction'),
    run: () => setSELinux('permissive')
  });

  /* ── panel updates ───────────────────────────────────────────────────── */
  function setUpdate(st: any) { upd = st; updForm = { repo: st.settings.repo ?? '', api: st.settings.api ?? '', channel: st.settings.channel, token: '', check_hours: st.settings.check_hours, auto_apply: st.settings.auto_apply }; }
  // Пустое значение для API значит «не трогать»: очищенное поле уходит как «-» —
  // для репозитория это выключает проверки, для адреса API возвращает api.github.com.
  const changed = (v: string, was: string | undefined) => { v = v.trim(); return v === (was ?? '') ? undefined : v || '-'; };
  function saveUpdate(e: Event) {
    e.preventDefault(); if (!upd) return;
    const body = { repo: changed(updForm.repo, upd.settings.repo), api: changed(updForm.api, upd.settings.api), channel: updForm.channel, token: updForm.token.trim() || undefined, check_hours: Number.isFinite(updForm.check_hours) ? updForm.check_hours : undefined, auto_apply: updForm.auto_apply };
    if (body.repo === '-') ask = { title: t('settings.updateOffTitle'), note: t('settings.updateOffNote'), danger: true, action: t('settings.updateOffAction'), run: () => putUpdate(body, t('settings.updateOff')) };
    else putUpdate(body, t('settings.updateSaved'));
  }
  async function putUpdate(body: Record<string, unknown>, done: string) { error = ''; try { setUpdate(await api('/system/update', { method: 'PUT', json: body })); notify(done); } catch (e) { fail(e); } }
  const askForgetToken = (): Ask => ({ title: t('settings.updateTokenForgetTitle'), note: t('settings.updateTokenForgetNote'), danger: true, action: t('settings.updateTokenForgetAction'), run: () => putUpdate({ clear_token: true }, t('settings.updateTokenForgotten')) });
  async function checkUpdate() { updating = t('settings.updateChecking'); try { setUpdate(await api('/system/update/check', { method: 'POST', json: {} })); notify(upd.available ? t('settings.updateAvailableToast', { version: upd.latest }) : t('settings.updateLatest')); } catch (e) { fail(e); } finally { updating = ''; } }
  // Пустая версия — последняя в канале; заданная ставится как есть, в том числе
  // более старая (откат) или текущая (переустановка).
  const askUpdate = (version: string): Ask => ({
    title: version ? t('settings.updateVersionTitle', { version }) : t('settings.updateTitle', { version: upd.latest }),
    note: t('settings.updateNote'),
    action: t('settings.updateAction'),
    run: () => applyUpdate(version)
  });
  function submitVersion(e: Event) { e.preventDefault(); const v = updVersion.trim(); if (v) ask = askUpdate(v); }
  async function applyUpdate(version: string) {
    let r: any;
    try { r = await api('/system/update/apply', { method: 'POST', json: version ? { version } : {} }); } catch (e) { fail(e); return; }
    updJob = r.job_id; updVersion = '';
    waitUpdate(version || upd.latest);
  }
  // Установка перезапускает саму панель: ждём, пока она пропадёт и ответит снова
  // (той версией, что получилась: при откате — прежней), и перезагружаем страницу.
  // Упавшая до перезапуска задача (нет релиза, подпись) останавливает ожидание, а
  // новая запись о попытке установки говорит, чем всё кончилось, даже если перезапуск
  // прошёл между опросами или до него не дошло (пакет не встал).
  async function waitUpdate(version: string) {
    updating = t('settings.updateInstalling', { version });
    const ver = (v: unknown) => String(v ?? '').replace(/^v/, '');
    const current = ver(upd?.current);
    const before = upd?.last_attempt?.started ?? '';
    const deadline = Date.now() + 300000;
    let down = false;
    while (Date.now() < deadline && updating) {
      await sleep(3000);
      let st: any;
      try { st = await api('/system/update'); } catch { down = true; continue; }
      const a = st.last_attempt;
      const fresh = !!a?.started && a.started !== before;
      if (down || ver(st.current) !== current || (fresh && a.status === 'done')) { location.reload(); return; }
      if (fresh && a.status !== 'installing') { setUpdate(st); updating = ''; notify(t('settings.updateFailed', { status: a.status }), 'err'); return; }
    }
    if (updating) { updating = ''; notify(t('settings.updateNoResponse'), 'err'); }
  }
  const keyText = (st: any) => (st.key_source === 'builtin' ? t('settings.updateKeyBuiltin') : st.key_source === 'config' ? t('settings.updateKeyConfig') : st.key_pinned ? t('settings.updateKeyOn') : t('settings.updateKeyOff'));
  // Строка состояния — одной строкой через «·»: пробел в начале блока {#if} Svelte обрезает.
  const updLine = $derived(upd ? [t('settings.updateVersion', { version: upd.current }), upd.checked_at ? t('settings.updateChecked', { when: when(upd.checked_at) }) : '', keyText(upd)].filter(Boolean).join(' · ') : t('settings.updateVersion', { version: '…' }));
  const attemptTag = (s: string) => (s === 'done' ? 'tag-ok' : s === 'installing' ? 'tag-accent' : 'tag-err');

  /* ── the panel's own settings (config.yaml) ──────────────────────────── */
  // Имя и адрес, на которых отвечает работающий monopanel-api, — API отдаёт их вместе
  // с файлом (running_hostname, running_listen). От них считается переезд: и тот, что
  // в форме, и сохранённый раньше без перезапуска — в этой вкладке, в другой или
  // другим администратором, — который применит кнопка «перезапустить».
  const live = $derived<{ hostname: string; listen: string } | null>(pcfg ? { hostname: pcfg.running_hostname ?? pcfg.hostname ?? '', listen: pcfg.running_listen ?? pcfg.listen ?? '' } : null);
  const hostName = (s: string) => s.trim().toLowerCase().replace(/\.$/, '');
  function setPanel(c: any) {
    pcfg = c; pform = { hostname: c.hostname ?? '', listen: c.listen ?? '', log_level: c.log_level || 'info', log_format: c.log_format || 'text', jobs_workers: c.jobs_workers || 1, restart: pform.restart };
  }
  // Только изменённые поля: остальные API оставляет как есть. Имя сравнивается
  // нормализованным: в файле оно может быть с заглавными (mp setup берёт имя хоста
  // как есть), и нетронутое поле не должно считаться изменённым.
  const panelBody = $derived.by(() => {
    const b: Record<string, unknown> = {};
    if (!pcfg) return b;
    if (hostName(pform.hostname) !== hostName(pcfg.hostname ?? '')) b.hostname = hostName(pform.hostname);
    const listen = pform.listen.trim();
    if (listen !== (pcfg.listen ?? '')) b.listen = listen;
    if (pform.log_level !== pcfg.log_level) b.log_level = pform.log_level;
    if (pform.log_format !== pcfg.log_format) b.log_format = pform.log_format;
    if (Number(pform.jobs_workers) !== pcfg.jobs_workers) b.jobs_workers = Number(pform.jobs_workers);
    return b;
  });
  const panelDirty = $derived(Object.keys(panelBody).length > 0);
  // Где панель ответит после перезапуска: порт — из web.listen, хост — тот, что в
  // адресной строке, если только не меняется имя или панель не привязана к одному
  // адресу, а страница открыта по другому.
  function panelURL(hostname: string, listen: string, nameChanged: boolean): string {
    const m = /^(?:\[([^\]]*)\]|([^:]*)):(\d+)$/.exec(listen.trim());
    const bind = m ? (m[1] ?? m[2] ?? '') : '';
    const port = m ? m[3] : location.port;
    const here = location.hostname.replace(/^\[|\]$/g, '');
    let host = location.hostname;
    if (nameChanged && hostname) host = hostname;
    else if (bind && bind !== '0.0.0.0' && bind !== '::' && /^[\d.]+$|:/.test(here) && here !== bind) host = bind.includes(':') ? `[${bind}]` : bind;
    return `https://${host}${port && port !== '443' ? ':' + port : ''}/settings`;
  }
  function moved(url: string): string { try { return new URL(url).origin === location.origin ? '' : url; } catch { return ''; } }
  // Значение для командной строки: в кавычках, если в нём есть что-то кроме
  // безопасных символов (например, «[::]:8443» — шаблон для shell).
  const shq = (v: string) => (/^[\w.:@%+=,/-]+$/.test(v) ? v : `'${v.replace(/'/g, `'\\''`)}'`);
  // Что будет после перезапуска с этими именем и адресом: где ответит панель (пусто —
  // там же, где открыта страница), переезжает ли она точно (имя или адрес отличаются от
  // тех, на которых она работает сейчас), и как вернуть прежние, если браузер её
  // потеряет: та же правка прямо в config.yaml от root на сервере (mp config set --local).
  function afterRestart(hostname: string, listen: string): { url: string; sure: boolean; undo: string } {
    const from = live ?? { hostname: pcfg?.hostname ?? '', listen: pcfg?.listen ?? '' };
    const name = hostName(hostname) !== hostName(from.hostname);
    const addr = listen.trim() !== from.listen.trim();
    const url = moved(panelURL(hostName(hostname), listen, name));
    const back: [string, string][] = [...(addr ? [['web.listen', from.listen] as [string, string]] : []), ...(name ? [['web.hostname', from.hostname] as [string, string]] : [])];
    const undo = back.map(([k, v], i) => `mp config set ${k} ${shq(v)} --local${i === back.length - 1 ? ' --restart' : ''}`).join(' && ');
    return { url, sure: !!url && (name || addr), undo };
  }
  // «Сохранить и перезапустить» применяет форму, «перезапустить» — то, что уже в файле.
  const panelMove = $derived(pcfg ? afterRestart(pform.hostname, pform.listen) : { url: '', sure: false, undo: '' });
  const fileMove = () => afterRestart(pcfg?.hostname ?? '', pcfg?.listen ?? '');
  // Переезд, который уже записан в файл и ждёт перезапуска, — его видит любая сессия.
  const pendingMove = $derived(pcfg?.restart_pending ? fileMove() : null);
  // Предупреждение у формы — только о несохранённой правке имени или адреса: о том,
  // что уже в файле, говорит плашка «ждёт перезапуска».
  const formMoves = $derived(panelMove.sure && ('hostname' in panelBody || 'listen' in panelBody));
  function savePanel(e: Event) { e.preventDefault(); if (!panelDirty && !pform.restart) return; if (pform.restart) ask = askPanelRestart(false); else putPanel(false); }
  const askPanelRestart = (restartOnly: boolean): Ask => {
    const mv = restartOnly ? fileMove() : panelMove;
    return {
      title: t('settings.panelRestartTitle'),
      note: t('settings.panelRestartNote') + (mv.sure ? ' ' + t('settings.panelMoveNote', { url: mv.url }) : ''),
      danger: mv.sure, action: t('settings.panelRestartAction'),
      run: () => putPanel(restartOnly)
    };
  };
  async function putPanel(restartOnly: boolean) {
    error = '';
    const body: Record<string, unknown> = restartOnly ? { restart: true } : { ...panelBody, ...(pform.restart ? { restart: true } : {}) };
    try {
      const r: any = await api('/system/config', { method: 'PUT', json: body });
      setPanel(r.config);
      // Новый адрес — из файла, каким его записал API (имя он приводит к своему виду),
      // и от адреса, на котором работает ещё старый процесс, а не из полей формы.
      if (r.restarted) { pform.restart = false; const mv = fileMove(); waitRestart(mv.url, mv.sure, mv.undo); }
      else notify(t(r.config.restart_pending ? 'settings.panelSavedPending' : 'settings.panelSaved'));
    } catch (e) { fail(e); }
  }
  // monopanel-api сначала отвечает и сразу после этого перезапускается. Страница
  // ждёт, пока ответит новый процесс (пропал и вернулся, или прочитал config.yaml —
  // ничего не ждёт перезапуска), и перезагружается. За переехавшей панелью отсюда не
  // уйти — вместо этого показан её новый адрес: сразу, если адрес меняется этим
  // сохранением, иначе — только когда панель долго молчит (за прокси порт страницы
  // и так не совпадает с web.listen).
  async function waitRestart(url: string, sure: boolean, undo: string) {
    restarting = { url, sure, undo, late: false };
    const start = Date.now();
    let down = false;
    while (Date.now() - start < 180000) {
      await sleep(2000);
      try {
        const c: any = await api('/system/config');
        if (down || (Date.now() - start > 12000 && !c.restart_pending)) { location.reload(); return; }
      } catch (e) {
        if (!(e instanceof ApiError)) down = true;
        else if (down) { location.reload(); return; }
      }
      if (restarting && Date.now() - start > 30000) restarting.late = true;
    }
    // Три минуты спустя панель всё ещё отвечает здесь со старыми значениями — значит,
    // перезапуска не было: вместо вечного «перезапускается» снова показываем её
    // состояние. Не отвечает — остаются новый адрес и команда возврата.
    try { const c: any = await api('/system/config'); restarting = null; setPanel(c); notify(t('settings.panelNoRestart'), 'err'); } catch { /* панели здесь нет */ }
  }

  /* ── templates ───────────────────────────────────────────────────────── */
  const tplOverrides = $derived<any[]>((tpl?.templates ?? []).filter((x: any) => x.override));
  const tplShown = $derived<any[]>(tplAll ? (tpl?.templates ?? []) : tplOverrides);

  const modes: [ThemeMode, string, MsgKey][] = [['system', 'monitor', 'settings.themeSystem'], ['light', 'sun', 'settings.themeLight'], ['dark', 'moon', 'settings.themeDark']];
  const langModes: [LangMode, string, 'lang.auto' | 'lang.ru' | 'lang.en'][] = [['auto', 'monitor', 'lang.auto'], ['ru', 'globe', 'lang.ru'], ['en', 'globe', 'lang.en']];
</script>

<PageHead title={t('settings.title')} sub={t('settings.sub')} />
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
<div class="grid md:grid-cols-2 gap-4">
  <div class="card rise">
    <div class="font-medium mb-1">{t('settings.themeTitle')}</div>
    <p class="text-xs text-muted mb-3">{t('settings.themeHint')}</p>
    <div class="grid grid-cols-3 gap-2">
      {#each modes as [m, icon, key]}
        <button class="flex flex-col items-center gap-1.5 py-3 rounded-lg border transition-all duration-150 {theme.mode === m ? 'border-accent bg-accent-soft text-accent-ink' : 'border-line hover:border-line-strong text-muted hover:text-ink'}" aria-pressed={theme.mode === m} onclick={() => setTheme(m)}><Icon name={icon} size={20} /><span class="text-xs">{t(key)}</span></button>
      {/each}
    </div>
  </div>
  <div class="card rise" style="--i:1">
    <div class="font-medium mb-1">{t('lang.title')}</div>
    <p class="text-xs text-muted mb-3">{t('lang.hint')}</p>
    <div class="grid grid-cols-3 gap-2">
      {#each langModes as [m, icon, key]}
        <button class="flex flex-col items-center gap-1.5 py-3 rounded-lg border transition-all duration-150 {lang.mode === m ? 'border-accent bg-accent-soft text-accent-ink' : 'border-line hover:border-line-strong text-muted hover:text-ink'}" aria-pressed={lang.mode === m} onclick={() => setLang(m)}><Icon name={icon} size={20} /><span class="text-xs">{t(key)}</span></button>
      {/each}
    </div>
  </div>
  <div class="card rise" style="--i:2">
    <div class="font-medium mb-2">{t('settings.totpTitle')}</div>
    {#if totp?.enabled}<p class="text-sm mb-3"><span class="tag tag-ok"><Icon name="check" size={11} /> {t('settings.totpOn')}</span></p><button class="btn btn-danger btn-sm" onclick={() => { totpPw = ''; totpOff = true; }}>{t('settings.totpTurnOff')}</button>
    {:else if setup}
      <p class="text-sm text-muted mb-2">{t('settings.totpScan')}</p>
      {#if qr}<img src={qr} alt="QR" class="mb-2 rounded-md border border-line" />{/if}
      <p class="text-xs text-muted mb-1">{t('settings.totpManual')}</p>
      <div class="text-xs font-mono mb-1 break-all select-all">{setup.secret}</div>
      <div class="text-[11px] font-mono text-muted mb-3 break-all select-all">{setup.url}</div>
      <form class="flex gap-2" onsubmit={enableTotp}><input class="input w-32 font-mono" bind:value={code} placeholder="123456" inputmode="numeric" autocomplete="one-time-code" aria-label={t('settings.totpCode')} required /><button class="btn btn-primary">{t('settings.totpConfirm')}</button></form>
    {:else}<button class="btn" onclick={startTotp}><Icon name="lock" size={14} /> {t('settings.totpSetup')}</button>{/if}
  </div>
  <div class="card rise" style="--i:3">
    <div class="flex justify-between items-center mb-2"><span class="font-medium">{t('settings.tokensTitle')}</span><button class="btn btn-sm {tokenForm ? 'btn-primary' : ''}" onclick={toggleTokenForm}><Icon name="plus" size={13} /> {t('settings.tokenCreate')}</button></div>
    {#if tokenForm}
      <form class="grid grid-cols-2 gap-2 items-end mb-3 rise" onsubmit={createToken}>
        <div><label class="label" for="tn">{t('settings.tokenName')}</label><input id="tn" class="input" bind:value={tokenForm.name} maxlength="64" required /></div>
        <div><label class="label" for="tx">{t('settings.tokenExpiresIn')}</label><input id="tx" class="input" type="number" min="0" max="3650" bind:value={tokenForm.expires} /></div>
        <div class="col-span-2"><label class="label" for="ts">{t('settings.tokenScopes')}</label><input id="ts" class="input font-mono" bind:value={tokenForm.scopes} placeholder="billing, monitoring" /></div>
        <p class="col-span-2 text-xs text-muted m-0">{t('settings.tokenFormHint')}</p>
        <div class="col-span-2 flex justify-end gap-2"><button type="button" class="btn" onclick={() => (tokenForm = null)}>{t('common.cancel')}</button><button class="btn btn-primary">{t('common.create')}</button></div>
      </form>
    {/if}
    {#if newToken}<div class="text-xs font-mono break-all mb-2 p-2 code"><div class="text-muted">{t('settings.tokenNew', { name: newToken.record?.name ?? '' })}</div><div class="select-all">{newToken.token}</div><div class="text-muted">{t('settings.tokenShownOnce')}</div></div>{/if}
    <ul class="text-sm divide-y divide-line">
      {#each tokens as tk (tk.id)}
        <li class="flex justify-between items-start gap-2 py-1.5">
          <div class="min-w-0">
            <div class="flex flex-wrap items-center gap-1.5"><span class="font-medium break-all">{tk.name}</span><span class="text-xs text-muted font-mono">#{tk.id}</span>{#each tk.scopes ?? [] as sc}<span class="tag tag-muted">{sc}</span>{/each}{#if expired(tk)}<span class="tag tag-err">{t('settings.tokenExpiredTag')}</span>{/if}</div>
            <div class="text-xs text-muted">{t('settings.tokenCreated', { when: when(tk.created_at) })} · {tk.expires_at ? t(expired(tk) ? 'settings.tokenExpired' : 'settings.tokenExpires', { date: date(tk.expires_at) }) : t('settings.tokenNever')} · {tk.last_used_at ? t('settings.tokenUsed', { when: when(tk.last_used_at) }) : t('settings.tokenUnused')}</div>
          </div>
          <button class="btn btn-danger btn-sm shrink-0" onclick={() => (ask = askRevoke(tk))}>{t('settings.tokenRevoke')}</button>
        </li>
      {/each}
      {#if !tokens.length}<li class="py-2 text-xs text-muted">{t('settings.tokenEmpty')}</li>{/if}
    </ul>
    <p class="text-xs text-muted mt-2 font-mono">mp --server https://{location.host} --token … status</p>
  </div>
  {#if admin}
    {#if pcfg}
      <div class="card md:col-span-2 rise" style="--i:4">
        <div class="flex justify-between items-start gap-3 mb-3 flex-wrap">
          <div class="min-w-0">
            <div class="font-medium">{t('settings.panelTitle')}</div>
            <p class="text-xs text-muted">{t('settings.panelHint')} <span class="font-mono">{pcfg.path}</span></p>
          </div>
          {#if pcfg.restart_pending && !restarting}<button class="btn btn-sm" onclick={() => (ask = askPanelRestart(true))}><Icon name="refresh" size={13} /> {t('settings.panelRestartNow')}</button>{/if}
        </div>
        {#if restarting}
          <div class="p-3 rounded-lg border border-accent bg-accent-soft text-sm mb-3 space-y-1">
            <div class="text-accent-ink flex items-center gap-2"><span class="dot dot-live"></span> {t('settings.panelRestarting')}</div>
            {#if restarting.url && (restarting.sure || restarting.late)}<div>{restarting.sure ? t('settings.panelNewAddress') : t('settings.panelFileAddress')} <a class="font-mono underline break-all" href={restarting.url}>{restarting.url}</a></div>{/if}
            {#if restarting.late}<div class="text-warn">{t('settings.panelLate')} <code class="font-mono break-all">{restarting.undo || 'mp config set <key> <value> --local --restart'}</code></div>{/if}
          </div>
        {:else if pcfg.restart_pending}
          <div class="p-3 rounded-lg border border-warn/40 bg-warn-soft text-sm mb-3 space-y-1">
            <div>{t('settings.panelPending')}</div>
            {#if pendingMove?.sure}
              <div class="font-medium">{t('settings.panelPendingMove')} <a class="font-mono underline break-all" href={pendingMove.url}>{pendingMove.url}</a></div>
              <div class="text-xs">{t('settings.panelRunningNow', { hostname: live?.hostname || '—', listen: live?.listen || '—' })}</div>
              <div class="text-xs">{t('settings.panelMoveHint')} <code class="font-mono break-all">{pendingMove.undo}</code></div>
            {/if}
          </div>
        {/if}
        <!-- Шаблоны проверяют только изменённые поля: значение, которое уже в файле (имя хоста от mp setup), API заново не судит, и форма из-за него не должна вставать. -->
        <form class="grid sm:grid-cols-2 lg:grid-cols-3 gap-3 items-start" onsubmit={savePanel}>
          <div><label class="label" for="pch">{t('settings.panelHostname')}</label><input id="pch" class="input" bind:value={pform.hostname} placeholder="panel.example.com" maxlength="253" pattern={'hostname' in panelBody ? '[A-Za-z0-9]([A-Za-z0-9.\\-]*[A-Za-z0-9])?\\.?' : undefined} /><p class="text-[11px] text-muted mt-1"><span class="font-mono">web.hostname</span> — {t('settings.panelHostnameHint')}</p></div>
          <div><label class="label" for="pcl">{t('settings.panelListen')}</label><input id="pcl" class="input" bind:value={pform.listen} placeholder=":8443" maxlength="64" pattern={'listen' in panelBody ? '(\\[[0-9A-Fa-f:.]+\\]|[A-Za-z0-9.\\-]*):[0-9]{1,5}' : undefined} required /><p class="text-[11px] text-muted mt-1"><span class="font-mono">web.listen</span> — {t('settings.panelListenHint')}</p></div>
          <div><label class="label" for="pcw">{t('settings.panelWorkers')}</label><input id="pcw" class="input" type="number" min="1" max="64" bind:value={pform.jobs_workers} required /><p class="text-[11px] text-muted mt-1"><span class="font-mono">jobs.workers</span> — {t('settings.panelWorkersHint')}</p></div>
          <div><label class="label" for="pcv">{t('settings.panelLogLevel')}</label><select id="pcv" class="input" bind:value={pform.log_level}>{#each ['debug', 'info', 'warn', 'error'] as v}<option value={v}>{v}</option>{/each}</select><p class="text-[11px] text-muted mt-1 font-mono">log.level</p></div>
          <div><label class="label" for="pcf">{t('settings.panelLogFormat')}</label><select id="pcf" class="input" bind:value={pform.log_format}><option value="text">text</option><option value="json">json</option></select><p class="text-[11px] text-muted mt-1 font-mono">log.format</p></div>
          <div class="flex flex-col gap-2 sm:pt-5">
            <label class="flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={pform.restart} /> {t('settings.panelRestart')}</label>
            <button class="btn btn-primary self-start" disabled={!!restarting || (!panelDirty && !pform.restart)}>{pform.restart ? t('settings.panelSaveRestart') : t('common.save')}</button>
          </div>
          {#if formMoves}<div class="sm:col-span-2 lg:col-span-3 p-3 rounded-lg border border-warn/40 bg-warn-soft text-sm"><div class="font-medium">{t('settings.panelMoveTitle', { url: panelMove.url })}</div><div class="text-xs mt-1">{t('settings.panelMoveHint')} <code class="font-mono break-all">{panelMove.undo}</code></div></div>{/if}
        </form>
        <p class="text-xs text-muted mt-3">{t('settings.panelDataDir')} <span class="font-mono">{pcfg.data_dir}</span> · {t('settings.panelTemplatesDir')} <span class="font-mono">{pcfg.templates_dir}</span> · {pcfg.key_pinned ? t('settings.updateKeyOn') : t('settings.updateKeyOff')} (<code class="font-mono">mp update trust</code>)</p>
      </div>
    {/if}
    <div class="card md:col-span-2 rise" style="--i:5">
      <div class="flex justify-between items-start gap-3 mb-2 flex-wrap">
        <div class="min-w-0">
          <div class="font-medium">{t('settings.updateCard')}</div>
          <p class="text-xs text-muted">{updLine}</p>
        </div>
        <div class="flex gap-2">
          <button class="btn btn-sm" onclick={checkUpdate} disabled={!!updating || !upd?.settings?.repo}><Icon name="refresh" size={13} /> {t('settings.updateCheck')}</button>
          {#if upd?.available}<button class="btn btn-primary btn-sm" onclick={() => (ask = askUpdate(''))} disabled={!!updating}>{t('settings.updateTo', { version: upd.latest })}</button>{/if}
        </div>
      </div>
      {#if updating}<p class="text-sm text-accent-ink mb-3">{updating}</p>{/if}
      {#if updJob}<div class="mb-3"><JobLog jobId={updJob} onfinish={(s) => { if (s === 'failed') updating = ''; }} /></div>{/if}
      {#if upd && !upd.settings?.repo}<p class="text-sm text-muted mb-3">{t('settings.updateRepoNone')}</p>{/if}
      {#if upd?.latest}
        <div class="p-3 rounded-lg border mb-3 {upd.available ? 'border-accent bg-accent-soft' : 'border-line'}">
          <div class="text-sm font-medium {upd.available ? 'text-accent-ink' : ''}">{upd.available ? t('settings.updateAvailableHead', { version: upd.latest }) : t('settings.updateLatestHead', { version: upd.latest })}{#if upd.tag && upd.tag !== upd.latest && upd.tag !== 'v' + upd.latest}{' '}({upd.tag}){/if}{#if upd.published_at}{' '}{t('settings.updatePublishedAt', { date: date(upd.published_at) })}{/if}</div>
          {#if upd.available && upd.notes}<pre class="text-xs whitespace-pre-wrap mt-1 max-h-40 overflow-y-auto">{upd.notes}</pre>{/if}
        </div>
      {/if}
      {#if upd?.last_error}<p class="text-sm text-danger mb-3">{upd.last_error}</p>{/if}
      {#if upd?.last_attempt}
        {@const a = upd.last_attempt}
        <div class="text-sm mb-3 {a.status === 'done' ? 'text-muted' : 'text-danger'}">
          <span class="tag {attemptTag(a.status)}">{a.status}</span> {[t('settings.updateLastAttempt', { from: a.from ?? '', to: a.to ?? '' }), a.finished || a.started ? when(a.finished ?? a.started) : ''].filter(Boolean).join(' · ')}{a.error ? ': ' + a.error : ''}
          {#if a.log}<details class="mt-1"><summary class="text-xs text-muted cursor-pointer">{t('settings.updateAttemptLog')}</summary><pre class="code mt-1 max-h-48">{a.log}</pre></details>{/if}
        </div>
      {/if}
      <form class="grid sm:grid-cols-2 lg:grid-cols-3 gap-3 items-start" onsubmit={saveUpdate}>
        <div><label class="label" for="ur">{t('settings.updateRepo')}</label><input id="ur" class="input font-mono" bind:value={updForm.repo} placeholder="owner/name" maxlength="140" /><p class="text-[11px] text-muted mt-1">{upd?.settings?.repo_builtin && updForm.repo === upd.settings.repo ? t('settings.updateRepoBuiltIn') : t('settings.updateRepoHint')}</p></div>
        <div><label class="label" for="ua">{t('settings.updateApi')}</label><input id="ua" class="input font-mono" bind:value={updForm.api} placeholder="https://api.github.com" maxlength="200" /><p class="text-[11px] text-muted mt-1">{t('settings.updateApiHint')}</p></div>
        <div><label class="label" for="uc">{t('settings.updateChannel')}</label><select id="uc" class="input" bind:value={updForm.channel}><option value="stable">stable</option><option value="beta">{t('settings.updateChannelBeta')}</option></select></div>
        <div><label class="label" for="ut">{t('settings.updateToken')}</label><div class="flex gap-1"><input id="ut" class="input" type="password" autocomplete="new-password" bind:value={updForm.token} maxlength="512" placeholder={upd?.settings?.has_token ? t('settings.updateTokenSaved') : t('settings.updateTokenHint')} />{#if upd?.settings?.has_token}<button type="button" class="btn btn-ghost btn-sm text-danger shrink-0" onclick={() => (ask = askForgetToken())}>{t('settings.updateTokenForget')}</button>{/if}</div></div>
        <div><label class="label" for="uh">{t('settings.updateCheckHours')}</label><input id="uh" class="input" type="number" min="0" max="720" bind:value={updForm.check_hours} /><p class="text-[11px] text-muted mt-1">{t('settings.updateCheckHoursHint')}</p></div>
        <div class="flex flex-col gap-2 sm:pt-5"><label class="flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={updForm.auto_apply} /> {t('settings.updateAutoApply')}</label><button class="btn btn-primary self-start">{t('common.save')}</button></div>
      </form>
      <form class="flex flex-wrap gap-2 items-end mt-4 pt-4 border-t border-line" onsubmit={submitVersion}>
        <div class="flex-1 min-w-48"><label class="label" for="uv">{t('settings.updateVersionLabel')}</label><input id="uv" class="input font-mono" bind:value={updVersion} placeholder="0.8.19" maxlength="64" /></div>
        <button class="btn" disabled={!!updating || !upd?.settings?.repo || !updVersion.trim()}><Icon name="download" size={13} /> {t('settings.updateInstall')}</button>
        <p class="basis-full text-xs text-muted m-0">{t('settings.updateVersionHint')} {t('settings.updateKeyHint')} <code class="font-mono">mp update trust</code></p>
      </form>
    </div>
    <div class="card md:col-span-2 rise" style="--i:6">
      <div class="font-medium mb-1">{t('settings.hooksTitle')}</div>
      <p class="text-xs text-muted mb-3">{t('settings.hooksHint')}</p>
      <form class="grid md:grid-cols-[2fr_1fr_1fr_auto] gap-2 items-end mb-3" onsubmit={addHook}>
        <div><label class="label" for="hu">{t('settings.hookUrl')}</label><input id="hu" class="input" type="url" bind:value={hook.url} placeholder="https://billing.example.com/monopanel" required /></div>
        <div><label class="label" for="he">{t('settings.hookEvents')}</label><input id="he" class="input font-mono" bind:value={hook.events} placeholder="*" /></div>
        <div><label class="label" for="hs">{t('settings.hookSecretLabel')}</label><input id="hs" class="input" type="password" autocomplete="new-password" bind:value={hook.secret} maxlength="128" placeholder={t('settings.hookSecretPh')} /></div>
        <button class="btn btn-primary">{t('common.add')}</button>
        <p class="md:col-span-4 text-xs text-muted m-0">{t('settings.hookEventsHint')}</p>
      </form>
      {#if newHook}<div class="text-xs font-mono break-all mb-3 p-2 code"><div class="text-muted">{t('settings.hookCreated', { id: newHook.id, url: newHook.url, events: (newHook.events ?? []).join(',') })}</div><div class="select-all">{newHook.secret}</div><div class="text-muted">{t('settings.tokenShownOnce')}</div></div>{/if}
      <div class="overflow-x-auto">
        <table class="tbl"><thead><tr><th>ID</th><th>{t('settings.hookUrl')}</th><th>{t('settings.hookEvents')}</th><th>{t('settings.hookColCreated')}</th><th></th></tr></thead><tbody>
          {#each hooks as h (h.id)}<tr><td data-label="ID" class="font-mono text-xs whitespace-nowrap">{h.id}</td><td data-label={t('settings.hookUrl')} class="font-mono text-xs break-all"><div>{h.url}{#if tested[h.id]}{@const r = tested[h.id]}<div class="font-sans {r.err ? 'text-danger' : 'text-ok'}">{r.err ?? t('settings.hookDelivered', { when: when(r.at) })}</div>{/if}</div></td><td data-label={t('settings.hookEvents')} class="font-mono text-xs">{(h.events ?? []).join(', ')}</td><td data-label={t('settings.hookColCreated')} class="text-xs text-muted whitespace-nowrap">{when(h.created_at)}</td><td data-label=""><div class="row-actions"><button class="btn btn-sm" disabled={!!testing} onclick={() => testHook(h)}><Icon name="send" size={13} /> {testing === h.id ? t('settings.hookTesting') : t('settings.hookTest')}</button><button class="btn btn-danger btn-sm" onclick={() => (ask = askHook(h))}>{t('settings.hookDelete')}</button></div></td></tr>{/each}
          {#if !hooks.length}<Empty text={t('settings.hookEmpty')} cols={5} />{/if}
        </tbody></table>
      </div>
    </div>
    <div class="card rise" style="--i:7">
      <div class="font-medium mb-1">{t('settings.realIpTitle')}</div>
      <p class="text-xs text-muted mb-3">{t('settings.realIpHint')}</p>
      {#if realip}
        <label class="flex items-center gap-2 text-sm mb-2"><input type="checkbox" bind:checked={realip.cloudflare} /> {t('settings.realIpCloudflare')}</label>
        <label class="label" for="rf">{t('settings.realIpFrom')}</label>
        <div class="flex flex-wrap gap-2"><input id="rf" class="input font-mono min-w-0 flex-1" bind:value={realipFrom} placeholder="10.0.0.5, 192.168.1.0/24" /><button class="btn btn-primary" onclick={saveRealIP}>{t('common.apply')}</button></div>
      {/if}
    </div>
    {#if sel?.supported}
      <div class="card rise" style="--i:8">
        <div class="flex justify-between items-start gap-3 flex-wrap">
          <div class="min-w-0">
            <div class="font-medium">SELinux</div>
            <p class="text-xs text-muted">{[t('settings.selinuxMode', { mode: sel.mode }), sel.configured && sel.configured !== sel.mode ? t('settings.selinuxAfterReboot', { mode: sel.configured }) : ''].filter(Boolean).join(' · ')}. {t('settings.selinuxHint')}</p>
          </div>
          {#if sel.mode === 'enforcing'}
            <button class="btn btn-danger btn-sm" onclick={() => (ask = askPermissive())}>{t('settings.selinuxToPermissive')}</button>
          {:else}
            <button class="btn btn-primary btn-sm" onclick={() => setSELinux('enforcing')}>{t('settings.selinuxToEnforcing')}</button>
          {/if}
        </div>
        {#if sel.mode !== 'enforcing' || (sel.configured && sel.configured !== 'enforcing')}<div class="p-3 rounded-lg border border-danger bg-danger-soft text-sm mt-3">{sel.warning}</div>{/if}
      </div>
    {/if}
    {#if tpl}
      <div class="card md:col-span-2 p-0 overflow-hidden rise" style="--i:9">
        <div class="px-4 py-3 border-b border-line flex flex-wrap justify-between items-start gap-3">
          <div class="min-w-0">
            <div class="font-medium">{t('settings.tplTitle')}</div>
            <p class="text-xs text-muted mt-1">{t('settings.tplHint')} <span class="font-mono">{tpl.dir}</span></p>
            <p class="text-xs text-muted">{t('settings.tplCount', { n: (tpl.templates ?? []).length, m: tplOverrides.length })}</p>
          </div>
          <button class="btn btn-sm" onclick={() => (tplAll = !tplAll)}>{tplAll ? t('settings.tplOnlyOverrides') : t('settings.tplShowAll')}</button>
        </div>
        <div class="overflow-x-auto">
          <table class="tbl"><thead><tr><th>{t('settings.tplColName')}</th><th>{t('settings.tplColSource')}</th><th>{t('settings.tplColPath')}</th></tr></thead><tbody>
            {#each tplShown as x (x.name)}<tr><td data-label={t('settings.tplColName')} class="font-mono text-xs">{x.name}</td><td data-label={t('settings.tplColSource')}><span class="tag {x.override ? 'tag-warn' : 'tag-muted'}">{x.override ? t('settings.tplOverride') : t('settings.tplBuiltin')}</span></td><td data-label={t('settings.tplColPath')} class="font-mono text-xs break-all">{x.path || '—'}</td></tr>{/each}
            {#if !tplShown.length}<Empty text={t('settings.tplNoOverrides')} cols={3} />{/if}
          </tbody></table>
        </div>
      </div>
    {/if}
  {/if}
</div>

<Confirm bind:ask />
<Modal open={totpOff} title={t('settings.totpOffTitle')} onclose={() => { totpOff = false; totpPw = ''; }}>
  <p class="text-muted">{t('settings.totpOffNote')}</p>
  <form id="totp-off" onsubmit={disableTotp}><label class="label" for="tpw">{t('settings.totpPassword')}</label><input id="tpw" class="input" type="password" autocomplete="current-password" bind:value={totpPw} required /></form>
  {#snippet footer()}
    <button class="btn" onclick={() => { totpOff = false; totpPw = ''; }}>{t('common.cancel')}</button>
    <button class="btn btn-danger" form="totp-off" disabled={!totpPw}>{t('settings.totpTurnOff')}</button>
  {/snippet}
</Modal>
