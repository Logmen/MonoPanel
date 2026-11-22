<script lang="ts">
  import { t } from '$lib/i18n/index.svelte';
  import { notify } from '$lib/state.svelte';
  import Icon from '$lib/components/Icon.svelte';

  // A value to carry to another server: a token, an address, a command.
  // The Clipboard API exists only in a secure context, and a panel reached by
  // plain http still has to copy, so the old way stays as the fallback.
  let { value, label = '' }: { value: string; label?: string } = $props();
  let copied = $state(false);
  let timer: ReturnType<typeof setTimeout> | undefined;

  function legacyCopy(text: string): boolean {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch { ok = false; }
    ta.remove();
    return ok;
  }
  async function copy() {
    let ok = false;
    try { await navigator.clipboard.writeText(value); ok = true; } catch { ok = legacyCopy(value); }
    if (!ok) { notify(t('migrate.copyFailed'), 'err'); return; }
    copied = true;
    clearTimeout(timer);
    timer = setTimeout(() => (copied = false), 1600);
  }
</script>

<div class="min-w-0">
  {#if label}<span class="label">{label}</span>{/if}
  <div class="flex items-start gap-2">
    <code class="code flex-1 min-w-0 py-1.5 [overflow-wrap:anywhere] select-all">{value}</code>
    <button type="button" class="btn btn-sm shrink-0 mt-0.5" onclick={copy} aria-label={label ? `${t('migrate.copy')}: ${label}` : t('migrate.copy')}>{#if copied}<Icon name="check" size={13} /> {t('migrate.copied')}{:else}{t('migrate.copy')}{/if}</button>
  </div>
</div>
