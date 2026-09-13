<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import { t, type MsgKey } from '$lib/i18n/index.svelte';

  // Everything the panel installs on the host besides sites: the web
  // servers, the database, fail2ban and the small tools. PHP branches have
  // their own page.
  let stack = $state<any[] | null>(null);
  let mem = $state<any>(null);
  let memForm = $state({ memory_mb: 128, max_connections: 1024 });
  let job = $state<number | null>(null);
  let error = $state('');
  let ask = $state<Ask | null>(null);
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function load() {
    try {
      const [s, m] = await Promise.all([api('/stack'), api('/stack/memcached')]);
      stack = s as any[]; mem = m; memForm = { memory_mb: mem.memory_mb, max_connections: mem.max_connections };
    } catch (e: any) { error = e.text || String(e); }
  }
  onMount(load);

  const info: Record<string, { title: string; text: MsgKey }> = {
    nginx: { title: 'nginx', text: 'stack.infoNginx' },
    apache: { title: 'Apache 2.4', text: 'stack.infoApache' },
    percona: { title: 'Percona Server 8.4', text: 'stack.infoDb' },
    mysql: { title: 'MySQL 8.4', text: 'stack.infoDb' },
    fail2ban: { title: 'fail2ban', text: 'stack.infoFail2ban' },
    memcached: { title: 'Memcached', text: 'stack.infoMemcached' },
    jpegoptim: { title: 'Jpegoptim', text: 'stack.infoJpegoptim' },
    git: { title: 'Git', text: 'stack.infoGit' },
    composer: { title: 'Composer', text: 'stack.infoComposer' },
    sphinx: { title: 'Sphinx', text: 'stack.infoSphinx' }
  };
  const groups: [MsgKey, string[]][] = [
    ['stack.groupWeb', ['nginx', 'apache']],
    ['stack.groupDatabase', ['percona', 'mysql']],
    ['stack.groupSecurity', ['fail2ban']],
    ['stack.groupTools', ['memcached', 'jpegoptim', 'git', 'composer', 'sphinx']]
  ];
  const byName = $derived(Object.fromEntries((stack ?? []).map((c: any) => [c.name, c])) as Record<string, any>);
  const dbInstalled = $derived(!!(byName.percona?.installed || byName.mysql?.installed));

  async function install(name: string) { error = ''; try { const r: any = await api('/stack/install', { method: 'POST', json: { component: name } }); job = r.job_id; } catch (e) { fail(e); } }
  const askInstall = (name: string): Ask => ({
    title: t('stack.askInstallTitle', { name: info[name]?.title ?? name }),
    note: name === 'composer' ? t('stack.installNoteComposer') :
      name === 'memcached' ? t('stack.installNoteMemcached') :
      name === 'sphinx' ? t('stack.installNoteSphinx') :
      name === 'percona' || name === 'mysql' ? t('stack.installNoteDb') :
      t('stack.installNoteDefault'),
    action: t('stack.install'),
    run: () => install(name)
  });
  const askRemove = (name: string): Ask => ({
    title: t('stack.askRemoveTitle', { name: info[name]?.title ?? name }),
    note: name === 'memcached' ? t('stack.removeNoteMemcached') :
      name === 'composer' ? t('stack.removeNoteComposer') :
      name === 'sphinx' ? t('stack.removeNoteSphinx') :
      t('stack.removeNoteDefault'),
    danger: true, action: t('stack.remove'),
    run: async () => { const r: any = await api(`/stack/${name}`, { method: 'DELETE' }); job = r.job_id; }
  });
  async function saveMem(e: Event) { e.preventDefault(); error = ''; try { mem = await api('/stack/memcached', { method: 'PUT', json: memForm }); notify(mem.installed ? t('stack.memReconfigured') : t('stack.memSavedPending')); } catch (e) { fail(e); } }
  const stateOf = (c: any) => !c ? t('stack.stateNotInstalled') : !c.installed ? t('stack.stateNotInstalled') : c.service ? `${c.service.active_state}` : t('stack.stateInstalled');
</script>

<PageHead title={t('stack.title')} sub={t('stack.sub')} />
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
{#if !stack}<div class="card p-0"><Skeleton rows={8} /></div>{:else}
  {#each groups as [group, names], gi}
    <div class="card overflow-x-auto p-0 mb-4 rise" style="--i:{gi}">
      <div class="px-4 pt-3 font-medium">{t(group)}</div>
      <table class="tbl"><thead><tr><th>{t('stack.colComponent')}</th><th>{t('stack.colWhat')}</th><th>{t('stack.colVersion')}</th><th>{t('stack.colState')}</th><th></th></tr></thead>
        <tbody>
          {#each names as name}
            {@const c = byName[name]}
            {#if c || group !== 'stack.groupDatabase'}
              <tr>
                <td data-label={t('stack.colComponent')} class="font-medium whitespace-nowrap">{info[name]?.title ?? name}</td>
                <td data-label={t('stack.colWhat')} class="text-sm text-muted max-w-md">{info[name] ? t(info[name].text) : ''}</td>
                <td data-label={t('stack.colVersion')} class="font-mono text-xs">{c?.version || '—'}</td>
                <td data-label={t('stack.colState')}><span class="tag {c?.installed ? (c.service && c.service.active_state !== 'active' ? 'tag-err' : 'tag-ok') : 'tag-muted'}">{stateOf(c)}</span></td>
                <td data-label=""><div class="row-actions">
                  {#if !c?.installed}
                    {#if (name === 'percona' || name === 'mysql') && dbInstalled}<span class="text-xs text-muted">{t('stack.otherDbInstalled')}</span>
                    {:else}<button class="btn btn-sm btn-primary" onclick={() => (ask = askInstall(name))}><Icon name="download" size={13} /> {t('stack.btnInstall')}</button>{/if}
                  {:else}
                    {#if name === 'composer'}<button class="btn btn-sm" onclick={() => (ask = askInstall(name))}><Icon name="refresh" size={13} /> {t('stack.btnUpdate')}</button>{/if}
                    {#if c.removable}<button class="btn btn-sm btn-danger" onclick={() => (ask = askRemove(name))}><Icon name="trash" size={13} /></button>{/if}
                  {/if}
                </div></td>
              </tr>
            {/if}
          {/each}
        </tbody></table>
    </div>
  {/each}
  {#if mem}
    <form class="card grid md:grid-cols-4 gap-3 items-end rise" onsubmit={saveMem}>
      <div class="md:col-span-4"><div class="font-medium">{t('stack.memTitle')}</div><div class="text-xs text-muted">{t('stack.memListens')} {mem.installed ? t('stack.memHintInstalled') : t('stack.memHintPending')}</div></div>
      <div><label class="label" for="mm">{t('stack.memMemory')}</label><input id="mm" class="input" type="number" min="16" max="65536" bind:value={memForm.memory_mb} /></div>
      <div><label class="label" for="mc">{t('stack.memConnections')}</label><input id="mc" class="input" type="number" min="64" max="65536" bind:value={memForm.max_connections} /></div>
      <button class="btn btn-primary">{mem.installed ? t('stack.saveAndApply') : t('common.save')}</button>
    </form>
  {/if}
{/if}
<Confirm bind:ask />
