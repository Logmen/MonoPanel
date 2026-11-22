<script lang="ts">
  import { untrack } from 'svelte';
  import { api } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import { t } from '$lib/i18n/index.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import { errText, withUnix, type Account } from './lib';

  // Cron jobs of one account, or of every account when login is empty (GET /cron,
  // administrators): then the owner is a column and the add form asks for it.
  let { login = '', accounts = [], nonce = 0 }: { login?: string; accounts?: Account[]; nonce?: number } = $props();
  const uid = $props.id();
  const all = $derived(!login);
  const owners = $derived(withUnix(accounts, false));
  let items = $state<any[] | null>(null);
  let error = $state('');
  let ask = $state<Ask | null>(null);
  let form = $state({ owner: '', schedule: '*/5 * * * *', command: '', comment: '', enabled: true });
  let edit = $state<{ id: number; login: string; schedule: string; command: string; comment: string; enabled: boolean } | null>(null);
  const fail = (e: unknown) => { error = errText(e); notify(error, 'err'); };
  const whose = (j: any): string => j.login || login;
  async function reload() { try { items = login ? await api(`/users/${login}/cron`) : await api('/cron'); } catch (e) { fail(e); } }
  $effect(() => { void nonce; untrack(reload); });
  $effect(() => { if (all && !form.owner && owners.length) form.owner = owners[0].login; });
  async function add(e: Event) {
    e.preventDefault(); error = '';
    const owner = login || form.owner;
    if (!owner) return;
    const body: any = { schedule: form.schedule.trim(), command: form.command.trim(), enabled: form.enabled };
    if (form.comment.trim()) body.comment = form.comment.trim();
    try { const j: any = await api(`/users/${owner}/cron`, { method: 'POST', json: body }); form.command = ''; form.comment = ''; form.enabled = true; notify(t('users.cronAdded', { id: j.id, login: owner })); await reload(); } catch (e) { fail(e); }
  }
  async function save(e: Event) {
    e.preventDefault(); if (!edit) return; error = '';
    const body = { schedule: edit.schedule.trim(), command: edit.command.trim(), comment: edit.comment.trim(), enabled: edit.enabled };
    try { await api(`/users/${edit.login}/cron/${edit.id}`, { method: 'PATCH', json: body }); edit = null; notify(t('users.cronSaved')); await reload(); } catch (e) { fail(e); }
  }
  async function toggle(j: any) { error = ''; try { await api(`/users/${whose(j)}/cron/${j.id}`, { method: 'PATCH', json: { enabled: !j.enabled } }); notify(t(j.enabled ? 'users.cronOff' : 'users.cronOn', { id: j.id })); await reload(); } catch (e) { fail(e); } }
  async function rm(j: any) { error = ''; try { await api(`/users/${whose(j)}/cron/${j.id}`, { method: 'DELETE' }); await reload(); } catch (e) { fail(e); } }
  const askRm = (j: any): Ask => ({ title: t('users.askCronTitle'), note: t('users.askCronNote', { schedule: j.schedule }), danger: true, action: t('common.delete'), run: () => rm(j) });
  const cols = $derived(all ? 6 : 5);
</script>

{#if error}<p class="text-danger text-sm mb-2">{error}</p>{/if}
{#if all && !owners.length}
  <p class="text-sm text-muted mb-3">{t('users.noOwners')}</p>
{:else}
  <form class="grid md:grid-cols-6 gap-2 items-end mb-1" onsubmit={add}>
    {#if all}<div><label class="label" for="{uid}-o">{t('users.owner')}</label><select id="{uid}-o" class="input" bind:value={form.owner} required>{#each owners as a}<option value={a.login}>{a.login}</option>{/each}</select></div>{/if}
    <div><label class="label" for="{uid}-s">{t('users.schedule')}</label><input id="{uid}-s" class="input font-mono" bind:value={form.schedule} maxlength="64" required /></div>
    <div class="md:col-span-2"><label class="label" for="{uid}-c">{t('users.command')}</label><input id="{uid}-c" class="input font-mono" bind:value={form.command} placeholder="php ~/data/www/site/cron.php" maxlength="1000" required /></div>
    <div class={all ? '' : 'md:col-span-2'}><label class="label" for="{uid}-m">{t('users.comment')}</label><input id="{uid}-m" class="input" bind:value={form.comment} maxlength="120" /></div>
    <div class="flex flex-wrap items-center justify-between gap-2"><label class="text-sm flex items-center gap-1.5"><input type="checkbox" bind:checked={form.enabled} /> {t('users.cronEnabled')}</label><button class="btn btn-primary">{t('common.add')}</button></div>
  </form>
  <p class="text-xs text-muted mb-3">{t('users.cronHint')}</p>
{/if}
<form id="{uid}-edit" onsubmit={save}></form>
{#if !items}<Skeleton rows={3} />{:else}
  <table class="tbl">
    <thead><tr>{#if all}<th>{t('users.owner')}</th>{/if}<th>ID</th><th>{t('users.schedule')}</th><th>{t('users.command')}</th><th>{t('users.comment')}</th><th></th></tr></thead>
    <tbody>
      {#each items as j (j.id)}
        {#if edit && edit.id === j.id}
          <tr>
            {#if all}<td data-label={t('users.owner')} class="font-mono">{whose(j)}</td>{/if}
            <td data-label="ID">{j.id}</td>
            <td data-label={t('users.schedule')}><input class="input font-mono" form="{uid}-edit" bind:value={edit.schedule} aria-label={t('users.schedule')} maxlength="64" required /></td>
            <td data-label={t('users.command')}><input class="input font-mono text-xs" form="{uid}-edit" bind:value={edit.command} aria-label={t('users.command')} maxlength="1000" required /></td>
            <td data-label={t('users.comment')}><input class="input text-xs" form="{uid}-edit" bind:value={edit.comment} aria-label={t('users.comment')} maxlength="120" /><label class="text-xs flex items-center gap-1.5 mt-1.5"><input type="checkbox" form="{uid}-edit" bind:checked={edit.enabled} /> {t('users.cronEnabled')}</label></td>
            <td data-label=""><div class="row-actions"><button class="btn btn-primary btn-sm" form="{uid}-edit" title={t('common.save')}><Icon name="check" size={13} /></button><button class="btn btn-sm" type="button" onclick={() => (edit = null)} title={t('common.cancel')}><Icon name="x" size={13} /></button></div></td>
          </tr>
        {:else}
          <tr>
            {#if all}<td data-label={t('users.owner')} class="font-mono">{whose(j)}</td>{/if}
            <td data-label="ID">{j.id}</td>
            <td data-label={t('users.schedule')} class="font-mono">{j.schedule}{#if !j.enabled}<span class="tag tag-muted ml-1.5">{t('common.off')}</span>{/if}</td>
            <td data-label={t('users.command')} class="font-mono text-xs break-all">{j.command}</td>
            <td data-label={t('users.comment')} class="text-xs text-muted">{j.comment || ''}</td>
            <td data-label=""><div class="row-actions"><button class="btn btn-sm" onclick={() => (edit = { id: j.id, login: whose(j), schedule: j.schedule, command: j.command, comment: j.comment || '', enabled: j.enabled })} title={t('users.cronEdit')}><Icon name="pencil" size={13} /></button><button class="btn btn-sm" onclick={() => toggle(j)} title={j.enabled ? t('users.cronDisable') : t('users.cronEnable')}><Icon name={j.enabled ? 'stop' : 'play'} size={13} /></button><button class="btn btn-danger btn-sm" onclick={() => (ask = askRm(j))} title={t('users.delete')}><Icon name="trash" size={13} /></button></div></td>
          </tr>
        {/if}
      {/each}
      {#if !items.length}<Empty text={all ? t('users.noCronAll') : t('users.noCron')} {cols} />{/if}
    </tbody>
  </table>
{/if}

<Confirm bind:ask />
