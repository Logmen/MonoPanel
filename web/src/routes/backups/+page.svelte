<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError, bytes, when } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Icon from '$lib/components/Icon.svelte';
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
  async function restore(id: string) { if (!snaps) return; const inc = prompt('Пути для восстановления через запятую (пусто = всё) — в /var/lib/monopanel/restore/' + id); if (inc === null) return; try { const r: any = await api('/backups/restore', { method: 'POST', json: { target: snaps.target, snapshot: id, include: (inc || '').split(',').map((s) => s.trim()).filter(Boolean) } }); job = r.job_id; } catch (e) { fail(e); } }
</script>

<PageHead title="Бэкапы" sub="restic: local, SFTP, S3, B2, REST · дампы баз, файлы сайтов и панель">
  <button class="btn btn-primary" onclick={() => (showForm = !showForm)}><Icon name="plus" size={15} /> Репозиторий</button>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if showForm}
  <form class="card grid md:grid-cols-4 gap-3 items-end mb-4 rise" onsubmit={create}>
    <div><label class="label" for="n">Имя</label><input id="n" class="input" bind:value={form.name} required /></div>
    <div><label class="label" for="t">Тип</label><select id="t" class="input" bind:value={form.type}><option value="local">local</option><option value="sftp">sftp</option><option value="s3">s3</option><option value="b2">b2</option><option value="rest">rest</option></select></div>
    <div class="md:col-span-2"><label class="label" for="r">Репозиторий</label><input id="r" class="input font-mono" bind:value={form.repository} required /></div>
    <div><label class="label" for="p">Пароль репозитория</label><input id="p" class="input" bind:value={form.password} placeholder="генерируется" /></div>
    <div><label class="label" for="s">Расписание</label><select id="s" class="input" bind:value={form.schedule}><option value="daily">ежедневно (весь сервер)</option><option value="">вручную</option></select></div>
    <div><span class="label">Хранить D/W/M</span><div class="flex gap-1"><input class="input" type="number" bind:value={form.keep_daily} /><input class="input" type="number" bind:value={form.keep_weekly} /><input class="input" type="number" bind:value={form.keep_monthly} /></div></div>
    <div><label class="label" for="e">Переменные (S3 и т.п.)</label><textarea id="e" class="input font-mono h-9" bind:value={form.env} placeholder="AWS_ACCESS_KEY_ID=…"></textarea></div>
    <button class="btn btn-primary">Добавить</button>
  </form>
{/if}
{#if created?.password}<div class="card mb-4 text-sm rise border-accent/40">Пароль репозитория <b>{created.target.name}</b> (сохраните, показывается один раз): <code class="font-mono select-all">{created.password}</code></div>{/if}
<div class="card overflow-x-auto p-0 mb-4 rise">
  <table class="tbl"><thead><tr><th>Цель</th><th>Тип</th><th>Репозиторий</th><th>Расписание</th><th>Последний запуск</th><th></th></tr></thead><tbody>
    {#each targets as t, i}<tr class="rise" style="--i:{i}"><td data-label="Цель" class="font-medium">{t.name}</td><td data-label="Тип">{t.type}</td><td data-label="Репозиторий" class="font-mono text-xs">{t.repository}</td><td data-label="Расписание" class="text-muted">{t.schedule || 'вручную'}</td><td data-label="Последний запуск" class="text-xs">{#if t.last_run_at}{when(t.last_run_at)} · <span class="tag {t.last_status === 'done' ? 'tag-ok' : 'tag-err'}">{t.last_status}</span>{:else}—{/if}{#if t.last_error}<div class="text-danger">{t.last_error}</div>{/if}</td><td data-label="" class="text-right"><button class="btn btn-sm" onclick={() => showSnaps(t.name)}><Icon name="archive" size={13} /> снимки</button></td></tr>{/each}
    {#if !targets.length}<Empty text="Репозиториев нет." cols={6} />{/if}
  </tbody></table>
</div>
{#if targets.length}
  <form class="card flex flex-wrap gap-3 items-end mb-4 rise" style="--i:1" onsubmit={start}>
    <div><label class="label" for="rt">Цель</label><select id="rt" class="input" bind:value={run.target}>{#each targets as t}<option value={t.name}>{t.name}</option>{/each}</select></div>
    <div class="flex-1 min-w-64"><label class="label" for="rs">Scope</label><input id="rs" class="input font-mono" bind:value={run.scope} placeholder="server | user:alex | site:example.com | db:alex_shop" /></div>
    <button class="btn btn-primary"><Icon name="play" size={14} /> Запустить бэкап</button>
  </form>
{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
{#if snaps}
  <div class="card mb-4 rise"><div class="flex justify-between mb-2"><span class="font-medium">Снимк<thead><tr><th>ID</th><th>Время</th><th>Теги</th><th>Пути</th><th></th></tr></thead><tbody>
      {#each snaps.list as s}<tr><td data-label="ID" class="font-mono">{s.short_id}</td><td data-label="Время" class="text-xs">{when(s.time)}</td><td data-label="Теги" class="text-xs">{s.tags.join(', ')}</td><td data-label="Пути" class="text-xs font-mono">{s.paths.join(' ')}</td><td data-label="" class="text-right"><button class="btn btn-sm" onclick={() => restore(s.short_id)}>восстановить</button></td></tr>{/each}
      {#if !snaps.list.length}<Empty text="Снимков нет." cols={5} />{/if}
    </tbody>)}>восстановить<thead><tr><th>ID</th><th>Scope</th><th>Статус</th><th>Снимок</th><th>Размер</th><th>Файлов</th><th>Начат</th></tr></thead><tbody>
    {#each runs as b}<tr><td data-label="ID" class="text-muted">{b.id}</td><td data-label="Scope" class="font-mono">{b.scope}</td><td data-label="Статус"><span class="tag {b.status === 'done' ? 'tag-ok' : b.status === 'failed' ? 'tag-err' : 'tag-warn'}">{b.status}</span>{#if b.error}<div class="text-xs text-danger">{b.error}</div>{/if}</td><td data-label="Снимок" class="font-mono text-xs">{b.snapshot_id?.slice(0, 8)}</td><td data-label="Размер" class="tabular-nums">{bytes(b.size_bytes)}</td><td data-label="Файлов" class="tabular-nums">{b.files}</td><td data-label="Начат" class="text-xs text-muted">{when(b.started_at)}</td></tr>{/each}
    {#if !runs.length}<Empty text="Бэкапов ещё не было." cols={7} />{/if}
  </tbody>s(b.size_bytes)}</td><td class="tabular-nums">{b.files}</td><td class="text-xs text-muted">{when(b.started_at)}</td></tr>{/each}
    {#if !runs.length}<Empty text="Бэкапов ещё не было." cols={7} />{/if}
  </tbody></table>
</div>
