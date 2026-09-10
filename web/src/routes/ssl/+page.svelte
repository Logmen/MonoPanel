<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError, daysLeft } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Icon from '$lib/components/Icon.svelte';

  // Three things live here and follow different rules: the panel's own
  // certificate (one name, the hostname), the certificates of sites (issued
  // from the site, wildcards included) and imported ones nobody uses yet.
  let panel = $state<any>(null);
  let certs = $state<any[]>([]);
  let sites = $state<any[]>([]);
  let providers = $state<any[]>([]);
  let job = $state<number | null>(null);
  let error = $state('');
  let mode = $state<'' | 'panel-issue' | 'panel-import' | 'provider'>('');
  let form = $state({ email: '', staging: false, dns: '' });
  let imp = $state({ certificate: '', private_key: '' });
  let pform = $state({ name: '', type: 'cloudflare', creds: '' });
  let siteIssue = $state<any>(null); // the site a certificate is being ordered for
  let sform = $state({ staging: false, dns: '' });
  let del = $state<any>(null);
  let ask = $state<Ask | null>(null);
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function load() {
    try { const [p, c, s, d] = await Promise.all([api('/ssl/panel'), api('/certificates'), api('/sites'), api('/dns-providers')]); panel = p; certs = c as any[]; sites = s as any[]; providers = d as any[]; } catch (e: any) { error = e.text || String(e); }
  }
  onMount(load);
  const days = (s: string) => Math.round((new Date(s).getTime() - Date.now()) / 86400000);
  const certById = (id: number | null) => certs.find((c) => c.id === id) ?? null;
  // A site's certificate: the referenced one, or the one named after it.
  const certOfSite = (s: any) => certById(s.certificate_id) ?? certs.find((c) => c.name === s.domain) ?? null;
  const spare = $derived(certs.filter((c) => !c.used_by_panel && !c.used_by_sites?.length));
  const panelIsIP = $derived(!!panel && /^\d+\.\d+\.\d+\.\d+$/.test(panel.hostname || ''));

  async function issuePanel(e: Event) { e.preventDefault(); error = ''; try { const r: any = await api('/ssl/panel/issue', { method: 'POST', json: { email: form.email || undefined, staging: form.staging, dns: form.dns || undefined } }); job = r.job_id; mode = ''; } catch (e) { fail(e); } }
  async function importPanel(e: Event) { e.preventDefault(); error = ''; try { await api('/ssl/panel/import', { method: 'POST', json: imp }); notify('Сертификат панели установлен'); imp = { certificate: '', private_key: '' }; mode = ''; await load(); } catch (e) { fail(e); } }
  const askSelfSigned = (): Ask => ({
    title: 'Вернуть панели самоподписанный сертификат?',
    note: 'Выпущенный сертификат панели будет удалён вместе с файлами, и браузеры снова начнут предупреждать о недоверенном сертификате на порту панели. Сайты это не затронет: если сайт использует тот же сертификат, панель откажется его удалять.',
    action: 'Вернуть самоподписанный', danger: true,
    run: async () => { await api('/ssl/panel', { method: 'DELETE' }); await load(); }
  });
  const askRenew = (c: any): Ask => ({
    title: `Продлить сертификат ${c.name}?`,
    note: 'Панель закажет новый сертификат прямо сейчас, не дожидаясь автопродления за 30 дней до конца. У Let\'s Encrypt есть лимиты — пять одинаковых сертификатов в неделю и пять неудачных проверок в час, — и ручные продления их расходуют. Действующий сертификат работает, пока новый не выпустится.',
    action: 'Продлить',
    run: async () => { const r: any = await api(`/certificates/${c.id}/renew`, { method: 'POST' }); job = r.job_id; }
  });
  async function issueSite(e: Event) { e.preventDefault(); if (!siteIssue) return; error = ''; try { const r: any = await api(`/sites/${encodeURIComponent(siteIssue.domain)}/tls/issue`, { method: 'POST', json: { staging: sform.staging, dns: sform.dns || undefined } }); job = r.job_id; siteIssue = null; } catch (e) { fail(e); } }
  async function remove() { if (!del) return; try { await api(`/certificates/${del.id}`, { method: 'DELETE' }); del = null; await load(); } catch (e) { fail(e); } }
  async function addProvider(e: Event) { e.preventDefault(); error = ''; try { const credentials: Record<string, string> = {}; for (const l of pform.creds.split('\n')) { const [k, ...v] = l.split('='); if (k.trim()) credentials[k.trim()] = v.join('=').trim(); } await api('/dns-providers', { method: 'POST', json: { name: pform.name, type: pform.type, credentials } }); pform = { name: '', type: 'cloudflare', creds: '' }; mode = ''; await load(); } catch (e) { fail(e); } }
  const expiryTag = (s: string | null) => { if (!s) return 'tag-muted'; const d = days(s); return d < 14 ? 'tag-err' : d < 30 ? 'tag-warn' : 'tag-muted'; };
  const statusTag = (st: string) => (st === 'valid' ? 'tag-ok' : st === 'error' ? 'tag-err' : 'tag-warn');
</script>

<PageHead title="SSL" sub="сертификат панели, сертификаты сайтов и DNS-провайдеры для DNS-01">
  <button class="btn {mode === 'provider' ? 'btn-primary' : ''}" onclick={() => (mode = mode === 'provider' ? '' : 'provider')}><Icon name="key" size={15} /> DNS-провайдер</button>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}

<!-- The panel's own certificate: one name, the hostname; no free-form input. -->
<div class="card mb-4 rise">
  <div class="flex flex-wrap items-start justify-between gap-3">
    <div>
      <div class="font-medium">Панель · <span class="font-mono">{panel?.hostname || '—'}</span></div>
      {#if panel}
        <div class="text-sm text-muted mt-1">
          {panel.source === 'acme' ? "Let's Encrypt / ACME" : 'самоподписанный'} · {panel.certificate?.issuer || '—'} · до {daysLeft(panel.certificate?.not_after)}
        </div>
        {#if panel.record}
          <div class="text-xs mt-1 flex flex-wrap items-center gap-2">
            <span class="tag {statusTag(panel.record.status)}">{panel.record.status}</span>
            <span class="text-muted">запись #{panel.record.id}, автопродление: {panel.record.auto_renew ? 'да' : 'нет'}{panel.record.dns_provider ? `, DNS-01 через ${panel.record.dns_provider}` : ''}</span>
            {#if panel.record.used_by_sites?.length}<span class="text-muted">· тот же сертификат у сайта {panel.record.used_by_sites.join(', ')}</span>{/if}
          </div>
          {#if panel.record.last_error}<div class="text-xs text-danger mt-1">{panel.record.last_error}</div>{/if}
        {/if}
        {#if panelIsIP}<div class="text-xs text-warn mt-1">Панель названа адресом: сертификат выпустить нельзя. Задайте имя: <code>mp config set web.hostname panel.example.com --restart</code>.</div>{/if}
      {/if}
    </div>
    <div class="flex flex-wrap gap-2">
      <button class="btn {mode === 'panel-issue' ? 'btn-primary' : ''}" disabled={panelIsIP} onclick={() => (mode = mode === 'panel-issue' ? '' : 'panel-issue')}><Icon name="plus" size={15} /> Выпустить</button>
      <button class="btn {mode === 'panel-import' ? 'btn-primary' : ''}" disabled={panelIsIP} onclick={() => (mode = mode === 'panel-import' ? '' : 'panel-import')}><Icon name="file" size={15} /> Импорт</button>
      {#if panel?.record?.kind === 'acme'}<button class="btn" onclick={() => (ask = askRenew(panel.record))}><Icon name="refresh" size={15} /> Продлить</button>{/if}
      {#if panel?.record}<button class="btn btn-danger" onclick={() => (ask = askSelfSigned())}>Самоподписанный</button>{/if}
    </div>
  </div>
  {#if mode === 'panel-issue'}
    <form class="grid md:grid-cols-4 gap-3 items-end mt-4 rise" onsubmit={issuePanel}>
      <div><label class="label" for="pe">E-mail аккаунта ACME</label><input id="pe" class="input" bind:value={form.email} placeholder="необязательно" /></div>
      <div><label class="label" for="pd">Проверка</label><select id="pd" class="input" bind:value={form.dns}><option value="">HTTP-01 (порт 80 снаружи)</option>{#each providers as p}<option value={p.name}>DNS-01: {p.name} ({p.type})</option>{/each}</select></div>
      <label class="text-sm flex items-center gap-1"><input type="checkbox" bind:checked={form.staging} /> staging (тестовый, недоверенный)</label>
      <button class="btn btn-primary">Выпустить для {panel?.hostname}</button>
    </form>
  {:else if mode === 'panel-import'}
    <form class="grid md:grid-cols-2 gap-3 mt-4 rise" onsubmit={importPanel}>
      <div><label class="label" for="ic">Сертификат + цепочка (PEM)</label><textarea id="ic" class="input font-mono text-xs h-40" bind:value={imp.certificate} required placeholder="-----BEGIN CERTIFICATE-----"></textarea></div>
      <div><label class="label" for="ik">Закрытый ключ (PEM)</label><textarea id="ik" class="input font-mono text-xs h-40" bind:value={imp.private_key} required placeholder="-----BEGIN PRIVATE KEY-----"></textarea></div>
      <div class="md:col-span-2 flex items-center gap-3 text-xs text-muted"><button class="btn btn-primary">Установить для {panel?.hostname}</button><span>Сертификат должен покрывать имя панели; сертификаты Let's Encrypt дальше продлеваются панелью через ACME.</span></div>
    </form>
  {/if}
</div>

{#if mode === 'provider'}
  <form class="card grid md:grid-cols-4 gap-3 items-end mb-4 rise" onsubmit={addProvider}>
    <div><label class="label" for="pn">Имя</label><input id="pn" class="input" bind:value={pform.name} required /></div>
    <div><label class="label" for="pt">Тип</label><select id="pt" class="input" bind:value={pform.type}>{#each ['cloudflare', 'hetzner', 'digitalocean', 'gandiv5', 'desec', 'namecheap', 'rfc2136'] as t}<option value={t}>{t}</option>{/each}</select></div>
    <div><label class="label" for="pc">Учётные данные (KEY=VALUE)</label><textarea id="pc" class="input font-mono h-16" bind:value={pform.creds} placeholder="CLOUDFLARE_DNS_API_TOKEN=…"></textarea></div>
    <button class="btn btn-primary">Добавить</button>
  </form>
{/if}

<!-- Sites: their certificate follows the site; wildcards are ordered here too. -->
<div class="card overflow-x-auto p-0 mb-4 rise">
  <div class="px-4 pt-3 font-medium">Сайты</div>
  <table class="tbl"><thead><tr><th>Сайт</th><th>SSL</th><th>Сертификат</th><th>Издатель</th><th>Истекает</th><th></th></tr></thead>
    <tbody>
      {#each sites as s, i}
        {@const c = certOfSite(s)}
        <tr class="rise" style="--i:{i}">
          <td data-label="Сайт" class="font-mono font-medium"><a href="/sites/{s.domain}">{s.domain}</a>{#if s.aliases?.length}<div class="text-xs text-muted">{s.aliases.join(', ')}</div>{/if}</td>
          <td data-label="SSL"><span class="tag {s.ssl === 'auto' ? (c?.status === 'valid' ? 'tag-ok' : 'tag-warn') : 'tag-muted'}">{s.ssl === 'auto' ? (c?.status === 'valid' ? 'https' : 'ожидает сертификат') : 'без HTTPS'}</span></td>
          <td data-label="Сертификат">{#if c}<span class="font-mono text-xs">{c.name}</span> <span class="tag {statusTag(c.status)}">{c.status}</span>{#if c.last_error}<div class="text-xs text-danger max-w-xs truncate" title={c.last_error}>{c.last_error}</div>{/if}{:else}<span class="text-muted">—</span>{/if}</td>
          <td data-label="Издатель" class="text-muted">{c?.issuer || '—'}</td>
          <td data-label="Истекает" class="text-xs">{#if c?.not_after}<span class="tag {expiryTag(c.not_after)}">{daysLeft(c.not_after)}</span>{:else}—{/if}</td>
          <td data-label=""><div class="row-actions">
            <button class="btn btn-sm" onclick={() => { siteIssue = s; sform = { staging: false, dns: '' }; }}><Icon name="plus" size={13} /> выпустить</button>
            {#if c?.kind === 'acme'}<button class="btn btn-sm" onclick={() => (ask = askRenew(c))}><Icon name="refresh" size={13} /> продлить</button>{/if}
          </div></td>
        </tr>
      {/each}
      {#if !sites.length}<Empty text="Сайтов нет." cols={6} />{/if}
    </tbody></table>
</div>

<!-- Certificates nobody serves: imported ahead of time, left after a site was removed. -->
{#if spare.length}
<div class="card overflow-x-auto p-0 mb-4 rise">
  <div class="px-4 pt-3 font-medium">Не используются</div>
  <table class="tbl"><thead><tr><th>Имя</th><th>SAN</th><th>Статус</th><th>Издатель</th><th>Истекает</th><th></th></tr></thead>
    <tbody>
      {#each spare as c, i}
        <tr class="rise" style="--i:{i}"><td data-label="Имя" class="font-mono font-medium">{c.name}</td><td data-label="SAN" class="text-xs text-muted max-w-xs truncate" title={c.names.join(', ')}>{c.names.join(', ')}</td><td data-label="Статус"><span class="tag {statusTag(c.status)}">{c.status}</span></td><td data-label="Издатель" class="text-muted">{c.issuer || '—'}</td><td data-label="Истекает" class="text-xs"><span class="tag {expiryTag(c.not_after)}">{daysLeft(c.not_after)}</span></td><td data-label=""><div class="row-actions">{#if c.kind === 'acme'}<button class="btn btn-sm" onclick={() => (ask = askRenew(c))}><Icon name="refresh" size={13} /> продлить</button>{/if}<button class="btn btn-danger btn-sm" onclick={() => (del = c)}><Icon name="trash" size={13} /></button></div></td></tr>
      {/each}
    </tbody></table>
</div>
{/if}

{#if providers.length}<div class="card rise text-sm"><div class="font-medium mb-1">DNS-провайдеры (DNS-01, wildcard)</div><ul class="font-mono text-xs text-muted">{#each providers as p}<li>{p.name} · {p.type}</li>{/each}</ul></div>{/if}
<Confirm bind:ask />

<Modal open={!!siteIssue} title="Сертификат для {siteIssue?.domain}" onclose={() => (siteIssue = null)}>
  <form id="site-issue" class="grid gap-3" onsubmit={issueSite}>
    <p class="text-sm text-muted">Будут покрыты {[siteIssue?.domain, ...(siteIssue?.aliases ?? [])].join(', ')}. Сайт переключится на HTTPS, как только сертификат выпустится.</p>
    <div><label class="label" for="sd">Проверка</label><select id="sd" class="input" bind:value={sform.dns}><option value="">HTTP-01: домен должен вести на этот сервер, порт 80 открыт</option>{#each providers as p}<option value={p.name}>DNS-01: {p.name} ({p.type}) — работает и до переключения DNS</option>{/each}</select></div>
    <label class="text-sm flex items-center gap-1"><input type="checkbox" bind:checked={sform.staging} /> staging (тестовый, недоверенный — для проверки настройки)</label>
  </form>
  {#snippet footer()}<button class="btn" onclick={() => (siteIssue = null)}>Отмена</button><button class="btn btn-primary" form="site-issue">Выпустить</button>{/snippet}
</Modal>

<Modal open={!!del} title="Удалить сертификат {del?.name}?" onclose={() => (del = null)}>
  <p>Файлы сертификата будут удалены. Его никто не использует, поэтому ни панель, ни сайты не пострадают.</p>
  {#snippet footer()}<button class="btn" onclick={() => (del = null)}>Отмена</button><button class="btn btn-danger" onclick={remove}>Удалить</button>{/snippet}
</Modal>
