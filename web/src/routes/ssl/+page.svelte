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
  let certs = $state<any[]>([]);
  let providers = $state<any[]>([]);
  let tls = $state<any>(null);
  let job = $state<number | null>(null);
  let error = $state('');
  let mode = $state<'' | 'issue' | 'import' | 'provider'>('');
  let form = $state({ names: '', email: '', staging: false, dns: '' });
  let imp = $state({ name: '', certificate: '', private_key: '' });
  let pform = $state({ name: '', type: 'cloudflare', creds: '' });
  let del = $state<any>(null);
  let ask = $state<Ask | null>(null);
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function load() { try { certs = await api('/certificates'); providers = await api('/dns-providers'); tls = await api('/web/tls'); } catch (e: any) { error = e.text || String(e); } }
  onMount(load);
  async function issue(e: Event) { e.preventDefault(); error = ''; try { const r: any = await api('/certificates', { method: 'POST', json: { names: form.names.split(/[\s,]+/).filter(Boolean), email: form.email || undefined, staging: form.staging, dns: form.dns || undefined } }); job = r.job_id; mode = ''; } catch (e) { fail(e); } }
  async function doImport(e: Event) { e.preventDefault(); error = ''; try { const c: any = await api('/certificates/import', { method: 'POST', json: { name: imp.name || undefined, certificate: imp.certificate, private_key: imp.private_key } }); notify(`Сертификат ${c.name} импортирован`); imp = { name: '', certificate: '', private_key: '' }; mode = ''; await load(); } catch (e) { fail(e); } }
  const askRenew = (c: any): Ask => ({
    title: `Продлить сертификат ${c.name}?`,
    note: 'Панель закажет новый сертификат прямо сейчас, не дожидаясь автопродления за 30 дней до конца. У Let\'s Encrypt есть лимиты — пять одинаковых сертификатов в неделю и пять неудачных проверок в час, — и ручные продления их расходуют. Действующий сертификат работает, пока новый не выпустится.',
    action: 'Продлить',
    run: () => renew(c.id)
  });
  async function renew(id: number) { try { const r: any = await api(`/certificates/${id}/renew`, { method: 'POST' }); job = r.job_id; } catch (e) { fail(e); } }
  async function remove() { if (!del) return; try { await api(`/certificates/${del.id}`, { method: 'DELETE' }); del = null; await load(); } catch (e) { fail(e); } }
  async function addProvider(e: Event) { e.preventDefault(); error = ''; try { const credentials: Record<string, string> = {}; for (const l of pform.creds.split('\n')) { const [k, ...v] = l.split('='); if (k.trim()) credentials[k.trim()] = v.join('=').trim(); } await api('/dns-providers', { method: 'POST', json: { name: pform.name, type: pform.type, credentials } }); pform = { name: '', type: 'cloudflare', creds: '' }; mode = ''; await load(); } catch (e) { fail(e); } }
  const days = (s: string) => Math.round((new Date(s).getTime() - Date.now()) / 86400000);
</script>

<PageHead title="SSL" sub={tls ? `панель: ${tls.source === 'acme' ? "Let's Encrypt" : 'самоподписанный'} · ${tls.certificate?.subject} · до ${daysLeft(tls.certificate?.not_after)}` : ''}>
  <button class="btn {mode === 'issue' ? 'btn-primary' : ''}" onclick={() => (mode = mode === 'issue' ? '' : 'issue')}><Icon name="plus" size={15} /> Выпустить</button>
  <button class="btn {mode === 'import' ? 'btn-primary' : ''}" onclick={() => (mode = mode === 'import' ? '' : 'import')}><Icon name="file" size={15} /> Импорт</button>
  <button class="btn {mode === 'provider' ? 'btn-primary' : ''}" onclick={() => (mode = mode === 'provider' ? '' : 'provider')}><Icon name="key" size={15} /> DNS-провайдер</button>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if mode === 'issue'}
  <form class="card grid md:grid-cols-5 gap-3 items-end mb-4 rise" onsubmit={issue}>
    <div class="md:col-span-2"><label class="label" for="n">Имена</label><input id="n" class="input" bind:value={form.names} placeholder="example.com www.example.com" required /></div>
    <div><label class="label" for="e">E-mail аккаунта</label><input id="e" class="input" bind:value={form.email} /></div>
    <div><label class="label" for="d">Проверка</label><select id="d" class="input" bind:value={form.dns}><option value="">HTTP-01 (webroot)</option>{#each providers as p}<option value={p.name}>DNS-01: {p.name} ({p.type})</option>{/each}</select></div>
    <div class="flex items-center gap-3"><label class="text-sm flex items-center gap-1"><input type="checkbox" bind:checked={form.staging} /> staging</label><button class="btn btn-primary">Выпустить</button></div>
  </form>
{:else if mode === 'import'}
  <form class="card grid md:grid-cols-2 gap-3 mb-4 rise" onsubmit={doImport}>
    <div class="md:col-span-2"><label class="label" for="in">Имя (по умолчанию из сертификата)</label><input id="in" class="input font-mono" bind:value={imp.name} placeholder="example.com" /></div>
    <div><label class="label" for="ic">Сертификат + цепочка (PEM)</label><textarea id="ic" class="input font-mono text-xs h-40" bind:value={imp.certificate} required placeholder="-----BEGIN CERTIFICATE-----"></textarea></div>
    <div><label class="label" for="ik">Закрытый ключ (PEM)</label><textarea id="ik" class="input font-mono text-xs h-40" bind:value={imp.private_key} required placeholder="-----BEGIN PRIVATE KEY-----"></textarea></div>
    <div class="md:col-span-2 flex items-center gap-3 text-xs text-muted"><button class="btn btn-primary">Импортировать</button><span>Сертификаты Let's Encrypt дальше продлеваются панелью через ACME.</span></div>
  </form>
{:else if mode === 'provider'}
  <form class="card grid md:grid-cols-4 gap-3 items-end mb-4 rise" onsubmit={addProvider}>
    <div><label class="label" for="pn">Имя</label><input id="pn" class="input" bind:value={pform.name} required /></div>
    <div><label class="label" for="pt">Тип</label><select id="pt" class="input" bind:value={pform.type}>{#each ['cloudflare', 'hetzner', 'digitalocean', 'gandiv5', 'desec', 'namecheap', 'rfc2136'] as t}<option value={t}>{t}</option>{/each}</select></div>
    <div><label class="label" for="pc">Учётные данные (KEY=VALUE)</label><textarea id="pc" class="input font-mono h-16" bind:value={pform.creds} placeholder="CLOUDFLARE_DNS_API_TOKEN=…"></textarea></div>
    <button class="btn btn-primary">Добавить</button>
  </form>
{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
<div class="card overflow-x-auto p-0 mb-4 rise">
  <table class="tbl"><thead><tr><th>Имя</th><th>SAN</th><th>Статус</th><th>Издатель</th><th>Истекает</th><th>Авто</th><th></th></tr></thead>
    <tbody>
      {#each certs as c, i}
        {@const d = c.not_after ? days(c.not_after) : 0}
        <tr class="rise" style="--i:{i}"><td data-label="Имя" class="font-mono font-medium">{c.name}</td><td data-label="SAN" class="text-xs text-muted max-w-xs truncate" title={c.names.join(', ')}>{c.names.join(', ')}</td><td data-label="Статус"><span class="tag {c.status === 'valid' ? 'tag-ok' : c.status === 'error' ? 'tag-err' : 'tag-warn'}">{c.status}</span>{#if c.last_error}<div class="text-xs text-danger max-w-xs truncate" title={c.last_error}>{c.last_error}</div>{/if}</td><td data-label="Издатель" class="text-muted">{c.issuer || '—'}</td><td data-label="Истекает" class="text-xs"><span class="tag {d < 14 ? 'tag-err' : d < 30 ? 'tag-warn' : 'tag-muted'}">{daysLeft(c.not_after)}</span></td><td data-label="Авто">{c.auto_renew ? 'да' : 'нет'}</td><td data-label=""><div class="row-actions"><button class="btn btn-sm" onclick={() => (ask = askRenew(c))}><Icon name="refresh" size={13} /> продлить</button><button class="btn btn-danger btn-sm" onclick={() => (del = c)}><Icon name="trash" size={13} /></button></div></td></tr>
      {/each}
      {#if !certs.length}<Empty text="Сертификатов нет." cols={7} />{/if}
    </tbody></table>
</div>
{#if providers.length}<div class="card rise text-sm"><div class="font-medium mb-1">DNS-провайдеры (DNS-01, wildcard)</div><ul class="font-mono text-xs text-muted">{#each providers as p}<li>{p.name} · {p.type}</li>{/each}</ul></div>{/if}
<Confirm bind:ask />

<Modal open={!!del} title="Удалить сертификат {del?.name}?" onclose={() => (del = null)}>
  <p>Файлы сертификата будут удалены; сайты, которые его используют, останутся без HTTPS до нового выпуска.</p>
  {#snippet footer()}<button class="btn" onclick={() => (del = null)}>Отмена</button><button class="btn btn-danger" onclick={remove}>Удалить</button>{/snippet}
</Modal>
