<script lang="ts">
  import '../app.css';
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { fly, fade } from 'svelte/transition';
  import { auth, loadMe, toastValue, initTheme, dur } from '$lib/state.svelte';
  import Login from '$lib/components/Login.svelte';
  import Nav from '$lib/components/Nav.svelte';
  let { children } = $props();
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
    <Nav />
    <main class="flex-1 min-w-0 px-6 py-6 lg:px-8">
      {#key page.url.pathname}
        <div class="max-w-6xl" in:fly={{ y: 8, duration: dur(220) }}>{@render children()}</div>
      {/key}
    </main>
  </div>
{/if}
{#if toastValue.visible}
  <div class="fixed bottom-5 right-5 z-50 max-w-md px-4 py-2.5 text-sm rounded-lg border shadow-lg backdrop-blur {toastValue.kind === 'err' ? 'bg-danger-soft border-danger/40 text-danger' : 'bg-accent-soft border-accent/40 text-accent-ink'}" in:fly={{ y: 12, duration: dur(200) }} out:fade={{ duration: dur(150) }} role="status">{toastValue.text}</div>
{/if}
