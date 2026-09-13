<script lang="ts">
  import { fly } from 'svelte/transition';
  import { api, ApiError } from '$lib/api';
  import { auth, loadMe, dur } from '$lib/state.svelte';
  import { t, lang, setLang } from '$lib/i18n/index.svelte';
  import Icon from './Icon.svelte';
  let login = $state('');
  let password = $state('');
  let code = $state('');
  let needCode = $state(false);
  let error = $state('');
  let busy = $state(false);
  async function submit(e: Event) {
    e.preventDefault();
    error = '';
    busy = true;
    try {
      await api('/auth/login', { method: 'POST', json: { login, password, totp_code: code || undefined } });
      await loadMe();
    } catch (err) {
      if (err instanceof ApiError && err.detail === 'totp_required') needCode = true;
      else error = err instanceof ApiError ? err.text : String(err);
    } finally {
      busy = false;
    }
  }
</script>

<div class="glow"></div>
<main class="min-h-screen grid place-items-center px-5">
  <div class="w-full max-w-sm" in:fly={{ y: 14, duration: dur(320) }}>
    <div class="flex items-center gap-3 mb-6">
      <span class="logo-mark w-11 h-11 bg-accent grid place-items-center font-bold text-lg">M</span>
      <div>
        <h1 class="brand text-2xl font-bold leading-none">MonoPanel</h1>
        <p class="text-muted text-xs mt-1 font-mono">{location.hostname}{auth.version ? ' · v' + auth.version : ''}</p>
      </div>
    </div>
    <form class="card space-y-3.5 p-5" onsubmit={submit}>
      <div><label class="label" for="l">{t('shell.login.username')}</label><input id="l" class="input" bind:value={login} autocomplete="username" required /></div>
      <div><label class="label" for="p">{t('shell.login.password')}</label><input id="p" class="input" type="password" bind:value={password} autocomplete="current-password" required /></div>
      {#if needCode}
        <div in:fly={{ y: -6, duration: dur(180) }}><label class="label" for="c">{t('shell.login.code')}</label><input id="c" class="input font-mono tracking-[.2em]" bind:value={code} inputmode="numeric" autocomplete="one-time-code" /></div>
      {/if}
      {#if error}<p class="text-danger text-sm flex items-center gap-1.5"><Icon name="alert" size={15} /> {error}</p>{/if}
      <button class="btn btn-primary w-full justify-center py-2" disabled={busy}>{busy ? t('shell.login.checking') : t('shell.login.submit')}</button>
    </form>
    <div class="flex justify-center gap-4 text-xs text-muted mt-4" role="group" aria-label={t('lang.title')}>
      <button type="button" class="transition-colors hover:text-ink {lang.locale === 'ru' ? 'text-ink font-medium' : ''}" aria-pressed={lang.locale === 'ru'} onclick={() => setLang('ru')}>{t('lang.ru')}</button>
      <button type="button" class="transition-colors hover:text-ink {lang.locale === 'en' ? 'text-ink font-medium' : ''}" aria-pressed={lang.locale === 'en'} onclick={() => setLang('en')}>{t('lang.en')}</button>
    </div>
    <p class="text-center text-[11px] text-muted mt-2">{t('shell.login.themeHint')}</p>
  </div>
</main>
