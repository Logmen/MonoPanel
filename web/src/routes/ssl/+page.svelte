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
  import { t } from '$lib/i18n/index.svelte';

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
  async function importPanel(e: Event) { e.preventDefault(); error = ''; try { await api('/ssl/panel/import', { method: 'POST', json: imp }); notify(t('ssl.panelImported')); imp = { certificate: '', private_key: '' }; mode = ''; await load(); } catch (e) { fail(e); } }
  const askSelfSigned = (): Ask => ({
    title: t('ssl.askSelfSignedTitle'),
    note: t('ssl.askSelfSignedNote'),
    action: t('ssl.askSelfSignedAction'), danger: true,
    run: async () => { await api('/ssl/panel', { method: 'DELETE' }); await load(); }
  });
  const askRenew = (c: any): Ask => ({
    title: t('ssl.askRenewTitle', { name: c.name }),
    note: t('ssl.askRenewNote'),
    action: t('ssl.renew'),
    run: async () => { const r: any = await api(`/certificates/${c.id}/renew`, { method: 'POST' }); job = r.job_id; }
  });
  async function issueSite(e: Event) { e.preventDefault(); if (!siteIssue) return; error = ''; try { const r: any = await api(`/sites/${encodeURIComponent(siteIssue.domain)}/tls/issue`, { method: 'POST', json: { staging: sform.staging, dns: sform.dns || undefined } }); job = r.job_id; siteIssue = null; } catch (e) { fail(e); } }
  async function remove() { if (!del) return; try { await api(`/certificates/${del.id}`, { method: 'DELETE' }); del = null; await load(); } catch (e) { fail(e); } }
  async function addProvider(e: Event) { e.preventDefault(); error = ''; try { const credentials: Record<string, string> = {}; for (const l of pform.creds.split('\n')) { const [k, ...v] = l.split('='); if (k.trim()) credentials[k.trim()] = v.join('=').trim(); } await api('/dns-providers', { method: 'POST', json: { name: pform.name, type: pform.type, credentials } }); pform = { name: '', type: 'cloudflare', creds: '' }; mode = ''; await load(); } catch (e) { fail(e); } }
  const expiryTag = (s: string | null) => { if (!s) return 'tag-muted'; const d = days(s); return d < 14 ? 'tag-err' : d < 30 ? 'tag-warn' : 'tag-muted'; };
  const statusTag = (st: string) => (st === 'valid' ? 'tag-ok' : st === 'error' ? 'tag-err' : 'tag-warn');
</script>

<PageHead title="SSL" sub={t('ssl.sub')}>
  <button class="btn {mode === 'provider' ? 'btn-primary' : ''}" onclick={() => (mode = mode === 'provider' ? '' : 'provider')}><Icon name="key" size={15} /> {t('ssl.dnsProvider')}</button>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if job}<div class="mb-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}

<!-- The panel's own certificate: one name, the hostname; no free-form input. -->
<div class="card mb-4 rise">
  <div class="flex flex-wrap items-start justify-between gap-3">
    <div>
      <div class="font-medium">{t('ssl.panel')} · <span class="font-mono">{panel?.hostname || '—'}</span></div>
      {#if panel}
        <div class="text-sm text-muted mt-1">
          {panel.source === 'acme' ? "Let's Encrypt / ACME" : t('ssl.selfSigned')} · {panel.certificate?.issuer || '—'} · {t('ssl.validUntil', { date: daysLeft(panel.certificate?.not_after) })}
        </div>
        {#if panel.record}
          <div class="text-xs mt-1 flex flex-wrap items-center gap-2">
            <span class="tag {statusTag(panel.record.status)}">{panel.record.status}</span>
            <span class="text-muted">{t('ssl.record', { id: panel.record.id, auto: panel.record.auto_renew ? t('ssl.yes') : t('ssl.no') })}{panel.record.dns_provider ? `, ${t('ssl.viaDns', { provider: panel.record.dns_provider })}` : ''}</span>
            {#if panel.record.used_by_sites?.length}<span class="text-muted">· {t('ssl.sameAsSite', { sites: panel.record.used_by_sites.join(', ') })}</span>{/if}
          </div>
          {#if panel.record.last_error}<div class="text-xs text-danger mt-1">{panel.record.last_error}</div>{/if}
        {/if}
        {#if panelIsIP}<div class="text-xs text-warn mt-1">{t('ssl.panelIsIp')} <code>mp config set web.hostname panel.example.com --restart</code>.</div>{/if}
      {/if}
    </div>
    <div class="flex flex-wrap gap-2">
      <button class="btn {mode === 'panel-issue' ? 'btn-primary' : ''}" disabled={panelIsIP} onclick={() => (mode = mode === 'panel-issue' ? '' : 'panel-issue')}><Icon name="plus" size={15} /> {t('ssl.issue')}</button>
      <button class="btn {mode === 'panel-import' ? 'btn-primary' : ''}" disabled={panelIsIP} onclick={() => (mode = mode === 'panel-import' ? '' : 'panel-import')}><Icon name="file" size={15} /> {t('ssl.import')}</button>
      {#if panel?.record?.kind === 'acme'}<button class="btn" onclick={() => (ask = askRenew(panel.record))}><Icon name="refresh" size={15} /> {t('ssl.renew')}</button>{/if}
      {#if panel?.record}<button class="btn btn-danger" onclick={() => (ask = askSelfSigned())}>{t('ssl.selfSignedBtn')}</button>{/if}
    </div>
  </div>
  {#if mode === 'panel-issue'}
    <form class="grid md:grid-cols-4 gap-3 items-end mt-4 rise" onsubmit={issuePanel}>
      <div><label class="label" for="pe">{t('ssl.acmeEmail')}</label><input id="pe" class="input" bind:value={form.email} placeholder={t('ssl.optional')} /></div>
      <div><label class="label" for="pd">{t('ssl.challenge')}</label><select id="pd" class="input" bind:value={form.dns}><option value="">{t('ssl.http01Panel')}</option>{#each providers as p}<option value={p.name}>DNS-01: {p.name} ({p.type})</option>{/each}</select></div>
      <label class="text-sm flex items-center gap-1"><input type="checkbox" bind:checked={form.staging} /> {t('ssl.stagingPanel')}</label>
      <button class="btn btn-primary">{t('ssl.issueFor', { host: panel?.hostname ?? '' })}</button>
    </form>
  {:else if mode === 'panel-import'}
    <form class="grid md:grid-cols-2 gap-3 mt-4 rise" onsubmit={importPanel}>
      <div><label class="label" for="ic">{t('ssl.certChainPem')}</label><textarea id="ic" class="input font-mono text-xs h-40" bind:value={imp.certificate} required placeholder="-----BEGIN CERTIFICATE-----"></textarea></div>
      <div><label class="label" for="ik">{t('ssl.privateKeyPem')}</label><textarea id="ik" class="input font-mono text-xs h-40" bind:value={imp.private_key} required placeholder="-----BEGIN PRIVATE KEY-----"></textarea></div>
      <div class="md:col-span-2 flex items-center gap-3 text-xs text-muted"><button class="btn btn-primary">{t('ssl.installFor', { host: panel?.hostname ?? '' })}</button><span>{t('ssl.importHint')}</span></div>
    </form>
  {/if}
</div>

{#if mode === 'provider'}
  <form class="card grid md:grid-cols-4 gap-3 items-end mb-4 rise" onsubmit={addProvider}>
    <div><label class="label" for="pn">{t('common.name')}</label><input id="pn" class="input" bind:value={pform.name} required /></div>
    <div><label class="label" for="pt">{t('ssl.type')}</label><select id="pt" class="input" bind:value={pform.type}>{#each ['cloudflare', 'hetzner', 'digitalocean', 'gandiv5', 'desec', 'namecheap', 'rfc2136'] as prov}<option value={prov}>{prov}</option>{/each}</select></div>
    <div><label class="label" for="pc">{t('ssl.credentials')}</label><textarea id="pc" class="input font-mono h-16" bind:value={pform.creds} placeholder="CLOUDFLARE_DNS_API_TOKEN=…"></textarea></div>
    <button class="btn btn-primary">{t('common.add')}</button>
  </form>
{/if}

<!-- Sites: their certificate follows the site; wildcards are ordered here too. -->
<div class="card overflow-x-auto p-0 mb-4 rise">
  <div class="px-4 pt-3 font-medium">{t('ssl.sites')}</div>
  <table class="tbl"><thead><tr><th>{t('ssl.colSite')}</th><th>SSL</th><th>{t('ssl.colCert')}</th><th>{t('ssl.colIssuer')}</th><th>{t('ssl.colExpires')}</th><th></th></tr></thead>
    <tbody>
      {#each sites as s, i}
        {@const c = certOfSite(s)}
        <tr class="rise" style="--i:{i}">
          <td data-label={t('ssl.colSite')} class="font-mono font-medium"><a href="/sites/{s.domain}">{s.domain}</a>{#if s.aliases?.length}<div class="text-xs text-muted">{s.aliases.join(', ')}</div>{/if}</td>
          <td data-label="SSL"><span class="tag {s.ssl === 'auto' ? (c?.status === 'valid' ? 'tag-ok' : 'tag-warn') : 'tag-muted'}">{s.ssl === 'auto' ? (c?.status === 'valid' ? 'https' : t('ssl.awaitingCert')) : t('ssl.noHttps')}</span></td>
          <td data-label={t('ssl.colCert')}>{#if c}<span class="font-mono text-xs">{c.name}</span> <span class="tag {statusTag(c.status)}">{c.status}</span>{#if c.last_error}<div class="text-xs text-danger max-w-xs truncate" title={c.last_error}>{c.last_error}</div>{/if}{:else}<span class="text-muted">—</span>{/if}</td>
          <td data-label={t('ssl.colIssuer')} class="text-muted">{c?.issuer || '—'}</td>
          <td data-label={t('ssl.colExpires')} class="text-xs">{#if c?.not_after}<span class="tag {expiryTag(c.not_after)}">{daysLeft(c.not_after)}</span>{:else}—{/if}</td>
          <td data-label=""><div class="row-actions">
            <button class="btn btn-sm" onclick={() => { siteIssue = s; sform = { staging: false, dns: '' }; }}><Icon name="plus" size={13} /> {t('ssl.issueRow')}</button>
            {#if c?.kind === 'acme'}<button class="btn btn-sm" onclick={() => (ask = askRenew(c))}><Icon name="refresh" size={13} /> {t('ssl.renewRow')}</button>{/if}
          </div></td>
        </tr>
      {/each}
      {#if !sites.length}<Empty text={t('ssl.noSites')} cols={6} />{/if}
    </tbody></table>
</div>

<!-- Certificates nobody serves: imported ahead of time, left after a site was removed. -->
{#if spare.length}
<div class="card overflow-x-auto p-0 mb-4 rise">
  <div class="px-4 pt-3 font-medium">{t('ssl.unused')}</div>
  <table class="tbl"><thead><tr><th>{t('common.name')}</th><th>SAN</th><th>{t('common.status')}</th><th>{t('ssl.colIssuer')}</th><th>{t('ssl.colExpires')}</th><th></th></tr></thead>
    <tbody>
      {#each spare as c, i}
        <tr class="rise" style="--i:{i}"><td data-label={t('common.name')} class="font-mono font-medium">{c.name}</td><td data-label="SAN" class="text-xs text-muted max-w-xs truncate" title={c.names.join(', ')}>{c.names.join(', ')}</td><td data-label={t('common.status')}><span class="tag {statusTag(c.status)}">{c.status}</span></td><td data-label={t('ssl.colIssuer')} class="text-muted">{c.issuer || '—'}</td><td data-label={t('ssl.colExpires')} class="text-xs"><span class="tag {expiryTag(c.not_after)}">{daysLeft(c.not_after)}</span></td><td data-label=""><div class="row-actions">{#if c.kind === 'acme'}<button class="btn btn-sm" onclick={() => (ask = askRenew(c))}><Icon name="refresh" size={13} /> {t('ssl.renewRow')}</button>{/if}<button class="btn btn-danger btn-sm" onclick={() => (del = c)}><Icon name="trash" size={13} /></button></div></td></tr>
      {/each}
    </tbody></table>
</div>
{/if}

{#if providers.length}<div class="card rise text-sm"><div class="font-medium mb-1">{t('ssl.dnsProviders')}</div><ul class="font-mono text-xs text-muted">{#each providers as p}<li>{p.name} · {p.type}</li>{/each}</ul></div>{/if}
<Confirm bind:ask />

<Modal open={!!siteIssue} title={t('ssl.certFor', { domain: siteIssue?.domain ?? '' })} onclose={() => (siteIssue = null)}>
  <form id="site-issue" class="grid gap-3" onsubmit={issueSite}>
    <p class="text-sm text-muted">{t('ssl.siteIssueNote', { names: [siteIssue?.domain, ...(siteIssue?.aliases ?? [])].join(', ') })}</p>
    <div><label class="label" for="sd">{t('ssl.challenge')}</label><select id="sd" class="input" bind:value={sform.dns}><option value="">{t('ssl.http01Site')}</option>{#each providers as p}<option value={p.name}>{t('ssl.dns01Site', { name: p.name, type: p.type })}</option>{/each}</select></div>
    <label class="text-sm flex items-center gap-1"><input type="checkbox" bind:checked={sform.staging} /> {t('ssl.stagingSite')}</label>
  </form>
  {#snippet footer()}<button class="btn" onclick={() => (siteIssue = null)}>{t('common.cancel')}</button><button class="btn btn-primary" form="site-issue">{t('ssl.issue')}</button>{/snippet}
</Modal>

<Modal open={!!del} title={t('ssl.deleteTitle', { name: del?.name ?? '' })} onclose={() => (del = null)}>
  <p>{t('ssl.deleteNote')}</p>
  {#snippet footer()}<button class="btn" onclick={() => (del = null)}>{t('common.cancel')}</button><button class="btn btn-danger" onclick={remove}>{t('common.delete')}</button>{/snippet}
</Modal>
