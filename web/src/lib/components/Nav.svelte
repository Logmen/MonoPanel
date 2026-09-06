<script lang="ts">
  import { page } from '$app/state';
  import { auth, logout, theme, setTheme, type ThemeMode } from '$lib/state.svelte';
  import Icon from './Icon.svelte';

  // На узких экранах меню выезжает поверх содержимого и закрывается после
  // перехода; на широких это обычная колонка слева.
  let { open = false, onclose }: { open?: boolean; onclose?: () => void } = $props();
  const items: [string, string, string][] = [
    ['/', 'Дашборд', 'home'], ['/sites', 'Сайты', 'globe'], ['/users', 'Пользователи', 'users'], ['/php', 'PHP', 'code'], ['/databases', 'Базы данных', 'db'],
    ['/files', 'Файлы', 'file'], ['/ssl', 'SSL', 'shield'], ['/jobs', 'Задачи', 'tasks'], ['/firewall', 'Firewall', 'fire'], ['/backups', 'Бэкапы', 'archive'], ['/settings', 'Настройки', 'settings']
  ];
  const admin = $derived(auth.me?.role === 'admin');
  const visible = $derived(items.filter(([href]) => admin || ['/', '/sites', '/databases', '/files', '/jobs', '/settings'].includes(href)));
  const active = (href: string) => page.url.pathname === href || (href !== '/' && page.url.pathname.startsWith(href));
  const modes: [ThemeMode, string, string][] = [['system', 'monitor', 'как в системе'], ['light', 'sun', 'светлая'], ['dark', 'moon', 'тёмная']];
</script>

<aside
  class="w-60 shrink-0 border-r border-line bg-surface p-4 flex flex-col transition-transform duration-200 lg:transition-colors
         fixed inset-y-0 left-0 z-40 h-dvh overflow-y-auto {open ? 'translate-x-0 shadow-2xl' : '-translate-x-full'}
         lg:sticky lg:top-0 lg:z-auto lg:h-screen lg:translate-x-0 lg:shadow-none"
>
  <a href="/" class="flex items-center gap-2.5 mb-6 group">
    <span class="w-8 h-8 rounded-lg bg-accent text-white grid place-items-center font-bold text-sm shadow-[0_0_0_4px_var(--accent-glow)] transition-transform duration-200 group-hover:rotate-[-6deg]">M</span>
    <span class="font-semibold text-[15px] tracking-tight">MonoPanel</span>
  </a>
  <nav class="flex flex-col gap-0.5 text-sm">
    {#each visible as [href, title, icon], i}
      <a {href} onclick={onclose} class="relative flex items-center gap-2.5 px-2.5 py-2.5 rounded-md transition-all duration-150 rise {active(href) ? 'bg-accent-soft text-accent-ink font-medium' : 'text-muted hover:text-ink hover:bg-surface-2'}" style="--i:{i}">
        {#if active(href)}<span class="absolute left-0 top-1/2 -translate-y-1/2 w-0.5 h-5 rounded-full bg-accent"></span>{/if}
        <Icon name={icon} size={17} class="shrink-0 opacity-80" />
        <span>{title}</span>
      </a>
    {/each}
  </nav>
  <div class="mt-auto pt-4 space-y-3">
    <div class="flex rounded-md border border-line p-0.5 bg-surface-2" role="group" aria-label="Тема">
      {#each modes as [m, icon, label]}
        <button class="flex-1 grid place-items-center py-1.5 rounded-[5px] transition-all duration-150 {theme.mode === m ? 'bg-surface text-accent-ink shadow-sm' : 'text-muted hover:text-ink'}" title={label} aria-pressed={theme.mode === m} onclick={() => setTheme(m)}>
          <Icon name={icon} size={15} />
        </button>
      {/each}
    </div>
    <div class="flex items-center justify-between text-xs">
      <div class="min-w-0">
        <div class="font-medium truncate">{auth.me?.login}</div>
        <div class="text-muted font-mono">{auth.me?.role}{auth.version ? ' · v' + auth.version : ''}</div>
      </div>
      <button class="btn btn-ghost btn-sm text-muted" onclick={logout} title="выйти"><Icon name="logout" size={15} /></button>
    </div>
    <a href="/api/v1/docs" target="_blank" class="text-[11px] text-muted hover:text-ink inline-flex items-center gap-1 transition-colors">API docs <Icon name="external" size={11} /></a>
  </div>
</aside>
