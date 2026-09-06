<script lang="ts">
  import '../app.css';
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { fly, fade } from 'svelte/transition';
  import { auth, loadMe, toastValue, initTheme, dur } from '$lib/state.svelte';
  import Login from '$lib/components/Login.svelte';
  import Nav from '$lib/components/Nav.svelte';
  import Icon from '$lib/components/Icon.svelte';
  let { children } = $props();
  let navOpen = $state(false);
  onMount(() => { initTheme(); loadMe(); });
</script>

{#if auth.loading}
  <div class="min-h-screen grid place-items-center">
    <div class="flex items-center gap-3 text-muted text-sm" in:fade={{ duration: dur(200) }}>
      <span class="w-2 h-2 rounded-full bg-accent dot-live"></span> загрузка…
    </div>
  </div>
{:else if !auth.me}
  <Login />
{:else}
  <div class="flex min-h-screen">
    <Nav open={navOpen} onclose={() => (navOpen = false)} />
    {#if navOpen}
      <button
        class="fixed inset-0 z-30 bg-black/40 backdrop-blur-[2px] lg:hidden"
        aria-label="закрыть меню"
        onclick={() => (navOpen = false)}
        transition:fade={{ duration: dur(150) }}
      ></button>
    {/if}
    <div class="flex-1 min-w-0 flex flex-col">
      <!-- Шапка с кнопкой меню — только там, где меню спрятано. -->
      <header class="lg:hidden sticky top-0 z-20 flex items-center gap-3 px-4 h-14 border-b border-line bg-surface/95 backdrop-blur">
        <button class="btn btn-ghost px-2" aria-label="меню" aria-expanded={navOpen} onclick={() => (navOpen = true)}>
          <Icon name="menu" size={20} />
        </button>
        <a href="/" class="flex items-center gap-2 font-semibold tracking-tight">
          <span class="w-7 h-7 rounded-lg bg-accent text-white grid place-items-center font-bold text-xs">M</span>
          MonoPanel
        </a>
      </header>
      <main class="flex-1 min-w-0 px-4 py-5 sm:px-6 sm:py-6 lg:px-8">
        {#key page.url.pathname}
          <div class="max-w-6xl" in:fly={{ y: 8, duration: dur(220) }}>{@render children()}</div>
        {/key}
      </main>
    </div>
  </div>
{/if}
{#if toastValue.visible}
  <div class="fixed bottom-4 right-4 left-4 sm:left-auto sm:bottom-5 sm:right-5 z-50 sm:max-w-md px-4 py-2.5 text-sm rounded-lg border shadow-lg backdrop-blur {toastValue.kind === 'err' ? 'bg-danger-soft border-danger/40 text-danger' : 'bg-accent-soft border-accent/40 text-accent-ink'}" in:fly={{ y: 12, duration: dur(200) }} out:fade={{ duration: dur(150) }} role="status">{toastValue.text}</div>
{/if}
