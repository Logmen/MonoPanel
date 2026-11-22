<script lang="ts">
  import { api, when } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import { t } from '$lib/i18n/index.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import { errText, type Account } from './lib';

  // Everything GET /users/{login} returns, plus what an account may change
  // about itself (e-mail, password) and, for an administrator, the 2FA reset.
  let { user, admin = false, self = false, onchanged, onpassword }: { user: Account; admin?: boolean; self?: boolean; onchanged?: () => unknown; onpassword?: () => void } = $props();
  let email = $state<string | null>(null);
  let busy = $state(false);
  let error = $state('');
  let ask = $state<Ask | null>(null);
  const fail = (e: unknown) => { error = errText(e); notify(error, 'err'); };
  async function saveEmail(e: Event) {
    e.preventDefault(); if (email === null || busy) return;
    error = ''; busy = true;
    try { await api(`/users/${user.login}`, { method: 'PATCH', json: { email: email.trim() } }); email = null; notify(t('users.emailSaved', { login: user.login })); await onchanged?.(); } catch (e) { fail(e); } finally { busy = false; }
  }
  async function resetTotp() { error = ''; try { await api(`/users/${user.login}/totp/reset`, { method: 'POST' }); notify(t('users.totpResetDone', { login: user.login })); } catch (e) { fail(e); } }
  const askTotp = (): Ask => ({ title: t('users.askTotpTitle', { login: user.login }), note: t('users.askTotpNote'), action: t('users.askTotpAction'), danger: true, run: resetTotp });
  const access = $derived(user.unix_uid == null ? t('users.noUnix') : user.shell ? 'SSH shell' : 'SFTP-only');
</script>

{#if error}<p class="text-danger text-sm mb-2">{error}</p>{/if}
<dl class="grid sm:grid-cols-2 lg:grid-cols-4 gap-x-6 gap-y-3 text-sm">
  <div><dt class="label">{t('users.login')}</dt><dd class="font-mono">{user.login} <span class="text-xs text-muted">#{user.id}</span></dd></div>
  <div><dt class="label">{t('users.role')}</dt><dd>{user.role}</dd></div>
  <div><dt class="label">{t('common.status')}</dt><dd><span class="tag {user.status === 'active' ? 'tag-ok' : user.status === 'deleting' ? 'tag-err' : 'tag-warn'}">{user.status}</span></dd></div>
  <div><dt class="label">{t('users.colAccess')}</dt><dd>{access}</dd></div>
  <div class="sm:col-span-2">
    <dt class="label">E-mail</dt>
    <dd>
      {#if email === null}
        <span class="break-all">{user.email || '—'}</span> <button class="btn btn-ghost btn-sm" onclick={() => (email = user.email ?? '')} title={t('users.emailEdit')}><Icon name="pencil" size={13} /></button>
      {:else}
        <form class="flex gap-1.5" onsubmit={saveEmail}><input class="input" type="email" bind:value={email} aria-label="E-mail" required /><button class="btn btn-primary btn-sm" disabled={busy} title={t('common.save')}><Icon name="check" size={13} /></button><button class="btn btn-sm" type="button" onclick={() => (email = null)} title={t('common.cancel')}><Icon name="x" size={13} /></button></form>
      {/if}
    </dd>
  </div>
  <div><dt class="label">UID / GID</dt><dd class="font-mono">{user.unix_uid ?? '—'} / {user.unix_gid ?? '—'}</dd></div>
  <div><dt class="label">{t('users.home')}</dt><dd class="font-mono break-all">{user.home || '—'}</dd></div>
  <div><dt class="label">{t('users.quotaShort')}</dt><dd title={t('users.quotaHint')}>{user.quota_mb != null ? `${user.quota_mb} ${t('common.unit.mb')}` : '—'}</dd></div>
  <div><dt class="label">{t('users.colCreated')}</dt><dd class="text-xs">{when(user.created_at)}</dd></div>
  <div><dt class="label">{t('users.colUpdated')}</dt><dd class="text-xs">{when(user.updated_at)}</dd></div>
</dl>
<div class="flex flex-wrap items-center gap-2 mt-4">
  <button class="btn btn-sm" onclick={() => onpassword?.()}><Icon name="key" size={13} /> {t('users.changePasswordBtn')}</button>
  {#if admin}<button class="btn btn-sm" onclick={() => (ask = askTotp())}><Icon name="lock" size={13} /> {t('users.totpReset')}</button>{/if}
  {#if self}<a class="btn btn-sm btn-ghost" href="/settings"><Icon name="lock" size={13} /> {t('users.totpOwn')}</a>{/if}
</div>

<Confirm bind:ask />
