<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError, bytes } from '$lib/api';
  import { auth, notify } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Modal from '$lib/components/Modal.svelte';
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
  let form = $state({ name: '', user: '', password: '' });
  const admin = $derived(auth.me?.role === 'admin');
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function load() {
    try { engine = await api('/db/engine'); dbs = await api('/databases'); if (admin) users = ((await api('/users')) as any[]).filter((u) => u.role === 'user'); } catch (e: any) { error = e.text || String(e); }
  }
  onMount(load);
  async function installEngine(c: string) { error = ''; try { const r: any = await api('/stack/install', { method: 'POST', json: { component: c } }); job = r.job_id; } catch (e) { fail(e); } }
  async function create(e: Event) { e.preventDefault(); error = ''; try { const body: any = { ...form }; if (!admin) delete body.user; if (!body.password) delete body.password; created = await api('/databases', { method: 'POST', json: body }); form.name = ''; showForm = false; await load(); } catch (e) { fail(e); } }
  async function drop() { if (!del) return; try { await api(`/databases/${del.name}`, { method: 'DELETE' }); notify(`База ${del.name} удалена`); del = null; await load(); } catch (e) { fail(e); } }
  async function passwd(name: string) { try { const r: any = await api(`/databases/${name}/password`, { method: 'POST', json: {} }); created = { database: { name }, password: r.password, reset: true }; } catch (e) { fail(e); } }
</script>

<PageHead title="Базы данных" sub={engine?.installed ? `${engine.instance.engine} ${engine.instance.version} · ${engine.service?.active_state} · ${engine.instance.socket}` : 'MySQL 8.4 / Percona Server 8.4'}>
  {#if engine?.installed}<button class="btn btn-primary" onclick={() => (showForm = !showForm)}><Icon name="plus" size={15} /> Новая база</button>{/if}
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if engine && !engine.installed && admin}
  <div class="card mb-4 flex flex-wrap items-center gap-3 text-sm rise"><span>Сервер БД не установлен.</span><button class="btn btn-primary" onclick={() => installEngine('percona')}>Установить Percona Server 8.4</button><button class="btn" onclick={() => installEngine('mysql')}>MySQL 8.4</button></div>
{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
{#if engine?.installed}
  {#if showForm}
    <form class="card grid md:grid-cols-5 gap-3 items-end mb-4 rise" onsubmit={create}>
      <div><label class="label" for="n">Имя (суффикс)</label><input id="n" class="input font-mono" bind:value={form.name} pattern="[a-z0-9_]{'{'}1,24{'}'}" required /></div>
      {#if admin}<div><label class="label" for="u">Владелец</label><select id="u" class="input" bind:value={form.user} required><option value="">—</option>{#each users as u}<option value={u.login}>{u.login}</option>{/each}</select></div>{/if}
      <div><label class="label" for="pw">Пароль</label><input id="pw" class="input" bind:value={form.password} placeholder="генерируется" /></div>
      <button class="btn btn-primary">Создать &lt;login&gt;_&lt;имя&gt;</button>
    </form>
  {/if}
  {#if created}<div class="card mb-4 text-sm font-mono rise border-accent/40">{created.reset ? 'новый пароль' : 'база'} <b>{created.database.name}</b>{#if created.database.users?.[0]} · {created.database.users[0].name}@localhost{/if}{#if created.password} · пароль: <b class="select-all">{created.password}</b>{/if}{#if created.dsn}<div class="text-xs text-muted mt-1">{created.dsn}</div>{/if}<div class="text-xs text-muted">показывается один раз</div></div>{/if}
  <div class="card overflow-x-auto p-0 rise">
    <table class="tbl"><thead><tr><th>База</th><th>Владелец</th><th>Аккаунты</th><th>Размер</th><th></th></tr></thead>
      <tbody>
        {#each dbs as d, i}<tr class="rise" style="--i:{i}"><td class="font-mono font-medium">{d.name}</td><td>{d.login}</td><td class="font-mono text-xs text-muted">{d.users.map((u: any) => u.name + '@' + u.host + ' (' + u.auth_plugin + ')').join(', ')}</td><td class="tabular-nums">{bytes(d.size_bytes)}</td><td><div class="row-actions"><button class="btn btn-sm" onclick={() => passwd(d.name)}><Icon name="key" size={13} /> пароль</button><button class="btn btn-danger btn-sm" onclick={() => (del = d)}><Icon name="trash" size={13} /></button></div></td></tr>{/each}
        {#if !dbs.length}<Empty text="Баз пока нет." cols={5} />{/if}
      </tbody></table>
  </div>
{/if}
<Modal open={!!del} title="Удалить базу {del?.name}?">
  <p>Данные и аккаунты базы будут удалены безвозвратно.</p>
  {#snippet footer()}<button class="btn" onclick={() => (del = null)}>Отмена</button><button class="btn btn-danger" onclick={drop}>Удалить</button>{/snippet}
</Modal>
