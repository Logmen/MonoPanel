<script lang="ts">
  import { onMount } from 'svelte';
  import QRCode from 'qrcode';
  import { api, ApiError, when } from '$lib/api';
  import { auth, notify, theme, setTheme, type ThemeMode } from '$lib/state.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Icon from '$lib/components/Icon.svelte';
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
  let error = $state('');
  let msg = $state('');
  const admin = $derived(auth.me?.role === 'admin');
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function load() { try { totp = await api('/auth/totp'); tokens = await api('/tokens'); if (admin) { hooks = await api('/webhooks'); realip = await api('/stack/nginx/real-ip'); realipFrom = (realip.from || []).join(', '); } } catch (e: any) { error = e.text || String(e); } }
  onMount(load);
  async function startTotp() { error = ''; try { setup = await api('/auth/totp/setup', { method: 'POST' }); qr = await QRCode.toDataURL(setup.url, { width: 180, margin: 1 }); } catch (e) { fail(e); } }
  async function enableTotp(e: Event) { e.preventDefault(); error = ''; try { await api('/auth/totp/enable', { method: 'POST', json: { code } }); setup = null; code = ''; notify('2FA включена'); await load(); } catch (e) { fail(e); } }
  async function disableTotp() { const p = prompt('Пароль для отключения 2FA:'); if (!p) return; try { await api('/auth/totp/disable', { method: 'POST', json: { password: p } }); notify('2FA выключена'); await load(); } catch (e) { fail(e); } }
  async function createToken() { const name = prompt('Название токена:', 'api'); if (!name) return; try { const r: any = await api('/tokens', { method: 'POST', json: { name } }); newToken = r.token; await load(); } catch (e) { fail(e); } }
  async function revoke(id: number) { await api(`/tokens/${id}`, { method: 'DELETE' }); await load(); }
  async function addHook(e: Event) { e.preventDefault(); try { const r: any = await api('/webhooks', { method: 'POST', json: { url: hook.url, events: hook.events.split(',').map((s) => s.trim()).filter(Boolean) } }); msg = 'секрет webhook: ' + r.secret; hook.url = ''; await load(); } catch (e) { fail(e); } }
  async function rmHook(id: string) { await api(`/webhooks/${id}`, { method: 'DELETE' }); await load(); }
  async function saveRealIP() { try { realip = await api('/stack/nginx/real-ip', { method: 'PUT', json: { cloudflare: realip.cloudflare, from: realipFrom.split(/[\s,]+/).filter(Boolean) } }); realipFrom = (realip.from || []).join(', '); notify('nginx real_ip обновлён'); } catch (e) { fail(e); } }
  const modes: [ThemeMode, string, string][] = [['system', 'monitor', 'Как в системе'], ['light', 'sun', 'Светлая'], ['dark', 'moon', 'Тёмная']];
</script>

<PageHead title="Настройки" sub="аккаунт, безопасность и интеграции" />
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if msg}<p class="text-accent-ink text-sm mb-3 font-mono">{msg}</p>{/if}
<div class="grid md:grid-cols-2 gap-4">
  <div class="card rise">
    <div class="font-medium mb-1">Оформление</div>
    <p class="text-xs text-muted mb-3">По умолчанию тема повторяет системную (светлая днём, тёмная ночью, если так настроена ОС). Выбор запоминается в этом браузере.</p>
    <div class="grid grid-cols-3 gap-2">
      {#each modes as [m, icon, label]}
        <button class="flex flex-col items-center gap-1.5 py-3 rounded-lg border transition-all duration-150 {theme.mode === m ? 'border-accent bg-accent-soft text-accent-ink' : 'border-line hover:border-line-strong text-muted hover:text-ink'}" aria-pressed={theme.mode === m} onclick={() => setTheme(m)}><Icon name={icon} size={20} /><span class="text-xs">{label}</span></button>
      {/each}
    </div>
  </div>
  <div class="card rise" style="--i:1">
    <div class="font-medium mb-2">Двухфакторная аутентификация</div>
    {#if totp?.enabled}<p class="text-sm mb-3"><span class="tag tag-ok"><Icon name="check" size={11} /> включена</span></p><button class="btn btn-danger btn-sm" onclick={disableTotp}>выключить</button>
    {:else if setup}
      <p class="text-sm text-muted mb-2">Отсканируйте QR в приложении (Google Authenticator, Aegis, 1Password) и введите код.</p>
      {#if qr}<img src={qr} alt="QR" class="mb-2 rounded-md border border-line" />{/if}
      <div class="text-xs font-mono mb-2 break-all select-all">{setup.secret}</div>
      <form class="flex gap-2" onsubmit={enableTotp}><input class="input w-32 font-mono" bind:value={code} placeholder="123456" /><button class="btn btn-primary">Подтвердить</button></form>
    {:else}<button class="btn" onclick={startTotp}><Icon name="lock" size={14} /> настроить 2FA</button>{/if}
  </div>
  <div class="card rise" style="--i:2">
    <div class="flex justify-between items-center mb-2"><span class="font-medium">API-токены</span><button class="btn btn-sm" onclick={createToken}><Icon name="plus" size={13} /> создать</button></div>
    {#if newToken}<div class="text-xs font-mono break-all mb-2 p-2 code select-all">{newToken}<div class="text-muted">показан один раз</div></div>{/if}
    <ul class="text-sm divide-y divide-line">{#each tokens as t}<li class="flex justify-between items-center py-1.5"><span>{t.name} <span class="text-xs text-muted">{t.last_used_at ? 'использован ' + when(t.last_used_at) : 'не использовался'}</span></span><button class="btn btn-danger btn-sm" onclick={() => revoke(t.id)}>отозвать</button></li>{/each}</ul>
    <p class="text-xs text-muted mt-2 font-mono">mp --server https://{location.host} --token … status</p>
  </div>
  {#if admin}
    <div class="card rise" style="--i:3">
      <div class="font-medium mb-1">Реальный IP за прокси</div>
      <p class="text-xs text-muted mb-3">nginx подставляет адрес клиента из заголовка доверенного прокси: allow-list сайтов, логи и PHP видят посетителя, а не Cloudflare.</p>
      {#if realip}
        <label class="flex items-center gap-2 text-sm mb-2"><input type="checkbox" bind:checked={realip.cloudflare} /> доверять сетям Cloudflare (CF-Connecting-IP)</label>
        <label class="label" for="rf">Свои прокси (IP / CIDR, X-Forwarded-For)</label>
        <div class="flex gap-2"><input id="rf" class="input font-mono" bind:value={realipFrom} placeholder="10.0.0.5, 192.168.1.0/24" /><button class="btn btn-primary" onclick={saveRealIP}>Применить</button></div>
      {/if}
    </div>
    <div class="card md:col-span-2 rise" style="--i:4">
      <div class="font-medium mb-2">Webhooks (HMAC-SHA256)</div>
      <form class="flex flex-wrap gap-2 items-end mb-3" onsubmit={addHook}><div class="flex-1 min-w-64"><label class="label" for="hu">URL</label><input id="hu" class="input" bind:value={hook.url} required /></div><div><label class="label" for="he">События</label><input id="he" class="input font-mono" bind:value={hook.events} /></div><button class="btn btn-primary">Добавить</button></form>
      <ul class="text-sm divide-y divide-line">{#each hooks as h}<li class="flex justify-between items-center py-1.5 font-mono text-xs"><span>{h.url} · {h.events.join(',')}</span><button class="btn btn-danger btn-sm" onclick={() => rmHook(h.id)}>удалить</button></li>{/each}</ul>
    </div>
  {/if}
</div>
