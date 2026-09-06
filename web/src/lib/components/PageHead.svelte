<script lang="ts">
  import type { Snippet } from 'svelte';
  let { title, sub = '', mono = false, back = '', children }: { title: string; sub?: string; mono?: boolean; back?: string; children?: Snippet } = $props();
</script>

<div class="flex flex-wrap items-end justify-between gap-3 mb-5 rise">
  <div class="min-w-0">
    {#if back}<a href={back} class="text-xs text-muted hover:text-ink transition-colors inline-flex items-center gap-1 mb-1">← назад</a>{/if}
    <h1 class="text-2xl font-semibold tracking-tight leading-tight {mono ? 'font-mono' : ''}">{title}</h1>
    {#if sub}<p class="text-sm text-muted mt-0.5">{sub}</p>{/if}
  </div>
  {#if children}<div class="flex flex-wrap items-center gap-2">{@render children()}</div>{/if}
</div>
