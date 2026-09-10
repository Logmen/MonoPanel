<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';

  // Everything the panel installs on the host besides sites: the web
  // servers, the database, fail2ban and the small tools. PHP branches have
  // their own page.
  let stack = $state<any[] | null>(null);
  let mem = $state<any>(null);
  let memForm = $state({ memory_mb: 128, max_connections: 1024 });
  let job = $state<number | null>(null);
  let error = $state('');
  let ask = $state<Ask | null>(null);
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function load() {
    try {
      const [s, m] = await Promise.all([api('/stack'), api('/stack/memcached')]);
      stack = s as any[]; mem = m; memForm = { memory_mb: mem.memory_mb, max_connections: mem.max_connections };
    } catch (e: any) { error = e.text || String(e); }
  }
  onMount(load);

  const info: Record<string, { title: string; text: string }> = {
    nginx: { title: 'nginx', text: 'фронт всех сайтов: статика, TLS, HTTP/2 и HTTP/3, прокси к php-fpm, Apache или приложению' },
    apache: { title: 'Apache 2.4', text: 'для сайтов в режиме apache: .htaccess и mod_rewrite за nginx, php-fpm через mod_proxy_fcgi' },
    percona: { title: 'Percona Server 8.4', text: 'СУБД для сайтов; базы и пользователи создаются из панели' },
    mysql: { title: 'MySQL 8.4', text: 'СУБД для сайтов; базы и пользователи создаются из панели' },
    fail2ban: { title: 'fail2ban', text: 'баны за перебор паролей: sshd, панель, nginx; работает вместе с firewall панели' },
    memcached: { title: 'Memcached', text: 'кеш в памяти для сайтов; слушает только 127.0.0.1:11211, у ветки PHP должно быть включено расширение memcached (для 1С-Битрикс — memcache)' },
    jpegoptim: { title: 'Jpegoptim', text: 'утилита сжатия JPEG без потерь и с потерями; её зовут скрипты и CMS' },
    git: { title: 'Git', text: 'система контроля версий: деплой сайтов из репозитория, composer ставит пакеты из git' },
    composer: { title: 'Composer', text: 'менеджер пакетов PHP; ставится с getcomposer.org с проверкой контрольной суммы и работает на новейшей ветке PHP панели' },
    sphinx: { title: 'Sphinx', text: 'полнотекстовый поиск для 1С-Битрикс: на Debian/Ubuntu — Sphinx 2.2 из дистрибутива, на EL — Manticore Search; индекс bitrix, SphinxQL на 127.0.0.1:9306. В Битриксе: Настройки → Поиск → Sphinx, строка подключения 127.0.0.1:9306' }
  };
  const groups: [string, string[]][] = [
    ['Веб-серверы', ['nginx', 'apache']],
    ['База данных', ['percona', 'mysql']],
    ['Защита', ['fail2ban']],
    ['Расширения', ['memcached', 'jpegoptim', 'git', 'composer', 'sphinx']]
  ];
  const byName = $derived(Object.fromEntries((stack ?? []).map((c: any) => [c.name, c])) as Record<string, any>);
  const dbInstalled = $derived(!!(byName.percona?.installed || byName.mysql?.installed));

  async function install(name: string) { error = ''; try { const r: any = await api('/stack/install', { method: 'POST', json: { component: name } }); job = r.job_id; } catch (e) { fail(e); } }
  const askInstall = (name: string): Ask => ({
    title: `Установить ${info[name]?.title ?? name}?`,
    note: name === 'composer' ? 'composer.phar скачается с getcomposer.org, контрольная сумма сверится с опубликованной. Запускаться будет на новейшей установленной ветке PHP; повторная установка обновляет его.' :
      name === 'memcached' ? 'Пакет установится, сервис поднимется на 127.0.0.1:11211 с настройками ниже. Сайтам ещё понадобится расширение memcached у их ветки PHP (страница PHP).' :
      name === 'sphinx' ? 'Установится сервер поиска с готовым индексом bitrix (все атрибуты по документации Битрикса) и SphinxQL на 127.0.0.1:9306. На EL подключится репозиторий Manticore.' :
      name === 'percona' || name === 'mysql' ? 'Подключится репозиторий вендора, установится сервер, root перейдёт на auth_socket, конфигурация подберётся по объёму памяти.' :
      'Подключится репозиторий, если нужен, установятся пакеты и, если есть, сервис. Займёт от нескольких секунд до пары минут.',
    action: 'Установить',
    run: () => install(name)
  });
  const askRemove = (name: string): Ask => ({
    title: `Удалить ${info[name]?.title ?? name}?`,
    note: name === 'memcached' ? 'Сервис остановится, пакет удалится. Сайты, которые держат в нём кеш и сессии, начнут получать ошибки подключения.' :
      name === 'composer' ? 'Файлы composer будут удалены; проекты на сервере продолжат работать, но обновлять зависимости будет нечем.' :
      name === 'sphinx' ? 'Сервис поиска остановится, пакет удалится; данные индекса останутся на диске. Битрикс, переключённый на Sphinx, вернётся к поиску по базе только после смены настройки.' :
      'Пакет будет удалён с сервера.',
    danger: true, action: 'Удалить',
    run: async () => { const r: any = await api(`/stack/${name}`, { method: 'DELETE' }); job = r.job_id; }
  });
  async function saveMem(e: Event) { e.preventDefault(); error = ''; try { mem = await api('/stack/memcached', { method: 'PUT', json: memForm }); notify(mem.installed ? 'memcached перенастроен и перезапущен' : 'настройки сохранены, применятся при установке'); } catch (e) { fail(e); } }
  const stateOf = (c: any) => !c ? 'не установлен' : !c.installed ? 'не установлен' : c.service ? `${c.service.active_state}` : 'установлен';
</script>

<PageHead title="Расширения" sub="что панель ставит на сервер: веб-серверы, СУБД, защита и инструменты; версии PHP — на странице PHP" />
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
{#if !stack}<div class="card p-0"><Skeleton rows={8} /></div>{:else}
  {#each groups as [group, names], gi}
    <div class="card overflow-x-auto p-0 mb-4 rise" style="--i:{gi}">
      <div class="px-4 pt-3 font-medium">{group}</div>
      <table class="tbl"><thead><tr><th>Компонент</th><th>Что делает</th><th>Версия</th><th>Состояние</th><th></th></tr></thead>
        <tbody>
          {#each names as name}
            {@const c = byName[name]}
            {#if c || group !== 'База данных'}
              <tr>
                <td data-label="Компонент" class="font-medium whitespace-nowrap">{info[name]?.title ?? name}</td>
                <td data-label="Что делает" class="text-sm text-muted max-w-md">{info[name]?.text}</td>
                <td data-label="Версия" class="font-mono text-xs">{c?.version || '—'}</td>
                <td data-label="Состояние"><span class="tag {c?.installed ? (c.service && c.service.active_state !== 'active' ? 'tag-err' : 'tag-ok') : 'tag-muted'}">{stateOf(c)}</span></td>
                <td data-label=""><div class="row-actions">
                  {#if !c?.installed}
                    {#if (name === 'percona' || name === 'mysql') && dbInstalled}<span class="text-xs text-muted">другой сервер уже стоит</span>
                    {:else}<button class="btn btn-sm btn-primary" onclick={() => (ask = askInstall(name))}><Icon name="download" size={13} /> установить</button>{/if}
                  {:else}
                    {#if name === 'composer'}<button class="btn btn-sm" onclick={() => (ask = askInstall(name))}><Icon name="refresh" size={13} /> обновить</button>{/if}
                    {#if c.removable}<button class="btn btn-sm btn-danger" onclick={() => (ask = askRemove(name))}><Icon name="trash" size={13} /></button>{/if}
                  {/if}
                </div></td>
              </tr>
            {/if}
          {/each}
        </tbody></table>
    </div>
  {/each}
  {#if mem}
    <form class="card grid md:grid-cols-4 gap-3 items-end rise" onsubmit={saveMem}>
      <div class="md:col-span-4"><div class="font-medium">Memcached · настройки</div><div class="text-xs text-muted">Слушает только 127.0.0.1:11211. {mem.installed ? 'Изменения применяются сразу, сервис перезапускается — кеш при этом сбрасывается.' : 'Сохраняются и применятся при установке.'}</div></div>
      <div><label class="label" for="mm">Память, МБ</label><input id="mm" class="input" type="number" min="16" max="65536" bind:value={memForm.memory_mb} /></div>
      <div><label class="label" for="mc">Соединений</label><input id="mc" class="input" type="number" min="64" max="65536" bind:value={memForm.max_connections} /></div>
      <button class="btn btn-primary">Сохранить{mem.installed ? ' и применить' : ''}</button>
    </form>
  {/if}
{/if}
<Confirm bind:ask />
