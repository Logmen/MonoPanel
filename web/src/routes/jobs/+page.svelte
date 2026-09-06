<script lang="ts">
  import { onMount } from 'svelte';
  import { api, when } from '$lib/api';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  let jobs = $state<any[] | null>(null);
  let open = $state<number | null>(null);
  let error = $state('');
  let filter = $state('');
  async function load() { try { jobs = await api('/jobs?limit=100'); } catch (e: any) { error = e.text || String(e); } }
  onMount(() => { load(); const t = setInterval(load, 5000); return () => clearInterval(t); });
  const shown = $derived((jobs || []).filter((j) => !filter || j.status === filter));
</script>

<PageHead title="Задачи" sub="очередь асинхронных операций: установка, применение конфигураций, сертификаты, бэкапы">
  <div class="flex gap-1">{#each [['', 'все'], ['running', 'идут'], ['failed', 'ошибки'], ['done', 'готово']] as [v, t]}<button class="btn btn-sm {filter === v ? 'btn-primary' : 'btn-ghost'}" onclick={() => (filter = v)}>{t}</button>{/each}</div>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if open}<div class="mb-4"><JobLog jobId={open} /></div>{/if}
<div class="card overflow-x-auto p-0 rise">
  {#if !jobs}<Skeleton rows={6} />{:else}
  <table class="tbl"><thead><tr><th>ID</th><th>Тип</th><th>Статус</th><th class="w-28">Прогресс</th><th>Кто</th><th>Создана</th><th>Сообщение</th></tr></thead>
    <tbody>
      {#each shown as j, i}<tr class="cursor-pointer rise {open === j.id ? 'bg-accent-soft/40' : ''}" style="--i:{Math.min(i, 12)}" onclick={() => (open = j.id)}><td class="font-mono text-muted">{j.id}</td><td class="font-mono">{j.type}</td><td><span class="tag {j.status === 'done' ? 'tag-ok' : j.status === 'failed' ? 'tag-err' : j.status === 'running' ? 'tag-accent' : 'tag-warn'}">{#if j.status === 'running'}<span class="dot dot-live"></span>{/if}{j.status}</span></td><td><div class="progress {j.status === 'running' ? 'running' : ''}"><i style="width:{j.progress}%"></i></div></td><td class="text-muted">{j.requested_by}</td><td class="text-xs text-muted whitespace-nowrap">{when(j.created_at)}</td><td class="text-xs max-w-md truncate {j.error ? 'text-danger' : 'text-muted'}" title={j.error || j.message}>{j.error || j.message}</td></tr>{/each}
      {#if !shown.length}<tr><td colspan="7" class="text-muted text-center py-6">Задач нет.</td></tr>{/if}
    </tbody></table>
  {/if}
</div>
