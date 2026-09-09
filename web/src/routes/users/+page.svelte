<script lang="ts">
  import { onMount } from 'svelte';
  import { slide } from 'svelte/transition';
  import { api, ApiError, when } from '$lib/api';
  import { auth, notify, dur } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Icon from '$lib/components/Icon.svelte';
  let users = $state<any[] | null>(null);
  let error = $state('');
  let job = $state<number | null>(null);
  let showForm = $state(false);
  let form = $state({ login: '', password: '', email: '', role: 'user', shell: false });
  let panel = $state<{ login: string; kind: 'cron' | 'apps'; items: any[] } | null>(null);
  let cronForm = $state({ schedule: '*/5 * * * *', command: '' });
  let appForm = $state({ name: '', command: '', workdir: '', env_file: '' });
  let del = $state<any>(null);
  let ask = $state<Ask | null>(null);
  let purge = $state(false);
  let confirmLogin = $state('');
  let pw = $state<{ login: string; value: string } | null>(null);
  async function load() { try { users = await api('/users'); } catch (e: any) { error = e.text || String(e); } }
  onMount(load);
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function create(e: Event) {
    e.preventDefault(); error = '';
    try {
      const body: any = { ...form }; if (!body.password) delete body.password; if (!body.email) delete body.email;
      const res: any = await api('/users', { method: 'POST', json: body });
      job = res.job_id; form = { login: '', password: '', email: '', role: 'user', shell: false }; showForm = false; await load();
    } catch (e) { fail(e); }
  }
  async function patch(login: string, body: any) {
    error = '';
    try { const res: any = await api(`/users/${login}`, { method: 'PATCH', json: body }); if (res.job_id) job = res.job_id; notify(`${login}: обновлено`); await load(); } catch (e) { fail(e); }
  }
  async function setPassword() { if (!pw || pw.value.length < 8) { notify('Пароль не короче 8 символов', 'err'); return; } await patch(pw.login, { password: pw.value }); pw = null; }
  async function remove() {
    if (!del || confirmLogin !== del.login) return;
    try { const res: any = await api(`/users/${del.login}?purge=${purge}`, { method: 'DELETE' }); job = res.job_id; notify(`Удаляю ${del.login}…`); del = null; confirmLogin = ''; } catch (e) { fail(e); }
  }
  const askShell = (u: any): Ask => u.shell
    ? { title: `Оставить ${u.login} только SFTP?`, action: 'Оставить SFTP',
        note: 'Вход по SSH закроется, аккаунт останется с доступом к своим файлам по SFTP в пределах домашнего каталога. Уже открытые сессии не разрываются.',
        run: () => patch(u.login, { shell: false }) }
    : { title: `Разрешить ${u.login} вход по SSH?`, action: 'Разрешить',
        note: 'Аккаунт получит настоящую командную оболочку на сервере вместо SFTP-хранилища в своём каталоге.',
        run: () => patch(u.login, { shell: true }) };
  const askStatus = (u: any): Ask => u.status === 'active'
    ? { title: `Заблокировать ${u.login}?`, danger: true, action: 'Заблокировать',
        note: 'Аккаунт перестанет пускать в панель и по SFTP/SSH. Сайты, базы и cron продолжат работать — это блокировка входа, а не остановка хозяйства.',
        run: () => patch(u.login, { status: 'suspended' }) }
    : { title: `Разблокировать ${u.login}?`, action: 'Разблокировать',
        note: 'Вход в панель и по SFTP/SSH снова заработает.',
        run: () => patch(u.login, { status: 'active' }) };
  const askCron = (j: any): Ask => ({
    title: 'Удалить задание cron?',
    note: `Расписание «${j.schedule}» и команда пропадут из crontab пользователя. Восстановить его можно только заново.`,
    danger: true, action: 'Удалить',
    run: () => rmCron(j.id)
  });
  const askApp = (a: any, act: string): Ask => act === 'delete'
    ? { title: `Удалить app-сервис ${a.app.name}?`, danger: true, action: 'Удалить',
        note: 'Systemd-юнит будет остановлен и удалён. Файлы приложения в каталоге пользователя останутся на месте.',
        run: () => appAction(a.app.name, 'delete') }
    : { title: `Остановить ${a.app.name}?`, danger: true, action: 'Остановить',
        note: 'Процесс будет остановлен, и всё, что на него завязано (сайт в режиме proxy, бот, очередь), перестанет отвечать до запуска.',
        run: () => appAction(a.app.name, 'stop') };
  async function openPanel(login: string, kind: 'cron' | 'apps') {
    try { panel = { login, kind, items: await api(kind === 'cron' ? `/users/${login}/cron` : `/users/${login}/apps`) }; } catch (e) { fail(e); }
  }
  async function refreshPanel() { if (panel) await openPanel(panel.login, panel.kind); }
  async function addCron(e: Event) { e.preventDefault(); if (!panel) return; try { await api(`/users/${panel.login}/cron`, { method: 'POST', json: cronForm }); cronForm.command = ''; await refreshPanel(); } catch (e) { fail(e); } }
  async function rmCron(id: number) { if (!panel) return; try { await api(`/users/${panel.login}/cron/${id}`, { method: 'DELETE' }); await refreshPanel(); } catch (e) { fail(e); } }
  async function addApp(e: Event) { e.preventDefault(); if (!panel) return; try { const body: any = { ...appForm }; if (!body.workdir) delete body.workdir; if (!body.env_file) delete body.env_file; await api(`/users/${panel.login}/apps`, { method: 'POST', json: body }); appForm = { name: '', command: '', workdir: '', env_file: '' }; await refreshPanel(); notify('App-сервис запущен'); } catch (e) { fail(e); } }
  async function appAction(name: string, act: string) { if (!panel) return; try { if (act === 'delete') await api(`/users/${panel.login}/apps/${name}`, { method: 'DELETE' }); else await api(`/users/${panel.login}/apps/${name}/${act}`, { method: 'POST' }); await refreshPanel(); } catch (e) { fail(e); } }
</script>

<PageHead title="Пользователи" sub="аккаунты панели с unix-пользователем, SFTP/SSH, cron и app-сервисами">
  <button class="btn btn-primary" onclick={() => (showForm = !showForm)}><Icon name="plus" size={15} /> Новый пользователь</button>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if showForm}
  <form class="card grid md:grid-cols-6 gap-3 items-end mb-4 rise" onsubmit={create}>
    <div><label class="label" for="l">Логин</label><input id="l" class="input font-mono" bind:value={form.login} pattern="[a-z_][a-z0-9_-]{'{'}0,31{'}'}" required /></div>
    <div><label class="label" for="p">Пароль</label><input id="p" class="input" type="password" bind:value={form.password} placeholder="панель + SFTP" /></div>
    <div><label class="label" for="e">E-mail</label><input id="e" class="input" type="email" bind:value={form.email} /></div>
    <div><label class="label" for="r">Роль</label><select id="r" class="input" bind:value={form.role}><option value="user">user</option><option value="admin">admin</option></select></div>
    <label class="text-sm flex items-center gap-1.5"><input type="checkbox" bind:checked={form.shell} /> SSH shell</label>
    <button class="btn btn-primary">Создать</button>
  </form>
{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
<div class="card overflow-x-auto p-0 mb-4 rise">
  {#if !users}<Skeleton rows={5} />{:else}
  <table class="tbl">
    <thead><tr><th>Логин</th><th>Роль</th><th>Статус</th><th>UID</th><th>Доступ</th><th>E-mail</th><th>Создан</th><th></th></tr></thead>
    <tbody>
      {#each users as u, i}
        <tr class="rise" style="--i:{i}">
          <td data-label="Логин" class="font-medium">{u.login}{#if u.login === auth.me?.login}<span class="tag tag-muted ml-1">вы</span>{/if}</td><td data-label="Роль" class="text-muted">{u.role}</td>
          <td><span class="tag {u.status === 'active' ? 'tag-ok' : u.status === 'deleting' ? 'tag-err' : 'tag-warn'}">{#if u.status === 'deleting'}<span class="dot dot-live"></span>{/if}{u.status}</span></td>
          <td data-label="UID" class="font-mono">{u.unix_uid ?? '—'}</td>
          <td>{#if u.role === 'user'}<span class="tag tag-muted">{u.shell ? 'SSH shell' : 'SFTP-only'}</span>{/if}</td>
          <td data-label="E-mail" class="text-muted">{u.email || '—'}</td><td data-label="Создан" class="text-xs text-muted">{when(u.created_at)}</td>
          <td><div class="row-actions">
            <button class="btn btn-sm" onclick={() => (pw = { login: u.login, value: '' })} title="сменить пароль"><Icon name="key" size={13} /></button>
            {#if u.role === 'user'}
              <button class="btn btn-sm" onclick={() => (ask = askShell(u))}>{u.shell ? '→ SFTP-only' : '→ shell'}</button>
              <button class="btn btn-sm" onclick={() => openPanel(u.login, 'cron')}><Icon name="clock" size={13} /> cron</button>
              <button class="btn btn-sm" onclick={() => openPanel(u.login, 'apps')}><Icon name="box" size={13} /> apps</button>
              <button class="btn btn-sm" onclick={() => (ask = askStatus(u))}>{u.status === 'active' ? 'заблокировать' : 'разблокировать'}</button>
            {/if}
            {#if u.login !== auth.me?.login}<button class="btn btn-danger btn-sm" onclick={() => { del = u; purge = false; confirmLogin = ''; }} title="удалить"><Icon name="trash" size={13} /></button>{/if}
          </div></td>
        </tr>
      {/each}
    </tbody>
  </table>
  {/if}
</div>

{#if panel}
  <div class="card rise" transition:slide={{ duration: dur(180) }}>
    <div class="flex justify-between items-center mb-3"><span class="font-medium">{panel.kind === 'cron' ? 'Cron' : 'App-сервисы'}: <span class="font-mono">{panel.login}</span></span><div class="flex gap-1"><button class="btn btn-sm" onclick={refreshPanel}><Icon name="refresh" size={13} /></button><button class="btn btn-sm" onclick={() => (panel = null)}>закрыть</button></div></div>
    {#if panel.kind === 'cron'}
      <form class="grid md:grid-cols-4 gap-2 items-end mb-3" onsubmit={addCron}>
        <div><label class="label" for="cs">Расписание</label><input id="cs" class="input font-mono" bind:value={cronForm.schedule} /></div>
        <div class="md:col-span-2"><label class="label" for="cc">Команда</label><input id="cc" class="input font-mono" bind:value={cronForm.command} placeholder="php ~/data/www/site/cron.php" required /></div>
        <button class="btn btn-primary">Добавить</button>
      </form>
      <table class="tbl"><thead><tr><th>ID</th><th>Расписание</th><th>Команда</th><th></th></tr></thead><tbody>
        {#each panel.items as j}<tr><td data-label="ID">{j.id}</td><td data-label="Расписание" class="font-mono">{j.schedule}{#if !j.enabled} <span class="tag tag-muted">off</span>{/if}</td><td data-label="Команда" class="font-mono text-xs">{j.command}</td><td data-label="" class="text-right"><button class="btn btn-danger btn-sm" onclick={() => (ask = askCron(j))}><Icon name="trash" size={13} /></button></td></tr>{/each}
        {#if !panel.items.length}<tr><td colspan="4" class="text-muted text-center py-4">Заданий нет.</td></tr>{/if}
      </tbody></table>
    {:else}
      <form class="grid md:grid-cols-5 gap-2 items-end mb-3" onsubmit={addApp}>
        <div><label class="label" for="an">Имя</label><input id="an" class="input font-mono" bind:value={appForm.name} pattern="[a-z0-9][a-z0-9_-]{'{'}0,31{'}'}" required /></div>
        <div class="md:col-span-2"><label class="label" for="ac">Команда (абсолютный путь)</label><input id="ac" class="input font-mono" bind:value={appForm.command} placeholder="/var/www/{panel.login}/data/venv/bin/gunicorn --bind 127.0.0.1:5000 app:app" required /></div>
        <div><label class="label" for="aw">Workdir</label><input id="aw" class="input font-mono" bind:value={appForm.workdir} placeholder="~/data" /></div>
        <button class="btn btn-primary">Запустить</button>
      </form>
      <table class="tbl"><thead><tr><th>Имя</th><th>Состояние</th><th>Команда</th><th></th></tr></thead><tbody>
        {#each panel.items as a}
          {@const st = a.service?.active_state || a.app.status}
          <tr><td data-label="Имя" class="font-mono">{a.app.name}</td><td data-label="Состояние"><span class="tag {st === 'active' ? 'tag-ok' : st === 'failed' ? 'tag-err' : 'tag-muted'}">{#if st === 'active'}<span class="dot dot-live"></span>{/if}{st}{a.service ? '/' + a.service.sub_state : ''}</span>{#if !a.app.enabled}<span class="tag tag-muted ml-1">автозапуск off</span>{/if}</td><td data-label="Команда" class="font-mono text-xs max-w-md truncate" title={a.app.command}>{a.app.command}</td>
          <td><div class="row-actions"><button class="btn btn-sm" onclick={() => appAction(a.app.name, 'restart')} title="перезапустить"><Icon name="refresh" size={13} /></button>{#if st === 'active'}<button class="btn btn-sm" onclick={() => (ask = askApp(a, 'stop'))} title="остановить"><Icon name="stop" size={13} /></button>{:else}<button class="btn btn-sm" onclick={() => appAction(a.app.name, 'start')}><Icon name="play" size={13} /></button>{/if}<button class="btn btn-danger btn-sm" onclick={() => (ask = askApp(a, 'delete'))} title="удалить"><Icon name="trash" size={13} /></button></div></td></tr>
        {/each}
        {#if !panel.items.length}<tr><td colspan="4" class="text-muted text-center py-4">App-сервисов нет.</td></tr>{/if}
      </tbody></table>
    {/if}
  </div>
{/if}

<Confirm bind:ask />

<Modal open={!!pw} title="Новый пароль для {pw?.login}" onclose={() => (pw = null)}>
  <p class="text-muted">Пароль панели и SFTP/SSH, не короче 8 символов.</p>
  {#if pw}<input class="input font-mono" type="text" bind:value={pw.value} autocomplete="new-password" onkeydown={(e) => e.key === 'Enter' && setPassword()} />{/if}
  {#snippet footer()}<button class="btn" onclick={() => (pw = null)}>Отмена</button><button class="btn btn-primary" onclick={setPassword}>Сохранить</button>{/snippet}
</Modal>

<Modal open={!!del} title="Удалить пользователя {del?.login}?" onclose={() => { del = null; confirmLogin = ''; }}>
  <p>Будут удалены сайты, базы данных, cron, app-сервисы, сертификаты сайтов и unix-аккаунт{#if del?.role === 'admin'} (это администратор панели){/if}.</p>
  {#if del?.unix_uid}
    <label class="flex items-start gap-2 p-2 rounded-md border {purge ? 'border-danger/50 bg-danger-soft' : 'border-line'} transition-colors"><input type="checkbox" bind:checked={purge} class="mt-0.5" /><span>удалить и все файлы в <code class="font-mono">/var/www/{del.login}</code><br /><span class="text-xs text-muted">без галочки каталог останется на диске</span></span></label>
  {/if}
  <div><label class="label" for="cl">Введите логин для подтверждения</label><input id="cl" class="input font-mono" bind:value={confirmLogin} placeholder={del?.login} onkeydown={(e) => e.key === 'Enter' && remove()} /></div>
  {#snippet footer()}<button class="btn" onclick={() => (del = null)}>Отмена</button><button class="btn btn-danger" disabled={confirmLogin !== del?.login} onclick={remove}><Icon name="trash" size={14} /> Удалить{purge ? ' с файлами' : ''}</button>{/snippet}
</Modal>
