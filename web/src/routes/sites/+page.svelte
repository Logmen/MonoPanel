<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError } from '$lib/api';
  import { auth, notify } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Icon from '$lib/components/Icon.svelte';
  let sites = $state<any[] | null>(null);
  let users = $state<any[]>([]);
  let php = $state<any[]>([]);
  let error = $state('');
  let job = $state<number | null>(null);
  let showForm = $state(false);
  let del = $state<any>(null);
  let purge = $state(false);
  let presets = $state<any[]>([]);
  let form = $state({ domain: '', user: '', www: true, mode: 'fpm', php_version: '', ssl: 'auto', backend: '', preset: '' });
  const presetInfo = $derived(presets.find((p) => p.id === form.preset));
  const admin = $derived(auth.me?.role === 'admin');
  async function load() {
    try {
      sites = await api('/sites');
      if (admin) users = ((await api('/users')) as any[]).filter((u) => u.role === 'user' && u.unix_uid);
      php = ((await api('/php/versions')) as any).installed.filter((v: any) => v.status === 'installed');
      presets = await api('/sites/presets');
    } catch (e: any) { error = e.text || String(e); }
  }
  onMount(load);
  async function create(e: Event) {
    e.preventDefault();
    error = '';
    try {
      const body: any = { ...form };
      if (!admin) delete body.user;
      if (!body.php_version) delete body.php_version;
      if (body.mode !== 'proxy') delete body.backend;
      if (!body.preset || body.mode === 'proxy') delete body.preset;
      const res: any = await api('/sites', { method: 'POST', json: body });
      job = res.job_id;
      form.domain = '';
      showForm = false;
      await load();
    } catch (e) { error = e instanceof ApiError ? e.text : String(e); }
  }
  async function action(domain: string, act: string) {
    error = '';
    try {
      const res: any = act === 'delete' ? await api(`/sites/${domain}?purge=${purge}`, { method: 'DELETE' }) : await api(`/sites/${domain}/${act}`, { method: 'POST' });
      job = res.job_id;
      del = null;
      notify(act === 'delete' ? `Удаляю ${domain}` : `${domain}: ${act}`);
    } catch (e) { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); }
  }
</script>

<PageHead title="Сайты" sub="nginx + php-fpm, nginx + Apache или proxy на приложение">
  <button class="btn btn-primary" onclick={() => (showForm = !showForm)}><Icon name="plus" size={15} /> Новый сайт</button>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if showForm}
  <form class="card grid md:grid-cols-6 gap-3 items-end mb-4 rise" onsubmit={create}>
    <div class="md:col-span-2"><label class="label" for="d">Домен</label><input id="d" class="input" bind:value={form.domain} placeholder="example.com" required /></div>
    {#if admin}<div><label class="label" for="u">Владелец</label><select id="u" class="input" bind:value={form.user} required><option value="">—</option>{#each users as u}<option value={u.login}>{u.login}</option>{/each}</select></div>{/if}
    <div><label class="label" for="m">Режим</label><select id="m" class="input" bind:value={form.mode}><option value="fpm">nginx + php-fpm</option><option value="apache">nginx + Apache</option><option value="proxy">proxy → backend</option></select></div>
    {#if form.mode === 'proxy'}
      <div><label class="label" for="b">Backend</label><input id="b" class="input font-mono" bind:value={form.backend} placeholder="http://127.0.0.1:3000" /></div>
    {:else}
      <div><label class="label" for="p">PHP</label><select id="p" class="input" bind:value={form.php_version}><option value="">новейшая</option>{#each php as v}<option value={v.version}>{v.version}</option>{/each}</select></div>
    {/if}
    {#if form.mode !== 'proxy'}
      <div><label class="label" for="pr">Пресет CMS</label><select id="pr" class="input" bind:value={form.preset}>{#each presets as p}<option value={p.id}>{p.name}</option>{/each}</select></div>
    {/if}
    <div class="flex items-center gap-3 text-sm"><label><input type="checkbox" bind:checked={form.www} /> www</label><label><input type="checkbox" checked={form.ssl === 'auto'} onchange={(e) => (form.ssl = (e.target as HTMLInputElement).checked ? 'auto' : 'none')} /> SSL</label></div>
    <button class="btn btn-primary">Создать</button>
    {#if presetInfo?.description && form.mode !== 'proxy'}<p class="md:col-span-6 text-xs text-muted -mt-1">{presetInfo.name}: {presetInfo.description} Свои правила можно дописать на вкладке nginx сайта.</p>{/if}
  </form>
{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
<div class="card overflow-x-auto p-0 rise">
  {#if !sites}<Skeleton rows={5} />{:else}
  <table class="tbl">
    <thead><tr><th>Домен</th><th>Владелец</th><th>PHP</th><th>Режим</th><th>SSL</th><th>Статус</th><th></th></tr></thead>
    <tbody>
      {#each sites as s, i}
        <tr class="rise" style="--i:{i}">
          <td data-label="Домен"><a class="link font-medium" href="/sites/{s.domain}">{s.domain}</a>{#if s.aliases?.length}<div class="text-xs text-muted">{s.aliases.join(', ')}</div>{/if}{#if s.allow_from?.length}<div class="text-[11px] text-muted font-mono flex items-center gap-1 mt-0.5"><Icon name="lock" size={11} /> {s.allow_from.length} IP</div>{/if}</td>
          <td data-label="Владелец">{s.login}</td><td data-label="PHP" class="font-mono">{s.php_version || '—'}</td><td data-label="Режим" class="text-muted">{s.mode}{#if s.preset}<span class="tag tag-accent ml-1">{s.preset}</span>{/if}</td>
          <td data-label="SSL"><span class="tag {s.certificate_id ? 'tag-ok' : 'tag-muted'}">{s.certificate_id ? 'https' : s.ssl}</span></td>
          <td data-label="Статус"><span class="tag {s.status === 'active' ? 'tag-ok' : s.status === 'error' ? 'tag-err' : 'tag-warn'}">{s.status}</span>{#if s.last_error}<div class="text-xs text-danger max-w-xs truncate" title={s.last_error}>{s.last_error}</div>{/if}</td>
          <td data-label=""><div class="row-actions">
            <button class="btn btn-sm" onclick={() => action(s.domain, 'apply')} title="перегенерировать и применить"><Icon name="refresh" size={13} /></button>
            {#if admin}{#if s.status === 'suspended'}<button class="btn btn-sm" onclick={() => action(s.domain, 'unsuspend')}><Icon name="play" size={13} /> включить</button>{:else}<button class="btn btn-sm" onclick={() => action(s.domain, 'suspend')}><Icon name="stop" size={13} /> стоп</button>{/if}{/if}
            <button class="btn btn-danger btn-sm" onclick={() => { del = s; purge = false; }}><Icon name="trash" size={13} /></button>
          </div></td>
        </tr>
      {/each}
      {#if !sites.length}<Empty text="Сайтов пока нет — создайте первый." cols={7} />{/if}
    </tbody>
  </table>
  {/if}
</div>
<Modal open={!!del} title="Удалить сайт {del?.domain}?">
  <p>Конфигурация nginx и пул php-fpm будут удалены. Сертификат остаётся.</p>
  <label class="flex items-center gap-2"><input type="checkbox" bind:checked={purge} /> удалить и файлы сайта</label>
  {#snippet footer()}<button class="btn" onclick={() => (del = null)}>Отмена</button><button class="btn btn-danger" onclick={() => action(del.domain, 'delete')}>Удалить</button>{/snippet}
</Modal>
