<script lang="ts">
  let { name, size = 18, class: cls = '' }: { name: string; size?: number; class?: string } = $props();
  const paths: Record<string, string> = {
    home: 'M3 11l9-8 9 8v9a2 2 0 0 1-2 2h-4v-6H9v6H5a2 2 0 0 1-2-2z',
    globe: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zm0 0c-3 3-3 15 0 18m0-18c3 3 3 15 0 18M3 12h18',
    users: 'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8zm13 10v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75',
    code: 'M16 18l6-6-6-6M8 6l-6 6 6 6',
    db: 'M12 3c5 0 9 1.3 9 3s-4 3-9 3-9-1.3-9-3 4-3 9-3zM3 6v12c0 1.7 4 3 9 3s9-1.3 9-3V6M3 12c0 1.7 4 3 9 3s9-1.3 9-3',
    shield: 'M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10zm-3-10l2 2 4-4',
    tasks: 'M9 6h11M9 12h11M9 18h11M4 6h.01M4 12h.01M4 18h.01',
    fire: 'M12 22c4 0 7-3 7-7 0-3-2-5-3-7-1 2-2 3-3 3 0-3-1-6-4-8 0 4-4 6-4 12 0 4 3 7 7 7z',
    archive: 'M21 8v13H3V8M1 3h22v5H1zM10 12h4',
    settings: 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zm8-3a8 8 0 0 0-.1-1.2l2-1.6-2-3.4-2.4 1a8 8 0 0 0-2-1.2L15 3H9l-.5 2.6a8 8 0 0 0-2 1.2l-2.4-1-2 3.4 2 1.6A8 8 0 0 0 4 12a8 8 0 0 0 .1 1.2l-2 1.6 2 3.4 2.4-1a8 8 0 0 0 2 1.2L9 21h6l.5-2.6a8 8 0 0 0 2-1.2l2.4 1 2-3.4-2-1.6c.1-.4.1-.8.1-1.2z',
    sun: 'M12 17a5 5 0 1 0 0-10 5 5 0 0 0 0 10zm0-15v2m0 16v2M4.2 4.2l1.4 1.4m12.8 12.8l1.4 1.4M2 12h2m16 0h2M4.2 19.8l1.4-1.4M18.4 5.6l1.4-1.4',
    moon: 'M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z',
    monitor: 'M2 4h20v12H2zM8 20h8m-4-4v4',
    logout: 'M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4m7 14l5-5-5-5m5 5H9',
    plus: 'M12 5v14M5 12h14',
    trash: 'M3 6h18M8 6V4h8v2m-9 0l1 14h8l1-14',
    pencil: 'M4 20h4L19 9a2.1 2.1 0 0 0-3-3L5 17v3M14.5 6.5l3 3',
    download: 'M12 3v12m0 0 4-4m-4 4-4-4M4 19h16',
    refresh: 'M21 12a9 9 0 1 1-3-6.7L21 8m0-5v5h-5',
    play: 'M6 4l14 8-14 8z',
    stop: 'M6 6h12v12H6z',
    check: 'M20 6L9 17l-5-5',
    x: 'M18 6L6 18M6 6l12 12',
    alert: 'M12 9v4m0 4h.01M10.3 3.9L2.4 18a2 2 0 0 0 1.7 3h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z',
    chevron: 'M9 18l6-6-6-6',
    external: 'M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6m4-3h6v6m-11 5L21 3',
    clock: 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zm0-14v4l3 2',
    box: 'M21 16V8a2 2 0 0 0-1-1.7l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.7l7 4a2 2 0 0 0 2 0l7-4a2 2 0 0 0 1-1.7zM3.3 7l8.7 5 8.7-5M12 22V12',
    file: 'M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8zM14 2v6h6M8 13h8M8 17h8',
    key: 'M21 2l-2 2m-7.6 7.6a5.5 5.5 0 1 1-7.8 7.8 5.5 5.5 0 0 1 7.8-7.8zm0 0L19 3.5m-3.5 3.5L18 9.5',
    terminal: 'M4 17l6-6-6-6m8 14h8',
    lock: 'M19 11H5a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7a2 2 0 0 0-2-2zM7 11V7a5 5 0 0 1 10 0v4',
    save: 'M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2zM17 21v-8H7v8M7 3v5h8',
    cpu: 'M9 3v3m6-3v3M9 18v3m6-3v3M3 9h3m-3 6h3m12-6h3m-3 6h3M6 6h12v12H6zM9 9h6v6H9z'
  };
</script>

<svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" class={cls} aria-hidden="true"><path d={paths[name] ?? ''} /></svg>
