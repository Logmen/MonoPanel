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
  // servers, the database, fail2ban and the small tools. PHP branches are
  // managed on their own page; here they are listed the way mp stack list
  // shows them, with the state of their php-fpm.
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
    valkey: { title: 'Valkey', text: 'stack.infoValkey' },
    jpegoptim: { title: 'Jpegoptim', text: 'stack.infoJpegoptim' },
    git: { title: 'Git', text: 'stack.infoGit' },
    composer: { title: 'Composer', text: 'stack.infoComposer' },
    sphinx: { title: 'Sphinx', text: 'stack.infoSphinx' }
  };
  const groups: [MsgKey, string[]][] = [
    ['stack.groupWeb', ['nginx', 'apache']],
    ['stack.groupDatabase', ['percona', 'mysql']],
    ['stack.groupSecurity', ['fail2ban']],
    ['stack.groupTools', ['memcached', 'valkey', 'jpegoptim', 'git', 'composer', 'sphinx']]
  ];
  const byName = $derived(Object.fromEntries((stack ?? []).map((c: any) => [c.name, c])) as Record<string, any>);
  // One database engine per server: the list carries only the one that was
  // installed (or tried); while none is ready both can be installed.
  const dbInstalled = $derived(!!(byName.percona?.installed || byName.mysql?.installed));
  const phpRows = $derived((stack ?? []).filter((c: any) => c.kind === 'php'));
  const titleOf = (name: string) => info[name]?.title ?? name;

  async function install(name: string) { error = ''; try { const r: any = await api('/stack/install', { method: 'POST', json: { component: name } }); job = r.job_id; } catch (e) { fail(e); } }
  const askInstall = (name: string): Ask => ({
    title: t('stack.askInstallTitle', { name: titleOf(name) }),
    note: name === 'composer' ? t('stack.installNoteComposer') :
      name === 'memcached' ? t('stack.installNoteMemcached') :
      name === 'valkey' ? t('stack.installNoteValkey') :
      name === 'sphinx' ? t('stack.installNoteSphinx') :
      name === 'percona' || name === 'mysql' ? t('stack.installNoteDb') :
      t('stack.installNoteDefault'),
    action: t('stack.install'),
    run: () => install(name)
  });
  // mp stack install on an installed component runs the installation again:
  // the way to repair one that broke (composer: to update it). The database
  // is marked «installing» for the whole run and stays «error» if it fails,
  // so the panel stops serving it until a retry succeeds — hence danger.
  const askReinstall = (name: string): Ask => {
    const db = name === 'percona' || name === 'mysql';
    return {
      title: t('stack.askReinstallTitle', { name: titleOf(name) }),
      note: db ? t('stack.reinstallNoteDb') : t('stack.reinstallNoteDefault'),
      danger: db, action: t('stack.reinstall'),
      run: () => install(name)
    };
  };
  const askRemove = (name: string): Ask => ({
    title: t('stack.askRemoveTitle', { name: titleOf(name) }),
    note: name === 'memcached' ? t('stack.removeNoteMemcached') :
      name === 'valkey' ? t('stack.removeNoteValkey') :
      name === 'composer' ? t('stack.removeNoteComposer') :
      name === 'sphinx' ? t('stack.removeNoteSphinx') :
      t('stack.removeNoteDefault'),
    danger: true, action: t('stack.remove'),
    run: async () => { const r: any = await api(`/stack/${name}`, { method: 'DELETE' }); job = r.job_id; }
  });
  async function saveMem(e: Event) { e.preventDefault(); error = ''; try { mem = await api('/stack/memcached', { method: 'PUT', json: memForm }); notify(mem.installed ? t('stack.memReconfigured') : t('stack.memSavedPending')); } catch (e) { fail(e); } }
  // Apache runs one server per account with sites in apache mode: the state is how many run.
  const stateOf = (c: any) => !c?.installed ? t('stack.stateNotInstalled') : c.instances?.total ? t('stack.stateInstances', { running: c.instances.running, total: c.instances.total }) : c.service ? `${c.service.active_state}` : t('stack.stateInstalled');
  const tagOf = (c: any) => (c?.installed ? ((c.service && c.service.active_state !== 'active') || (c.instances && c.instances.running < c.instances.total) ? 'tag-err' : 'tag-ok') : c?.version ? 'tag-err' : 'tag-muted');
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
            {@const db = name === 'percona' || name === 'mysql'}
            <tr>
              <td data-label={t('stack.colComponent')} class="font-medium whitespace-nowrap">{titleOf(name)}</td>
              <td data-label={t('stack.colWhat')} class="text-sm text-muted max-w-md">{info[name] ? t(info[name].text) : ''}</td>
              <td data-label={t('stack.colVersion')} class="font-mono text-xs">{c?.installed ? c.version || '—' : '—'}</td>
              <td data-label={t('stack.colState')}><span class="tag {tagOf(c)}">{stateOf(c)}</span>{#if c && !c.installed && c.version}<div class="text-xs text-danger mt-1 max-w-xs break-words">{c.version}</div>{/if}</td>
              <td data-label=""><div class="row-actions">
                {#if !c?.installed}
                  {#if db && dbInstalled}<span class="text-xs text-muted">{t('stack.otherDbInstalled')}</span>
                  {:else}<button class="btn btn-sm btn-primary" onclick={() => (ask = askInstall(name))}><Icon name="download" size={13} /> {c?.version ? t('stack.btnRetry') : t('stack.btnInstall')}</button>{/if}
                {:else}
                  {#if name === 'composer'}<button class="btn btn-sm" onclick={() => (ask = askInstall(name))}><Icon name="refresh" size={13} /> {t('stack.btnUpdate')}</button>
                  {:else}<button class="btn btn-sm" onclick={() => (ask = askReinstall(name))} title={t('stack.reinstallTitle')} aria-label="{t('stack.reinstallTitle')}: {titleOf(name)}"><Icon name="refresh" size={13} /></button>{/if}
                  {#if c.removable}<button class="btn btn-sm btn-danger" onclick={() => (ask = askRemove(name))} title={t('stack.remove')} aria-label="{t('stack.remove')}: {titleOf(name)}"><Icon name="trash" size={13} /></button>{/if}
                {/if}
              </div></td>
            </tr>
          {/each}
        </tbody></table>
    </div>
    {#if group === 'stack.groupDatabase'}
      <div class="card overflow-x-auto p-0 mb-4 rise" style="--i:{gi}">
        <div class="px-4 pt-3 font-medium">PHP</div>
        <table class="tbl"><thead><tr><th>{t('stack.colComponent')}</th><th>{t('stack.colWhat')}</th><th>{t('stack.colVersion')}</th><th>{t('stack.colState')}</th><th></th></tr></thead>
          <tbody>
            {#each phpRows as c}
              <tr>
                <td data-label={t('stack.colComponent')} class="font-medium whitespace-nowrap font-mono">{c.name}</td>
                <td data-label={t('stack.colWhat')} class="text-sm text-muted max-w-md">{t('stack.infoPhp', { version: c.name.replace(/^php-/, '') })}</td>
                <td data-label={t('stack.colVersion')} class="font-mono text-xs">{c.version || '—'}</td>
                <td data-label={t('stack.colState')}><span class="tag {c.installed ? (c.service && c.service.active_state !== 'active' ? 'tag-err' : 'tag-ok') : 'tag-warn'}">{c.installed ? stateOf(c) : t('stack.statePhpNotReady')}</span></td>
                <td data-label=""><div class="row-actions"><a class="btn btn-sm whitespace-nowrap" href="/php"><Icon name="code" size={13} /> {t('stack.phpPage')}</a></div></td>
              </tr>
            {/each}
            {#if !phpRows.length}<tr><td colspan="5" class="text-muted text-sm py-4 text-center">{t('stack.noPhp')} <a class="link" href="/php">{t('stack.phpPage')}</a></td></tr>{/if}
          </tbody></table>
      </div>
    {/if}
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
