<script lang="ts">
  import { onMount } from 'svelte';
  import QRCode from 'qrcode';
  import { api, ApiError, when } from '$lib/api';
  import { auth, notify, theme, setTheme, type ThemeMode } from '$lib/state.svelte';
  import { t, lang, setLang, dateLocale, type LangMode, type MsgKey } from '$lib/i18n/index.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  let totp = $state<any>(null);
  let setup = $state<any>(null);
  let qr = $state('');
  let code = $state('');
  let tokens = $state<any[]>([]);
  let newToken = $state('');
  let hooks = $state<any[]>([]);
  let hook = $state({ url: '', events: 'job.failed,cert.issue.*' });
  let realip = $state<any>(null);
  let realipFrom = $state('');
  let upd = $state<any>(null);
  let sel = $state<any>(null);
  let updForm = $state({ repo: '', channel: 'stable', token: '', check_hours: 24, auto_apply: false });
  let updating = $state('');
  let error = $state('');
  let msg = $state('');
  let ask = $state<Ask | null>(null);
  const admin = $derived(auth.me?.role === 'admin');
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function load() { try { totp = await api('/auth/totp'); tokens = await api('/tokens'); if (admin) { hooks = await api('/webhooks'); realip = await api('/stack/nginx/real-ip'); realipFrom = (realip.from || []).join(', '); setUpdate(await api('/system/update')); sel = await api('/system/selinux'); } } catch (e: any) { error = e.text || String(e); } }
  // Переключение SELinux: permissive только через окно с объяснением цены, обратно — сразу.
  async function setSELinux(mode: string) { try { sel = await api('/system/selinux', { method: 'PUT', json: { mode } }); notify('SELinux: ' + sel.mode, mode === 'permissive' ? 'err' : undefined); } catch (e) { fail(e); } }
  const askPermissive = (): Ask => ({
    title: t('settings.selinuxPermissiveTitle'),
    note: sel?.warning,
    danger: true, action: t('settings.selinuxPermissiveAction'),
    run: () => setSELinux('permissive')
  });
  function setUpdate(st: any) { upd = st; updForm = { repo: st.settings.repo, channel: st.settings.channel, token: '', check_hours: st.settings.check_hours, auto_apply: st.settings.auto_apply }; }
  async function saveUpdate(e: Event) { e.preventDefault(); try { setUpdate(await api('/system/update', { method: 'PUT', json: { ...updForm, token: updForm.token || undefined } })); notify(t('settings.updateSaved')); } catch (e) { fail(e); } }
  async function checkUpdate() { updating = t('settings.updateChecking'); try { setUpdate(await api('/system/update/check', { method: 'POST', json: {} })); notify(upd.available ? t('settings.updateAvailableToast', { version: upd.latest }) : t('settings.updateLatest')); } catch (e) { fail(e); } finally { updating = ''; } }
  // Установка перезапускает саму панель: ждём, пока она ответит новой версией, и перезагружаем страницу.
  const askUpdate = (): Ask => ({
    title: t('settings.updateTitle', { version: upd.latest }),
    note: t('settings.updateNote'),
    action: t('settings.updateAction'),
    run: () => applyUpdate()
  });
  async function applyUpdate() {
    try { await api('/system/update/apply', { method: 'POST', json: {} }); } catch (e) { fail(e); return; }
    updating = t('settings.updateInstalling', { version: upd.latest });
    const deadline = Date.now() + 240000;
    while (Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 3000));
      try {
        const h: any = await api('/health');
        if ((h.version || '').replace(/^v/, '') === upd.latest) { location.reload(); return; }
      } catch { /* панель перезапускается */ }
    }
    updating = '';
    notify(t('settings.updateNoResponse'), 'err');
  }
  onMount(load);
  async function startTotp() { error = ''; try { setup = await api('/auth/totp/setup', { method: 'POST' }); qr = await QRCode.toDataURL(setup.url, { width: 180, margin: 1 }); } catch (e) { fail(e); } }
  async function enableTotp(e: Event) { e.preventDefault(); error = ''; try { await api('/auth/totp/enable', { method: 'POST', json: { code } }); setup = null; code = ''; notify(t('settings.totpEnabled')); await load(); } catch (e) { fail(e); } }
  async function disableTotp() { const p = prompt(t('settings.totpDisablePrompt')); if (!p) return; try { await api('/auth/totp/disable', { method: 'POST', json: { password: p } }); notify(t('settings.totpDisabled')); await load(); } catch (e) { fail(e); } }
  async function createToken() { const name = prompt(t('settings.tokenNamePrompt'), 'api'); if (!name) return; try { const r: any = await api('/tokens', { method: 'POST', json: { name } }); newToken = r.token; await load(); } catch (e) { fail(e); } }
  const askRevoke = (tk: any): Ask => ({
    title: t('settings.tokenRevokeTitle', { name: tk.name }),
    note: t('settings.tokenRevokeNote'),
    danger: true, action: t('settings.tokenRevokeAction'),
    run: () => revoke(tk.id)
  });
  async function revoke(id: number) { await api(`/tokens/${id}`, { method: 'DELETE' }); await load(); }
  async function addHook(e: Event) { e.preventDefault(); try { const r: any = await api('/webhooks', { method: 'POST', json: { url: hook.url, events: hook.events.split(',').map((s) => s.trim()).filter(Boolean) } }); msg = t('settings.hookSecret', { secret: r.secret }); hook.url = ''; await load(); } catch (e) { fail(e); } }
  const askHook = (h: any): Ask => ({
    title: t('settings.hookDeleteTitle'),
    note: t('settings.hookDeleteNote', { url: h.url, events: h.events.join(', ') }),
    danger: true, action: t('common.delete'),
    run: () => rmHook(h.id)
  });
  async function rmHook(id: string) { await api(`/webhooks/${id}`, { method: 'DELETE' }); await load(); }
  async function saveRealIP() { try { realip = await api('/stack/nginx/real-ip', { method: 'PUT', json: { cloudflare: realip.cloudflare, from: realipFrom.split(/[\s,]+/).filter(Boolean) } }); realipFrom = (realip.from || []).join(', '); notify(t('settings.realIpSaved')); } catch (e) { fail(e); } }
  const modes: [ThemeMode, string, MsgKey][] = [['system', 'monitor', 'settings.themeSystem'], ['light', 'sun', 'settings.themeLight'], ['dark', 'moon', 'settings.themeDark']];
  const langModes: [LangMode, string, 'lang.auto' | 'lang.ru' | 'lang.en'][] = [['auto', 'monitor', 'lang.auto'], ['ru', 'globe', 'lang.ru'], ['en', 'globe', 'lang.en']];
</script>

<PageHead title={t('settings.title')} sub={t('settings.sub')} />
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if msg}<p class="text-accent-ink text-sm mb-3 font-mono">{msg}</p>{/if}
<div class="grid md:grid-cols-2 gap-4">
  <div class="card rise">
    <div class="font-medium mb-1">{t('settings.themeTitle')}</div>
    <p class="text-xs text-muted mb-3">{t('settings.themeHint')}</p>
    <div class="grid grid-cols-3 gap-2">
      {#each modes as [m, icon, key]}
        <button class="flex flex-col items-center gap-1.5 py-3 rounded-lg border transition-all duration-150 {theme.mode === m ? 'border-accent bg-accent-soft text-accent-ink' : 'border-line hover:border-line-strong text-muted hover:text-ink'}" aria-pressed={theme.mode === m} onclick={() => setTheme(m)}><Icon name={icon} size={20} /><span class="text-xs">{t(key)}</span></button>
      {/each}
    </div>
  </div>
  <div class="card rise" style="--i:1">
    <div class="font-medium mb-1">{t('lang.title')}</div>
    <p class="text-xs text-muted mb-3">{t('lang.hint')}</p>
    <div class="grid grid-cols-3 gap-2">
      {#each langModes as [m, icon, key]}
        <button class="flex flex-col items-center gap-1.5 py-3 rounded-lg border transition-all duration-150 {lang.mode === m ? 'border-accent bg-accent-soft text-accent-ink' : 'border-line hover:border-line-strong text-muted hover:text-ink'}" aria-pressed={lang.mode === m} onclick={() => setLang(m)}><Icon name={icon} size={20} /><span class="text-xs">{t(key)}</span></button>
      {/each}
    </div>
  </div>
  <div class="card rise" style="--i:2">
    <div class="font-medium mb-2">{t('settings.totpTitle')}</div>
    {#if totp?.enabled}<p class="text-sm mb-3"><span class="tag tag-ok"><Icon name="check" size={11} /> {t('settings.totpOn')}</span></p><button class="btn btn-danger btn-sm" onclick={disableTotp}>{t('settings.totpTurnOff')}</button>
    {:else if setup}
      <p class="text-sm text-muted mb-2">{t('settings.totpScan')}</p>
      {#if qr}<img src={qr} alt="QR" class="mb-2 rounded-md border border-line" />{/if}
      <div class="text-xs font-mono mb-2 break-all select-all">{setup.secret}</div>
      <form class="flex gap-2" onsubmit={enableTotp}><input class="input w-32 font-mono" bind:value={code} placeholder="123456" /><button class="btn btn-primary">{t('settings.totpConfirm')}</button></form>
    {:else}<button class="btn" onclick={startTotp}><Icon name="lock" size={14} /> {t('settings.totpSetup')}</button>{/if}
  </div>
  <div class="card rise" style="--i:3">
    <div class="flex justify-between items-center mb-2"><span class="font-medium">{t('settings.tokensTitle')}</span><button class="btn btn-sm" onclick={createToken}><Icon name="plus" size={13} /> {t('settings.tokenCreate')}</button></div>
    {#if newToken}<div class="text-xs font-mono break-all mb-2 p-2 code select-all">{newToken}<div class="text-muted">{t('settings.tokenShownOnce')}</div></div>{/if}
    <ul class="text-sm divide-y divide-line">{#each tokens as tk}<li class="flex justify-between items-center py-1.5"><span>{tk.name} <span class="text-xs text-muted">{tk.last_used_at ? t('settings.tokenUsed', { when: when(tk.last_used_at) }) : t('settings.tokenUnused')}</span></span><button class="btn btn-danger btn-sm" onclick={() => (ask = askRevoke(tk))}>{t('settings.tokenRevoke')}</button></li>{/each}</ul>
    <p class="text-xs text-muted mt-2 font-mono">mp --server https://{location.host} --token … status</p>
  </div>
  {#if admin}
    <div class="card rise" style="--i:4">
      <div class="font-medium mb-1">{t('settings.realIpTitle')}</div>
      <p class="text-xs text-muted mb-3">{t('settings.realIpHint')}</p>
      {#if realip}
        <label class="flex items-center gap-2 text-sm mb-2"><input type="checkbox" bind:checked={realip.cloudflare} /> {t('settings.realIpCloudflare')}</label>
        <label class="label" for="rf">{t('settings.realIpFrom')}</label>
        <div class="flex flex-wrap gap-2"><input id="rf" class="input font-mono min-w-0 flex-1" bind:value={realipFrom} placeholder="10.0.0.5, 192.168.1.0/24" /><button class="btn btn-primary" onclick={saveRealIP}>{t('common.apply')}</button></div>
      {/if}
    </div>
    <div class="card md:col-span-2 rise" style="--i:5">
      <div class="flex justify-between items-start gap-3 mb-2 flex-wrap">
        <div>
          <div class="font-medium">{t('settings.updateCard')}</div>
          <p class="text-xs text-muted">{t('settings.updateVersion', { version: upd?.current ?? '…' })}{#if upd?.checked_at} · {t('settings.updateChecked', { when: when(upd.checked_at) })}{/if}{#if upd?.key_pinned} · {t('settings.updateKeyPinned')}{/if}</p>
        </div>
        <div class="flex gap-2">
          <button class="btn btn-sm" onclick={checkUpdate} disabled={!!updating || !upd?.settings?.repo}><Icon name="refresh" size={13} /> {t('settings.updateCheck')}</button>
          {#if upd?.available}<button class="btn btn-primary btn-sm" onclick={() => (ask = askUpdate())} disabled={!!updating}>{t('settings.updateTo', { version: upd.latest })}</button>{/if}
        </div>
      </div>
      {#if updating}<p class="text-sm text-accent-ink mb-3">{updating}</p>{/if}
      {#if upd?.available}
        <div class="p-3 rounded-lg border border-accent bg-accent-soft mb-3">
          <div class="text-sm font-medium text-accent-ink">{t('settings.updateAvailableHead', { version: upd.latest })}{#if upd.published_at} {t('settings.updatePublishedAt', { date: new Date(upd.published_at).toLocaleDateString(dateLocale()) })}{/if}</div>
          {#if upd.notes}<pre class="text-xs whitespace-pre-wrap mt-1 max-h-40 overflow-y-auto">{upd.notes}</pre>{/if}
        </div>
      {/if}
      {#if upd?.last_error}<p class="text-sm text-danger mb-3">{upd.last_error}</p>{/if}
      {#if upd?.last_attempt && upd.last_attempt.status !== 'done'}<p class="text-sm text-danger mb-3">{t('settings.updateLastAttempt', { from: upd.last_attempt.from, to: upd.last_attempt.to, status: upd.last_attempt.status, error: upd.last_attempt.error ?? '' })}</p>{/if}
      <form class="grid sm:grid-cols-2 lg:grid-cols-4 gap-3 items-end" onsubmit={saveUpdate}>
        <div><label class="label" for="ur">{t('settings.updateRepo')}</label><input id="ur" class="input font-mono" bind:value={updForm.repo} placeholder="owner/name" /></div>
        <div><label class="label" for="uc">{t('settings.updateChannel')}</label><select id="uc" class="input" bind:value={updForm.channel}><option value="stable">stable</option><option value="beta">{t('settings.updateChannelBeta')}</option></select></div>
        <div><label class="label" for="ut">{t('settings.updateToken')}</label><input id="ut" class="input" type="password" bind:value={updForm.token} placeholder={upd?.settings?.has_token ? t('settings.updateTokenSaved') : t('settings.updateTokenHint')} /></div>
        <div><label class="label" for="uh">{t('settings.updateCheckHours')}</label><input id="uh" class="input" type="number" min="0" max="720" bind:value={updForm.check_hours} /></div>
        <label class="flex items-center gap-2 text-sm sm:col-span-2"><input type="checkbox" bind:checked={updForm.auto_apply} /> {t('settings.updateAutoApply')}</label>
        <div class="sm:col-span-2 flex justify-end"><button class="btn btn-primary">{t('common.save')}</button></div>
      </form>
    </div>
    {#if sel?.supported}
      <div class="card md:col-span-2 rise" style="--i:6">
        <div class="flex justify-between items-start gap-3 flex-wrap">
          <div>
            <div class="font-medium">SELinux</div>
            <p class="text-xs text-muted">{t('settings.selinuxMode', { mode: sel.mode })}{#if sel.configured && sel.configured !== sel.mode} · {t('settings.selinuxAfterReboot', { mode: sel.configured })}{/if}. {t('settings.selinuxHint')}</p>
          </div>
          {#if sel.mode === 'enforcing'}
            <button class="btn btn-danger btn-sm" onclick={() => (ask = askPermissive())}>{t('settings.selinuxToPermissive')}</button>
          {:else}
            <button class="btn btn-primary btn-sm" onclick={() => setSELinux('enforcing')}>{t('settings.selinuxToEnforcing')}</button>
          {/if}
        </div>
        {#if sel.mode !== 'enforcing' || (sel.configured && sel.configured !== 'enforcing')}<div class="p-3 rounded-lg border border-danger bg-danger-soft text-sm mt-3">{sel.warning}</div>{/if}
      </div>
    {/if}
    <div class="card md:col-span-2 rise" style="--i:7">
      <div class="font-medium mb-2">Webhooks (HMAC-SHA256)</div>
      <form class="flex flex-wrap gap-2 items-end mb-3" onsubmit={addHook}><div class="flex-1 min-w-64"><label class="label" for="hu">URL</label><input id="hu" class="input" bind:value={hook.url} required /></div><div><label class="label" for="he">{t('settings.hookEvents')}</label><input id="he" class="input font-mono" bind:value={hook.events} /></div><button class="btn btn-primary">{t('common.add')}</button></form>
      <ul class="text-sm divide-y divide-line">{#each hooks as h}<li class="flex justify-between items-center py-1.5 font-mono text-xs"><span>{h.url} · {h.events.join(',')}</span><button class="btn btn-danger btn-sm" onclick={() => (ask = askHook(h))}>{t('settings.hookDelete')}</button></li>{/each}</ul>
    </div>
  {/if}
</div>

<Confirm bind:ask />
