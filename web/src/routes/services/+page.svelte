<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { afterNavigate, replaceState } from '$app/navigation';
  import { api, ApiError, when } from '$lib/api';
  import { auth, notify } from '$lib/state.svelte';
  import { t, dateLocale, type MsgKey } from '$lib/i18n/index.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import LogView from '$lib/components/LogView.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';

  // systemd-юниты: те, за которыми следит панель (GET /services), и любой
  // другой юнит сервера по имени (GET /services/{unit}). Действия и журнал у
  // них одни и те же; что можно, решает API — страница только спрашивает
  // перед тем, что останавливает работу, и переживает перезапуск самой панели.
  // ?unit=<имя> открывает карточку юнита сразу (ссылки с дашборда).
  type Unit = { unit: string; load_state: string; active_state: string; sub_state: string; unit_file_state: string; active_since?: string | null };
  type Action = 'start' | 'stop' | 'restart' | 'reload' | 'reload-or-restart' | 'enable' | 'disable';

  const API_UNIT = 'monopanel-api.service';
  const AGENT_UNIT = 'monopanel-agent.service';
  const nowActions: Action[] = ['start', 'stop', 'restart', 'reload', 'reload-or-restart'];
  const bootActions: Action[] = ['enable', 'disable'];
  const btn: Record<Action, MsgKey> = { start: 'services.btnStart', stop: 'services.btnStop', restart: 'services.btnRestart', reload: 'services.btnReload', 'reload-or-restart': 'services.btnReloadOrRestart', enable: 'services.btnEnable', disable: 'services.btnDisable' };
  const hint: Record<Action, MsgKey> = { start: 'services.hintStart', stop: 'services.hintStop', restart: 'services.hintRestart', reload: 'services.hintReload', 'reload-or-restart': 'services.hintReloadOrRestart', enable: 'services.hintEnable', disable: 'services.hintDisable' };
  const doing: Record<Action, MsgKey> = { start: 'services.doing.start', stop: 'services.doing.stop', restart: 'services.doing.restart', reload: 'services.doing.reload', 'reload-or-restart': 'services.doing.reload-or-restart', enable: 'services.doing.enable', disable: 'services.doing.disable' };
  const lsHint: Record<string, MsgKey> = { 'not-found': 'services.lsNotFound', masked: 'services.lsMasked', error: 'services.lsError', 'bad-setting': 'services.lsError', unknown: 'services.lsUnknown' };
  const ufHint: Record<string, MsgKey> = { enabled: 'services.ufEnabled', 'enabled-runtime': 'services.ufEnabled', disabled: 'services.ufDisabled', static: 'services.ufStatic', masked: 'services.ufMasked', 'masked-runtime': 'services.ufMasked', indirect: 'services.ufIndirect', generated: 'services.ufGenerated', transient: 'services.ufTransient', alias: 'services.ufAlias' };
  const moving = ['activating', 'deactivating', 'reloading', 'refreshing'];
  // Suggestions for the name field: units the panel installs that are not in the watched list.
  const known = ['monopanel-api', 'monopanel-firewall', 'fail2ban', 'postfix', 'dovecot', 'opendkim', 'memcached'];
  // The API's own pattern; "." and ".." alone would make the browser resolve the URL to another path.
  const validUnit = (s: string) => /^[A-Za-z0-9_.@:-]+$/.test(s) && s !== '.' && s !== '..';

  const admin = $derived(auth.me?.role === 'admin');
  let units = $state<Unit[] | null>(null);
  let error = $state('');
  let ask = $state<Ask | null>(null);
  let busy = $state<Record<string, Action>>({});
  let selName = $state('');
  let extra = $state<Unit | null>(null); // the open unit when it is not in the watched list
  let selError = $state('');
  let other = $state('');
  let lines = $state(200);
  let log = $state<ReturnType<typeof LogView>>();
  let card = $state<HTMLElement>();
  // Our own units take this very process down: the page waits for the panel to answer again.
  let waiting = $state<{ unit: string; action: Action; late: boolean } | null>(null);
  let gen = 0; // bumps on every finished action: a list read before it is older than the action's answer
  let mounted = true;
  const sel = $derived(selName ? (units?.find((u) => u.unit === selName) ?? (extra?.unit === selName ? extra : null)) : null);
  const agentDown = $derived(!!units?.length && units.every((u) => u.load_state === 'unknown'));
  const errText = (e: unknown) => (e instanceof ApiError ? e.text : String(e));

  const self = (unit: string) => unit === API_UNIT || unit === AGENT_UNIT;
  const running = (u: Unit) => ['active', 'activating', 'reloading', 'refreshing'].includes(u.active_state);
  // A unit that has not been active since boot carries Go's zero time.
  const sinceOf = (u: Unit) => (u.active_since && !u.active_since.startsWith('0001-') ? u.active_since : '');
  const stateTag = (s: string) => (s === 'active' ? 'tag-ok' : s === 'failed' ? 'tag-err' : s === 'unknown' ? 'tag-warn' : moving.includes(s) ? 'tag-accent' : 'tag-muted');
  const ufTag = (s: string) => (s === 'enabled' || s === 'enabled-runtime' ? 'tag-ok' : s.startsWith('masked') || s === 'bad' ? 'tag-err' : 'tag-muted');
  const loadTag = (s: string) => (s === 'loaded' || s === 'not-found' ? 'tag-muted' : s === 'unknown' ? 'tag-warn' : 'tag-err');
  function full(s: string): string {
    const d = new Date(s);
    return isNaN(d.getTime()) ? s : d.toLocaleString(dateLocale());
  }
  // How long a unit has been up; the page re-reads states every 15 s, so minutes are precise enough.
  function activeFor(s: string): string {
    const at = new Date(s).getTime();
    if (isNaN(at)) return '';
    const sec = Math.max(0, Math.floor((Date.now() - at) / 1000));
    const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
    if (sec < 60) return t('services.upMoment');
    return t('services.activeFor', { dur: d ? t('services.durDays', { d, h }) : h ? t('services.durHours', { h, m }) : t('services.durMinutes', { m }) });
  }
  // The table cell shows a short date; the tooltip has the full time and how long it has been up.
  function sinceTitle(u: Unit): string | undefined {
    const s = sinceOf(u);
    if (!s) return undefined;
    const more = running(u) ? activeFor(s) : t('services.sinceNotActive');
    return more ? full(s) + ' · ' + more : full(s);
  }
  const base = (unit: string) => unit.replace(/\.service$/, '');
  // php8.4-fpm (Sury), php84-php-fpm (Remi), monopanel-php-fpm@8.4 (the panel's own builds), plain php-fpm.
  const isPhpFpm = (u: string) => /^(php[\d.]*-fpm|php\d+-php-fpm|monopanel-php-fpm@[\d.]+)$/.test(u);
  const phpOf = (u: string) => { const m = u.match(/(\d)\.?(\d{1,2})/); return m ? `PHP ${m[1]}.${m[2]}` : ''; };
  const isDb = (u: string) => u === 'mysql' || u === 'mysqld' || u === 'mariadb';
  function role(unit: string): string {
    const u = base(unit);
    if (unit === AGENT_UNIT) return t('services.roleAgent');
    if (unit === API_UNIT) return t('services.roleApi');
    if (u === 'monopanel-firewall') return t('services.roleFirewall');
    if (u.startsWith('monopanel-app-')) return t('services.roleApp');
    if (u.startsWith('monopanel-valkey-')) return t('services.roleValkey');
    if (u === 'nginx') return t('services.roleNginx');
    if (u.startsWith('monopanel-apache@')) return t('services.roleApacheAccount', { login: u.slice('monopanel-apache@'.length) });
    if (u === 'apache2' || u === 'httpd') return t('services.roleApache');
    if (isDb(u)) return t('services.roleDb');
    if (isPhpFpm(u)) return t('services.rolePhp', { php: phpOf(u) || 'PHP' });
    if (u === 'fail2ban') return t('services.roleFail2ban');
    if (u === 'postfix') return t('services.rolePostfix');
    if (u === 'dovecot') return t('services.roleDovecot');
    if (u === 'opendkim') return t('services.roleOpendkim');
    if (u === 'memcached') return t('services.roleMemcached');
    if (u === 'ssh' || u === 'sshd') return t('services.roleSsh');
    return '';
  }
  function stopNote(unit: string): string {
    const u = base(unit);
    if (unit === API_UNIT) return t('services.askStopApiNote');
    if (unit === AGENT_UNIT) return t('services.askStopAgentNote');
    if (u === 'monopanel-firewall') return t('services.askStopFirewallNote');
    if (u === 'nginx') return t('services.askStopNginxNote');
    if (u.startsWith('monopanel-apache@')) return t('services.askStopApacheAccountNote', { login: u.slice('monopanel-apache@'.length) });
    if (u === 'apache2' || u === 'httpd') return t('services.askStopApacheNote');
    if (isDb(u)) return t('services.askStopDbNote');
    if (isPhpFpm(u)) return t('services.askStopPhpNote', { php: phpOf(u) || t('services.thisPhp') });
    return t('services.askStopNote');
  }

  async function load() {
    const g = gen;
    try {
      const list = await api<Unit[]>('/services');
      if (g === gen) units = list;
      error = '';
    } catch (e) {
      error = errText(e);
    }
  }
  // Only the watched units and the open one are kept: an answer for a unit
  // the person has already closed must not replace the card's unit.
  function put(st: Unit) {
    const i = units ? units.findIndex((u) => u.unit === st.unit) : -1;
    if (units && i >= 0) units[i] = st;
    else if (st.unit === selName) extra = st;
  }
  // quiet: the 15 s poll. Its failure is already on top of the page (the list
  // fails the same way) and must not stay under the card once the panel answers again.
  async function refreshUnit(name: string, quiet = false) {
    try {
      put(await api<Unit>(`/services/${name}`));
    } catch (e) {
      if (!quiet && name === selName) selError = errText(e);
    }
  }
  async function refreshAll() {
    selError = '';
    await load();
    if (selName && !units?.some((u) => u.unit === selName)) await refreshUnit(selName);
    log?.reload();
  }
  async function refreshSel() {
    if (!selName) return;
    selError = '';
    await refreshUnit(selName);
    log?.reload();
  }

  function syncURL(unit: string) {
    const url = new URL(location.href);
    if ((url.searchParams.get('unit') ?? '') === unit) return;
    if (unit) url.searchParams.set('unit', unit);
    else url.searchParams.delete('unit');
    try {
      replaceState(url, {});
    } catch {
      /* the router is not ready yet: the address stays as it is */
    }
  }
  async function open(name: string) {
    selName = name;
    selError = '';
    syncURL(name);
    await tick();
    card?.scrollIntoView({ behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth', block: 'start' });
  }
  function close() {
    selName = '';
    extra = null;
    selError = '';
    syncURL('');
  }
  // GET /services/{unit} answers with the canonical name (".service" added).
  async function show(name: string): Promise<boolean> {
    try {
      const st = await api<Unit>(`/services/${name}`);
      selName = st.unit;
      put(st);
      await open(st.unit);
      return true;
    } catch (e) {
      notify(errText(e), 'err');
      return false;
    }
  }
  async function check(e: Event) {
    e.preventDefault();
    const name = other.trim();
    if (!validUnit(name)) {
      notify(t('services.badUnit'), 'err');
      return;
    }
    if (await show(name)) other = '';
  }

  async function answers(): Promise<boolean> {
    try {
      await api('/health');
      return true;
    } catch {
      return false;
    }
  }
  // needDown: the API answered before acting (it restarts itself half a
  // second after the answer), so an answer counts only once the old process
  // has been seen gone — or after 10 s, when the restart beat the probes.
  async function waitForPanel(needDown = false) {
    const started = Date.now();
    let down = !needDown;
    while (mounted && waiting) {
      const pause = !down ? 1000 : waiting.action === 'stop' || waiting.late ? 5000 : 2000;
      await new Promise((r) => setTimeout(r, pause));
      if (!mounted || !waiting) return;
      if (!(await answers())) {
        down = true;
        if (Date.now() - started > 180000) waiting.late = true;
        continue;
      }
      if (!down && Date.now() - started < 10000) continue;
      waiting = null;
      notify(t('services.panelBack'));
      await refreshAll();
    }
  }

  async function act(name: string, action: Action) {
    if (busy[name] || waiting) return;
    // Stopping or restarting monopanel-api (or the agent it requires) stops
    // the process that serves this very request: the API answers first and
    // acts half a second later (an older one broke off instead of answering).
    const drops = self(name) && (action === 'stop' || action === 'restart' || action === 'reload-or-restart');
    busy[name] = action;
    if (name === selName) selError = '';
    if (drops) waiting = { unit: name, action, late: false };
    try {
      const st = await api<Unit>(`/services/${name}`, { method: 'POST', json: { action } });
      gen++;
      if (drops) {
        // The answer is the state before the action, which is about to happen.
        delete busy[name];
        await waitForPanel(true);
        return;
      }
      put(st);
      notify(action === 'enable' || action === 'disable' ? t('services.resultBoot', { unit: st.unit, state: st.unit_file_state || '—' }) : t('services.resultState', { unit: st.unit, active: st.active_state, sub: st.sub_state }));
      if (name === selName) log?.reload();
    } catch (e) {
      // For our own units a broken-off request is the expected outcome (the
      // restarted panel may even answer already); an API error is a refusal,
      // unless the panel went away right after sending it.
      if (drops && (!(e instanceof ApiError) || !(await answers()))) {
        delete busy[name];
        await waitForPanel();
        return;
      }
      if (drops) waiting = null;
      const msg = errText(e);
      notify(msg, 'err');
      // Why it failed is in the unit's journal: show the card with the fresh state next to the error.
      await refreshUnit(name);
      if (name !== selName) await open(name);
      else log?.reload();
      selError = msg;
    } finally {
      delete busy[name];
    }
  }
  // Asked first: whatever stops work, and a restart of the panel itself (the page loses its connection).
  function askFor(unit: string, action: Action): Ask | null {
    // The dialog closes at once; the row and the card show the progress.
    const run = () => {
      void act(unit, action);
    };
    if (action === 'stop') return { title: t('services.askStopTitle', { unit }), note: stopNote(unit), danger: true, action: t('services.stopAction'), run };
    if (action === 'disable') return { title: t('services.askDisableTitle', { unit }), note: t(self(unit) ? 'services.askDisableSelfNote' : 'services.askDisableNote'), danger: true, action: t('services.disableAction'), run };
    if (self(unit) && (action === 'restart' || action === 'reload-or-restart'))
      return { title: t('services.askRestartTitle', { unit }), note: t(unit === AGENT_UNIT ? 'services.askRestartAgentNote' : 'services.askRestartApiNote'), action: t('services.restartAction'), run };
    return null;
  }
  function request(unit: string, action: Action) {
    const a = askFor(unit, action);
    if (a) ask = a;
    else void act(unit, action);
  }

  onMount(() => {
    if (!admin) return;
    const q = new URL(location.href).searchParams.get('unit');
    // The card sits under the list: open it once the list is drawn, or the
    // list pushes the card that was just scrolled to out of view (a phone screen or more).
    load().then(() => {
      if (mounted && q && validUnit(q)) show(q);
    });
    const timer = setInterval(() => {
      if (waiting || document.visibilityState !== 'visible') return;
      load();
      if (selName && !units?.some((u) => u.unit === selName)) refreshUnit(selName, true);
    }, 15000);
    return () => {
      mounted = false;
      clearInterval(timer);
    };
  });
  // A later navigation to this same page (back and forward, the menu's link
  // without ?unit) keeps the component: the card follows the address. The
  // first arrival is handled above, once the list is drawn.
  afterNavigate(({ from, to }) => {
    if (!admin || !from || !to || from.url.pathname !== to.url.pathname) return;
    const q = to.url.searchParams.get('unit') ?? '';
    if (!q) {
      if (selName) close();
    } else if (validUnit(q) && q !== selName) show(q);
  });
</script>

<PageHead title={t('services.title')} sub={t('services.sub')}>
  {#if admin}<button class="btn" onclick={refreshAll} disabled={!!waiting}><Icon name="refresh" size={14} /> {t('common.refresh')}</button>{/if}
</PageHead>

{#if !admin}
  <div class="card text-sm text-muted rise">{t('services.adminOnly')}</div>
{:else}
  <!-- While the panel restarts the banner says why requests fail; the raw network error is noise. -->
  {#if error && !waiting}<p class="text-danger text-sm mb-3 break-words">{error}</p>{/if}
  {#if waiting}
    <div class="card mb-4 flex items-start gap-3 text-sm rise" role="status">
      <span class="dot dot-live mt-1.5 shrink-0 {waiting.action === 'stop' || waiting.late ? 'text-danger' : 'text-accent'}"></span>
      <span class="break-words">{waiting.action === 'stop' ? t('services.waitStopped', { unit: waiting.unit }) : waiting.late ? t('services.waitLate') : t('services.waitRestart', { unit: waiting.unit })}</span>
    </div>
  {/if}
  {#if agentDown}<p class="text-warn text-sm mb-3 break-words">{t('services.agentDown')}</p>{/if}

  <form class="card flex flex-wrap items-end gap-3 mb-4 rise" onsubmit={check}>
    <div class="flex-1 min-w-[12rem]"><label class="label" for="svc-unit">{t('services.other')}</label><input id="svc-unit" class="input" list="svc-known" bind:value={other} placeholder="postfix, fail2ban, monopanel-api…" autocomplete="off" spellcheck="false" required /></div>
    <button class="btn btn-primary"><Icon name="eye" size={14} /> {t('services.otherShow')}</button>
    <p class="basis-full text-xs text-muted m-0">{t('services.otherHint')}</p>
    <datalist id="svc-known">{#each known as k}<option value={k}></option>{/each}</datalist>
  </form>

  <div class="card overflow-x-auto p-0 mb-4 rise" style="--i:1">
    <div class="px-4 pt-3"><span class="font-medium">{t('services.listTitle')}</span> <span class="text-xs text-muted">· {t('services.listHint')}</span></div>
    {#if units}
      <table class="tbl"><thead><tr><th>{t('services.colUnit')}</th><th>{t('services.colLoad')}</th><th>{t('services.colState')}</th><th>{t('services.colEnabled')}</th><th>{t('services.colSince')}</th><th></th></tr></thead>
        <tbody>
          {#each units as u, i (u.unit)}
            {@const r = role(u.unit)}
            {@const since = sinceOf(u)}
            {@const job = busy[u.unit]}
            <tr class="rise {selName === u.unit ? 'bg-accent-soft/40' : ''}" style="--i:{Math.min(i, 12)}">
              <td data-label={t('services.colUnit')}><div class="min-w-0"><button class="font-mono text-left break-words hover:text-accent-ink transition-colors" onclick={() => open(u.unit)} title={t('services.openTitle')}>{u.unit}</button>{#if r}<div class="text-xs text-muted">{r}</div>{/if}</div></td>
              <td data-label={t('services.colLoad')}>{#if u.load_state === 'loaded'}<span class="text-xs text-muted font-mono">{u.load_state}</span>{:else}<span class="tag {loadTag(u.load_state)}" title={lsHint[u.load_state] ? t(lsHint[u.load_state]) : undefined}>{u.load_state || '—'}</span>{/if}</td>
              <td data-label={t('services.colState')}><span class="tag {stateTag(u.active_state)}">{#if moving.includes(u.active_state)}<span class="dot dot-live"></span>{:else if u.active_state === 'active'}<span class="dot"></span>{/if}{u.active_state}{u.sub_state ? '/' + u.sub_state : ''}</span></td>
              <td data-label={t('services.colEnabled')}>{#if u.unit_file_state}<span class="tag {ufTag(u.unit_file_state)}" title={ufHint[u.unit_file_state] ? t(ufHint[u.unit_file_state]) : undefined}>{u.unit_file_state}</span>{:else}<span class="text-muted">—</span>{/if}</td>
              <td data-label={t('services.colSince')} class="text-xs whitespace-nowrap {running(u) ? '' : 'text-muted'}" title={sinceTitle(u)}>{since ? when(since) : '—'}</td>
              <td data-label=""><div class="row-actions">
                {#if job}<span class="tag tag-accent"><span class="dot dot-live"></span>{t(doing[job])}</span>
                {:else}
                  {#if running(u)}<button class="btn btn-sm" onclick={() => request(u.unit, 'stop')} disabled={!!waiting} title={t('services.btnStop')} aria-label="{t('services.btnStop')} {u.unit}"><Icon name="stop" size={13} /></button>
                  {:else}<button class="btn btn-sm" onclick={() => request(u.unit, 'start')} disabled={!!waiting} title={t('services.btnStart')} aria-label="{t('services.btnStart')} {u.unit}"><Icon name="play" size={13} /></button>{/if}
                  <button class="btn btn-sm" onclick={() => request(u.unit, 'restart')} disabled={!!waiting} title={t('services.btnRestart')} aria-label="{t('services.btnRestart')} {u.unit}"><Icon name="refresh" size={13} /></button>
                {/if}
                <button class="btn btn-sm" onclick={() => open(u.unit)} title={t('services.openTitle')} aria-label="{t('services.openTitle')}: {u.unit}"><Icon name="terminal" size={13} /></button>
              </div></td>
            </tr>
          {/each}
          {#if !units.length}<Empty text={t('services.empty')} cols={6} />{/if}
        </tbody></table>
    {:else if !error}<Skeleton rows={5} />{/if}
  </div>

  {#if sel}
    {@const since = sinceOf(sel)}
    {@const r = role(sel.unit)}
    {@const on = running(sel)}
    {@const upFor = on && since ? activeFor(since) : ''}
    {@const job = busy[sel.unit]}
    <div bind:this={card} class="card mb-4 rise scroll-mt-20 lg:scroll-mt-4">
      <div class="flex flex-wrap items-start justify-between gap-3 mb-3">
        <div class="min-w-0">
          <div class="font-mono font-medium break-words">{sel.unit}</div>
          {#if r}<div class="text-xs text-muted">{r}</div>{/if}
        </div>
        <div class="flex flex-wrap items-center gap-1">
          {#if job}<span class="tag tag-accent"><span class="dot dot-live"></span>{t(doing[job])}</span>{/if}
          <button class="btn btn-sm" onclick={refreshSel} title={t('common.refresh')} aria-label={t('common.refresh')}><Icon name="refresh" size={13} /></button>
          <button class="btn btn-sm" onclick={close}>{t('common.close')}</button>
        </div>
      </div>
      <dl class="grid grid-cols-2 md:grid-cols-4 gap-x-4 gap-y-3 text-sm mb-4">
        <div class="min-w-0"><dt class="label">{t('services.colLoad')}</dt><dd><span class="tag {loadTag(sel.load_state)}">{sel.load_state || '—'}</span>{#if lsHint[sel.load_state]}<div class="text-xs text-muted mt-1">{t(lsHint[sel.load_state])}</div>{/if}</dd></div>
        <div class="min-w-0"><dt class="label">{t('services.colState')}</dt><dd><span class="tag {stateTag(sel.active_state)}">{#if moving.includes(sel.active_state)}<span class="dot dot-live"></span>{:else if sel.active_state === 'active'}<span class="dot"></span>{/if}{sel.active_state}{sel.sub_state ? '/' + sel.sub_state : ''}</span></dd></div>
        <div class="min-w-0"><dt class="label">{t('services.colEnabled')}</dt><dd>{#if sel.unit_file_state}<span class="tag {ufTag(sel.unit_file_state)}">{sel.unit_file_state}</span>{#if ufHint[sel.unit_file_state]}<div class="text-xs text-muted mt-1">{t(ufHint[sel.unit_file_state])}</div>{/if}{:else}<span class="text-muted">—</span>{/if}</dd></div>
        <div class="min-w-0"><dt class="label">{on ? t('services.colSince') : t('services.lastActive')}</dt><dd class="text-xs font-mono">{since ? full(since) : '—'}{#if upFor}<div class="text-muted mt-1">{upFor}</div>{/if}</dd></div>
      </dl>
      {#if sel.load_state === 'not-found'}<p class="text-sm text-warn mb-3">{t('services.notFound')}</p>{/if}
      <div class="flex flex-col gap-2">
        <div class="flex flex-wrap items-center gap-1.5"><span class="label mb-0 w-full sm:w-28 shrink-0">{t('services.now')}</span>{#each nowActions as a}<button class="btn btn-sm {a === 'stop' ? 'btn-danger' : ''}" disabled={!!job || !!waiting} onclick={() => request(sel.unit, a)} title={t(hint[a])}>{t(btn[a])}</button>{/each}</div>
        <div class="flex flex-wrap items-center gap-1.5"><span class="label mb-0 w-full sm:w-28 shrink-0">{t('services.atBoot')}</span>{#each bootActions as a}<button class="btn btn-sm {a === 'disable' ? 'btn-danger' : ''}" disabled={!!job || !!waiting} onclick={() => request(sel.unit, a)} title={t(hint[a])}>{t(btn[a])}</button>{/each}</div>
      </div>
      <p class="text-xs text-muted mt-2">{t('services.actionsHint')}</p>
      {#if selError}<p class="text-danger text-sm mt-2 break-words">{selError}</p>{/if}
      <div class="divider"></div>
      <LogView bind:this={log} src={`/system/logs/${sel.unit}`} bind:lines title={t('services.journal')} />
    </div>
  {/if}
{/if}

<Confirm bind:ask />
