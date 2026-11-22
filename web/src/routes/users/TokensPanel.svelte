<script lang="ts">
  import { untrack } from 'svelte';
  import { api, when } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import { t } from '$lib/i18n/index.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import { copyText, errText, scopeList } from './lib';

  // API tokens of an account. An administrator names the account (?user= and
  // "user" in the body); an ordinary account may only list and issue its own,
  // and the API answers 403 to a "user" from it, so then neither is sent.
  let { login, admin = false, self = false, nonce = 0 }: { login: string; admin?: boolean; self?: boolean; nonce?: number } = $props();
  const uid = $props.id();
  let items = $state<any[] | null>(null);
  let error = $state('');
  let ask = $state<Ask | null>(null);
  let form = $state({ name: '', scopes: '', days: 0 });
  let created = $state<{ name: string; token: string } | null>(null);
  let busy = $state(false);
  const fail = (e: unknown) => { error = errText(e); notify(error, 'err'); };
  async function reload() { try { items = admin ? await api(`/tokens?user=${encodeURIComponent(login)}`) : await api('/tokens'); } catch (e) { fail(e); } }
  $effect(() => { void nonce; untrack(reload); });
  async function create(e: Event) {
    e.preventDefault(); if (busy) return;
    error = ''; busy = true;
    const body: any = { name: form.name.trim() };
    const scopes = scopeList(form.scopes);
    if (scopes.length) body.scopes = scopes;
    if (form.days > 0) body.expires_in_days = form.days;
    if (admin) body.user = login;
    try {
      const r: any = await api('/tokens', { method: 'POST', json: body });
      created = { name: r.record?.name ?? body.name, token: r.token };
      form = { name: '', scopes: '', days: 0 };
      await reload();
    } catch (e) { fail(e); } finally { busy = false; }
  }
  async function revoke(id: number) { error = ''; try { await api(`/tokens/${id}`, { method: 'DELETE' }); notify(t('users.tokenRevoked')); await reload(); } catch (e) { fail(e); } }
  const askRevoke = (tk: any): Ask => ({ title: t('users.askTokenTitle', { name: tk.name }), note: t('users.askTokenNote'), danger: true, action: t('users.askTokenAction'), run: () => revoke(tk.id) });
  const expired = (tk: any) => !!tk.expires_at && new Date(tk.expires_at).getTime() < Date.now();
  async function copy() { if (created && (await copyText(created.token))) notify(t('users.tokenCopied')); }
</script>

{#if error}<p class="text-danger text-sm mb-2">{error}</p>{/if}
<form class="grid md:grid-cols-6 gap-2 items-end mb-1" onsubmit={create}>
  <div class="md:col-span-2"><label class="label" for="{uid}-n">{t('common.name')}</label><input id="{uid}-n" class="input" bind:value={form.name} maxlength="64" placeholder="billing" required /></div>
  <div class="md:col-span-2"><label class="label" for="{uid}-s">{t('users.tokenScopes')}</label><input id="{uid}-s" class="input font-mono" bind:value={form.scopes} placeholder="billing, backup" /></div>
  <div><label class="label" for="{uid}-d">{t('users.tokenDays')}</label><input id="{uid}-d" class="input" type="number" min="0" max="3650" bind:value={form.days} /></div>
  <button class="btn btn-primary" disabled={busy}><Icon name="plus" size={14} /> {t('common.create')}</button>
</form>
<p class="text-xs text-muted mb-3">{t('users.tokenHint')}{admin && !self ? ' ' + t('users.tokenActsAs', { login }) : ''}</p>
{#if created}
  <div class="rounded-lg border border-accent/40 bg-accent-soft p-3 mb-3 text-sm">
    <div class="flex flex-wrap items-center justify-between gap-2 mb-1"><span class="font-medium text-accent-ink">{t('users.tokenCreated', { name: created.name })}</span><span class="flex gap-1"><button class="btn btn-sm" onclick={copy}>{t('users.copy')}</button><button class="btn btn-sm btn-ghost" onclick={() => (created = null)} title={t('common.close')}><Icon name="x" size={13} /></button></span></div>
    <div class="font-mono text-xs break-all select-all">{created.token}</div>
    <div class="text-xs text-muted mt-1">{t('users.tokenShownOnce')} <span class="font-mono">mp --server https://{location.host} --token … status</span></div>
  </div>
{/if}
{#if !items}<Skeleton rows={2} />{:else}
  <table class="tbl">
    <thead><tr><th>ID</th><th>{t('common.name')}</th><th>{t('users.tokenScopes')}</th><th>{t('users.tokenExpires')}</th><th>{t('users.tokenLastUsed')}</th><th>{t('users.colCreated')}</th><th></th></tr></thead>
    <tbody>
      {#each items as tk (tk.id)}
        <tr>
          <td data-label="ID" class="tabular-nums">{tk.id}</td>
          <td data-label={t('common.name')} class="font-medium">{tk.name}</td>
          <td data-label={t('users.tokenScopes')}>{#each tk.scopes || [] as sc}<span class="tag {sc.startsWith('migrate:') ? 'tag-warn' : 'tag-muted'} mr-1">{sc}</span>{:else}<span class="text-muted">—</span>{/each}</td>
          <td data-label={t('users.tokenExpires')} class="text-xs">{#if !tk.expires_at}<span class="text-muted">{t('users.tokenNever')}</span>{:else if expired(tk)}<span class="tag tag-err">{t('users.tokenExpired')}</span> {when(tk.expires_at)}{:else}{when(tk.expires_at)}{/if}</td>
          <td data-label={t('users.tokenLastUsed')} class="text-xs text-muted">{tk.last_used_at ? when(tk.last_used_at) : t('users.tokenUnused')}</td>
          <td data-label={t('users.colCreated')} class="text-xs text-muted">{when(tk.created_at)}</td>
          <td data-label=""><div class="row-actions"><button class="btn btn-danger btn-sm" onclick={() => (ask = askRevoke(tk))} title={t('users.askTokenAction')}><Icon name="trash" size={13} /></button></div></td>
        </tr>
      {/each}
      {#if !items.length}<Empty text={t('users.noTokens')} cols={7} />{/if}
    </tbody>
  </table>
{/if}

<Confirm bind:ask />
