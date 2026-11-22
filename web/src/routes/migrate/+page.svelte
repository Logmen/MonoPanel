<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { api, ApiError, when } from '$lib/api';
  import { auth, notify } from '$lib/state.svelte';
  import { t, tn, type MsgKey } from '$lib/i18n/index.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import CopyField from './CopyField.svelte';
  import PlanView from './PlanView.svelte';
  import type { Grant, Panel, Plan } from './types';

  // A move has two sides (docs/07-migration.md). The old MonoPanel hands an
  // account over with a one-off token that reads that account and nothing
  // else; the new panel checks what would arrive (plan: changes nothing on
  // either side) and then pulls the account in with a job (run). FASTPANEL and
  // BitrixVM have no API: the new panel reads them over ssh as root.
  const admin = $derived(auth.me?.role === 'admin');
  let tab = $state<'take' | 'grant'>('take');
  let error = $state('');
  let ask = $state<Ask | null>(null);
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };

  /* ── hand an account over: POST /migrate/grant ───────────────────────── */
  let users = $state<any[] | null>(null);
  let grants = $state<any[]>([]);
  let gform = $state({ login: '', hours: 24 });
  let granted = $state<Grant | null>(null);
  let granting = $state(false);
  let grantsError = $state(''); // kept apart from `error`, which a tab switch clears
  const isMigrationScope = (s: string) => s.startsWith('migrate:');
  async function loadGrants() {
    // Each list on its own: a token list that fails to load must not leave
    // the account picker disabled.
    grantsError = '';
    const [u, tk] = await Promise.allSettled([api<any[]>('/users'), api<any[]>('/tokens')]);
    if (u.status === 'fulfilled') {
      users = u.value;
      if (!gform.login && users.length) gform.login = (users.find((x) => x.role === 'user') ?? users[0]).login;
    }
    // Grant tokens are ordinary API tokens of the administrator who issued
    // them, told apart by their scope: list them here so they can be revoked
    // as soon as the move is over instead of living out their hours.
    if (tk.status === 'fulfilled') grants = tk.value.filter((x) => (x.scopes ?? []).some(isMigrationScope));
    const failed = [u, tk].find((r): r is PromiseRejectedResult => r.status === 'rejected');
    if (failed) grantsError = failed.reason instanceof ApiError ? failed.reason.text : String(failed.reason);
  }
  async function grant(e: Event) {
    e.preventDefault(); error = '';
    granting = true;
    try {
      granted = await api<Grant>('/migrate/grant', { method: 'POST', json: { scope: 'user:' + gform.login, hours: gform.hours > 0 ? gform.hours : undefined } });
      await loadGrants();
    } catch (e) { fail(e); } finally { granting = false; }
  }
  // The command names the address the old panel answers on (web.hostname or
  // the request's host, with the port): the new panel's form needs the same.
  const grantSource = $derived(granted ? (/--source\s+(\S+)/.exec(granted.command)?.[1] ?? location.origin) : '');
  const grantAccount = (tk: any) => ((tk.scopes ?? []).find(isMigrationScope) ?? '').slice('migrate:'.length);
  const expired = (s: string | null | undefined) => !!s && new Date(s).getTime() < Date.now();
  const askRevoke = (tk: any): Ask => ({
    title: t('migrate.revokeTitle', { account: grantAccount(tk) || tk.name }),
    note: t('migrate.revokeNote'),
    danger: true, action: t('migrate.revoke'),
    run: async () => { try { await api(`/tokens/${tk.id}`, { method: 'DELETE' }); notify(t('migrate.revoked')); await loadGrants(); } catch (e) { fail(e); } }
  });

  /* ── take an account: POST /migrate/plan, POST /migrate/run ─────────── */
  const panels: [Panel, string, MsgKey][] = [['monopanel', 'MonoPanel', 'migrate.panelMono'], ['fastpanel', 'FASTPANEL', 'migrate.panelFast'], ['bitrixvm', 'BitrixVM', 'migrate.panelBitrix']];
  // The address and the account mean different things for another MonoPanel
  // (https://…, its login) and for FASTPANEL (root@host, a login there), so
  // each panel keeps its own and switching back and forth loses nothing.
  let src = $state({ panel: 'monopanel' as Panel, url: '', token: '', scope: '', insecure: false, ssh: '', password: '', key: '', fpScope: '', domain: '', as: '' });
  const foreign = $derived(src.panel !== 'monopanel');
  let showToken = $state(false);
  let keyName = $state('');
  let keyInput: HTMLInputElement | undefined = $state();
  let asInput: HTMLInputElement | undefined = $state();

  // A bare login is what people type; the API wants user:<login>.
  const scopeOf = (s: string) => { s = s.trim(); return !s || s.includes(':') ? s : 'user:' + s; };
  function request(): Record<string, unknown> {
    const r: Record<string, unknown> = { panel: src.panel };
    if (src.panel === 'monopanel') {
      r.source = src.url.trim();
      r.token = src.token.trim();
      r.scope = scopeOf(src.scope);
      if (src.insecure) r.insecure = true;
    } else {
      r.source = src.ssh.trim();
      if (src.password) r.password = src.password;
      if (src.key.trim()) r.key = src.key;
      if (src.panel === 'bitrixvm') {
        r.scope = 'user:bitrix';
        if (src.domain.trim()) r.domain = src.domain.trim().toLowerCase();
      } else if (src.fpScope.trim()) r.scope = scopeOf(src.fpScope);
    }
    if (src.as.trim()) r.as = src.as.trim();
    return r;
  }
  const reqKey = $derived(JSON.stringify(request()));

  // The plan is replaced whole and never edited: no need for a deep proxy
  // over what may be hundreds of mailboxes.
  let plan = $state.raw<Plan | null>(null);
  let planKey = $state(''); // the request the shown plan answers
  let planning = $state(false);
  let planError = $state('');
  let listed = $state<string[]>([]);
  const stale = $derived(!!plan && planKey !== reqKey);
  async function check(e?: Event) {
    e?.preventDefault();
    planError = ''; listed = []; runError = '';
    if (foreign && !src.password && !src.key.trim()) { planError = t('migrate.needSSH'); return; }
    const req = request();
    planning = true;
    try {
      plan = await api<Plan>('/migrate/plan', { method: 'POST', json: req });
      planKey = JSON.stringify(req);
      runKey = '';
    } catch (e) {
      plan = null; planKey = '';
      planError = e instanceof ApiError ? e.text : String(e);
      // FASTPANEL answers with the accounts it has when none is named
      // ("…; the source has: a, b") or the named one is not there ("…; it
      // has: a, b"): they become buttons. The first answer is worded for the
      // terminal (--scope), so the buttons replace it; the second one stays.
      const m = req.panel === 'fastpanel' ? /(?:the source|; it) has: (.+)$/.exec(planError) : null;
      if (m) {
        listed = m[1].split(',').map((s) => s.trim()).filter(Boolean);
        if (listed.length && !req.scope) planError = '';
      }
    } finally { planning = false; }
  }
  function pickAccount(login: string) { src.fpScope = 'user:' + login; check(); }
  async function focusLogin() { await tick(); asInput?.scrollIntoView({ block: 'center', behavior: 'smooth' }); asInput?.focus(); }

  // The old panel shows a ready «mp migrate plan --source … --token … --scope …»
  // line: pasted into the address or the token field, it fills the whole form.
  function fromCommand(line: string): boolean {
    if (!/(?:^|\s)--(?:source|token)[\s=]/.test(line)) return false;
    const flag = (name: string) => {
      const m = new RegExp(`(?:^|\\s)--${name}(?:=|\\s+)("[^"]*"|'[^']*'|\\S+)`).exec(line);
      return m ? m[1].replace(/^(["'])(.*)\1$/, '$2') : '';
    };
    const from = flag('from') || 'monopanel';
    if (from !== 'monopanel' && from !== 'fastpanel' && from !== 'bitrixvm') return false;
    src.panel = from;
    const source = flag('source');
    if (from === 'monopanel') {
      if (source) src.url = source;
      src.token = flag('token') || src.token;
      if (/(?:^|\s)--insecure(?:\s|$)/.test(line)) src.insecure = true;
    } else if (source) src.ssh = source;
    const scope = flag('scope');
    if (scope && from === 'monopanel') src.scope = scope;
    else if (scope && from === 'fastpanel') src.fpScope = scope;
    src.as = flag('as') || src.as;
    src.domain = flag('domain') || src.domain;
    return true;
  }
  function pasted(e: ClipboardEvent) {
    if (fromCommand((e.clipboardData?.getData('text') ?? '').trim())) { e.preventDefault(); notify(t('migrate.pasted')); }
  }
  async function loadKey(e: Event) {
    const input = e.currentTarget as HTMLInputElement;
    const f = input.files?.[0];
    input.value = '';
    if (!f) return;
    if (f.size > 65536) { notify(t('migrate.keyTooBig'), 'err'); return; }
    try { src.key = await f.text(); keyName = f.name; } catch (err) { notify(String(err), 'err'); }
  }

  let job = $state<number | null>(null);
  let runKey = $state(''); // the request the last job was started with, until the next check
  let runError = $state('');
  const runStarted = $derived(!!plan && !!runKey && runKey === planKey);
  const canRun = $derived(!!plan?.ok && !stale && !runStarted && !runError);
  let moved = $state<{ login: string; foreign: boolean; status: string; sites: any[] | null } | null>(null);
  const planHint = $derived(!plan ? '' : runError || (stale ? t('migrate.stale') : !plan.ok ? t('migrate.blockedHint') : runStarted ? t('migrate.startedHint') : t('migrate.readyHint')));
  function askRun(): Ask | null {
    if (!plan) return null;
    const p = plan;
    const w = (p.warnings ?? []).length;
    return {
      title: t('migrate.runTitle', { login: p.login }),
      note: t('migrate.runNote', { login: p.login }) + (w ? ' ' + tn('migrate.runWarnings', w) : ''),
      action: t('migrate.runAction'),
      run: () => run(p)
    };
  }
  // The API plans once more before it queues the job and refuses with 409
  // when something appeared here meanwhile (a login, a site, a database):
  // the reason stays next to the button until the next check.
  async function run(p: Plan) {
    runError = '';
    const req = request();
    try {
      const r = await api<{ job_id: number }>('/migrate/run', { method: 'POST', json: req });
      job = r.job_id;
      runKey = JSON.stringify(req);
      moved = { login: p.login, foreign: req.panel !== 'monopanel', status: 'running', sites: null };
      await tick();
      document.getElementById('migrate-job')?.scrollIntoView({ block: 'start', behavior: 'smooth' });
    } catch (e) {
      runError = e instanceof ApiError ? e.text : String(e);
      notify(runError, 'err');
    }
  }
  let pollUntil = 0;
  async function finished(status: string) {
    const m = moved;
    if (!m || m.status !== 'running') return;
    m.status = status;
    if (status === 'done') { notify(t('migrate.done', { login: m.login })); pollUntil = Date.now() + 300000; await loadMoved(); }
  }
  async function loadMoved() {
    const m = moved;
    if (!m) return;
    try { m.sites = (await api<any[]>('/sites')).filter((s) => s.login === m.login); } catch (e) { fail(e); }
  }
  // The job queues site.apply for every site once it is done: until those
  // run, the sites are pending and have no address yet. Look again shortly,
  // for a few minutes at most — the refresh button is there after that.
  $effect(() => {
    if (moved?.status !== 'done' || !moved.sites?.some((s) => s.status === 'pending') || Date.now() > pollUntil) return;
    const timer = setTimeout(loadMoved, 5000);
    return () => clearTimeout(timer);
  });
  // The README's check before the DNS switch: the site answers from here.
  // The line is pasted into a shell: values are the panel's (validated), and
  // quoted all the same, so an odd one stays one word.
  const sq = (v: string) => (/^[A-Za-z0-9._:\[\]-]+$/.test(v) ? v : `'${v.replace(/'/g, `'\\''`)}'`);
  const curlLine = (s: any) => (s.certificate_id ? `curl --resolve ${sq(`${s.domain}:443:${s.ip}`)} ${sq(`https://${s.domain}/`)}` : `curl --resolve ${sq(`${s.domain}:80:${s.ip}`)} ${sq(`http://${s.domain}/`)}`);
  const hostsLines = $derived((moved?.sites ?? []).filter((s) => s.ip).map((s) => [s.ip, s.domain, ...(s.aliases ?? [])].join(' ')).join('\n'));

  // A move started earlier (from here, from mp or before a reload) is a job:
  // its log is shown again instead of leaving the page looking idle. A job
  // still waits in the queue while another one holds the account's lock or
  // every worker is busy, so the queued ones count as well.
  let running = $state<{ id: number; login: string; source: string }[]>([]);
  async function loadRunning() {
    try {
      const lists = await Promise.all(['queued', 'running'].map((s) => api<any[]>(`/jobs?status=${s}&limit=100`)));
      // A job may start between the two answers and come in both: one row per id.
      const byID = new Map(lists.flat().filter((j) => j.type === 'migrate.run').map((j) => [j.id as number, j]));
      running = [...byID.values()].sort((a, b) => a.id - b.id).map((j) => ({ id: j.id, login: j.payload?.login ?? '', source: j.payload?.source ?? '' }));
    } catch { /* the Jobs page lists them as well */ }
  }

  onMount(() => { if (admin) { loadGrants(); loadRunning(); } });
</script>

<PageHead title={t('migrate.title')} sub={t('migrate.sub')} />
{#if !admin}
  <div class="card text-sm text-muted rise">{t('migrate.adminOnly')}</div>
{:else}
  {#if error}<p class="text-danger text-sm mb-3 break-words">{error}</p>{/if}
  <div class="tabs rise">
    <button class="tab {tab === 'take' ? 'tab-on' : ''} inline-flex items-center gap-1.5" onclick={() => { tab = 'take'; error = ''; }}><Icon name="download" size={14} /> {t('migrate.tabTake')}</button>
    <button class="tab {tab === 'grant' ? 'tab-on' : ''} inline-flex items-center gap-1.5" onclick={() => { tab = 'grant'; error = ''; }}><Icon name="send" size={14} /> {t('migrate.tabGrant')}</button>
  </div>

  {#if tab === 'take'}
    <p class="text-sm text-muted mb-4">{t('migrate.takeIntro')}</p>
    {#each running.filter((r) => r.id !== job) as r (r.id)}
      <div class="mb-4"><div class="text-sm mb-1">{t('migrate.runningEarlier', { login: r.login || '—', source: r.source || '—' })}</div><JobLog jobId={r.id} /></div>
    {/each}

    <form class="card mb-4 rise" onsubmit={check} autocomplete="off">
      <div class="flex flex-wrap items-center gap-x-3 gap-y-2 mb-2">
        <span class="label mb-0">{t('migrate.from')}</span>
        <div class="flex flex-wrap rounded-md border border-line p-0.5 bg-surface-2" role="group" aria-label={t('migrate.from')}>
          {#each panels as [id, name]}
            <button type="button" class="px-3 py-1 text-sm rounded-md transition-colors {src.panel === id ? 'bg-surface text-accent-ink font-medium' : 'text-muted hover:text-ink'}" aria-pressed={src.panel === id} onclick={() => (src.panel = id)}>{name}</button>
          {/each}
        </div>
      </div>
      <p class="text-xs text-muted mb-4">{t(panels.find(([id]) => id === src.panel)?.[2] ?? 'migrate.panelMono')}</p>

      {#if src.panel === 'monopanel'}
        <div class="grid md:grid-cols-3 gap-3">
          <div class="md:col-span-2">
            <label class="label" for="m-url">{t('migrate.url')}</label>
            <input id="m-url" class="input" bind:value={src.url} placeholder="https://old.example.com:8443" required spellcheck="false" onpaste={pasted} />
            <div class="text-xs text-muted mt-1">{t('migrate.urlHint')}</div>
          </div>
          <div>
            <label class="label" for="m-scope">{t('migrate.scope')}</label>
            <input id="m-scope" class="input" bind:value={src.scope} placeholder="user:alex" required spellcheck="false" onpaste={pasted} />
            <div class="text-xs text-muted mt-1">{t('migrate.scopeHint')}</div>
          </div>
          <div class="md:col-span-2">
            <label class="label" for="m-token">{t('migrate.token')}</label>
            <div class="flex gap-1">
              <input id="m-token" class="input" type={showToken ? 'text' : 'password'} bind:value={src.token} required spellcheck="false" autocomplete="new-password" onpaste={pasted} />
              <button type="button" class="btn shrink-0" onclick={() => (showToken = !showToken)} title={t('migrate.showToken')} aria-label={t('migrate.showToken')} aria-pressed={showToken}><Icon name="eye" size={14} /></button>
            </div>
          </div>
          <label class="flex items-start gap-2 text-sm md:mt-5">
            <input type="checkbox" class="mt-0.5" bind:checked={src.insecure} />
            <span>{t('migrate.insecure')}<span class="block text-xs text-muted">{t('migrate.insecureHint')}</span></span>
          </label>
        </div>
      {:else}
        <div class="grid md:grid-cols-3 gap-3">
          <div>
            <label class="label" for="f-ssh">{t('migrate.ssh')}</label>
            <input id="f-ssh" class="input" bind:value={src.ssh} placeholder="root@old.example.com" required spellcheck="false" onpaste={pasted} />
            <div class="text-xs text-muted mt-1">{t('migrate.sshHint')}</div>
          </div>
          <div>
            <label class="label" for="f-pw">{t('migrate.password')}</label>
            <input id="f-pw" class="input" type="password" bind:value={src.password} autocomplete="new-password" />
          </div>
          {#if src.panel === 'bitrixvm'}
            <div>
              <label class="label" for="f-domain">{t('migrate.domain')}</label>
              <input id="f-domain" class="input" bind:value={src.domain} placeholder="shop.example.com" spellcheck="false" />
              <div class="text-xs text-muted mt-1">{t('migrate.domainHint')}</div>
            </div>
          {:else}
            <div>
              <label class="label" for="f-scope">{t('migrate.scope')}</label>
              <input id="f-scope" class="input" bind:value={src.fpScope} placeholder={t('migrate.scopeFastPlaceholder')} spellcheck="false" />
              <div class="text-xs text-muted mt-1">{t('migrate.scopeFastHint')}</div>
            </div>
          {/if}
          <div class="md:col-span-3">
            <div class="flex items-end justify-between gap-2 mb-1">
              <label class="label mb-0" for="f-key">{t('migrate.key')}</label>
              <button type="button" class="btn btn-sm" onclick={() => keyInput?.click()}><Icon name="file" size={13} /> {t('migrate.keyFile')}</button>
            </div>
            <textarea id="f-key" class="input font-mono text-xs h-24" bind:value={src.key} oninput={() => (keyName = '')} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----" spellcheck="false" autocomplete="off" autocapitalize="off"></textarea>
            <input type="file" class="hidden" bind:this={keyInput} onchange={loadKey} />
            <div class="text-xs text-muted mt-1">{#if keyName}{t('migrate.keyLoaded', { name: keyName }) + ' '}{/if}{t('migrate.keyHint')}</div>
          </div>
        </div>
        {#if src.panel === 'bitrixvm'}<p class="text-xs text-muted mt-3">{t('migrate.bitrixScope')}</p>{/if}
      {/if}

      <div class="grid md:grid-cols-3 gap-3 items-end mt-4">
        <div>
          <label class="label" for="m-as">{t('migrate.as')}</label>
          <input id="m-as" class="input" bind:this={asInput} bind:value={src.as} pattern={'[a-z_][a-z0-9_\\-]{0,31}'} placeholder={t('migrate.asPlaceholder')} spellcheck="false" />
        </div>
        <div class="md:col-span-2 flex flex-wrap items-center gap-3">
          <button class="btn btn-primary" disabled={planning}><Icon name="eye" size={14} /> {planning ? t('migrate.checking') : t('migrate.check')}</button>
          <span class="text-xs text-muted">{planning && foreign ? t('migrate.checkingSSH') : t('migrate.checkHint')}</span>
        </div>
      </div>
      <div class="text-xs text-muted mt-1">{t('migrate.asHint')}</div>

      {#if planError}
        <div class="mt-4 p-3 rounded-lg border border-danger/40 bg-danger-soft text-sm text-danger break-words whitespace-pre-wrap">{planError}</div>
        {#if src.panel === 'monopanel' && !src.insecure && /x509|certificate/i.test(planError)}<p class="text-xs text-muted mt-1">{t('migrate.certErrorHint')}</p>{/if}
      {/if}
      {#if listed.length && src.panel === 'fastpanel'}
        <div class="mt-4 flex flex-wrap items-center gap-1.5 text-sm">
          <span class="text-muted">{t('migrate.listed')}</span>
          {#each listed as l}<button type="button" class="btn btn-sm font-mono" onclick={() => pickAccount(l)} disabled={planning}>user:{l}</button>{/each}
        </div>
      {/if}
    </form>

    <div id="migrate-job" class="scroll-mt-4">
      {#if job}<div class="mb-4"><JobLog jobId={job} onfinish={finished} /></div>{/if}

      {#if moved?.status === 'done'}
        <div class="card mb-4 rise">
          <div class="flex justify-between items-center gap-2 mb-3"><span class="font-medium">{t('migrate.afterTitle', { login: moved.login })}</span><button class="btn btn-sm" onclick={loadMoved} title={t('common.refresh')} aria-label={t('common.refresh')}><Icon name="refresh" size={13} /></button></div>
          <ol class="list-decimal pl-5 space-y-3 text-sm">
            <li>
              <div>{t('migrate.afterCheck')}</div>
              {#if moved.sites === null}
                <div class="text-xs text-muted mt-1">{t('common.loading')}</div>
              {:else if !moved.sites.length}
                <div class="text-xs text-muted mt-1">{t('migrate.afterNoSites')}</div>
              {:else}
                <div class="overflow-x-auto mt-2">
                  <table class="tbl"><thead><tr><th>{t('migrate.colDomain')}</th><th>{t('common.status')}</th><th>{t('migrate.colCheck')}</th></tr></thead><tbody>
                    {#each moved.sites as s}
                      <tr>
                        <td data-label={t('migrate.colDomain')}><a class="link font-mono" href="/sites/{encodeURIComponent(s.domain)}">{s.domain}</a></td>
                        <td data-label={t('common.status')}><span class="tag {s.status === 'active' ? 'tag-ok' : s.status === 'error' ? 'tag-err' : 'tag-warn'}">{s.status}</span></td>
                        <td data-label={t('migrate.colCheck')} class="min-w-0 w-full">{#if s.ip}<CopyField value={curlLine(s)} />{:else}<span class="text-xs text-muted">{t('migrate.afterNoIP')}</span>{/if}</td>
                      </tr>
                    {/each}
                  </tbody></table>
                </div>
                {#if hostsLines}<div class="mt-2"><div class="text-xs text-muted mb-1">{t('migrate.afterHosts')}</div><CopyField value={hostsLines} /></div>{/if}
              {/if}
            </li>
            {#if moved.foreign}<li>{t('migrate.afterPassword', { login: moved.login })} <a class="link" href="/users">{t('shell.nav.users')}</a></li>{/if}
            <li>{t('migrate.afterDNS')} <a class="link" href="/ssl">{t('shell.nav.ssl')}</a></li>
          </ol>
        </div>
      {:else if moved?.status === 'failed'}
        <div class="card mb-4 rise text-sm"><div class="flex gap-2 items-start"><Icon name="alert" size={16} class="text-danger shrink-0 mt-0.5" /><span>{t('migrate.failedHint', { login: moved.login })} <a class="link" href="/users">{t('shell.nav.users')}</a></span></div></div>
      {/if}
    </div>

    {#if plan}
      <PlanView {plan} hint={planHint} hintTone={runError || stale || !plan.ok ? 'danger' : 'muted'} onlogin={focusLogin}>
        {#snippet actions()}
          <button class="btn btn-primary" disabled={!canRun} onclick={() => (ask = askRun())}><Icon name="move" size={14} /> {t('migrate.run')}</button>
        {/snippet}
      </PlanView>
    {/if}
  {:else}
    <p class="text-sm text-muted mb-4">{t('migrate.grantIntro')}</p>
    {#if grantsError}<p class="text-danger text-sm mb-3 break-words">{grantsError}</p>{/if}
    <form class="card grid md:grid-cols-4 gap-3 items-end mb-4 rise" onsubmit={grant}>
      <div class="md:col-span-2">
        <label class="label" for="g-user">{t('migrate.grantAccount')}</label>
        <select id="g-user" class="input" bind:value={gform.login} required disabled={!users?.length}>
          {#each users ?? [] as u}<option value={u.login}>{u.login}{u.role === 'admin' ? ' · admin' : ''}{u.status !== 'active' ? ' · ' + u.status : ''}</option>{/each}
        </select>
      </div>
      <div><label class="label" for="g-hours">{t('migrate.grantHours')}</label><input id="g-hours" class="input" type="number" min="1" max="168" bind:value={gform.hours} required /></div>
      <button class="btn btn-primary" disabled={granting || !gform.login}><Icon name="key" size={14} /> {granting ? t('migrate.granting') : t('migrate.grantIssue')}</button>
      <p class="md:col-span-4 text-xs text-muted">{users && !users.length ? t('migrate.grantNoUsers') : t('migrate.grantHint')}</p>
    </form>

    {#if granted}
      <div class="card mb-4 rise">
        <div class="flex gap-2 items-start p-3 rounded-lg border border-danger/40 bg-danger-soft text-sm mb-4"><Icon name="alert" size={16} class="text-danger shrink-0 mt-0.5" /><span>{t('migrate.grantWarning', { login: granted.scope.replace(/^user:/, ''), until: when(granted.expires_at) })}</span></div>
        <div class="grid md:grid-cols-2 gap-3">
          <CopyField label={t('migrate.grantSource')} value={grantSource} />
          <CopyField label={t('migrate.scope')} value={granted.scope} />
          <div class="md:col-span-2"><CopyField label={t('migrate.token')} value={granted.token} /></div>
          <div class="md:col-span-2"><CopyField label={t('migrate.grantCommand')} value={granted.command} /></div>
        </div>
        <p class="text-xs text-muted mt-3">{t('migrate.grantValid', { when: when(granted.expires_at) })} {t('migrate.grantNext')}</p>
        <div class="flex justify-end mt-3"><button class="btn" onclick={() => (granted = null)}>{t('migrate.grantHide')}</button></div>
      </div>
    {/if}

    <div class="card overflow-x-auto p-0 rise" style="--i:1">
      <div class="px-4 pt-3 pb-1 font-medium">{t('migrate.grantsTitle')}</div>
      <table class="tbl"><thead><tr><th>{t('migrate.grantAccount')}</th><th>{t('migrate.colIssued')}</th><th>{t('migrate.colUntil')}</th><th>{t('migrate.colUsed')}</th><th></th></tr></thead><tbody>
        {#each grants as tk}
          <tr>
            <td data-label={t('migrate.grantAccount')} class="font-mono">{grantAccount(tk) || tk.name}</td>
            <td data-label={t('migrate.colIssued')} class="text-xs text-muted">{when(tk.created_at)}</td>
            <td data-label={t('migrate.colUntil')} class="text-xs"><span>{when(tk.expires_at)}{#if expired(tk.expires_at)}<span class="tag tag-muted ml-1">{t('migrate.expired')}</span>{/if}</span></td>
            <td data-label={t('migrate.colUsed')} class="text-xs">{tk.last_used_at ? when(tk.last_used_at) : t('migrate.unused')}</td>
            <td data-label="" class="text-right"><button class="btn btn-danger btn-sm" onclick={() => (ask = askRevoke(tk))}>{t('migrate.revoke')}</button></td>
          </tr>
        {/each}
        {#if !grants.length}<Empty text={t('migrate.grantsEmpty')} cols={5} />{/if}
      </tbody></table>
    </div>
    <p class="text-xs text-muted mt-3">{t('migrate.grantsNote')}</p>
  {/if}
{/if}

<Confirm bind:ask />
