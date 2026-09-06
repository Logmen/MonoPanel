<script lang="ts">
  import { onMount } from 'svelte';
  import { api, bytes } from '$lib/api';
  import Chart from '$lib/components/Chart.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Icon from '$lib/components/Icon.svelte';
  let status = $state<any>(null);
  let metrics = $state<any>(null);
  let doctor = $state<any>(null);
  let range = $state('24h');
  let error = $state('');
  async function load() {
    try {
      status = await api('/system/status');
      metrics = await api('/system/metrics?range=' + range);
    } catch (e: any) {
      error = e.text || String(e);
    }
    try { doctor = await api('/system/doctor'); } catch { doctor = null; }
  }
  onMount(() => { load(); const t = setInterval(() => api('/system/status').then((s) => (status = s)).catch(() => {}), 15000); return () => clearInterval(t); });
  $effect(() => { range; api('/system/metrics?range=' + range).then((m) => (metrics = m)).catch(() => {}); });
  const host = $derived(status?.host);
  const pts = (key: string) => (metrics?.points || []).map((p: any) => ({ x: p.ts, y: key === 'mem' ? p.mem_used / 1048576 : key === 'net' ? (p.net_rx + p.net_tx) / 1024 : p[key] }));
  const pct = (used: number, total: number) => (total ? Math.round((used / total) * 100) : 0);
  const memUsed = $derived(host ? host.mem_total_bytes - host.mem_available_bytes : 0);
  const disk = $derived(host?.disks?.[0]);
  const up = (s: number) => { const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600); return d ? `${d} д ${h} ч` : `${h} ч ${Math.floor((s % 3600) / 60)} м`; };
</script>

<PageHead title="Дашборд" sub={host ? `${host.hostname} · ${host.release?.pretty_name} · ядро ${host.kernel}` : ''}>
  <div class="flex gap-1">
    {#each ['1h', '6h', '24h', '7d', '30d'] as r}
      <button class="btn btn-sm {range === r ? 'btn-primary' : 'btn-ghost'}" onclick={() => (range = r)}>{r}</button>
    {/each}
  </div>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if status}
  <div class="grid grid-cols-2 lg:grid-cols-4 gap-3 mb-4">
    <div class="kpi rise" style="--i:0"><div class="label flex items-center gap-1"><Icon name="cpu" size={12} /> Нагрузка</div><div class="kpi-v">{host?.load?.map((x: number) => x.toFixed(2)).join('  ')}</div><div class="text-xs text-muted">{host?.cpus} CPU · uptime {up(host?.uptime_seconds || 0)}</div></div>
    <div class="kpi rise" style="--i:1"><div class="label">Память</div><div class="kpi-v">{pct(memUsed, host?.mem_total_bytes)}%</div><div class="progress mt-1"><i style="width:{pct(memUsed, host?.mem_total_bytes)}%"></i></div><div class="text-xs text-muted">{bytes(memUsed)} из {bytes(host?.mem_total_bytes)}</div></div>
    <div class="kpi rise" style="--i:2"><div class="label">Диск {disk?.mount || '/'}</div><div class="kpi-v">{pct(disk?.total_bytes - disk?.free_bytes, disk?.total_bytes)}%</div><div class="progress mt-1"><i style="width:{pct(disk?.total_bytes - disk?.free_bytes, disk?.total_bytes)}%"></i></div><div class="text-xs text-muted">свободно {bytes(disk?.free_bytes)} из {bytes(disk?.total_bytes)}</div></div>
    <div class="kpi rise" style="--i:3"><div class="label">Панель</div><div class="kpi-v">v{status.panel.version}</div><div class="text-xs text-muted">задачи: {Object.entries(status.panel.jobs).map(([k, v]) => `${k} ${v}`).join(' · ') || 'нет'} · аккаунты {status.panel.users.admin || 0}+{status.panel.users.user || 0}</div></div>
  </div>
  <div class="card mb-4 rise" style="--i:4">
    <div class="flex flex-wrap gap-2 text-sm">
      {#each status.services || [] as s}
        <span class="tag {s.active_state === 'active' ? 'tag-ok' : s.load_state === 'not-found' ? 'tag-muted' : 'tag-err'}"><span class="dot"></span>{s.unit.replace('.service', '')}</span>
      {/each}
      <span class="tag {status.panel.agent_ok ? 'tag-ok' : 'tag-err'}"><span class="dot {status.panel.agent_ok ? '' : 'dot-live'}"></span>agent {status.panel.agent_ok ? 'ok' : status.panel.agent_error}</span>
    </div>
  </div>
  <div class="grid md:grid-cols-2 gap-3 mb-4">
    <Chart points={pts('cpu')} label="CPU, %" format={(v) => v.toFixed(0) + '%'} />
    <Chart points={pts('load1')} label="Load average (1 мин)" format={(v) => v.toFixed(2)} />
    <Chart points={pts('mem')} label="Память, МБ" format={(v) => v.toFixed(0) + ' МБ'} />
    <Chart points={pts('net')} label="Сеть, КБ за шаг" format={(v) => v.toFixed(0) + ' КБ'} />
  </div>
{/if}
{#if doctor}
  <div class="card rise">
    <div class="flex justify-between items-center mb-3"><span class="font-medium">Диагностика</span><span class="text-xs text-muted font-mono">{doctor.summary}</span></div>
    <ul class="text-sm grid md:grid-cols-2 gap-x-6 gap-y-1">
      {#each doctor.checks as c, i}
        <li class="flex items-start gap-2 py-0.5 rise" style="--i:{i}"><span class="tag {c.status === 'ok' ? 'tag-ok' : c.status === 'warn' ? 'tag-warn' : 'tag-err'} w-12 justify-center">{c.status}</span><span class="font-mono text-xs pt-0.5 shrink-0">{c.name}</span><span class="text-muted text-xs pt-0.5 truncate" title={c.detail}>{c.detail}</span></li>
      {/each}
    </ul>
  </div>
{/if}
