<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError, bytes, when } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import { t } from '$lib/i18n/index.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Modal from '$lib/components/Modal.svelte';
  let targets = $state<any[]>([]);
  let runs = $state<any[]>([]);
  let snaps = $state<{ target: string; list: any[] } | null>(null);
  let created = $state<any>(null);
  let job = $state<number | null>(null);
  let error = $state('');
  let showForm = $state(false);
  let form = $state({ name: '', type: 'local', repository: '/var/backups/monopanel', password: '', env: '', keep_daily: 7, keep_weekly: 4, keep_monthly: 3, schedule: 'daily' });
  let run = $state({ target: '', scope: 'server' });
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function load() { try { targets = await api('/backups/targets'); runs = await api('/backups?limit=30'); if (!run.target && targets.length) run.target = targets[0].name; } catch (e: any) { error = e.text || String(e); } }
  onMount(load);
  async function create(e: Event) { e.preventDefault(); error = ''; try { const env: Record<string, string> = {}; for (const l of form.env.split('\n')) { const [k, ...v] = l.split('='); if (k.trim()) env[k.trim()] = v.join('=').trim(); } created = await api('/backups/targets', { method: 'POST', json: { ...form, env, password: form.password || undefined } }); showForm = false; await load(); } catch (e) { fail(e); } }
  async function start(e: Event) { e.preventDefault(); error = ''; try { const r: any = await api('/backups/run', { method: 'POST', json: run }); job = r.job_id; } catch (e) { fail(e); } }
  async function showSnaps(name: string) { try { snaps = { target: name, list: await api(`/backups/targets/${name}/snapshots`) }; } catch (e) { fail(e); } }
  // Восстановление спрашивает пути отдельным окном: системный prompt() не
  // умеет объяснить, куда именно ляжет снимок.
  let rest = $state<{ id: string; include: string } | null>(null);
  async function restore() {
    if (!rest || !snaps) return;
    const { id, include } = rest;
    rest = null;
    try {
      const r: any = await api('/backups/restore', { method: 'POST', json: { target: snaps.target, snapshot: id, include: include.split(',').map((s) => s.trim()).filter(Boolean) } });
      job = r.job_id;
    } catch (e) { fail(e); }
  }
</script>

<PageHead title={t('backups.title')} sub={t('backups.sub')}>
  <button class="btn btn-primary" onclick={() => (showForm = !showForm)}><Icon name="plus" size={15} /> {t('backups.repository')}</button>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if showForm}
  <form class="card grid md:grid-cols-4 gap-3 items-end mb-4 rise" onsubmit={create}>
    <div><label class="label" for="n">{t('common.name')}</label><input id="n" class="input" bind:value={form.name} required /></div>
    <div><label class="label" for="t">{t('backups.type')}</label><select id="t" class="input" bind:value={form.type}><option value="local">local</option><option value="sftp">sftp</option><option value="s3">s3</option><option value="b2">b2</option><option value="rest">rest</option></select></div>
    <div class="md:col-span-2"><label class="label" for="r">{t('backups.repository')}</label><input id="r" class="input font-mono" bind:value={form.repository} required /></div>
    <div><label class="label" for="p">{t('backups.repoPassword')}</label><input id="p" class="input" bind:value={form.password} placeholder={t('backups.generated')} /></div>
    <div><label class="label" for="s">{t('backups.schedule')}</label><select id="s" class="input" bind:value={form.schedule}><option value="daily">{t('backups.scheduleDaily')}</option><option value="">{t('backups.manual')}</option></select></div>
    <div><span class="label">{t('backups.keep')}</span><div class="flex gap-1"><input class="input" type="number" bind:value={form.keep_daily} /><input class="input" type="number" bind:value={form.keep_weekly} /><input class="input" type="number" bind:value={form.keep_monthly} /></div></div>
    <div><label class="label" for="e">{t('backups.envVars')}</label><textarea id="e" class="input font-mono h-9" bind:value={form.env} placeholder="AWS_ACCESS_KEY_ID=…"></textarea></div>
    <button class="btn btn-primary">{t('common.add')}</button>
  </form>
{/if}
{#if created?.password}<div class="card mb-4 text-sm rise border-accent/40">{t('backups.repoPasswordFor')} <b>{created.target.name}</b> {t('backups.saveShownOnce')} <code class="font-mono select-all">{created.password}</code></div>{/if}
<div class="card overflow-x-auto p-0 mb-4 rise">
  <table class="tbl"><thead><tr><th>{t('backups.target')}</th><th>{t('backups.type')}</th><th>{t('backups.repository')}</th><th>{t('backups.schedule')}</th><th>{t('backups.lastRun')}</th><th></th></tr></thead><tbody>
    {#each targets as tg, i}<tr class="rise" style="--i:{i}"><td data-label={t('backups.target')} class="font-medium">{tg.name}</td><td data-label={t('backups.type')}>{tg.type}</td><td data-label={t('backups.repository')} class="font-mono text-xs">{tg.repository}</td><td data-label={t('backups.schedule')} class="text-muted">{tg.schedule || t('backups.manual')}</td><td data-label={t('backups.lastRun')} class="text-xs">{#if tg.last_run_at}{when(tg.last_run_at)} · <span class="tag {tg.last_status === 'done' ? 'tag-ok' : 'tag-err'}">{tg.last_status}</span>{:else}—{/if}{#if tg.last_error}<div class="text-danger">{tg.last_error}</div>{/if}</td><td data-label="" class="text-right"><button class="btn btn-sm" onclick={() => showSnaps(tg.name)}><Icon name="archive" size={13} /> {t('backups.snapshots')}</button></td></tr>{/each}
    {#if !targets.length}<Empty text={t('backups.noRepos')} cols={6} />{/if}
  </tbody></table>
</div>
{#if targets.length}
  <form class="card flex flex-wrap gap-3 items-end mb-4 rise" style="--i:1" onsubmit={start}>
    <div><label class="label" for="rt">{t('backups.target')}</label><select id="rt" class="input" bind:value={run.target}>{#each targets as tg}<option value={tg.name}>{tg.name}</option>{/each}</select></div>
    <div class="flex-1 min-w-64"><label class="label" for="rs">Scope</label><input id="rs" class="input font-mono" bind:value={run.scope} placeholder="server | user:alex | site:example.com | db:alex_shop" /></div>
    <button class="btn btn-primary"><Icon name="play" size={14} /> {t('backups.run')}</button>
  </form>
{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
{#if snaps}
  <div class="card mb-4 rise"><div class="flex justify-between mb-2"><span class="font-medium">{t('backups.snapshotsOf', { target: snaps.target })}</span><button class="btn btn-sm" onclick={() => (snaps = null)}>{t('backups.close')}</button></div>
    <table class="tbl"><thead><tr><th>ID</th><th>{t('backups.time')}</th><th>{t('backups.tags')}</th><th>{t('backups.paths')}</th><th></th></tr></thead><tbody>
      {#each snaps.list as s}<tr><td data-label="ID" class="font-mono">{s.short_id}</td><td data-label={t('backups.time')} class="text-xs">{when(s.time)}</td><td data-label={t('backups.tags')} class="text-xs">{s.tags.join(', ')}</td><td data-label={t('backups.paths')} class="text-xs font-mono">{s.paths.join(' ')}</td><td data-label="" class="text-right"><button class="btn btn-sm" onclick={() => (rest = { id: s.short_id, include: '' })}>{t('backups.restore')}</button></td></tr>{/each}
      {#if !snaps.list.length}<Empty text={t('backups.noSnapshots')} cols={5} />{/if}
    </tbody></table></div>
{/if}
<div class="card overflow-x-auto p-0 rise" style="--i:2">
  <table class="tbl"><thead><tr><th>ID</th><th>Scope</th><th>{t('common.status')}</th><th>{t('backups.snapshot')}</th><th>{t('backups.size')}</th><th>{t('backups.files')}</th><th>{t('backups.started')}</th></tr></thead><tbody>
    {#each runs as b}<tr><td data-label="ID" class="text-muted">{b.id}</td><td data-label="Scope" class="font-mono">{b.scope}</td><td data-label={t('common.status')}><span class="tag {b.status === 'done' ? 'tag-ok' : b.status === 'failed' ? 'tag-err' : 'tag-warn'}">{b.status}</span>{#if b.error}<div class="text-xs text-danger">{b.error}</div>{/if}</td><td data-label={t('backups.snapshot')} class="font-mono text-xs">{b.snapshot_id?.slice(0, 8)}</td><td data-label={t('backups.size')} class="tabular-nums">{bytes(b.size_bytes)}</td><td data-label={t('backups.files')} class="tabular-nums">{b.files}</td><td data-label={t('backups.started')} class="text-xs text-muted">{when(b.started_at)}</td></tr>{/each}
    {#if !runs.length}<Empty text={t('backups.noRuns')} cols={7} />{/if}
  </tbody></table>
</div>

<Modal open={!!rest} title={t('backups.restoreTitle', { id: rest?.id ?? '' })} onclose={() => (rest = null)}>
  <p class="text-muted">{t('backups.restoreWhere')} <span class="font-mono">/var/lib/monopanel/restore/{rest?.id}</span> {t('backups.restoreHint')}</p>
  <div>
    <label class="label" for="rinc">{t('backups.pathsComma')}</label>
    <input id="rinc" class="input font-mono" bind:value={rest!.include} placeholder={t('backups.pathsPlaceholder')} />
  </div>
  {#snippet footer()}
    <button class="btn" onclick={() => (rest = null)}>{t('common.cancel')}</button>
    <button class="btn btn-primary" onclick={restore}>{t('backups.restoreAction')}</button>
  {/snippet}
</Modal>
