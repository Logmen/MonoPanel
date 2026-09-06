<script lang="ts">
  import { slide } from 'svelte/transition';
  import { jobEvents } from '$lib/api';
  import { dur } from '$lib/state.svelte';
  let { jobId, onfinish }: { jobId: number; onfinish?: (status: string) => void } = $props();
  let lines = $state<string[]>([]);
  let progress = $state(0);
  let message = $state('');
  let status = $state('running');
  let error = $state('');
  let open = $state(true);
  let pre: HTMLPreElement | undefined = $state();
  $effect(() => {
    lines = [];
    progress = 0;
    status = 'running';
    error = '';
    const stop = jobEvents(jobId, (type, data) => {
      if (type === 'snapshot') {
        const j = data.job;
        progress = j.progress;
        message = j.message;
        status = j.status;
        error = j.error || '';
        lines = (j.log || '').split('\n').filter(Boolean);
        if (j.status === 'done' || j.status === 'failed') onfinish?.(j.status);
      } else if (type === 'progress') {
        progress = data.progress;
        message = data.message;
      } else if (type === 'log') {
        lines = [...lines, data.line];
      } else if (type === 'done') {
        progress = 100;
        status = 'done';
        onfinish?.('done');
      } else if (type === 'failed') {
        status = 'failed';
        error = data.error;
        onfinish?.('failed');
      }
    });
    return stop;
  });
  $effect(() => { lines.length; if (pre) pre.scrollTop = pre.scrollHeight; });
</script>

<div class="card font-mono text-xs rise" transition:slide={{ duration: dur(200) }}>
  <div class="flex items-center gap-3">
    <button class="text-muted hover:text-ink transition-colors" onclick={() => (open = !open)} aria-expanded={open}>задача #{jobId} {open ? '▾' : '▸'}</button>
    <div class="flex-1 progress {status === 'running' ? 'running' : ''}"><i style="width:{Math.max(2, progress)}%"></i></div>
    <span class="tabular-nums w-9 text-right text-muted">{progress}%</span>
    <span class={status === 'failed' ? 'tag tag-err' : status === 'done' ? 'tag tag-ok' : 'tag tag-accent'}>{#if status === 'running'}<span class="dot dot-live"></span>{/if}{status}</span>
  </div>
  {#if message}<div class="text-muted mt-2">{message}</div>{/if}
  {#if open && (lines.length || error)}
    <pre bind:this={pre} class="code mt-2 max-h-64" transition:slide={{ duration: dur(160) }}>{lines.join('\n')}</pre>
  {/if}
  {#if error}<div class="text-danger mt-2">{error}</div>{/if}
</div>
