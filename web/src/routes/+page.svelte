<script lang="ts">
  import { auth, notify, dur } from '$lib/state.svelte';
  import { onMount } from 'svelte';
  import { slide } from 'svelte/transition';
  import { api, ApiError, bytes, when } from '$lib/api';
  import { t, tn, dateLocale } from '$lib/i18n/index.svelte';
  import Chart from '$lib/components/Chart.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Icon from '$lib/components/Icon.svelte';
  const admin = $derived(auth.me?.role === 'admin');
  let status = $state<any>(null);
  let metrics = $state<any>(null);
  let doctor = $state<any>(null);
  let doctorBusy = $state(false);
  let doctorError = $state('');
  let fixing = $state('');
  // Doctor's findings carry an action: site.fix (owner, modes, ACLs, SELinux
  // labels of one site or every site) or selinux.enforcing. One click, then
  // the diagnostics are read again.
  async function fixSites(target: string) {
    fixing = target;
    try {
      const domains: string[] = target === '*' ? ((await api('/sites')) as any[]).map((s) => s.domain) : [target];
      const jobs: number[] = [];
      for (const d of domains) { const r: any = await api(`/sites/${d}/fix`, { method: 'POST' }); jobs.push(r.job_id); }
      notify(domains.length === 1 ? t('dashboard.fixOneStarted', { domain: domains[0], job: jobs[0] }) : tn('dashboard.fixManyStarted', domains.length, { first: jobs[0], last: jobs[jobs.length - 1] }));
      await new Promise((r) => setTimeout(r, 4000));
      await loadDoctor(true);
    } catch (e: any) { notify(e?.text || String(e), 'err'); } finally { fixing = ''; }
  }
  async function enforce() {
    fixing = 'selinux';
    try { await api('/system/selinux', { method: 'PUT', json: { mode: 'enforcing' } }); notify('SELinux: enforcing'); await loadDoctor(true); }
    catch (e: any) { notify(e?.text || String(e), 'err'); } finally { fixing = ''; }
  }
  let range = $state('24h');
  let error = $state('');
  async function loadStatus() {
    try { status = await api('/system/status'); error = ''; } catch (e) { error = e instanceof ApiError ? e.text : String(e); }
  }
  // mp doctor: the checks take a while (DNS, certificates), so a re-run keeps
  // the old report on screen until the new one arrives. Admins only.
  async function loadDoctor(keep = false) {
    if (!admin) return;
    doctorBusy = true;
    try { doctor = await api('/system/doctor'); doctorError = ''; }
    catch (e) { if (!keep) doctor = null; doctorError = e instanceof ApiError ? e.text : String(e); }
    finally { doctorBusy = false; }
  }
  onMount(() => { loadStatus(); loadDoctor(); const timer = setInterval(loadStatus, 15000); return () => clearInterval(timer); });
  // Метрики: по смене диапазона и раз в минуту — сэмплер пишет точку раз в минуту.
  $effect(() => {
    const r = range;
    const get = () => api('/system/metrics?range=' + r).then((m) => { if (r === range) metrics = m; }).catch(() => {});
    get();
    const timer = setInterval(get, 60000);
    return () => clearInterval(timer);
  });
  const host = $derived(status?.host);
  const pts = (key: string) => (metrics?.points || []).map((p: any) => ({ x: p.ts, y: key === 'mem' ? p.mem_used / 1048576 : key === 'disk' ? p.disk_used / 1073741824 : key === 'rx' || key === 'tx' ? p['net_' + key] / 1024 : p[key] }));
  const lastPoint = $derived(metrics?.points?.length ? metrics.points[metrics.points.length - 1] : null);
  const pct = (used: number, total: number) => (total ? Math.round((used / total) * 100) : 0);
  const memUsed = $derived(host ? host.mem_total_bytes - host.mem_available_bytes : 0);
  const disks = $derived<any[]>(host?.disks || []);
  const disk = $derived(disks[0]);
  const up = (s: number) => { const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600); return d ? t('dashboard.uptimeDays', { d, h }) : t('dashboard.uptimeHours', { h, m: Math.floor((s % 3600) / 60) }); };
  const step = $derived(!metrics?.step_seconds ? '' : metrics.step_seconds >= 3600 ? t('dashboard.stepHours', { n: metrics.step_seconds / 3600 }) : t('dashboard.stepMinutes', { n: Math.round(metrics.step_seconds / 60) }));
  const jobOrder = ['queued', 'running', 'failed', 'done', 'cancelled'];
  const jobCounts = $derived(Object.entries(status?.panel?.jobs || {}).sort(([a], [b]) => (jobOrder.indexOf(a) + 1 || 99) - (jobOrder.indexOf(b) + 1 || 99)) as [string, number][]);
  const svcTitle = (s: any) => [`${s.active_state}${s.sub_state ? ' (' + s.sub_state + ')' : ''}`, s.unit_file_state, s.active_since && !s.active_since.startsWith('0001') ? t('dashboard.since', { when: when(s.active_since) }) : ''].filter(Boolean).join(' · ');
  // The table is what mp metrics prints: every point of the range, newest first.
  let table = $state(false);
  const rows = $derived([...(metrics?.points || [])].reverse());
  const pointTime = (ts: number) => new Date(ts * 1000).toLocaleString(dateLocale(), { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' });
  let openCheck = $state<string[]>([]);
  const toggleCheck = (name: string) => (openCheck = openCheck.includes(name) ? openCheck.filter((n) => n !== name) : [...openCheck, name]);
  const fails = $derived((doctor?.checks || []).filter((c: any) => c.status === 'fail').length);
</script>

<PageHead title={t('dashboard.title')} sub={host ? t('dashboard.sub', { hostname: host.hostname, release: host.release?.pretty_name, kernel: host.kernel }) : ''}>
  <div class="flex gap-1">
    {#each ['1h', '6h', '24h', '7d', '30d'] as r}
      <button class="btn btn-sm {range === r ? 'btn-primary' : 'btn-ghost'}" aria-pressed={range === r} onclick={() => (range = r)}>{r}</button>
    {/each}
  </div>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if status}
  {#if !status.panel.agent_ok}
    <div class="card mb-4 border-danger/40 text-sm rise"><div class="flex items-start gap-2 text-danger"><Icon name="alert" size={16} class="shrink-0 mt-0.5" /><div><div class="font-medium">{t('dashboard.agentDown')}</div><div class="font-mono text-xs break-all mt-1">{status.panel.agent_error || t('common.unknown')}</div><p class="text-muted text-xs mt-1">{t('dashboard.agentDownHint')}</p></div></div></div>
  {/if}
  <div class="grid grid-cols-2 lg:grid-cols-4 gap-3 mb-4">
    {#if host}
      <div class="kpi rise" style="--i:0"><div class="label flex items-center gap-1"><Icon name="cpu" size={12} /> {t('dashboard.load')}</div><div class="kpi-v">{host.load?.map((x: number) => x.toFixed(2)).join('  ')}</div><div class="text-xs text-muted">{host.cpus} CPU · uptime {up(host.uptime_seconds || 0)}</div></div>
      <div class="kpi rise" style="--i:1"><div class="label">{t('dashboard.memory')}</div><div class="kpi-v">{pct(memUsed, host.mem_total_bytes)}%</div><div class="progress mt-1"><i style="width:{pct(memUsed, host.mem_total_bytes)}%"></i></div><div class="text-xs text-muted">{t('dashboard.usedOf', { used: bytes(memUsed), total: bytes(host.mem_total_bytes) })}</div>{#if host.swap_total_bytes}<div class="text-xs text-muted">{t('dashboard.swap', { used: bytes(host.swap_total_bytes - host.swap_free_bytes), total: bytes(host.swap_total_bytes) })}</div>{/if}</div>
      <div class="kpi rise" style="--i:2"><div class="label">{t('dashboard.disk', { mount: disk?.mount || '/' })}</div><div class="kpi-v">{pct((disk?.total_bytes ?? 0) - (disk?.free_bytes ?? 0), disk?.total_bytes ?? 0)}%</div><div class="progress mt-1"><i style="width:{pct((disk?.total_bytes ?? 0) - (disk?.free_bytes ?? 0), disk?.total_bytes ?? 0)}%"></i></div><div class="text-xs text-muted">{t('dashboard.freeOf', { free: bytes(disk?.free_bytes), total: bytes(disk?.total_bytes) })}</div>
        {#each disks.slice(1) as d}<div class="text-xs text-muted truncate" title={t('dashboard.freeOf', { free: bytes(d.free_bytes), total: bytes(d.total_bytes) })}><span class="font-mono">{d.mount}</span> {pct(d.total_bytes - d.free_bytes, d.total_bytes)}% · {t('dashboard.freeShort', { free: bytes(d.free_bytes) })}</div>{/each}
      </div>
    {/if}
    <div class="kpi rise" style="--i:3"><div class="label">{t('dashboard.panel')}</div><div class="kpi-v">v{status.panel.version}</div>
      <div class="text-xs text-muted">{t('dashboard.panelUp', { up: up(status.panel.uptime_seconds || 0), schema: status.panel.schema_version })}</div>
      <div class="text-xs text-muted">{t('dashboard.jobsLabel')} {#each jobCounts as [k, v], i}{#if i}{' · '}{/if}<a class="hover:text-accent-ink transition-colors {k === 'failed' && v ? 'text-danger' : ''}" href="/jobs?status={k}">{k} {v}</a>{:else}{t('common.none')}{/each} · {t('dashboard.accounts', { admins: status.panel.users.admin || 0, users: status.panel.users.user || 0 })}</div>
    </div>
  </div>
  <div class="card mb-4 rise" style="--i:4">
    <div class="flex flex-wrap gap-2 text-sm">
      {#each status.services || [] as s}
        {@const cls = s.active_state === 'active' ? 'tag-ok' : s.load_state === 'not-found' ? 'tag-muted' : 'tag-err'}
        {@const label = s.unit.replace('.service', '') + (s.active_state === 'active' ? '' : ' · ' + s.active_state)}
        {#if admin}<a class="tag {cls} hover:brightness-110 transition" href="/services?unit={encodeURIComponent(s.unit)}" title={svcTitle(s)}><span class="dot"></span>{label}</a>
        {:else}<span class="tag {cls}" title={svcTitle(s)}><span class="dot"></span>{label}</span>{/if}
      {/each}
      <span class="tag {status.panel.agent_ok ? 'tag-ok' : 'tag-err'}" title={status.panel.agent_ok ? t('dashboard.agentVersion', { version: status.panel.agent_version || '?' }) : status.panel.agent_error}><span class="dot {status.panel.agent_ok ? '' : 'dot-live'}"></span>agent {status.panel.agent_ok ? 'ok' + (status.panel.agent_version ? ' · v' + status.panel.agent_version : '') : t('dashboard.agentUnreachable')}</span>
      {#if admin && status.services?.length}<a class="text-xs text-muted hover:text-ink self-center ml-auto transition-colors" href="/services">{t('dashboard.allServices')} →</a>{/if}
    </div>
  </div>
{/if}
<div class="flex flex-wrap items-center gap-2 mb-2 text-xs text-muted">
  <span>{t('dashboard.metrics', { range })}{#if step}{' · ' + t('dashboard.step', { step })}{/if}</span>
  <button class="btn btn-ghost btn-sm ml-auto" aria-pressed={table} onclick={() => (table = !table)}>{table ? t('dashboard.showCharts') : t('dashboard.showTable')}</button>
</div>
{#if !table}
  <div class="grid md:grid-cols-2 gap-3 mb-4">
    <Chart points={pts('cpu')} label="CPU, %" format={(v) => v.toFixed(0) + '%'} />
    <Chart points={pts('load1')} label={t('dashboard.chartLoad')} format={(v) => v.toFixed(2)} />
    <Chart points={pts('mem')} label={lastPoint ? t('dashboard.chartMemOf', { total: bytes(lastPoint.mem_total) }) : t('dashboard.chartMem')} format={(v) => t('dashboard.mb', { v: v.toFixed(0) })} />
    <Chart points={pts('disk')} label={lastPoint ? t('dashboard.chartDiskOf', { total: bytes(lastPoint.disk_total) }) : t('dashboard.chartDisk')} format={(v) => t('dashboard.gb', { v: v.toFixed(1) })} />
    <Chart points={pts('rx')} label={t('dashboard.chartRx')} format={(v) => t('dashboard.kb', { v: v.toFixed(0) })} />
    <Chart points={pts('tx')} label={t('dashboard.chartTx')} format={(v) => t('dashboard.kb', { v: v.toFixed(0) })} />
  </div>
{:else}
  <div class="card overflow-auto p-0 mb-4 max-h-[32rem] rise" transition:slide={{ duration: dur(160) }}>
    <table class="tbl">
      <thead><tr><th>{t('dashboard.colTime')}</th><th>CPU</th><th>Load 1</th><th>{t('dashboard.memory')}</th><th>{t('dashboard.colDisk')}</th><th>RX</th><th>TX</th></tr></thead>
      <tbody>
        {#each rows as p (p.ts)}
          <tr><td data-label={t('dashboard.colTime')} class="text-xs text-muted whitespace-nowrap">{pointTime(p.ts)}</td><td data-label="CPU" class="font-mono text-xs tabular-nums">{p.cpu.toFixed(1)}%</td><td data-label="Load 1" class="font-mono text-xs tabular-nums">{p.load1.toFixed(2)}</td><td data-label={t('dashboard.memory')} class="font-mono text-xs tabular-nums whitespace-nowrap">{bytes(p.mem_used)} / {bytes(p.mem_total)}</td><td data-label={t('dashboard.colDisk')} class="font-mono text-xs tabular-nums whitespace-nowrap">{bytes(p.disk_used)} / {bytes(p.disk_total)}</td><td data-label="RX" class="font-mono text-xs tabular-nums whitespace-nowrap">{bytes(p.net_rx)}</td><td data-label="TX" class="font-mono text-xs tabular-nums whitespace-nowrap">{bytes(p.net_tx)}</td></tr>
        {/each}
        {#if !rows.length}<tr><td colspan="7" class="text-muted text-center py-6">{t('dashboard.noPoints')}</td></tr>{/if}
      </tbody>
    </table>
  </div>
{/if}
{#if admin && (doctor || doctorBusy || doctorError)}
  <div class="card rise">
    <div class="flex flex-wrap justify-between items-center gap-2 mb-3">
      <span class="font-medium">{t('dashboard.doctor')}{#if fails}<span class="tag tag-err ml-2">{t('dashboard.fails', { n: fails })}</span>{/if}</span>
      <span class="flex items-center gap-2">
        {#if doctor}<span class="text-xs text-muted font-mono">{doctor.summary}</span>{/if}
        <button class="btn btn-ghost btn-sm" onclick={() => loadDoctor(true)} disabled={doctorBusy} title={t('dashboard.doctorRerun')} aria-label={t('dashboard.doctorRerun')}><Icon name="refresh" size={13} class={doctorBusy ? 'animate-spin' : ''} />{doctorBusy ? ' ' + t('dashboard.doctorRunning') : ''}</button>
      </span>
    </div>
    {#if doctorError}<p class="text-danger text-sm mb-2">{doctorError}</p>{/if}
    {#if doctor}
      <ul class="text-sm grid md:grid-cols-2 gap-x-6 gap-y-1">
        {#each doctor.checks as c, i}
          <li class="flex flex-wrap items-start gap-2 py-0.5 rise min-w-0" style="--i:{Math.min(i, 24)}"><span class="tag {c.status === 'ok' ? 'tag-ok' : c.status === 'warn' ? 'tag-warn' : 'tag-err'} w-12 justify-center">{c.status}</span>{#if c.name.startsWith('service ')}<a class="font-mono text-xs pt-0.5 shrink-0 hover:text-accent-ink transition-colors" href="/services?unit={encodeURIComponent(c.name.slice(8))}" title={t('dashboard.openService')}>{c.name}</a>{:else}<span class="font-mono text-xs pt-0.5 shrink-0">{c.name}</span>{/if}
            {#if c.detail}<button class="text-muted text-xs pt-0.5 text-left min-w-0 {openCheck.includes(c.name) ? 'whitespace-pre-wrap break-words basis-full' : 'truncate max-w-full'}" title={openCheck.includes(c.name) ? '' : c.detail} onclick={() => toggleCheck(c.name)}>{c.detail}</button>{/if}
            {#if c.action === 'site.fix' && c.target}<button class="btn btn-sm shrink-0" onclick={() => fixSites(c.target)} disabled={!!fixing}>{fixing ? t('dashboard.fixing') : c.target === '*' ? t('dashboard.fixAllSites') : t('dashboard.fixSite', { domain: c.target })}</button>{/if}
            {#if c.action === 'selinux.enforcing'}<button class="btn btn-sm shrink-0" onclick={enforce} disabled={!!fixing}>{t('dashboard.enforce')}</button>{/if}
          </li>
        {/each}
      </ul>
    {:else if doctorBusy}<p class="text-sm text-muted">{t('dashboard.doctorRunning')}</p>{/if}
  </div>
{/if}
