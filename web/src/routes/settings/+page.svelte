<script lang="ts">
  import { onMount } from 'svelte';
  import QRCode from 'qrcode';
  import { api, ApiError, when } from '$lib/api';
  import { auth, notify, theme, setTheme, type ThemeMode } from '$lib/state.svelte';
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
  let updForm = $state({ repo: '', channel: 'stable', token: '', check_hours: 24, auto_apply: false });
  let updating = $state('');
  let error = $state('');
  let msg = $state('');
  let ask = $state<Ask | null>(null);
  const admin = $derived(auth.me?.role === 'admin');
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function load() { try { totp = await api('/auth/totp'); tokens = await api('/tokens'); if (admin) { hooks = await api('/webhooks'); realip = await api('/stack/nginx/real-ip'); realipFrom = (realip.from || []).join(', '); setUpdate(await api('/system/update')); } } catch (e: any) { error = e.text || String(e); } }
  function setUpdate(st: any) { upd = st; updForm = { repo: st.settings.repo, channel: st.settings.channel, token: '', check_hours: st.settings.check_hours, auto_apply: st.settings.auto_apply }; }
  async function saveUpdate(e: Event) { e.preventDefault(); try { setUpdate(await api('/system/update', { method: 'PUT', json: { ...updForm, token: updForm.token || undefined } })); notify('настройки обновлений сохранены'); } catch (e) { fail(e); } }
  async function checkUpdate() { updating = 'проверяем репозиторий…'; try { setUpdate(await api('/system/update/check', { method: 'POST', json: {} })); notify(upd.available ? 'доступна версия ' + upd.latest : 'установлена последняя версия'); } catch (e) { fail(e); } finally { updating = ''; } }
  // Установка перезапускает саму панель: ждём, пока она ответит новой версией, и перезагружаем страницу.
  const askUpdate = (): Ask => ({
    title: `Обновить панель до ${upd.latest}?`,
    note: 'Пакет скачается и поставится отдельной задачей systemd: панель и агент перезапустятся, страница сама перезагрузится через минуту-две. Сайты, почта и базы продолжают работать всё это время; если новая версия не ответит, панель откатится на прежнюю.',
    action: 'Обновить',
    run: () => applyUpdate()
  });
  async function applyUpdate() {
    try { await api('/system/update/apply', { method: 'POST', json: {} }); } catch (e) { fail(e); return; }
    updating = 'устанавливаем ' + upd.latest + '…';
    const deadline = Date.now() + 240000;
    while (Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 3000));
      try {
        const h: any = await api('/health');
        if ((h.version || '').replace(/^v/, '') === upd.latest) { location.reload(); return; }
      } catch { /* панель перезапускается */ }
    }
    updating = '';
    notify('панель не ответила новой версией — проверьте mp update', 'err');
  }
  onMount(load);
  async function startTotp() { error = ''; try { setup = await api('/auth/totp/setup', { method: 'POST' }); qr = await QRCode.toDataURL(setup.url, { width: 180, margin: 1 }); } catch (e) { fail(e); } }
  async function enableTotp(e: Event) { e.preventDefault(); error = ''; try { await api('/auth/totp/enable', { method: 'POST', json: { code } }); setup = null; code = ''; notify('2FA включена'); await load(); } catch (e) { fail(e); } }
  async function disableTotp() { const p = prompt('Пароль для отключения 2FA:'); if (!p) return; try { await api('/auth/totp/disable', { method: 'POST', json: { password: p } }); notify('2FA выключена'); await load(); } catch (e) { fail(e); } }
  async function createToken() { const name = prompt('Название токена:', 'api'); if (!name) return; try { const r: any = await api('/tokens', { method: 'POST', json: { name } }); newToken = r.token; await load(); } catch (e) { fail(e); } }
  const askRevoke = (t: any): Ask => ({
    title: `Отозвать токен «${t.name}»?`,
    note: 'Всё, что ходит в API с этим токеном — скрипты, интеграции, другой сервер — получит 401 сразу после отзыва. Вернуть тот же токен нельзя, только выпустить новый.',
    danger: true, action: 'Отозвать',
    run: () => revoke(t.id)
  });
  async function revoke(id: number) { await api(`/tokens/${id}`, { method: 'DELETE' }); await load(); }
  async function addHook(e: Event) { e.preventDefault(); try { const r: any = await api('/webhooks', { method: 'POST', json: { url: hook.url, events: hook.events.split(',').map((s) => s.trim()).filter(Boolean) } }); msg = 'секрет webhook: ' + r.secret; hook.url = ''; await load(); } catch (e) { fail(e); } }
  const askHook = (h: any): Ask => ({
    title: 'Удалить webhook?',
    note: `${h.url} перестанет получать события (${h.events.join(', ')}). Секрет подписи пропадёт вместе с ним — новый webhook получит другой.`,
    danger: true, action: 'Удалить',
    run: () => rmHook(h.id)
  });
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
    <ul class="text-sm divide-y divide-line">{#each tokens as t}<li class="flex justify-between items-center py-1.5"><span>{t.name} <span class="text-xs text-muted">{t.last_used_at ? 'использован ' + when(t.last_used_at) : 'не использовался'}</span></span><button class="btn btn-danger btn-sm" onclick={() => (ask = askRevoke(t))}>отозвать</button></li>{/each}</ul>
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
      <div class="flex justify-between items-start gap-3 mb-2 flex-wrap">
        <div>
          <div class="font-medium">Обновление панели</div>
          <p class="text-xs text-muted">Версия {upd?.current ?? '…'}{#if upd?.checked_at} · проверено {when(upd.checked_at)}{/if}{#if upd?.key_pinned} · подпись релиза обязательна{/if}</p>
        </div>
        <div class="flex gap-2">
          <button class="btn btn-sm" onclick={checkUpdate} disabled={!!updating || !upd?.settings?.repo}><Icon name="refresh" size={13} /> проверить</button>
          {#if upd?.available}<button class="btn btn-primary btn-sm" onclick={() => (ask = askUpdate())} disabled={!!updating}>обновить до {upd.latest}</button>{/if}
        </div>
      </div>
      {#if updating}<p class="text-sm text-accent-ink mb-3">{updating}</p>{/if}
      {#if upd?.available}
        <div class="p-3 rounded-lg border border-accent bg-accent-soft mb-3">
          <div class="text-sm font-medium text-accent-ink">Доступна версия {upd.latest}{#if upd.published_at} от {new Date(upd.published_at).toLocaleDateString('ru-RU')}{/if}</div>
          {#if upd.notes}<pre class="text-xs whitespace-pre-wrap mt-1 max-h-40 overflow-y-auto">{upd.notes}</pre>{/if}
        </div>
      {/if}
      {#if upd?.last_error}<p class="text-sm text-danger mb-3">{upd.last_error}</p>{/if}
      {#if upd?.last_attempt && upd.last_attempt.status !== 'done'}<p class="text-sm text-danger mb-3">последняя установка {upd.last_attempt.from} → {upd.last_attempt.to}: {upd.last_attempt.status} {upd.last_attempt.error}</p>{/if}
      <form class="grid sm:grid-cols-2 lg:grid-cols-4 gap-3 items-end" onsubmit={saveUpdate}>
        <div><label class="label" for="ur">Репозиторий</label><input id="ur" class="input font-mono" bind:value={updForm.repo} placeholder="owner/name" /></div>
        <div><label class="label" for="uc">Канал</label><select id="uc" class="input" bind:value={updForm.channel}><option value="stable">stable</option><option value="beta">beta (предрелизы)</option></select></div>
        <div><label class="label" for="ut">Токен доступа</label><input id="ut" class="input" type="password" bind:value={updForm.token} placeholder={upd?.settings?.has_token ? 'сохранён' : 'для приватного репозитория'} /></div>
        <div><label class="label" for="uh">Проверять, часов</label><input id="uh" class="input" type="number" min="0" max="720" bind:value={updForm.check_hours} /></div>
        <label class="flex items-center gap-2 text-sm sm:col-span-2"><input type="checkbox" bind:checked={updForm.auto_apply} /> устанавливать обновления автоматически</label>
        <div class="sm:col-span-2 flex justify-end"><button class="btn btn-primary">Сохранить</button></div>
      </form>
    </div>
    <div class="card md:col-span-2 rise" style="--i:5">
      <div class="font-medium mb-2">Webhooks (HMAC-SHA256)</div>
      <form class="flex flex-wrap gap-2 items-end mb-3" onsubmit={addHook}><div class="flex-1 min-w-64"><label class="label" for="hu">URL</label><input id="hu" class="input" bind:value={hook.url} required /></div><div><label class="label" for="he">События</label><input id="he" class="input font-mono" bind:value={hook.events} /></div><button class="btn btn-primary">Добавить</button></form>
      <ul class="text-sm divide-y divide-line">{#each hooks as h}<li class="flex justify-between items-center py-1.5 font-mono text-xs"><span>{h.url} · {h.events.join(',')}</span><button class="btn btn-danger btn-sm" onclick={() => (ask = askHook(h))}>удалить</button></li>{/each}</ul>
    </div>
  {/if}
</div>

<Confirm bind:ask />
