<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError, bytes, when } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import { t, type MsgKey } from '$lib/i18n/index.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  // Пример адреса для каждого типа: локальный путь подставляется значением, остальные —
  // только подсказкой, чтобы не завести цель с чужим примером вместо своего бакета.
  const repoExample: Record<string, string> = { local: '/var/backups/monopanel', sftp: 'sftp:backup@203.0.113.7:/srv/restic', s3: 's3:https://s3.amazonaws.com/bucket/monopanel', b2: 'b2:bucket:monopanel', rest: 'rest:https://backup.example.com:8000/monopanel' };
  const envExample: Record<string, string> = { local: '', sftp: '', s3: 'AWS_ACCESS_KEY_ID=…\nAWS_SECRET_ACCESS_KEY=…', b2: 'B2_ACCOUNT_ID=…\nB2_ACCOUNT_KEY=…', rest: 'RESTIC_REST_USERNAME=…\nRESTIC_REST_PASSWORD=…' };
  const scopeKinds = ['server', 'user', 'site', 'db'] as const;
  type ScopeKind = (typeof scopeKinds)[number];
  const kindLabel: Record<ScopeKind, MsgKey> = { server: 'backups.kindServer', user: 'backups.kindUser', site: 'backups.kindSite', db: 'backups.kindDb' };
  const argLabel: Record<ScopeKind, MsgKey> = { server: 'backups.kindServer', user: 'backups.argUser', site: 'backups.argSite', db: 'backups.argDb' };
  let targets = $state<any[]>([]);
  let runs = $state<any[]>([]);
  let runsTarget = $state('');
  let runsLimit = $state(30);
  let snaps = $state<{ target: string; list: any[] } | null>(null);
  let snapsBusy = $state('');
  let created = $state<any>(null);
  let job = $state<number | null>(null);
  let error = $state('');
  let ask = $state<Ask | null>(null);
  let showForm = $state(false);
  // Каталог данных панели: сюда (в restore/<снимок>) кладёт файлы восстановление «рядом».
  let dataDir = $state('/var/lib/monopanel');
  const blank = () => ({ name: '', type: 'local', repository: repoExample.local, password: '', env: '', keep_daily: 7, keep_weekly: 4, keep_monthly: 3, schedule: 'daily' });
  let form = $state(blank());
  let run = $state<{ target: string; kind: ScopeKind; arg: string }>({ target: '', kind: 'server', arg: '' });
  let scopeOptions = $state<Partial<Record<ScopeKind, string[]>>>({});
  const errText = (e: unknown) => (e instanceof ApiError ? e.text : String(e));
  const fail = (e: unknown) => { error = errText(e); notify(error, 'err'); };
  async function loadTargets() { try { targets = await api('/backups/targets'); if (!targets.some((x) => x.name === run.target)) run.target = targets[0]?.name ?? ''; } catch (e) { error = errText(e); } }
  const runsQuery = $derived(`limit=${runsLimit}${runsTarget ? '&target=' + encodeURIComponent(runsTarget) : ''}`);
  async function loadRuns() { try { runs = await api(`/backups?${runsQuery}`); } catch (e) { error = errText(e); } }
  // Фильтр по цели и глубина истории — параметры самого запроса, а не фильтр в браузере.
  $effect(() => { loadRuns(); });
  const load = () => Promise.all([loadTargets(), loadRuns()]);
  onMount(() => {
    loadTargets();
    api<any>('/system/config').then((c) => { if (c?.data_dir) dataDir = c.data_dir; }).catch(() => {});
  });
  const targetName = (id: number) => targets.find((x) => x.id === id)?.name ?? '#' + id;

  /* ── repositories ────────────────────────────────────────────────────── */
  function setType(type: string) {
    if (form.repository === '' || Object.values(repoExample).includes(form.repository)) form.repository = type === 'local' ? repoExample.local : '';
    form.type = type;
  }
  async function create(e: Event) {
    e.preventDefault(); error = '';
    const env: Record<string, string> = {};
    for (const raw of form.env.split('\n')) {
      const l = raw.trim(); if (!l) continue;
      const i = l.indexOf('=');
      if (i < 1) { fail(t('backups.envBad', { line: l })); return; }
      env[l.slice(0, i).trim()] = l.slice(i + 1).trim();
    }
    const body: Record<string, unknown> = { name: form.name.trim(), type: form.type, repository: form.repository.trim(), schedule: form.schedule, keep_daily: Number(form.keep_daily) || 0, keep_weekly: Number(form.keep_weekly) || 0, keep_monthly: Number(form.keep_monthly) || 0 };
    if (form.password) body.password = form.password;
    if (Object.keys(env).length) body.env = env;
    try { created = await api('/backups/targets', { method: 'POST', json: body }); form = blank(); showForm = false; await loadTargets(); } catch (e) { fail(e); }
  }
  const askForget = (tg: any): Ask => ({
    title: t('backups.forgetTitle', { name: tg.name }),
    note: t('backups.forgetNote', { repo: tg.repository }),
    danger: true, action: t('backups.forgetAction'),
    run: async () => {
      try {
        await api(`/backups/targets/${encodeURIComponent(tg.name)}`, { method: 'DELETE' });
        notify(t('backups.forgotten', { name: tg.name }));
        if (snaps?.target === tg.name) snaps = null;
        if (runsTarget === tg.name) runsTarget = '';
        if (created?.target?.name === tg.name) created = null;
        await load();
      } catch (e) { fail(e); }
    }
  });

  /* ── run ─────────────────────────────────────────────────────────────── */
  // Подсказки для области: логины, домены и базы, чтобы не ловить опечатку от API.
  async function loadScope(kind: ScopeKind) {
    if (kind === 'server' || scopeOptions[kind]) return;
    try {
      const list: any[] = await api(kind === 'user' ? '/users' : kind === 'site' ? '/sites' : '/databases');
      scopeOptions[kind] = list.map((x) => (kind === 'user' ? x.login : kind === 'site' ? x.domain : x.name));
    } catch { scopeOptions[kind] = []; }
  }
  function setKind(kind: ScopeKind) { run.kind = kind; run.arg = ''; loadScope(kind); }
  const scope = $derived(run.kind === 'server' ? 'server' : `${run.kind}:${run.arg.trim()}`);
  async function start(e: Event) { e.preventDefault(); error = ''; try { const r: any = await api('/backups/run', { method: 'POST', json: { target: run.target, scope } }); job = r.job_id; } catch (e) { fail(e); } }

  /* ── snapshots and restore ───────────────────────────────────────────── */
  async function showSnaps(name: string) { error = ''; snapsBusy = name; try { snaps = { target: name, list: await api(`/backups/targets/${encodeURIComponent(name)}/snapshots`) }; } catch (e) { fail(e); } finally { snapsBusy = ''; } }
  // Восстановление спрашивает пути отдельным окном: системный prompt() не умеет
  // объяснить, куда именно ляжет снимок, а «на место» требует явного согласия.
  let rest = $state<{ snap: any; where: 'aside' | 'inplace'; include: string; sure: boolean } | null>(null);
  const includes = $derived(rest ? rest.include.split(/[\n,]+/).map((s) => s.trim()).filter(Boolean) : []);
  const badInclude = $derived(includes.find((p) => !p.startsWith('/')) ?? '');
  const canRestore = $derived(!!rest && !badInclude && (rest.where === 'aside' || (includes.length > 0 && rest.sure)));
  function addPath(p: string) { if (rest && !includes.includes(p)) rest.include = [...includes, p].join(', '); }
  async function restore() {
    if (!rest || !snaps || !canRestore) return;
    const body: Record<string, unknown> = { target: snaps.target, snapshot: rest.snap.short_id || rest.snap.id };
    if (includes.length) body.include = includes;
    if (rest.where === 'inplace') body.in_place = true;
    rest = null;
    try { const r: any = await api('/backups/restore', { method: 'POST', json: body }); job = r.job_id; } catch (e) { fail(e); }
  }
</script>

<PageHead title={t('backups.title')} sub={t('backups.sub')}>
  <button class="btn btn-primary" onclick={() => (showForm = !showForm)}><Icon name="plus" size={15} /> {t('backups.repository')}</button>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if showForm}
  <form class="card grid md:grid-cols-4 gap-3 items-start mb-4 rise" onsubmit={create}>
    <div><label class="label" for="n">{t('common.name')}</label><input id="n" class="input font-mono" bind:value={form.name} pattern="[a-z0-9][a-z0-9_\-]{'{'}0,31{'}'}" maxlength="32" required /></div>
    <div><label class="label" for="t">{t('backups.type')}</label><select id="t" class="input" value={form.type} onchange={(e) => setType(e.currentTarget.value)}><option value="local">local</option><option value="sftp">sftp</option><option value="s3">s3</option><option value="b2">b2</option><option value="rest">rest</option></select></div>
    <div class="md:col-span-2"><label class="label" for="r">{t('backups.repository')}</label><input id="r" class="input font-mono" bind:value={form.repository} placeholder={repoExample[form.type]} required /><p class="text-[11px] text-muted mt-1">{form.type === 'local' ? t('backups.repoLocalHint') : t('backups.repoPrefixHint', { prefix: form.type + ':' })}</p></div>
    <div><label class="label" for="p">{t('backups.repoPassword')}</label><input id="p" class="input" type="password" autocomplete="new-password" bind:value={form.password} placeholder={t('backups.generated')} /></div>
    <div><label class="label" for="s">{t('backups.schedule')}</label><select id="s" class="input" bind:value={form.schedule}><option value="daily">{t('backups.scheduleDaily')}</option><option value="">{t('backups.manual')}</option></select></div>
    <div class="md:col-span-2"><span class="label">{t('backups.keep')}</span><div class="grid grid-cols-3 gap-1"><input class="input" type="number" min="0" max="365" bind:value={form.keep_daily} aria-label={t('backups.keepDaily')} title={t('backups.keepDaily')} /><input class="input" type="number" min="0" max="104" bind:value={form.keep_weekly} aria-label={t('backups.keepWeekly')} title={t('backups.keepWeekly')} /><input class="input" type="number" min="0" max="120" bind:value={form.keep_monthly} aria-label={t('backups.keepMonthly')} title={t('backups.keepMonthly')} /></div><p class="text-[11px] text-muted mt-1">{t('backups.keepHint')}</p></div>
    <div class="md:col-span-3"><label class="label" for="e">{t('backups.envVars')}</label><textarea id="e" class="input font-mono h-16" bind:value={form.env} placeholder={envExample[form.type] || 'KEY=VALUE'}></textarea><p class="text-[11px] text-muted mt-1">{t('backups.envHint')}</p></div>
    <div class="md:pt-5"><button class="btn btn-primary">{t('common.add')}</button></div>
  </form>
{/if}
{#if created}
  <div class="card mb-4 text-sm rise border-accent/40">
    <div>{t('backups.created', { name: created.target.name, type: created.target.type, repo: created.target.repository, keep: `${created.target.keep_daily}/${created.target.keep_weekly}/${created.target.keep_monthly}` })}</div>
    {#if created.password}<div class="mt-1">{t('backups.repoPasswordFor')} <b>{created.target.name}</b> {t('backups.saveShownOnce')} <code class="font-mono select-all">{created.password}</code></div>{/if}
  </div>
{/if}
<div class="card overflow-x-auto p-0 mb-4 rise">
  <table class="tbl"><thead><tr><th>{t('backups.target')}</th><th>{t('backups.type')}</th><th>{t('backups.repository')}</th><th>{t('backups.keep')}</th><th>{t('backups.schedule')}</th><th>{t('backups.lastRun')}</th><th></th></tr></thead><tbody>
    {#each targets as tg, i (tg.id)}<tr class="rise" style="--i:{i}"><td data-label={t('backups.target')} class="font-medium">{tg.name} <span class="text-xs text-muted font-mono">#{tg.id}</span></td><td data-label={t('backups.type')}>{tg.type}</td><td data-label={t('backups.repository')} class="font-mono text-xs break-all">{tg.repository}</td><td data-label={t('backups.keep')} class="tabular-nums text-xs">{tg.keep_daily}/{tg.keep_weekly}/{tg.keep_monthly}</td><td data-label={t('backups.schedule')} class="text-muted">{tg.schedule || t('backups.manual')}</td><td data-label={t('backups.lastRun')} class="text-xs">{#if tg.last_run_at}{when(tg.last_run_at)} · <span class="tag {tg.last_status === 'done' ? 'tag-ok' : 'tag-err'}">{tg.last_status}</span>{:else}—{/if}{#if tg.last_error}<div class="text-danger">{tg.last_error}</div>{/if}</td><td data-label=""><div class="row-actions"><button class="btn btn-sm" disabled={!!snapsBusy} onclick={() => showSnaps(tg.name)}><Icon name="archive" size={13} /> {snapsBusy === tg.name ? t('backups.snapshotsBusy') : t('backups.snapshots')}</button><button class="btn btn-danger btn-sm" title={t('backups.forget')} aria-label={t('backups.forget')} onclick={() => (ask = askForget(tg))}><Icon name="trash" size={13} /></button></div></td></tr>{/each}
    {#if !targets.length}<Empty text={t('backups.noRepos')} cols={7} />{/if}
  </tbody></table>
</div>
{#if targets.length}
  <form class="card flex flex-wrap gap-3 items-end mb-4 rise" style="--i:1" onsubmit={start}>
    <div><label class="label" for="rt">{t('backups.target')}</label><select id="rt" class="input" bind:value={run.target}>{#each targets as tg}<option value={tg.name}>{tg.name}</option>{/each}</select></div>
    <div><label class="label" for="rk">{t('backups.scope')}</label><select id="rk" class="input" value={run.kind} onchange={(e) => setKind(e.currentTarget.value as ScopeKind)}>{#each scopeKinds as k}<option value={k}>{t(kindLabel[k])}</option>{/each}</select></div>
    {#if run.kind !== 'server'}<div class="flex-1 min-w-48"><label class="label" for="ra">{t(argLabel[run.kind])}</label><input id="ra" class="input font-mono" list="scope-options" bind:value={run.arg} required /><datalist id="scope-options">{#each scopeOptions[run.kind] ?? [] as o}<option value={o}></option>{/each}</datalist></div>{/if}
    <button class="btn btn-primary"><Icon name="play" size={14} /> {t('backups.run')}</button>
  </form>
{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
{#if snaps}
  <div class="card mb-4 rise"><div class="flex justify-between mb-2"><span class="font-medium">{t('backups.snapshotsOf', { target: snaps.target })}</span><button class="btn btn-sm" onclick={() => (snaps = null)}>{t('backups.close')}</button></div>
    <table class="tbl"><thead><tr><th>ID</th><th>{t('backups.time')}</th><th>{t('backups.tags')}</th><th>{t('backups.paths')}</th><th></th></tr></thead><tbody>
      {#each snaps.list as s (s.id)}<tr><td data-label="ID" class="font-mono" title={s.id}>{s.short_id}</td><td data-label={t('backups.time')} class="text-xs" title={s.hostname}>{when(s.time)}</td><td data-label={t('backups.tags')} class="text-xs">{(s.tags ?? []).join(', ')}</td><td data-label={t('backups.paths')} class="text-xs font-mono break-all">{(s.paths ?? []).join(' ')}</td><td data-label="" class="text-right"><button class="btn btn-sm" onclick={() => (rest = { snap: s, where: 'aside', include: '', sure: false })}>{t('backups.restore')}</button></td></tr>{/each}
      {#if !snaps.list.length}<Empty text={t('backups.noSnapshots')} cols={5} />{/if}
    </tbody></table></div>
{/if}
<div class="flex flex-wrap items-end justify-between gap-2 mb-2 rise" style="--i:2">
  <span class="font-medium text-sm">{t('backups.runsTitle')}</span>
  <div class="flex flex-wrap gap-2">
    <select class="input w-auto" bind:value={runsTarget} aria-label={t('backups.target')}><option value="">{t('backups.allTargets')}</option>{#each targets as tg}<option value={tg.name}>{tg.name}</option>{/each}</select>
    <select class="input w-auto" bind:value={runsLimit} aria-label={t('backups.runsLimit')}>{#each [30, 50, 100, 200, 500] as n}<option value={n}>{t('backups.lastN', { n })}</option>{/each}</select>
  </div>
</div>
<div class="card overflow-x-auto p-0 rise" style="--i:2">
  <table class="tbl"><thead><tr><th>ID</th><th>{t('backups.target')}</th><th>{t('backups.scope')}</th><th>{t('common.status')}</th><th>{t('backups.snapshot')}</th><th>{t('backups.size')}</th><th>{t('backups.files')}</th><th>{t('backups.started')}</th><th>{t('backups.finished')}</th></tr></thead><tbody>
    {#each runs as b (b.id)}<tr><td data-label="ID" class="text-muted">{b.id}</td><td data-label={t('backups.target')}>{targetName(b.target_id)}</td><td data-label={t('backups.scope')} class="font-mono">{b.scope}</td><td data-label={t('common.status')}><span class="tag {b.status === 'done' ? 'tag-ok' : b.status === 'failed' ? 'tag-err' : 'tag-warn'}">{b.status}</span>{#if b.error}<div class="text-xs text-danger">{b.error}</div>{/if}</td><td data-label={t('backups.snapshot')} class="font-mono text-xs" title={b.snapshot_id}>{b.snapshot_id?.slice(0, 8)}</td><td data-label={t('backups.size')} class="tabular-nums">{bytes(b.size_bytes)}</td><td data-label={t('backups.files')} class="tabular-nums">{b.files}</td><td data-label={t('backups.started')} class="text-xs text-muted">{when(b.started_at)}</td><td data-label={t('backups.finished')} class="text-xs text-muted">{when(b.finished_at)}</td></tr>{/each}
    {#if !runs.length}<Empty text={t('backups.noRuns')} cols={9} />{/if}
  </tbody></table>
</div>

<Confirm bind:ask />
<Modal open={!!rest} title={t('backups.restoreTitle', { id: rest?.snap.short_id ?? '' })} onclose={() => (rest = null)}>
  {#if rest}
    <div class="space-y-1.5">
      <label class="flex items-start gap-2"><input type="radio" name="where" value="aside" bind:group={rest.where} class="mt-1" /><span>{t('backups.restoreAside')} <span class="font-mono text-xs break-all">{dataDir}/restore/{rest.snap.short_id}</span></span></label>
      <label class="flex items-start gap-2"><input type="radio" name="where" value="inplace" bind:group={rest.where} class="mt-1" /><span>{t('backups.restoreInPlace')}</span></label>
    </div>
    {#if rest.where === 'aside'}<p class="text-muted">{t('backups.restoreAsideHint')}</p>{/if}
    <div>
      <label class="label" for="rinc">{t('backups.pathsComma')}</label>
      <textarea id="rinc" class="input font-mono text-xs h-16" bind:value={rest.include} placeholder={rest.where === 'inplace' ? t('backups.pathsRequired') : t('backups.pathsPlaceholder')}></textarea>
      {#if rest.snap.paths?.length}<div class="flex flex-wrap gap-1 mt-1.5">{#each rest.snap.paths as p}<button type="button" class="tag tag-muted hover:text-ink cursor-pointer max-w-full whitespace-normal break-all text-left" onclick={() => addPath(p)}>+ {p}</button>{/each}</div>{/if}
      {#if badInclude}<p class="text-xs text-danger mt-1">{t('backups.pathAbsolute', { path: badInclude })}</p>{/if}
    </div>
    {#if rest.where === 'inplace'}
      <div class="p-3 rounded-lg border border-danger bg-danger-soft space-y-2">
        <p class="font-medium text-danger">{t('backups.inPlaceWarnTitle')}</p>
        <p>{t('backups.inPlaceWarn')}</p>
        <label class="flex items-start gap-2"><input type="checkbox" bind:checked={rest.sure} class="mt-1" /><span>{t('backups.inPlaceSure')}</span></label>
      </div>
    {/if}
    <p class="text-xs text-muted">{t('backups.restoreSlow')}</p>
  {/if}
  {#snippet footer()}
    <button class="btn" onclick={() => (rest = null)}>{t('common.cancel')}</button>
    <button class="btn {rest?.where === 'inplace' ? 'btn-danger' : 'btn-primary'}" disabled={!canRestore} onclick={restore}>{rest?.where === 'inplace' ? t('backups.restoreOverwrite') : t('backups.restoreAction')}</button>
  {/snippet}
</Modal>
