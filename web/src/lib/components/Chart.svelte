<script lang="ts">
  let { points, format = (v: number) => v.toFixed(0), label = '' }: { points: { x: number; y: number }[]; format?: (v: number) => string; label?: string } = $props();
  const W = 640, H = 150, P = 28;
  const geo = $derived.by(() => {
    if (points.length < 2) return null;
    const xs = points.map((p) => p.x), ys = points.map((p) => p.y);
    const x0 = Math.min(...xs), x1 = Math.max(...xs), y1 = Math.max(1, ...ys);
    const px = (p: { x: number; y: number }) => [P + ((p.x - x0) / (x1 - x0 || 1)) * (W - 2 * P), H - P - (p.y / y1) * (H - 2 * P)] as const;
    const pts = points.map(px);
    const line = pts.map(([x, y], i) => `${i ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)}`).join(' ');
    const area = `${line} L${pts[pts.length - 1][0].toFixed(1)},${H - P} L${pts[0][0].toFixed(1)},${H - P} Z`;
    return { line, area, last: pts[pts.length - 1], max: y1 };
  });
  const last = $derived(points.length ? points[points.length - 1].y : 0);
  const id = 'g' + Math.random().toString(36).slice(2, 8);
</script>

<figure class="card rise">
  <div class="flex justify-between items-baseline text-xs text-muted mb-1">
    <span class="font-medium text-ink">{label}</span>
    <span class="font-mono tabular-nums">сейчас <b class="text-ink">{format(last)}</b> · макс {format(geo?.max ?? 1)}</span>
  </div>
  <svg viewBox="0 0 {W} {H}" class="w-full h-auto" role="img" aria-label={label}>
    <defs>
      <linearGradient {id} x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="var(--accent)" stop-opacity="0.35" /><stop offset="1" stop-color="var(--accent)" stop-opacity="0" /></linearGradient>
    </defs>
    {#each [0.25, 0.5, 0.75] as f}
      <line x1={P} y1={H - P - f * (H - 2 * P)} x2={W - P} y2={H - P - f * (H - 2 * P)} stroke="currentColor" stroke-opacity="0.08" stroke-dasharray="2 5" />
    {/each}
    <line x1={P} y1={H - P} x2={W - P} y2={H - P} stroke="currentColor" stroke-opacity="0.25" />
    <text x={P} y={P - 8} font-size="10" fill="currentColor" fill-opacity="0.55" font-family="var(--font-mono)">{format(geo?.max ?? 1)}</text>
    {#if geo}
      <path d={geo.area} fill="url(#{id})" />
      <path d={geo.line} fill="none" stroke="var(--accent)" stroke-width="1.8" stroke-linejoin="round" />
      <circle cx={geo.last[0]} cy={geo.last[1]} r="3.5" fill="var(--accent)" stroke="var(--surface)" stroke-width="2" />
    {:else}
      <text x={W / 2} y={H / 2} text-anchor="middle" font-size="12" fill="currentColor" fill-opacity="0.4">нет данных</text>
    {/if}
  </svg>
</figure>
