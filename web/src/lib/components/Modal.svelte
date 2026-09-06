<script lang="ts">
  import type { Snippet } from 'svelte';
  import { fade, scale } from 'svelte/transition';
  import { dur } from '$lib/state.svelte';
  let { open = $bindable(false), title = '', children, footer }: { open?: boolean; title?: string; children?: Snippet; footer?: Snippet } = $props();
  function close() { open = false; }
  function key(e: KeyboardEvent) { if (e.key === 'Escape') close(); }
</script>

<svelte:window onkeydown={key} />
{#if open}
  <div class="fixed inset-0 z-40 flex items-center justify-center p-4" transition:fade={{ duration: dur(150) }}>
    <button class="absolute inset-0 bg-black/40 backdrop-blur-[2px] cursor-default" aria-label="закрыть" onclick={close}></button>
    <div class="relative card w-full max-w-md shadow-2xl" role="dialog" aria-modal="true" transition:scale={{ start: 0.96, duration: dur(180) }}>
      {#if title}<div class="text-base font-semibold mb-3 pr-8">{title}</div>{/if}
      <button class="absolute top-3 right-3 btn btn-ghost btn-sm" onclick={close} aria-label="закрыть">✕</button>
      <div class="text-sm space-y-3">{@render children?.()}</div>
      {#if footer}<div class="flex justify-end gap-2 mt-5">{@render footer()}</div>{/if}
    </div>
  </div>
{/if}
