<script lang="ts">
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { api } from '$lib/api';
  import { auth } from '$lib/state.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import FileManager from '$lib/components/FileManager.svelte';

  const admin = $derived(auth.me?.role === 'admin');
  let users = $state<any[]>([]);
  let sites = $state<any[]>([]);
  let user = $state('');
  let error = $state('');

  // Ссылка вида /files?user=alex&path=/data/www/example.com ведёт прямо в каталог сайта.
  const start = $derived(page.url.searchParams.get('path') || '/');

  onMount(async () => {
    const wanted = page.url.searchParams.get('user') || '';
    try {
      if (admin) {
        const list: any[] = await api('/users');
        // Файлы есть только у аккаунтов с unix-пользователем.
        users = list.filter((u) => u.unix_uid);
        user = wanted && users.some((u) => u.login === wanted) ? wanted : (users[0]?.login ?? '');
      } else {
        user = auth.me?.login ?? '';
      }
      sites = await api('/sites');
    } catch (e: any) {
      error = e.text || String(e);
    }
  });

  const userSites = $derived(sites.filter((s) => s.login === user).map((s) => ({ domain: s.domain, docroot: s.docroot })));
</script>

<PageHead title="Файлы" sub="каталоги сайтов, загрузка и редактор — от имени владельца">
  {#if admin && users.length}
    <select class="input w-auto py-1.5 text-sm" bind:value={user}>
      {#each users as u}<option value={u.login}>{u.login}</option>{/each}
    </select>
  {/if}
</PageHead>

{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}

{#if user}
  {#key user}
    <FileManager {user} {start} sites={userSites} />
  {/key}
{:else if !error}
  <div class="card text-sm text-muted">Нет аккаунтов с домашним каталогом — файлы появятся после создания пользователя.</div>
{/if}
