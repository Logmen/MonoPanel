<script lang="ts">
  import { untrack } from 'svelte';
  import { api } from '$lib/api';
  import { auth, notify } from '$lib/state.svelte';
  import { t } from '$lib/i18n/index.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import { errText, withUnix, type Account } from './lib';

  // Valkey of an account: a cache and a PHP sessions instance, memory in MB.
  // Without login: every instance on the server (GET /valkey, administrators),
  // and one form creates an instance for any account or changes its memory.
  let { login = '', accounts = [], nonce = 0 }: { login?: string; accounts?: Account[]; nonce?: number } = $props();
  const uid = $props.id();
  const purposes = ['cache', 'sessions'] as const;
  type Purpose = (typeof purposes)[number];
  const defaults: Record<Purpose, number> = { cache: 128, sessions: 64 };
  const all = $derived(!login);
  // Installing the server is the Stack page's business, and that page is for
  // administrators: an ordinary account gets told who does it instead of a link.
  const admin = $derived(auth.me?.role === 'admin');
  const owners = $derived(withUnix(accounts));
  let list = $state<{ engine?: string; instances: any[] } | null>(null);
  let mem = $state<Record<Purpose, number>>({ ...defaults });
  let form = $state<{ owner: string; purpose: Purpose; memory: number }>({ owner: '', purpose: 'cache', memory: defaults.cache });
  let busy = $state('');
  let error = $state('');
  let ask = $state<Ask | null>(null);
  const fail = (e: unknown) => { error = errText(e); notify(error, 'err'); };
  const title = (p: string) => (p === 'cache' ? t('users.vkCache') : t('users.vkSessions'));
  const of = (p: Purpose) => list?.instances.find((v) => v.instance.purpose === p);
  const stateOf = (v: any): string => v.service?.active_state || v.instance.status || 'unknown';
  const keyOf = (v: any) => v.instance.login + '-' + v.instance.purpose;

  async function reload() {
    try {
      const r: any = login ? await api(`/users/${login}/valkey`) : await api('/valkey');
      list = r;
      if (login) for (const v of r.instances) mem[v.instance.purpose as Purpose] = v.instance.memory_mb;
    } catch (e) { fail(e); }
  }
  $effect(() => { void nonce; untrack(reload); });
  $effect(() => { if (all && !form.owner && owners.length) form.owner = owners[0].login; });
  // The memory field follows the instance the form points at: its own size
  // when it exists, the default for a new one.
  $effect(() => {
    if (!all) return;
    const owner = form.owner, purpose = form.purpose, instances = list?.instances;
    untrack(() => { const v = instances?.find((x) => x.instance.login === owner && x.instance.purpose === purpose); form.memory = v ? v.instance.memory_mb : defaults[purpose]; });
  });

  async function put(owner: string, purpose: Purpose, memory: number) {
    const had = !!list?.instances.some((v) => v.instance.login === owner && v.instance.purpose === purpose);
    busy = owner + '-' + purpose; error = '';
    try {
      await api(`/users/${owner}/valkey/${purpose}`, { method: 'PUT', json: { memory_mb: memory } });
      notify(t(had ? 'users.vkSaved' : 'users.vkStarted', { name: all ? `${owner}: ${title(purpose)}` : title(purpose) }));
      await reload();
    } catch (e) { fail(e); } finally { busy = ''; }
  }
  async function restart(owner: string, purpose: Purpose) {
    busy = owner + '-' + purpose; error = '';
    try { await api(`/users/${owner}/valkey/${purpose}/restart`, { method: 'POST' }); notify(t('users.vkRestarted', { name: all ? `${owner}: ${title(purpose)}` : title(purpose) })); await reload(); } catch (e) { fail(e); } finally { busy = ''; }
  }
  async function rm(owner: string, purpose: Purpose) {
    error = '';
    try { await api(`/users/${owner}/valkey/${purpose}`, { method: 'DELETE' }); await reload(); } catch (e) { fail(e); }
  }
  const askRm = (owner: string, purpose: Purpose): Ask => ({
    title: purpose === 'cache' ? t('users.askVkCacheTitle', { login: owner }) : t('users.askVkSessionsTitle', { login: owner }),
    note: purpose === 'cache' ? t('users.askVkCacheNote') : t('users.askVkSessionsNote'),
    danger: true, action: t('common.delete'),
    run: () => rm(owner, purpose)
  });
  function pick(v: any) { form = { owner: v.instance.login, purpose: v.instance.purpose, memory: v.instance.memory_mb }; }
  const formHas = $derived(!!list?.instances.some((v) => v.instance.login === form.owner && v.instance.purpose === form.purpose));
</script>

{#if error}<p class="text-danger text-sm mb-2">{error}</p>{/if}
{#if !list}<Skeleton rows={3} />
{:else if !all && !list.engine}
  <p class="text-sm text-muted">{t('users.vkNotInstalled')} {#if admin}<a href="/stack" class="text-accent-ink hover:underline">{t('users.vkToStack')}</a>{:else}{t('users.vkAskAdmin')}{/if}</p>
{:else if !all}
  <div class="grid md:grid-cols-2 gap-3">
    {#each purposes as purpose}
      {@const v = of(purpose)}
      {@const st = v ? stateOf(v) : ''}
      <form class="rounded-lg border border-line p-3 flex flex-col gap-2" onsubmit={(e) => { e.preventDefault(); put(login, purpose, mem[purpose]); }}>
        <div class="flex items-start justify-between gap-2">
          <div><div class="font-medium">{title(purpose)}</div><div class="text-xs text-muted">{purpose === 'cache' ? t('users.vkCacheHint') : t('users.vkSessionsHint')}</div></div>
          {#if v}<span class="tag {st === 'active' ? 'tag-ok' : st === 'failed' ? 'tag-err' : 'tag-muted'}">{#if st === 'active'}<span class="dot dot-live"></span>{/if}{st}{v.service?.sub_state ? '/' + v.service.sub_state : ''}</span>{:else}<span class="tag tag-muted">{t('users.vkNone')}</span>{/if}
        </div>
        {#if v}<div class="text-xs font-mono break-all">{v.socket}</div>{/if}
        {#if v?.instance.last_error}<div class="text-xs text-danger break-words">{v.instance.last_error}</div>{/if}
        <div class="flex items-end gap-2 mt-auto">
          <div class="flex-1"><label class="label" for="{uid}-{purpose}">{t('users.vkMemory')}</label><input id="{uid}-{purpose}" class="input" type="number" min="16" max="8192" required bind:value={mem[purpose]} /></div>
          <button class="btn btn-primary" disabled={busy === login + '-' + purpose}>{v ? t('common.save') : t('common.create')}</button>
          {#if v}
            <button type="button" class="btn" disabled={busy === login + '-' + purpose} onclick={() => restart(login, purpose)} title={t('users.restart')}><Icon name="refresh" size={13} /></button>
            <button type="button" class="btn btn-danger" onclick={() => (ask = askRm(login, purpose))} title={t('users.delete')}><Icon name="trash" size={13} /></button>
          {/if}
        </div>
        {#if v}<p class="text-xs text-muted break-words">{purpose === 'cache' ? t('users.vkCacheUse', { socket: v.socket }) : t('users.vkSessionsUse')}</p>{/if}
      </form>
    {/each}
  </div>
  <p class="text-xs text-muted mt-3">{t('users.vkIsolation', { login, engine: list.engine === 'valkey' ? 'Valkey' : 'Redis' })}</p>
{:else}
  {#if !list.engine}
    <p class="text-sm text-muted mb-3">{t('users.vkNotInstalled')} <a href="/stack" class="text-accent-ink hover:underline">{t('users.vkToStack')}</a></p>
  {:else if owners.length}
    <form class="grid sm:grid-cols-2 md:grid-cols-4 gap-2 items-end mb-3" onsubmit={(e) => { e.preventDefault(); put(form.owner, form.purpose, form.memory); }}>
      <div><label class="label" for="{uid}-o">{t('users.owner')}</label><select id="{uid}-o" class="input" bind:value={form.owner} required>{#each owners as a}<option value={a.login}>{a.login}</option>{/each}</select></div>
      <div><label class="label" for="{uid}-p">{t('users.vkInstance')}</label><select id="{uid}-p" class="input" bind:value={form.purpose}>{#each purposes as p}<option value={p}>{title(p)}</option>{/each}</select></div>
      <div><label class="label" for="{uid}-m">{t('users.vkMemory')}</label><input id="{uid}-m" class="input" type="number" min="16" max="8192" required bind:value={form.memory} /></div>
      <button class="btn btn-primary" disabled={busy === form.owner + '-' + form.purpose}>{formHas ? t('users.vkChangeMemory') : t('common.create')}</button>
    </form>
  {:else}<p class="text-sm text-muted mb-3">{t('users.noOwners')}</p>{/if}
  <table class="tbl">
    <thead><tr><th>{t('users.owner')}</th><th>{t('users.vkInstance')}</th><th>{t('users.vkMemoryCol')}</th><th>{t('users.colState')}</th><th>{t('users.vkSocket')}</th><th></th></tr></thead>
    <tbody>
      {#each list.instances as v (keyOf(v))}
        {@const st = stateOf(v)}
        <tr>
          <td data-label={t('users.owner')} class="font-mono">{v.instance.login}</td>
          <td data-label={t('users.vkInstance')}>{title(v.instance.purpose)}</td>
          <td data-label={t('users.vkMemoryCol')} class="tabular-nums">{v.instance.memory_mb} {t('common.unit.mb')}</td>
          <td data-label={t('users.colState')}><span class="tag {st === 'active' ? 'tag-ok' : st === 'failed' ? 'tag-err' : 'tag-muted'}">{#if st === 'active'}<span class="dot dot-live"></span>{/if}{st}{v.service?.sub_state ? '/' + v.service.sub_state : ''}</span>{#if v.instance.last_error}<div class="text-xs text-danger break-words mt-0.5">{v.instance.last_error}</div>{/if}</td>
          <td data-label={t('users.vkSocket')} class="font-mono text-xs break-all">{v.socket}</td>
          <td data-label=""><div class="row-actions">
            <button class="btn btn-sm" onclick={() => pick(v)} title={t('users.vkChangeMemory')}><Icon name="pencil" size={13} /></button>
            <button class="btn btn-sm" disabled={busy === keyOf(v)} onclick={() => restart(v.instance.login, v.instance.purpose)} title={t('users.restart')}><Icon name="refresh" size={13} /></button>
            <button class="btn btn-danger btn-sm" onclick={() => (ask = askRm(v.instance.login, v.instance.purpose))} title={t('users.delete')}><Icon name="trash" size={13} /></button>
          </div></td>
        </tr>
      {/each}
      {#if !list.instances.length}<Empty text={t('users.vkNoneAll')} cols={6} />{/if}
    </tbody>
  </table>
  {#if list.engine}<p class="text-xs text-muted mt-2">{t('users.vkAllNote', { engine: list.engine === 'valkey' ? 'Valkey' : 'Redis' })}</p>{/if}
{/if}

<Confirm bind:ask />
