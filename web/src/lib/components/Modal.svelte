<script lang="ts">
  import type { Snippet } from 'svelte';
  import { fade, scale } from 'svelte/transition';
  import { dur } from '$lib/state.svelte';
  import { t } from '$lib/i18n/index.svelte';
  import { portal } from '$lib/portal';
  // onclose нужен там, где открытость выводится из состояния снаружи
  // (open={!!del}): без него Esc и клик по фону гасят окно, а состояние
  // остаётся — и та же кнопка второй раз уже ничего не открывает.
  let { open = $bindable(false), title = '', children, footer, onclose }: { open?: boolean; title?: string; children?: Snippet; footer?: Snippet; onclose?: () => void } = $props();
  function close() { open = false; onclose?.(); }
  function key(e: KeyboardEvent) { if (e.key === 'Escape') close(); }
</script>

<svelte:window onkeydown={key} />
<!-- Окно живёт в <body>, где бы ни стоял Modal: карточка вокруг обрезала бы
     его своим clip-path (см. $lib/portal). -->
<div class="contents"><div use:portal>
  {#if open}
    <!-- Окно выше экрана прокручивается: m-auto ставит его по центру, пока оно
         помещается, и к верху, когда нет, — items-center обрезал бы и заголовок,
         и кнопки без всякой прокрутки. Фон fixed, чтобы закрывать экран и при ней. -->
    <div class="fixed inset-0 z-40 flex overflow-y-auto overscroll-contain p-4" transition:fade={{ duration: dur(150) }}>
      <button class="fixed inset-0 bg-black/40 backdrop-blur-[2px] cursor-default" aria-label={t('shell.modal.close')} onclick={close}></button>
      <div class="relative card no-cut w-full max-w-md m-auto shadow-2xl" role="dialog" aria-modal="true" transition:scale={{ start: 0.96, duration: dur(180) }}>
        {#if title}<div class="text-base font-semibold mb-3 pr-8">{title}</div>{/if}
        <button class="absolute top-3 right-3 btn btn-ghost btn-sm" onclick={close} aria-label={t('shell.modal.close')}>✕</button>
        <div class="text-sm space-y-3">{@render children?.()}</div>
        {#if footer}<div class="flex justify-end gap-2 mt-5">{@render footer()}</div>{/if}
      </div>
    </div>
  {/if}
</div></div>
