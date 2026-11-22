<script lang="ts">
  import { untrack } from 'svelte';
  import { api } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import { t } from '$lib/i18n/index.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import { errText, type Account } from './lib';

  // Shared folders: a folder of another account's site, bind-mounted into this
  // account's home. Only an administrator gives and takes them; the account
  // itself sees the list.
  let { account, admin = false, nonce = 0, onjob }: { account: Account; admin?: boolean; nonce?: number; onjob?: (id: number) => void } = $props();
  const uid = $props.id();
  const login = $derived(account.login);
  let items = $state<any[] | null>(null);
  let sites = $state<any[]>([]);
  let error = $state('');
  let ask = $state<Ask | null>(null);
  const blank = () => ({ site: '', path: '', name: '', no_php: true, entry: false });
  let form = $state(blank());
  const fail = (e: unknown) => { error = errText(e); notify(error, 'err'); };
  async function reload() { try { items = await api(`/users/${account.login}/shares`); } catch (e) { fail(e); } }
  $effect(() => { void nonce; untrack(reload); });
  // Sites of the other accounts, as suggestions for the domain field.
  $effect(() => { if (admin) untrack(async () => { try { const all: any[] = await api('/sites'); sites = all.filter((s) => s.login !== account.login); } catch { sites = []; } }); });
  async function add(e: Event) {
    e.preventDefault(); error = '';
    const body: any = { site: form.site.trim(), path: form.path.trim(), no_php: form.no_php, entry: form.entry };
    if (form.name.trim()) body.name = form.name.trim();
    try {
      const r: any = await api(`/users/${login}/shares`, { method: 'POST', json: body });
      if (r?.job_id) onjob?.(r.job_id);
      form = blank();
      notify(t('users.shareAdded'));
      await reload();
    } catch (e) { fail(e); }
  }
  async function entry(name: string, on: boolean) {
    error = '';
    try { await api(`/users/${login}/shares/${name}`, { method: 'PATCH', json: { entry: on } }); notify(t(on ? 'users.shareEntrySet' : 'users.shareEntryUnset')); await reload(); } catch (e) { fail(e); }
  }
  async function rm(name: string) {
    error = '';
    try { const r: any = await api(`/users/${login}/shares/${name}`, { method: 'DELETE' }); if (r?.job_id) onjob?.(r.job_id); notify(t('users.shareRemoved', { name })); await reload(); } catch (e) { fail(e); }
  }
  const askRm = (sh: any): Ask => ({
    title: t('users.askShareTitle', { name: sh.share.name, login }),
    note: t('users.askShareNote', { folder: sh.share.domain + '/' + sh.share.path }),
    danger: true, action: t('users.askShareAction'),
    run: () => rm(sh.share.name)
  });
</script>

{#if error}<p class="text-danger text-sm mb-2">{error}</p>{/if}
{#if admin}
  <form class="grid md:grid-cols-5 gap-2 items-end mb-3" onsubmit={add}>
    <div><label class="label" for="{uid}-s">{t('users.shareSite')}</label><input id="{uid}-s" class="input font-mono" bind:value={form.site} list="{uid}-sites" placeholder="shop.example.com" required /><datalist id="{uid}-sites">{#each sites as s}<option value={s.domain}>{s.login}</option>{/each}</datalist></div>
    <div><label class="label" for="{uid}-p">{t('users.sharePath')}</label><input id="{uid}-p" class="input font-mono" bind:value={form.path} placeholder="upload/exchange" maxlength="200" required /></div>
    <div><label class="label" for="{uid}-n">{t('users.shareName')}</label><input id="{uid}-n" class="input font-mono" bind:value={form.name} placeholder="exchange" pattern="[a-z0-9][a-z0-9_\-]{'{'}0,31{'}'}" /></div>
    <div class="flex flex-col gap-1 pb-2"><label class="flex items-center gap-1.5 text-sm"><input type="checkbox" bind:checked={form.no_php} /> {t('users.shareNoPHP')}</label><label class="flex items-center gap-1.5 text-sm {account.shell ? 'opacity-50' : ''}" title={account.shell ? t('users.shareEntryShell') : ''}><input type="checkbox" bind:checked={form.entry} disabled={account.shell} /> {t('users.shareEntry')}</label></div>
    <button class="btn btn-primary">{t('common.add')}</button>
  </form>
{/if}
{#if !items}<Skeleton rows={3} />{:else}
  <table class="tbl">
    <thead><tr><th>{t('common.name')}</th><th>{t('users.shareInHome')}</th><th>{t('users.shareFolder')}</th><th>{t('users.colState')}</th><th>PHP</th><th>{t('users.shareEntryCol')}</th>{#if admin}<th></th>{/if}</tr></thead>
    <tbody>
      {#each items as sh (sh.share.name)}
        <tr>
          <td data-label={t('common.name')} class="font-mono">{sh.share.name}</td>
          <td data-label={t('users.shareInHome')} class="font-mono text-xs break-all">{sh.where || '—'}</td>
          <td data-label={t('users.shareFolder')} class="font-mono text-xs break-all">{sh.share.domain}/{sh.share.path} <span class="text-muted">({sh.share.owner_login})</span>{#if sh.target}<div class="text-muted">{sh.target}</div>{/if}</td>
          <td data-label={t('users.colState')}><span class="tag {sh.mounted ? 'tag-ok' : 'tag-err'}">{sh.mounted ? t('users.shareMounted') : t('users.shareNotMounted')}</span>{#if sh.share.last_error}<div class="text-xs text-danger break-words mt-0.5">{sh.share.last_error}</div>{/if}</td>
          <td data-label="PHP"><span class="tag {sh.share.no_php ? 'tag-ok' : 'tag-warn'}">{sh.share.no_php ? t('users.sharePHPDenied') : t('users.sharePHPAllowed')}</span></td>
          <td data-label={t('users.shareEntryCol')}>{#if sh.share.entry}<span class="tag tag-ok">{t('users.shareEntryYes')}</span>{/if}</td>
          {#if admin}<td data-label=""><div class="row-actions"><button class="btn btn-sm {sh.share.entry ? 'btn-primary' : ''}" disabled={account.shell && !sh.share.entry} onclick={() => entry(sh.share.name, !sh.share.entry)} title={account.shell && !sh.share.entry ? t('users.shareEntryShell') : sh.share.entry ? t('users.shareEntryUnsetBtn') : t('users.shareEntrySetBtn')}>{t('users.shareEntry')}</button><button class="btn btn-danger btn-sm" onclick={() => (ask = askRm(sh))} title={t('users.delete')}><Icon name="trash" size={13} /></button></div></td>{/if}
        </tr>
      {/each}
      {#if !items.length}<Empty text={t('users.noShares')} cols={admin ? 7 : 6} />{/if}
    </tbody>
  </table>
{/if}
<p class="text-xs text-muted mt-3">{admin ? t('users.shareNote') : t('users.shareNoteSelf')}</p>

<Confirm bind:ask />
