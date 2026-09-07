<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError } from '$lib/api';
  import { auth, notify } from '$lib/state.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Icon from '$lib/components/Icon.svelte';

  const admin = $derived(auth.me?.role === 'admin');
  let st = $state<any>(null);
  let domains = $state<any[]>([]);
  let boxes = $state<any[]>([]);
  let aliases = $state<any[]>([]);
  let users = $state<any[]>([]);
  let error = $state('');
  let loading = $state(true);
  let job = $state<number | null>(null);
  let tab = $state<'domains' | 'boxes' | 'aliases'>('domains');
  let created = $state<any>(null);
  let dns = $state<any>(null);
  let dnsLoading = $state(false);
  let del = $state<{ kind: string; name: string; note: string } | null>(null);
  let purge = $state(false);

  let install = $state({ hostname: '' });
  let domainForm = $state({ name: '', user: '', lenient: false });
  let boxForm = $state({ address: '', password: '', name: '', quota_mb: 1024 });
  let aliasForm = $state({ address: '', destinations: '' });
  let webmailForm = $state({ domain: '', user: '', port: 2096 });
  let settings = $state({ hostname: '', max_size_mb: 50, pop3: true, dkim: true, port25: true, rbl: '', webmail_port: 0 });
  let showSettings = $state(false);

  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };

  // Всё разом: статус опрашивает почтовые порты, и ждать его, чтобы потом
  // сходить ещё три раза подряд, — это лишняя секунда на пустом месте.
  async function load() {
    try {
      const [status, d, b, a, u] = await Promise.all([
        api('/mail'), api('/mail/domains'), api('/mail/mailboxes'), api('/mail/aliases'),
        admin ? api('/users') : Promise.resolve([])
      ]);
      st = status;
      settings = { hostname: st.hostname ?? '', max_size_mb: st.max_size_mb || 50, pop3: !!st.pop3, dkim: !!st.dkim, port25: !!st.port25, rbl: (st.rbl ?? []).join(', '), webmail_port: st.webmail_port ?? 0 };
      domains = d as any[];
      boxes = b as any[];
      aliases = a as any[];
      users = (u as any[]).filter((x) => x.role === 'user' && x.unix_uid);
    } catch (e) { fail(e); } finally { loading = false; }
  }
  onMount(load);

  async function doInstall() {
    error = '';
    try { const r: any = await api('/mail/install', { method: 'POST', json: install }); job = r.job_id; } catch (e) { fail(e); }
  }
  async function saveSettings(e: Event) {
    e.preventDefault(); error = '';
    try {
      const body: any = { hostname: settings.hostname, max_size_mb: settings.max_size_mb, pop3: settings.pop3, dkim: settings.dkim, port25: settings.port25,
        webmail_port: Number(settings.webmail_port) || 0, rbl: settings.rbl.split(',').map((s) => s.trim()).filter(Boolean) };
      st = await api('/mail/settings', { method: 'PUT', json: body });
      notify('настройки сохранены'); showSettings = false; await load();
    } catch (e) { fail(e); }
  }
  async function addDomain(e: Event) {
    e.preventDefault(); error = '';
    try {
      const body: any = { name: domainForm.name.trim(), lenient: domainForm.lenient };
      if (admin) body.user = domainForm.user;
      await api('/mail/domains', { method: 'POST', json: body });
      domainForm.name = ''; notify('домен добавлен — пропишите записи в DNS'); await load();
    } catch (e) { fail(e); }
  }
  async function addBox(e: Event) {
    e.preventDefault(); error = '';
    try {
      const body: any = { address: boxForm.address.trim(), quota_mb: boxForm.quota_mb, name: boxForm.name };
      if (boxForm.password) body.password = boxForm.password;
      created = await api('/mail/mailboxes', { method: 'POST', json: body });
      boxForm = { address: '', password: '', name: '', quota_mb: 1024 }; await load();
    } catch (e) { fail(e); }
  }
  async function addAlias(e: Event) {
    e.preventDefault(); error = '';
    try {
      await api('/mail/aliases', { method: 'POST', json: { address: aliasForm.address.trim(), destinations: aliasForm.destinations.split(',').map((s) => s.trim()).filter(Boolean) } });
      aliasForm = { address: '', destinations: '' }; notify('алиас создан'); await load();
    } catch (e) { fail(e); }
  }
  async function newPassword(address: string) {
    try { const r: any = await api(`/mail/mailboxes/${address}`, { method: 'PATCH', json: {} }); created = { mailbox: { address }, password: r.password, reset: true }; } catch (e) { fail(e); }
  }
  async function toggleBox(b: any) {
    try { await api(`/mail/mailboxes/${b.address}`, { method: 'PATCH', json: { active: !b.active } }); await load(); } catch (e) { fail(e); }
  }
  async function remove() {
    if (!del) return;
    try {
      if (del.kind === 'domain') await api(`/mail/domains/${del.name}`, { method: 'DELETE' });
      if (del.kind === 'box') await api(`/mail/mailboxes/${del.name}${purge ? '?purge=true' : ''}`, { method: 'DELETE' });
      if (del.kind === 'alias') await api(`/mail/aliases/${del.name}`, { method: 'DELETE' });
      notify(`${del.name} удалён`); del = null; purge = false; await load();
    } catch (e) { fail(e); }
  }
  async function showDNS(name: string) {
    dnsLoading = true; dns = { domain: name, records: [] };
    try { dns = await api(`/mail/domains/${name}/dns`); } catch (e) { fail(e); dns = null; } finally { dnsLoading = false; }
  }
  async function toggleLenient(d: any) {
    try { await api(`/mail/domains/${d.name}`, { method: 'PATCH', json: { lenient: !d.lenient } }); await load(); } catch (e) { fail(e); }
  }
  async function rotateDKIM(name: string) {
    try { await api(`/mail/domains/${name}/dkim`, { method: 'POST', json: {} }); notify('новый ключ выпущен — обновите TXT-запись'); await load(); await showDNS(name); } catch (e) { fail(e); }
  }
  async function installWebmail(e: Event) {
    e.preventDefault(); error = '';
    try { const r: any = await api('/mail/webmail', { method: 'POST', json: { ...webmailForm, port: Number(webmailForm.port) || 0 } }); job = r.job_id; } catch (e) { fail(e); }
  }
  const mark = (s: string) => ({ ok: 'text-ok', missing: 'text-danger', mismatch: 'text-warn', unknown: 'text-muted' })[s] ?? 'text-muted';
  const markText = (s: string) => ({ ok: 'опубликовано', missing: 'нет записи', mismatch: 'не совпадает', unknown: 'не проверено' })[s] ?? s;
</script>

<PageHead title="Почта" sub={loading ? '' : st?.installed ? `${st.hostname} · postfix ${st.versions?.postfix ?? ''} · dovecot ${st.versions?.dovecot ?? ''}` : 'IMAP, POP3, SMTP и вебпочта Roundcube'}>
  {#if st?.installed && admin}
    <button class="btn" onclick={() => (showSettings = !showSettings)}><Icon name="settings" size={15} /> Настройки</button>
  {/if}
</PageHead>

{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => { job = null; load(); }} /></div>{/if}

{#if loading}
  <div class="card rise"><Skeleton rows={5} /></div>
{:else if st && !st.installed}
  <div class="card mb-4 rise">
    <div class="text-sm mb-3">Почтовый сервер не установлен. Панель поставит <b>postfix</b> (SMTP), <b>dovecot</b> (IMAP/POP3, пароли, квоты) и <b>opendkim</b> (подпись писем), выпустит сертификат и откроет порты.</div>
    {#if admin}
      <form class="flex flex-wrap gap-3 items-end" onsubmit={(e) => { e.preventDefault(); doInstall(); }}>
        <div class="grow max-w-sm"><label class="label" for="h">Имя почтового сервера</label>
          <input id="h" class="input font-mono" bind:value={install.hostname} placeholder="mail.example.com" />
          <p class="text-xs text-muted mt-1">Оно попадёт в MX, HELO и сертификат. Пусто — возьмём FQDN хоста.</p></div>
        <button class="btn btn-primary">Установить</button>
      </form>
    {:else}
      <div class="text-sm text-muted">Обратитесь к администратору сервера.</div>
    {/if}
  </div>
{/if}

{#if st?.installed && !loading}
  {#if showSettings && admin}
    <form class="card mb-4 grid md:grid-cols-3 gap-3 items-end rise" onsubmit={saveSettings}>
      <div><label class="label" for="sh">Имя сервера</label><input id="sh" class="input font-mono" bind:value={settings.hostname} /></div>
      <div><label class="label" for="sm">Размер письма, МБ</label><input id="sm" class="input" type="number" min="1" max="512" bind:value={settings.max_size_mb} /></div>
      <div><label class="label" for="sr">Чёрные списки</label><input id="sr" class="input font-mono" bind:value={settings.rbl} placeholder="zen.spamhaus.org" /></div>
      <div><label class="label" for="sw">Порт вебпочты</label><input id="sw" class="input" type="number" min="0" max="65535" bind:value={settings.webmail_port} />
        <p class="text-xs text-muted mt-1">0 — только по своему домену</p></div>
      <label class="flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={settings.pop3} /> POP3 (110/995)</label>
      <label class="flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={settings.dkim} /> Подписывать письма DKIM</label>
      <label class="flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={settings.port25} /> Принимать почту на 25 порту</label>
      <div class="md:col-span-3 flex gap-2"><button class="btn btn-primary">Сохранить и применить</button><button type="button" class="btn" onclick={() => (showSettings = false)}>Отмена</button></div>
    </form>
  {/if}

  <div class="grid gap-3 md:grid-cols-3 mb-4">
    <div class="card rise">
      <div class="text-xs text-muted mb-2">Сервисы</div>
      {#each st.services ?? [] as s}
        <div class="flex justify-between text-sm py-0.5"><span class="font-mono">{s.unit.replace('.service', '')}</span>
          <span class={s.active_state === 'active' ? 'text-ok' : 'text-danger'}>{s.active_state}</span></div>
      {/each}
      <div class="flex justify-between text-sm py-0.5 border-t border-line mt-2 pt-2"><span>TLS</span>
        <span class={st.tls === 'acme' || st.tls === 'custom' ? 'text-ok' : 'text-warn'}>{st.tls}{st.cert_until ? ' до ' + st.cert_until.slice(0, 10) : ''}</span></div>
    </div>
    <div class="card rise" style="--i:1">
      <div class="text-xs text-muted mb-2">Порты</div>
      <div class="flex flex-wrap gap-1.5">
        {#each st.ports ?? [] as p}
          <span class="text-xs px-1.5 py-0.5 rounded border {p.managed && p.open ? 'border-ok/40 text-ok' : p.managed ? 'border-danger/40 text-danger' : p.open ? 'border-warn/40 text-warn' : 'border-line text-muted'}"
                title={p.open ? p.owner || 'слушает' : 'не слушает'}>{p.port} {p.name}{#if p.open && !p.managed} · чужой{/if}</span>
        {/each}
      </div>
    </div>
    <div class="card rise" style="--i:2">
      <div class="text-xs text-muted mb-2">Вебпочта</div>
      {#if st.webmail}
        <a class="text-sm text-accent-ink hover:underline inline-flex items-center gap-1 break-all" href={st.webmail_url} target="_blank">{st.webmail_url.replace('https://', '').replace(/\/$/, '')} <Icon name="external" size={12} /></a>
        <div class="text-xs text-muted mt-1">Roundcube {st.versions?.roundcube}{#if st.webmail_port} · сайт {st.webmail}{/if}</div>
      {:else if admin}
        <form class="space-y-2" onsubmit={installWebmail}>
          <input class="input font-mono text-sm" bind:value={webmailForm.domain} placeholder="webmail.example.com" required />
          <div class="flex gap-2">
            <select class="input text-sm" bind:value={webmailForm.user} required><option value="">владелец</option>{#each users as u}<option value={u.login}>{u.login}</option>{/each}</select>
            <input class="input text-sm w-24" type="number" min="0" max="65535" bind:value={webmailForm.port} title="порт на имени почтового сервера; 0 — только по домену" />
            <button class="btn btn-primary btn-sm whitespace-nowrap">Поставить</button>
          </div>
          <p class="text-xs text-muted">Порт открывает почту на {st.hostname} с его сертификатом — своя запись в DNS не нужна.</p>
        </form>
      {:else}
        <div class="text-sm text-muted">не установлена</div>
      {/if}
    </div>
  </div>

  {#if st.warnings?.length}
    <div class="card mb-4 border-warn/40 rise">
      {#each st.warnings as w}<div class="text-sm flex gap-2 items-start"><Icon name="alert" size={15} class="text-warn shrink-0 mt-0.5" /><span>{w}</span></div>{/each}
    </div>
  {/if}

  {#if created}
    <div class="card mb-4 text-sm font-mono rise border-accent/40">
      {created.reset ? 'новый пароль' : 'ящик'} <b>{created.mailbox.address}</b>{#if created.password} · пароль: <b class="select-all">{created.password}</b>{/if}
      {#if created.imap}<div class="text-xs text-muted mt-1">IMAP: {created.imap} · SMTP: {created.smtp}</div>{/if}
      <div class="text-xs text-muted">показывается один раз</div>
    </div>
  {/if}

  <div class="flex gap-1 mb-3 text-sm">
    {#each [['domains', 'Домены', domains.length], ['boxes', 'Ящики', boxes.length], ['aliases', 'Алиасы', aliases.length]] as [key, title, n]}
      <button class="px-3 py-1.5 rounded-md transition-colors {tab === key ? 'bg-accent-soft text-accent-ink font-medium' : 'text-muted hover:text-ink'}" onclick={() => (tab = key as any)}>{title} <span class="tabular-nums opacity-60">{n}</span></button>
    {/each}
  </div>

  {#if tab === 'domains'}
    <form class="card grid md:grid-cols-4 gap-3 items-end mb-3 rise" onsubmit={addDomain}>
      <div><label class="label" for="dn">Домен</label><input id="dn" class="input font-mono" bind:value={domainForm.name} placeholder="example.com" required /></div>
      {#if admin}<div><label class="label" for="du">Владелец</label><select id="du" class="input" bind:value={domainForm.user} required><option value="">—</option>{#each users as u}<option value={u.login}>{u.login}</option>{/each}</select></div>{/if}
      <label class="flex items-center gap-2 text-sm pb-2" title="принимать письма и от криво настроенных отправителей — для диагностических приёмников">
        <input type="checkbox" bind:checked={domainForm.lenient} /> домен-приёмник
      </label>
      <button class="btn btn-primary">Добавить домен</button>
    </form>
    <div class="card overflow-x-auto p-0 rise">
      <table class="tbl"><thead><tr><th>Домен</th><th>Владелец</th><th>Ящиков</th><th>Алиасов</th><th>DKIM</th><th></th></tr></thead>
        <tbody>
          {#each domains as d, i}
            <tr class="rise" style="--i:{i}">
              <td data-label="Домен" class="font-mono font-medium">{d.name}{#if d.lenient}<span class="ml-2 text-[10px] px-1.5 py-0.5 rounded border border-warn/40 text-warn font-sans">приёмник</span>{/if}</td>
              <td data-label="Владелец">{d.login}</td>
              <td data-label="Ящиков" class="tabular-nums">{d.mailboxes}</td>
              <td data-label="Алиасов" class="tabular-nums">{d.aliases}</td>
              <td data-label="DKIM" class="font-mono text-xs text-muted">{d.dkim_selector || '—'}</td>
              <td data-label=""><div class="row-actions">
                <button class="btn btn-sm" onclick={() => showDNS(d.name)}><Icon name="globe" size={13} /> DNS</button>
                <button class="btn btn-sm" onclick={() => rotateDKIM(d.name)} title="выпустить новый ключ DKIM"><Icon name="key" size={13} /></button>
                <button class="btn btn-sm" onclick={() => toggleLenient(d)} title={d.lenient ? 'вернуть строгие проверки отправителя' : 'домен-приёмник: принимать письма и от криво настроенных отправителей'}><Icon name="shield" size={13} /></button>
                <button class="btn btn-danger btn-sm" onclick={() => (del = { kind: 'domain', name: d.name, note: 'Домен, его ящики и все письма будут удалены.' })}><Icon name="trash" size={13} /></button>
              </div></td>
            </tr>
          {/each}
          {#if !domains.length}<Empty text="Доменов пока нет." cols={6} />{/if}
        </tbody></table>
    </div>
  {:else if tab === 'boxes'}
    <form class="card grid md:grid-cols-5 gap-3 items-end mb-3 rise" onsubmit={addBox}>
      <div class="md:col-span-2"><label class="label" for="ba">Адрес</label><input id="ba" class="input font-mono" bind:value={boxForm.address} placeholder="user@example.com" required /></div>
      <div><label class="label" for="bn">Имя</label><input id="bn" class="input" bind:value={boxForm.name} placeholder="Иван Петров" /></div>
      <div><label class="label" for="bq">Квота, МБ</label><input id="bq" class="input" type="number" min="0" bind:value={boxForm.quota_mb} /></div>
      <button class="btn btn-primary">Создать ящик</button>
    </form>
    <div class="card overflow-x-auto p-0 rise">
      <table class="tbl"><thead><tr><th>Адрес</th><th>Имя</th><th>Квота</th><th>Состояние</th><th></th></tr></thead>
        <tbody>
          {#each boxes as b, i}
            <tr class="rise" style="--i:{i}">
              <td data-label="Адрес" class="font-mono font-medium">{b.address}</td>
              <td data-label="Имя">{b.name || '—'}</td>
              <td data-label="Квота" class="tabular-nums">{b.quota_mb ? b.quota_mb + ' МБ' : 'без лимита'}</td>
              <td data-label="Состояние"><span class={b.active ? 'text-ok' : 'text-muted'}>{b.active ? 'активен' : 'выключен'}</span></td>
              <td data-label=""><div class="row-actions">
                <button class="btn btn-sm" onclick={() => newPassword(b.address)}><Icon name="key" size={13} /> пароль</button>
                <button class="btn btn-sm" onclick={() => toggleBox(b)}>{b.active ? 'выключить' : 'включить'}</button>
                <button class="btn btn-danger btn-sm" onclick={() => (del = { kind: 'box', name: b.address, note: 'Ящик перестанет принимать почту.' })}><Icon name="trash" size={13} /></button>
              </div></td>
            </tr>
          {/each}
          {#if !boxes.length}<Empty text="Ящиков пока нет." cols={5} />{/if}
        </tbody></table>
    </div>
  {:else}
    <form class="card grid md:grid-cols-4 gap-3 items-end mb-3 rise" onsubmit={addAlias}>
      <div><label class="label" for="aa">Адрес</label><input id="aa" class="input font-mono" bind:value={aliasForm.address} placeholder="info@example.com" required /></div>
      <div class="md:col-span-2"><label class="label" for="ad">Куда пересылать</label><input id="ad" class="input font-mono" bind:value={aliasForm.destinations} placeholder="user@example.com, second@example.com" required /></div>
      <button class="btn btn-primary">Создать алиас</button>
    </form>
    <p class="text-xs text-muted mb-3">Адрес вида <span class="font-mono">@example.com</span> — catch-all: заберёт письма на все несуществующие ящики домена.</p>
    <div class="card overflow-x-auto p-0 rise">
      <table class="tbl"><thead><tr><th>Адрес</th><th>Куда</th><th></th></tr></thead>
        <tbody>
          {#each aliases as a, i}
            <tr class="rise" style="--i:{i}">
              <td data-label="Адрес" class="font-mono font-medium">{a.address}</td>
              <td data-label="Куда" class="font-mono text-xs">{a.destination}</td>
              <td data-label=""><div class="row-actions"><button class="btn btn-danger btn-sm" onclick={() => (del = { kind: 'alias', name: a.address, note: 'Пересылка перестанет работать.' })}><Icon name="trash" size={13} /></button></div></td>
            </tr>
          {/each}
          {#if !aliases.length}<Empty text="Алиасов пока нет." cols={3} />{/if}
        </tbody></table>
    </div>
  {/if}
{/if}

<Modal open={!!dns} title="DNS для {dns?.domain}">
  {#if dnsLoading}
    <p class="text-muted">проверяю записи…</p>
  {:else if dns}
    <div class="space-y-3 max-h-[60vh] overflow-y-auto -mx-1 px-1">
      {#each dns.records as r}
        <div class="border border-line rounded-md p-2.5">
          <div class="flex justify-between gap-2 text-xs mb-1">
            <span class="font-mono text-muted">{r.type} · {r.name}</span>
            <span class={mark(r.status)}>{markText(r.status)}</span>
          </div>
          <div class="font-mono text-xs break-all select-all">{r.value}</div>
          {#if r.found && r.status !== 'ok'}<div class="text-xs text-muted mt-1 break-all">сейчас: {r.found}</div>{/if}
          {#if r.note}<div class="text-xs text-muted mt-1">{r.note}</div>{/if}
        </div>
      {/each}
    </div>
  {/if}
  {#snippet footer()}<button class="btn" onclick={() => (dns = null)}>Закрыть</button>{/snippet}
</Modal>

<Modal open={!!del} title="Удалить {del?.name}?">
  <p>{del?.note}</p>
  {#if del?.kind === 'box'}<label class="flex items-center gap-2"><input type="checkbox" bind:checked={purge} /> удалить и письма с диска</label>{/if}
  {#snippet footer()}<button class="btn" onclick={() => { del = null; purge = false; }}>Отмена</button><button class="btn btn-danger" onclick={remove}>Удалить</button>{/snippet}
</Modal>
