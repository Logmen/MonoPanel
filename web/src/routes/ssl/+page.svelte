<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError, daysLeft, when } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import { t, tn } from '$lib/i18n/index.svelte';

  // Three things live here and follow different rules: the panel's own
  // certificate (one name, the hostname), the certificates of sites (ordered
  // from the site, with its aliases) and the others — any names ordered or
  // imported in their own block (the mail hostname, a domain with its
  // wildcard), or left after a site was removed. Each block orders its kind only.
  let panel = $state<any>(null);
  let certs = $state<any[]>([]);
  let sites = $state<any[]>([]);
  let providers = $state<any[]>([]);
  let loaded = $state(false);
  let jobs = $state<number[]>([]); // orders run as jobs; «renew all» starts several at once
  let error = $state('');
  let mode = $state<'' | 'panel-issue' | 'panel-import' | 'provider' | 'issue' | 'import'>('');
  let form = $state({ email: '', staging: false, dns: '', key_type: 'ec256' });
  let imp = $state({ certificate: '', private_key: '' });
  // The provider form takes its fields from GET /dns-providers/types: one input
  // per credential key of the chosen type, plus free lines for lego's optional settings.
  let types = $state<Record<string, string[]> | null>(null);
  const blankProvider = () => ({ name: '', type: 'cloudflare', creds: {} as Record<string, string>, extra: '' });
  let pform = $state(blankProvider());
  let showCreds = $state(false);
  // A certificate for any names (POST /certificates) and an import under any name.
  const blankIssue = () => ({ names: '', dns: '', ca: 'le', directory: '', email: '', key_type: 'ec256', auto_renew: true });
  let iform = $state(blankIssue());
  const blankImport = () => ({ name: '', certificate: '', private_key: '', no_renew: false });
  let nimp = $state(blankImport());
  let siteIssue = $state<any>(null); // the site a certificate is being ordered for
  let sform = $state({ staging: false, dns: '', email: '' });
  let siteImport = $state<any>(null); // the site an existing certificate is being installed for
  let simp = $state({ certificate: '', private_key: '', no_renew: false });
  let del = $state<any>(null);
  let ask = $state<Ask | null>(null);
  let detail = $state<{ cert: any; error?: string } | null>(null); // a certificate record, every field
  let served = $state<{ info?: any; error?: string } | null>(null); // what the panel's HTTPS port serves now
  let users = $state<any[] | null>(null); // logins for the owner field, fetched with the first details
  const errText = (e: unknown) => (e instanceof ApiError ? e.text : String(e));
  const fail = (e: unknown) => { error = errText(e); notify(error, 'err'); };
  async function load() {
    try { const [p, c, s, d] = await Promise.all([api('/ssl/panel'), api('/certificates'), api('/sites'), api('/dns-providers')]); panel = p; certs = c as any[]; sites = s as any[]; providers = d as any[]; loaded = true; } catch (e) { error = errText(e); }
  }
  onMount(load);
  // A job can report its end more than once (a stream that reconnects replays
  // the final snapshot, every few seconds while lib/api.ts keeps it open):
  // reload once per job.
  const finished = new Set<number>();
  const jobFinished = (id: number) => { if (!finished.has(id)) { finished.add(id); load(); } };
  const days = (s: string) => Math.round((new Date(s).getTime() - Date.now()) / 86400000);
  const certById = (id: number | null) => certs.find((c) => c.id === id) ?? null;
  // A site's certificate: the referenced one, or the one named after it.
  const certOfSite = (s: any) => certById(s.certificate_id) ?? certs.find((c) => c.name === s.domain) ?? null;
  const spare = $derived(certs.filter((c) => !c.used_by_panel && !c.used_by_sites?.length));
  const acmeCerts = $derived(certs.filter((c) => c.kind === 'acme'));
  // No name to certify: an IPv4/IPv6 address or nothing at all (the API refuses both).
  const panelNoName = $derived(!!panel && (!panel.hostname || /^[\d.]+$/.test(panel.hostname) || panel.hostname.includes(':')));
  // Let's Encrypt's staging CA signs test certificates that browsers do not trust.
  const isStaging = (c: any) => !!c?.directory_url?.includes('staging');
  // A name is covered by itself or by a wildcard one label up.
  const covers = (names: string[], n: string) => names.includes(n) || names.includes('*.' + n.split('.').slice(1).join('.'));
  const uncovered = (s: any, c: any): string[] => (c?.names?.length ? [s.domain, ...(s.aliases ?? [])].filter((n: string) => !covers(c.names, n)) : []);
  const certsVia = (provider: string) => certs.filter((c) => c.dns_provider === provider).map((c) => c.name);

  async function issuePanel(e: Event) { e.preventDefault(); error = ''; try { const r: any = await api('/ssl/panel/issue', { method: 'POST', json: { email: form.email || undefined, staging: form.staging, dns: form.dns || undefined, key_type: form.key_type } }); jobs = [r.job_id]; mode = ''; await load(); } catch (e) { fail(e); } }
  async function importPanel(e: Event) { e.preventDefault(); error = ''; try { await api('/ssl/panel/import', { method: 'POST', json: imp }); notify(t('ssl.panelImported')); imp = { certificate: '', private_key: '' }; mode = ''; await load(); } catch (e) { fail(e); } }
  const askSelfSigned = (): Ask => ({
    title: t('ssl.askSelfSignedTitle'),
    note: t('ssl.askSelfSignedNote'),
    action: t('ssl.askSelfSignedAction'), danger: true,
    run: async () => { try { await api('/ssl/panel', { method: 'DELETE' }); notify(t('ssl.selfSignedDone')); await load(); } catch (e) { fail(e); } }
  });
  const askRenew = (c: any): Ask => ({
    title: t('ssl.askRenewTitle', { name: c.name }),
    note: t('ssl.askRenewNote'),
    action: t('ssl.renew'),
    run: async () => { try { const r: any = await api(`/certificates/${c.id}/renew`, { method: 'POST' }); jobs = [r.job_id]; } catch (e) { fail(e); } }
  });
  // mp ssl renew --all: every ACME certificate is ordered anew. Each order is a
  // job of its own with its own lock, so they run side by side.
  const askRenewAll = (): Ask => {
    const list = acmeCerts.slice();
    return {
      title: tn('ssl.askRenewAllTitle', list.length),
      note: t('ssl.askRenewAllNote', { names: list.map((c) => c.name).join(', ') }),
      action: t('ssl.renewAll'),
      run: async () => {
        const ids: number[] = [];
        try { for (const c of list) { const r: any = await api(`/certificates/${c.id}/renew`, { method: 'POST' }); ids.push(r.job_id); } } catch (e) { fail(e); }
        if (ids.length) jobs = [...new Set(ids)];
      }
    };
  };
  async function issueSite(e: Event) { e.preventDefault(); if (!siteIssue) return; error = ''; try { const r: any = await api(`/sites/${encodeURIComponent(siteIssue.domain)}/tls/issue`, { method: 'POST', json: { staging: sform.staging, dns: sform.dns || undefined, email: sform.email || undefined } }); jobs = [r.job_id]; siteIssue = null; await load(); } catch (e) { fail(e); } }
  // An existing certificate for a site: the panel checks it covers the domain,
  // and the sites it covers switch to it. A site without HTTPS gets it turned
  // on — that is what installing a certificate is for.
  async function importSite(e: Event) {
    e.preventDefault(); if (!siteImport) return; error = '';
    const site = siteImport;
    try {
      const c: any = await api('/certificates/import', { method: 'POST', json: { name: site.domain, certificate: simp.certificate, private_key: simp.private_key, auto_renew: simp.no_renew ? false : undefined } });
      if (site.ssl !== 'auto') { const r: any = await api(`/sites/${encodeURIComponent(site.domain)}`, { method: 'PATCH', json: { ssl: 'auto' } }); if (r?.job_id) jobs = [r.job_id]; }
      notify(t(c.auto_renew ? 'ssl.siteImportedRenew' : 'ssl.siteImported', { domain: site.domain }));
      siteImport = null; simp = { certificate: '', private_key: '', no_renew: false }; await load();
    } catch (e) { fail(e); }
  }
  async function remove() { if (!del) return; const c = del; try { await api(`/certificates/${c.id}`, { method: 'DELETE' }); notify(t('ssl.deleted', { name: c.name })); del = null; await load(); } catch (e) { fail(e); } }

  // Any names (mp ssl issue): the mail hostname, a domain with its wildcard. A
  // wildcard is validated through DNS only; the API refuses it too, but in CLI words.
  async function issueNames(e: Event) {
    e.preventDefault(); error = '';
    const names = iform.names.split(/[\s,;]+/).filter(Boolean);
    if (!names.length) return fail(t('ssl.namesRequired'));
    if (!iform.dns && names.some((n) => n.includes('*'))) return fail(t(providers.length ? 'ssl.wildcardNeedsDns' : 'ssl.wildcardNoProvider'));
    try {
      const r: any = await api('/certificates', { method: 'POST', json: { names, dns: iform.dns || undefined, email: iform.email || undefined, key_type: iform.key_type, auto_renew: iform.auto_renew, staging: iform.ca === 'staging', directory: iform.ca === 'custom' ? iform.directory.trim() : undefined } });
      notify(t('ssl.ordered', { name: r.certificate.name, names: r.certificate.names.join(', ') }));
      jobs = [r.job_id]; iform = blankIssue(); mode = ''; await load();
    } catch (e) { fail(e); }
  }
  // Import under any name (mp ssl import [hostname]); without one the API names
  // it after the certificate itself.
  async function importNamed(e: Event) {
    e.preventDefault(); error = '';
    try {
      const c: any = await api('/certificates/import', { method: 'POST', json: { name: nimp.name.trim() || undefined, certificate: nimp.certificate, private_key: nimp.private_key, auto_renew: nimp.no_renew ? false : undefined } });
      notify(t(c.auto_renew ? 'ssl.importedRenew' : 'ssl.imported', { name: c.name }));
      nimp = blankImport(); mode = ''; await load();
    } catch (e) { fail(e); }
  }
  // PEM from a file, as mp ssl import --cert/--chain/--key reads it: the
  // certificate field appends (the leaf first, then the chain file), the key
  // field replaces. No extension filter: CAs ship .crt, .cer, .cert, .chain or
  // none at all; the API checks the contents.
  function pickPEM(set: (text: string) => void) {
    const input = document.createElement('input');
    input.type = 'file';
    input.onchange = async () => {
      const f = input.files?.[0];
      if (!f) return;
      if (f.size > 1 << 20) return fail(t('ssl.fileTooBig'));
      try { set((await f.text()).trim()); } catch (e) { fail(e); }
    };
    input.click();
  }
  const appendPEM = (cur: string, add: string) => (cur.trim() ? cur.trimEnd() + '\n' + add : add);

  const typeNames = $derived(types ? Object.keys(types).sort() : []);
  const typeKeys = $derived(types?.[pform.type] ?? []);
  // lego reads its optional settings from <PREFIX>_…, e.g. CLOUDFLARE_PROPAGATION_TIMEOUT.
  const keyPrefix = $derived((typeKeys[0] ?? 'CLOUDFLARE_').split('_')[0]);
  // Tokens, secrets and API keys are typed blind; user names, servers and the
  // TSIG key name are not secrets. A blind field is marked new-password:
  // browsers ignore autocomplete=off there and would offer the panel's own
  // saved login, the field before it taken for the user name.
  const secretKey = (k: string) => /TOKEN|SECRET|PASSWORD|API_KEY/.test(k);
  async function toggleProvider() {
    mode = mode === 'provider' ? '' : 'provider';
    if (mode !== 'provider' || types) return;
    try { const tp = await api<Record<string, string[]>>('/dns-providers/types'); types = tp; if (!tp[pform.type]) pform.type = Object.keys(tp).sort()[0] ?? ''; } catch (e) { fail(e); }
  }
  async function addProvider(e: Event) {
    e.preventDefault(); error = '';
    // The pasted lines go first: a field filled in above wins over the same key below.
    const credentials: Record<string, string> = {};
    const lines = pform.extra.split('\n');
    for (let i = 0; i < lines.length; i++) {
      const l = lines[i].trim();
      if (!l || l.startsWith('#')) continue;
      const at = l.indexOf('=');
      if (at < 1) return fail(t('ssl.credLineBad', { n: i + 1 }));
      credentials[l.slice(0, at).trim()] = l.slice(at + 1).trim();
    }
    for (const k of typeKeys) { const v = (pform.creds[k] ?? '').trim(); if (v) credentials[k] = v; }
    try {
      const p: any = await api('/dns-providers', { method: 'POST', json: { name: pform.name, type: pform.type, credentials } });
      notify(t('ssl.providerAdded', { name: p.name }));
      pform = blankProvider(); showCreds = false; mode = ''; await load();
    } catch (e) { fail(e); }
  }
  // Certificates validated through a provider stop renewing without it: say which.
  const askDelProvider = (p: any): Ask => {
    const used = certsVia(p.name);
    return {
      title: t('ssl.askDelProviderTitle', { name: p.name }),
      note: used.length ? t('ssl.askDelProviderUsed', { certs: used.join(', ') }) : t('ssl.askDelProviderNote'),
      action: t('common.delete'), danger: true,
      run: async () => { try { await api(`/dns-providers/${encodeURIComponent(p.name)}`, { method: 'DELETE' }); notify(t('ssl.providerDeleted', { name: p.name })); await load(); } catch (e) { fail(e); } }
    };
  };

  // Details open at once from the listing and refresh from GET /certificates/{id}.
  async function openCert(c: any) {
    detail = { cert: c };
    try {
      const fresh: any = await api(`/certificates/${c.id}`);
      // The record by id carries no usage: the API computes it for listings only.
      if (detail?.cert.id === c.id) detail = { cert: { ...fresh, used_by_panel: c.used_by_panel, used_by_sites: c.used_by_sites } };
    } catch (e) { if (detail?.cert.id === c.id) detail = { cert: c, error: errText(e) }; }
    if (c.user_id && !users) users = await api<any[]>('/users').catch(() => []);
  }
  async function openServed() {
    served = {};
    try { served = { info: await api('/web/tls') }; } catch (e) { served = { error: errText(e) }; }
  }
  const kindText = (k: string) => (k === 'acme' ? t('ssl.kindAcme') : k === 'custom' ? t('ssl.kindCustom') : k === 'selfsigned' ? t('ssl.selfSigned') : k);
  const caText = (u: string) => (!u ? '—' : u.includes('acme-staging') ? t('ssl.caStaging') : u.includes('letsencrypt.org') ? "Let's Encrypt" : t('ssl.caOther'));
  const keyText = (k: string) => (k === 'ec256' ? 'ECDSA P-256' : k === 'rsa2048' ? 'RSA-2048' : k || '—');
  const usedText = (c: any) => [c.used_by_panel ? t('ssl.usedPanel') : '', c.used_by_sites?.length ? t('ssl.usedSites', { sites: c.used_by_sites.join(', ') }) : ''].filter(Boolean).join('; ') || t('ssl.usedNone');
  const ownerOf = (id: number | null | undefined) => (!id ? '—' : (users?.find((u) => u.id === id)?.login ?? sites.find((s) => s.user_id === id)?.login ?? '#' + id));
  const expiryTag = (s: string | null) => { if (!s) return 'tag-muted'; const d = days(s); return d < 14 ? 'tag-err' : d < 30 ? 'tag-warn' : 'tag-muted'; };
  const statusTag = (st: string) => (st === 'valid' ? 'tag-ok' : st === 'error' ? 'tag-err' : 'tag-warn');
</script>

{#snippet certStatus(c: any, full: boolean)}
  <span class="tag {statusTag(c.status)}">{c.status}</span>{#if isStaging(c)} <span class="tag tag-warn">staging</span>{/if}
  {#if c.last_error}<div class="text-xs text-danger {full ? 'break-words' : 'max-w-xs truncate'}" title={c.last_error}>{c.last_error}</div>{/if}
{/snippet}
{#snippet pemLabel(id: string, label: string, add: boolean, set: (text: string) => void)}
  <div class="flex items-end justify-between gap-2"><label class="label" for={id}>{label}</label><button type="button" class="btn btn-ghost btn-sm mb-1" onclick={() => pickPEM(set)}><Icon name="file" size={12} /> {add ? t('ssl.addFromFile') : t('ssl.fromFile')}</button></div>
{/snippet}
{#snippet kv(label: string, value: string, mono = false)}
  <dt class="text-muted">{label}</dt><dd class="min-w-0 break-words {mono ? 'font-mono text-xs' : ''}">{value}</dd>
{/snippet}

<PageHead title="SSL" sub={t('ssl.sub')}>
  {#if acmeCerts.length > 1}<button class="btn" onclick={() => (ask = askRenewAll())}><Icon name="refresh" size={15} /> {t('ssl.renewAll')}</button>{/if}
  <button class="btn {mode === 'provider' ? 'btn-primary' : ''}" onclick={toggleProvider}><Icon name="key" size={15} /> {t('ssl.dnsProvider')}</button>
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if jobs.length}<div class="space-y-2 mb-4">{#each jobs as id (id)}<JobLog jobId={id} onfinish={() => jobFinished(id)} />{/each}</div>{/if}

<!-- The panel's own certificate: one name, the hostname; no free-form input. -->
<div class="card mb-4 rise">
  <div class="flex flex-wrap items-start justify-between gap-3">
    <div>
      <div class="font-medium">{t('ssl.panel')} · <span class="font-mono">{panel?.hostname || '—'}</span></div>
      {#if panel}
        <div class="text-sm text-muted mt-1">
          {panel.source !== 'acme' ? t('ssl.selfSigned') : panel.record?.kind === 'custom' ? t('ssl.importedShort') : "Let's Encrypt / ACME"} · {panel.certificate?.issuer || '—'} · {t('ssl.validUntil', { date: daysLeft(panel.certificate?.not_after) })}
        </div>
        {#if panel.record}
          <div class="text-xs mt-1 flex flex-wrap items-center gap-2">
            <span class="tag {statusTag(panel.record.status)}">{panel.record.status}</span>{#if isStaging(panel.record)}<span class="tag tag-warn">staging</span>{/if}
            <span class="text-muted">{t('ssl.record', { id: panel.record.id, auto: panel.record.auto_renew ? t('ssl.yes') : t('ssl.no') })}{panel.record.dns_provider ? `, ${t('ssl.viaDns', { provider: panel.record.dns_provider })}` : ''}</span>
            {#if panel.record.used_by_sites?.length}<span class="text-muted">· {t('ssl.sameAsSite', { sites: panel.record.used_by_sites.join(', ') })}</span>{/if}
          </div>
          {#if panel.record.last_error}<div class="text-xs text-danger mt-1">{panel.record.last_error}</div>{/if}
        {/if}
        {#if panelNoName}<div class="text-xs text-warn mt-1">{t('ssl.panelIsIp')} <a class="link" href="/settings">{t('ssl.panelIsIpSettings')}</a> {t('ssl.panelIsIpShell')} <code>mp config set web.hostname panel.example.com --restart</code>.</div>{/if}
      {/if}
    </div>
    <div class="flex flex-wrap gap-2">
      <button class="btn {mode === 'panel-issue' ? 'btn-primary' : ''}" disabled={panelNoName} onclick={() => (mode = mode === 'panel-issue' ? '' : 'panel-issue')}><Icon name="plus" size={15} /> {t('ssl.issue')}</button>
      <button class="btn {mode === 'panel-import' ? 'btn-primary' : ''}" disabled={panelNoName} onclick={() => (mode = mode === 'panel-import' ? '' : 'panel-import')}><Icon name="file" size={15} /> {t('ssl.import')}</button>
      {#if panel?.record?.kind === 'acme'}<button class="btn" onclick={() => (ask = askRenew(panel.record))}><Icon name="refresh" size={15} /> {t('ssl.renew')}</button>{/if}
      {#if panel}<button class="btn" onclick={openServed}><Icon name="eye" size={15} /> {t('ssl.details')}</button>{/if}
      {#if panel?.record}<button class="btn btn-danger" onclick={() => (ask = askSelfSigned())}>{t('ssl.selfSignedBtn')}</button>{/if}
    </div>
  </div>
  {#if mode === 'panel-issue'}
    <form class="grid sm:grid-cols-2 lg:grid-cols-3 gap-3 items-end mt-4 rise" onsubmit={issuePanel}>
      <div><label class="label" for="pe">{t('ssl.acmeEmail')}</label><input id="pe" type="email" class="input" bind:value={form.email} placeholder={t('ssl.optional')} /></div>
      <div><label class="label" for="pd">{t('ssl.challenge')}</label><select id="pd" class="input" bind:value={form.dns}><option value="">{t('ssl.http01Panel')}</option>{#each providers as p}<option value={p.name}>DNS-01: {p.name} ({p.type})</option>{/each}</select></div>
      <div><label class="label" for="pky">{t('ssl.keyType')}</label><select id="pky" class="input" bind:value={form.key_type}><option value="ec256">{t('ssl.keyEc')}</option><option value="rsa2048">{t('ssl.keyRsa')}</option></select></div>
      <label class="text-sm flex items-center gap-1"><input type="checkbox" bind:checked={form.staging} /> {t('ssl.stagingPanel')}</label>
      <button class="btn btn-primary">{t('ssl.issueFor', { host: panel?.hostname ?? '' })}</button>
    </form>
  {:else if mode === 'panel-import'}
    <form class="grid md:grid-cols-2 gap-3 mt-4 rise" onsubmit={importPanel}>
      <div>{@render pemLabel('ic', t('ssl.certChainPem'), true, (v: string) => (imp.certificate = appendPEM(imp.certificate, v)))}<textarea id="ic" class="input font-mono text-xs h-40" bind:value={imp.certificate} required placeholder="-----BEGIN CERTIFICATE-----"></textarea></div>
      <div>{@render pemLabel('ik', t('ssl.privateKeyPem'), false, (v: string) => (imp.private_key = v))}<textarea id="ik" class="input font-mono text-xs h-40" bind:value={imp.private_key} required placeholder="-----BEGIN PRIVATE KEY-----"></textarea></div>
      <div class="md:col-span-2 flex items-center gap-3 text-xs text-muted"><button class="btn btn-primary">{t('ssl.installFor', { host: panel?.hostname ?? '' })}</button><span>{t('ssl.importHint')}</span></div>
    </form>
  {/if}
</div>

{#if mode === 'provider'}
  <form class="card grid sm:grid-cols-2 lg:grid-cols-4 gap-3 items-end mb-4 rise" onsubmit={addProvider} autocomplete="off">
    <div><label class="label" for="pn">{t('common.name')}</label><input id="pn" class="input" bind:value={pform.name} required pattern="[a-z0-9][a-z0-9_\-]{'{0,31}'}" title={t('ssl.providerNameHint')} placeholder={pform.type} spellcheck="false" /></div>
    <div><label class="label" for="pt">{t('ssl.type')}</label><select id="pt" class="input" bind:value={pform.type}>{#each typeNames as tp (tp)}<option value={tp}>{tp}</option>{/each}</select></div>
    {#each typeKeys as k (k)}<div><label class="label" for="pk-{k}">{k}</label><input id="pk-{k}" class="input" type={showCreds || !secretKey(k) ? 'text' : 'password'} bind:value={pform.creds[k]} autocomplete={secretKey(k) ? 'new-password' : 'off'} spellcheck="false" /></div>{/each}
    <div class="sm:col-span-2 lg:col-span-4"><label class="label" for="px">{t('ssl.extraVars')}</label><textarea id="px" class="input font-mono text-xs h-16" bind:value={pform.extra} placeholder="{keyPrefix}_PROPAGATION_TIMEOUT=300" spellcheck="false"></textarea></div>
    <div class="sm:col-span-2 lg:col-span-4 flex flex-wrap items-center gap-3 text-xs text-muted">
      <button class="btn btn-primary" disabled={!types}>{t('common.add')}</button>
      <button type="button" class="btn btn-ghost btn-sm" aria-pressed={showCreds} onclick={() => (showCreds = !showCreds)}><Icon name="eye" size={13} /> {showCreds ? t('ssl.hideValues') : t('ssl.showValues')}</button>
      <span>{t('ssl.providerHint')}</span>
    </div>
  </form>
{/if}

<!-- Sites: their certificate follows the site and is ordered from its row. -->
<div class="card overflow-x-auto p-0 mb-4 rise">
  <div class="px-4 pt-3 font-medium">{t('ssl.sites')}</div>
  {#if !loaded}<Skeleton rows={4} />{:else}
  <table class="tbl"><thead><tr><th>{t('ssl.colSite')}</th><th>SSL</th><th>{t('ssl.colCert')}</th><th>{t('ssl.colIssuer')}</th><th>{t('ssl.colExpires')}</th><th></th></tr></thead>
    <tbody>
      {#each sites as s, i (s.id)}
        {@const c = certOfSite(s)}
        {@const miss = uncovered(s, c)}
        <tr class="rise" style="--i:{i}">
          <td data-label={t('ssl.colSite')} class="font-mono font-medium"><a href="/sites/{s.domain}">{s.domain}</a>{#if s.aliases?.length}<div class="text-xs text-muted">{s.aliases.join(', ')}</div>{/if}</td>
          <td data-label="SSL"><span class="tag {s.ssl === 'auto' ? (c?.status === 'valid' ? 'tag-ok' : 'tag-warn') : 'tag-muted'}">{s.ssl === 'auto' ? (c?.status === 'valid' ? 'https' : t('ssl.awaitingCert')) : t('ssl.noHttps')}</span></td>
          <!-- One block: on a phone the cell is a flex row, and loose parts would squeeze the name into a column. -->
          <td data-label={t('ssl.colCert')}>{#if c}<div class="min-w-0"><span class="font-mono text-xs">{c.name}</span> {@render certStatus(c, false)}{#if miss.length}<div class="text-xs text-warn">{t('ssl.notCovered', { names: miss.join(', ') })}</div>{/if}</div>{:else}<span class="text-muted">—</span>{/if}</td>
          <td data-label={t('ssl.colIssuer')} class="text-muted">{c?.issuer || '—'}</td>
          <td data-label={t('ssl.colExpires')} class="text-xs">{#if c?.not_after}<span class="tag {expiryTag(c.not_after)}">{daysLeft(c.not_after)}</span>{:else}—{/if}</td>
          <td data-label=""><div class="row-actions">
            <button class="btn btn-sm" onclick={() => { siteIssue = s; sform = { staging: false, dns: '', email: '' }; }}><Icon name="plus" size={13} /> {t('ssl.issueRow')}</button>
            <button class="btn btn-sm" onclick={() => { siteImport = s; simp = { certificate: '', private_key: '', no_renew: false }; }}><Icon name="file" size={13} /> {t('ssl.importRow')}</button>
            {#if c?.kind === 'acme'}<button class="btn btn-sm" onclick={() => (ask = askRenew(c))}><Icon name="refresh" size={13} /> {t('ssl.renewRow')}</button>{/if}
            {#if c}<button class="btn btn-sm" title={t('ssl.details')} aria-label={t('ssl.details')} onclick={() => openCert(c)}><Icon name="eye" size={13} /></button>{/if}
          </div></td>
        </tr>
      {/each}
      {#if !sites.length}<Empty text={t('ssl.noSites')} cols={6} />{/if}
    </tbody></table>
  {/if}
</div>

<!-- Certificates neither the panel nor a site serves. Orders for any names and
     imports under any name start here, apart from the panel's and the sites' own. -->
<div class="card overflow-x-auto p-0 mb-4 rise">
  <!-- On a phone the card gives up its frame to the rows: the buttons need their own gap above the first one. -->
  <div class="px-4 pt-3 pb-3 sm:pb-0 flex flex-wrap items-start justify-between gap-3">
    <div class="min-w-0"><div class="font-medium">{t('ssl.others')}</div><div class="text-xs text-muted mt-0.5">{t('ssl.othersNote')}</div></div>
    <div class="flex flex-wrap gap-2">
      <button class="btn btn-sm {mode === 'issue' ? 'btn-primary' : ''}" onclick={() => (mode = mode === 'issue' ? '' : 'issue')}><Icon name="plus" size={13} /> {t('ssl.issue')}</button>
      <button class="btn btn-sm {mode === 'import' ? 'btn-primary' : ''}" onclick={() => (mode = mode === 'import' ? '' : 'import')}><Icon name="file" size={13} /> {t('ssl.import')}</button>
    </div>
  </div>
  {#if mode === 'issue'}
    <form class="grid sm:grid-cols-2 gap-3 items-end px-4 pt-3 pb-1 rise" onsubmit={issueNames}>
      <div class="sm:col-span-2"><label class="label" for="nn">{t('ssl.names')}</label><input id="nn" class="input" bind:value={iform.names} required placeholder={t('ssl.namesPlaceholder')} spellcheck="false" autocomplete="off" /></div>
      <div><label class="label" for="nd">{t('ssl.challenge')}</label><select id="nd" class="input" bind:value={iform.dns}><option value="">{t('ssl.http01Names')}</option>{#each providers as p}<option value={p.name}>{t('ssl.dns01Names', { name: p.name, type: p.type })}</option>{/each}</select></div>
      <div><label class="label" for="nc">{t('ssl.ca')}</label><select id="nc" class="input" bind:value={iform.ca}><option value="le">Let's Encrypt</option><option value="staging">{t('ssl.caStaging')}</option><option value="custom">{t('ssl.caCustom')}</option></select></div>
      <div><label class="label" for="ne">{t('ssl.acmeEmail')}</label><input id="ne" type="email" class="input" bind:value={iform.email} placeholder={t('ssl.optional')} /></div>
      <div><label class="label" for="nk">{t('ssl.keyType')}</label><select id="nk" class="input" bind:value={iform.key_type}><option value="ec256">{t('ssl.keyEc')}</option><option value="rsa2048">{t('ssl.keyRsa')}</option></select></div>
      {#if iform.ca === 'custom'}<div class="sm:col-span-2"><label class="label" for="nu">{t('ssl.directory')}</label><input id="nu" type="url" class="input" bind:value={iform.directory} required placeholder="https://acme.example.com/directory" spellcheck="false" /></div>{/if}
      <div class="sm:col-span-2 flex flex-wrap items-center gap-3 text-xs text-muted">
        <label class="text-sm text-ink flex items-center gap-1"><input type="checkbox" bind:checked={iform.auto_renew} /> {t('ssl.autoRenew')}</label>
        <button class="btn btn-primary">{t('ssl.issue')}</button>
        <span>{t('ssl.issueNamesHint')}</span>
      </div>
    </form>
  {:else if mode === 'import'}
    <form class="grid md:grid-cols-2 gap-3 px-4 pt-3 pb-1 rise" onsubmit={importNamed}>
      <div class="md:col-span-2"><label class="label" for="nin">{t('ssl.importName')}</label><input id="nin" class="input" bind:value={nimp.name} placeholder={t('ssl.importNamePlaceholder')} spellcheck="false" autocomplete="off" /></div>
      <div>{@render pemLabel('nic', t('ssl.certChainPem'), true, (v: string) => (nimp.certificate = appendPEM(nimp.certificate, v)))}<textarea id="nic" class="input font-mono text-xs h-36" bind:value={nimp.certificate} required placeholder="-----BEGIN CERTIFICATE-----"></textarea></div>
      <div>{@render pemLabel('nik', t('ssl.privateKeyPem'), false, (v: string) => (nimp.private_key = v))}<textarea id="nik" class="input font-mono text-xs h-36" bind:value={nimp.private_key} required placeholder="-----BEGIN PRIVATE KEY-----"></textarea></div>
      <div class="md:col-span-2 flex flex-wrap items-center gap-3 text-xs text-muted">
        <label class="text-sm text-ink flex items-center gap-1"><input type="checkbox" bind:checked={nimp.no_renew} /> {t('ssl.noAutoRenew')}</label>
        <button class="btn btn-primary">{t('ssl.install')}</button>
        <span>{t('ssl.importNamedHint')}</span>
      </div>
    </form>
  {/if}
  {#if !loaded}<Skeleton rows={2} />{:else}
  <table class="tbl"><thead><tr><th>{t('common.name')}</th><th>SAN</th><th>{t('common.status')}</th><th>{t('ssl.colIssuer')}</th><th>{t('ssl.colExpires')}</th><th>{t('ssl.colAuto')}</th><th></th></tr></thead>
    <tbody>
      {#each spare as c, i (c.id)}
        <tr class="rise" style="--i:{i}">
          <td data-label={t('common.name')} class="font-mono font-medium">{c.name}</td>
          <td data-label="SAN" class="text-xs text-muted max-w-xs truncate" title={c.names.join(', ')}>{c.names.join(', ')}</td>
          <td data-label={t('common.status')}><div class="min-w-0">{@render certStatus(c, false)}</div></td>
          <td data-label={t('ssl.colIssuer')} class="text-muted">{c.issuer || '—'}</td>
          <td data-label={t('ssl.colExpires')} class="text-xs">{#if c.not_after}<span class="tag {expiryTag(c.not_after)}">{daysLeft(c.not_after)}</span>{:else}—{/if}</td>
          <td data-label={t('ssl.colAuto')} class="text-xs">{c.auto_renew ? t('ssl.yes') : t('ssl.no')}</td>
          <td data-label=""><div class="row-actions">
            <button class="btn btn-sm" title={t('ssl.details')} aria-label={t('ssl.details')} onclick={() => openCert(c)}><Icon name="eye" size={13} /></button>
            {#if c.kind === 'acme'}<button class="btn btn-sm" onclick={() => (ask = askRenew(c))}><Icon name="refresh" size={13} /> {t('ssl.renewRow')}</button>{/if}
            <button class="btn btn-danger btn-sm" title={t('common.delete')} aria-label={t('common.delete')} onclick={() => (del = c)}><Icon name="trash" size={13} /></button>
          </div></td>
        </tr>
      {/each}
      {#if !spare.length}<Empty text={t('ssl.noOthers')} cols={7} />{/if}
    </tbody></table>
  {/if}
</div>

{#if providers.length}
<div class="card overflow-x-auto p-0 rise">
  <div class="px-4 pt-3 font-medium">{t('ssl.dnsProviders')}</div>
  <table class="tbl"><thead><tr><th>{t('common.name')}</th><th>{t('ssl.type')}</th><th>{t('ssl.colProviderCerts')}</th><th>{t('ssl.colAdded')}</th><th></th></tr></thead>
    <tbody>
      {#each providers as p, i (p.name)}
        {@const used = certsVia(p.name)}
        <tr class="rise" style="--i:{i}">
          <td data-label={t('common.name')} class="font-mono font-medium">{p.name}</td>
          <td data-label={t('ssl.type')} class="font-mono text-xs">{p.type}</td>
          <td data-label={t('ssl.colProviderCerts')} class="font-mono text-xs text-muted max-w-xs truncate" title={used.join(', ')}>{used.join(', ') || '—'}</td>
          <td data-label={t('ssl.colAdded')} class="text-xs text-muted">{when(p.created_at)}</td>
          <td data-label=""><div class="row-actions"><button class="btn btn-danger btn-sm" title={t('common.delete')} aria-label={t('common.delete')} onclick={() => (ask = askDelProvider(p))}><Icon name="trash" size={13} /></button></div></td>
        </tr>
      {/each}
    </tbody></table>
</div>
{/if}
<Confirm bind:ask />

<Modal open={!!siteIssue} title={t('ssl.certFor', { domain: siteIssue?.domain ?? '' })} onclose={() => (siteIssue = null)}>
  <form id="site-issue" class="grid gap-3" onsubmit={issueSite}>
    <p class="text-sm text-muted">{t('ssl.siteIssueNote', { names: [siteIssue?.domain, ...(siteIssue?.aliases ?? [])].join(', ') })}</p>
    <div><label class="label" for="sd">{t('ssl.challenge')}</label><select id="sd" class="input" bind:value={sform.dns}><option value="">{t('ssl.http01Site')}</option>{#each providers as p}<option value={p.name}>{t('ssl.dns01Site', { name: p.name, type: p.type })}</option>{/each}</select></div>
    <div><label class="label" for="se">{t('ssl.acmeEmail')}</label><input id="se" type="email" class="input" bind:value={sform.email} placeholder={t('ssl.optional')} /></div>
    <label class="text-sm flex items-center gap-1"><input type="checkbox" bind:checked={sform.staging} /> {t('ssl.stagingSite')}</label>
  </form>
  {#snippet footer()}<button class="btn" onclick={() => (siteIssue = null)}>{t('common.cancel')}</button><button class="btn btn-primary" form="site-issue">{t('ssl.issue')}</button>{/snippet}
</Modal>

<Modal open={!!siteImport} title={t('ssl.certFor', { domain: siteImport?.domain ?? '' })} onclose={() => (siteImport = null)}>
  <form id="site-import" class="grid gap-3" onsubmit={importSite}>
    <p class="text-sm text-muted">{t('ssl.siteImportNote', { domain: siteImport?.domain ?? '' })}</p>
    {#if certOfSite(siteImport ?? {})}<p class="text-sm text-warn">{t('ssl.siteImportReplaces', { name: certOfSite(siteImport).name, issuer: certOfSite(siteImport).issuer || '—' })}</p>{/if}
    <div>{@render pemLabel('sic', t('ssl.certChainPem'), true, (v: string) => (simp.certificate = appendPEM(simp.certificate, v)))}<textarea id="sic" class="input font-mono text-xs h-36" bind:value={simp.certificate} required placeholder="-----BEGIN CERTIFICATE-----"></textarea></div>
    <div>{@render pemLabel('sik', t('ssl.privateKeyPem'), false, (v: string) => (simp.private_key = v))}<textarea id="sik" class="input font-mono text-xs h-36" bind:value={simp.private_key} required placeholder="-----BEGIN PRIVATE KEY-----"></textarea></div>
    <label class="text-sm flex items-center gap-1"><input type="checkbox" bind:checked={simp.no_renew} /> {t('ssl.noAutoRenew')}</label>
  </form>
  {#snippet footer()}<button class="btn" onclick={() => (siteImport = null)}>{t('common.cancel')}</button><button class="btn btn-primary" form="site-import">{t('ssl.installFor', { host: siteImport?.domain ?? '' })}</button>{/snippet}
</Modal>

<Modal open={!!del} title={t('ssl.deleteTitle', { name: del?.name ?? '' })} onclose={() => (del = null)}>
  <p>{t('ssl.deleteNote')}</p>
  {#snippet footer()}<button class="btn" onclick={() => (del = null)}>{t('common.cancel')}</button><button class="btn btn-danger" onclick={remove}>{t('common.delete')}</button>{/snippet}
</Modal>

<!-- mp ssl show: every stored field of the record. -->
<Modal open={!!detail} title={t('ssl.detailTitle', { name: detail?.cert.name ?? '' })} onclose={() => (detail = null)}>
  {#if detail}
    {@const c = detail.cert}
    {#if detail.error}<p class="text-danger">{detail.error}</p>{/if}
    <div class="max-h-[65vh] overflow-y-auto">
      <dl class="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1.5">
        {@render kv('ID', '#' + c.id, true)}
        {@render kv(t('ssl.fNames'), c.names?.join(', ') || '—', true)}
        <dt class="text-muted">{t('common.status')}</dt><dd class="min-w-0">{@render certStatus(c, true)}</dd>
        {@render kv(t('ssl.fKind'), kindText(c.kind))}
        {#if c.kind === 'acme'}
          <dt class="text-muted">{t('ssl.ca')}</dt><dd class="min-w-0">{caText(c.directory_url)}{#if c.directory_url}<div class="font-mono text-xs text-muted break-words">{c.directory_url}</div>{/if}</dd>
          {@render kv(t('ssl.challenge'), c.dns_provider ? t('ssl.viaDns', { provider: c.dns_provider }) : t('ssl.http01'))}
          {@render kv(t('ssl.acmeEmail'), c.email || '—')}
        {/if}
        {@render kv(t('ssl.colAuto'), c.auto_renew ? t('ssl.autoRenewOn') : t('ssl.no'))}
        {@render kv(t('ssl.colIssuer'), c.issuer || '—')}
        {@render kv(t('ssl.fSerial'), c.serial || '—', true)}
        {@render kv(t('ssl.fValid'), c.not_after ? `${when(c.not_before)} — ${daysLeft(c.not_after)}` : '—')}
        {@render kv(t('ssl.keyType'), keyText(c.key_type))}
        {@render kv(t('ssl.fLastAttempt'), when(c.last_attempt))}
        {@render kv(t('ssl.fUsedBy'), usedText(c))}
        {@render kv(t('ssl.fOwner'), ownerOf(c.user_id))}
        <dt class="text-muted">{t('ssl.fFiles')}</dt><dd class="min-w-0 font-mono text-xs break-words">{#each [c.cert_path, c.key_path, c.chain_path].filter(Boolean) as f}<div>{f}</div>{:else}—{/each}</dd>
        {@render kv(t('ssl.fCreated'), when(c.created_at))}
        {@render kv(t('ssl.fUpdated'), when(c.updated_at))}
      </dl>
    </div>
  {/if}
  {#snippet footer()}<button class="btn" onclick={() => (detail = null)}>{t('common.close')}</button>{/snippet}
</Modal>

<!-- mp web tls: what the HTTPS port serves right now (it may still be the
     self-signed one while an order is pending). -->
<Modal open={!!served} title={t('ssl.servedTitle')} onclose={() => (served = null)}>
  {#if served?.error}<p class="text-danger">{served.error}</p>
  {:else if served?.info}
    {@const i = served.info}
    {@const x = i.certificate?.fingerprint_sha256 ? i.certificate : null}
    <dl class="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1.5">
      {@render kv(t('ssl.fHostname'), i.hostname || '—', true)}
      {@render kv(t('ssl.fSource'), i.source === 'acme' ? t('ssl.srcStored') : t('ssl.selfSigned'))}
      {@render kv(t('ssl.fSubject'), x?.subject || '—', true)}
      {@render kv(t('ssl.fNames'), x?.names?.join(', ') || '—', true)}
      {@render kv(t('ssl.colIssuer'), [x?.issuer, x?.issuer_org].filter(Boolean).join(' · ') || '—')}
      {@render kv(t('ssl.fSerial'), x?.serial || '—', true)}
      {@render kv(t('ssl.fValid'), x ? `${when(x.not_before)} — ${daysLeft(x.not_after)}` : '—')}
      {@render kv(t('ssl.fSelfSigned'), x ? (x.self_signed ? t('ssl.yes') : t('ssl.no')) : '—')}
      <!-- Lines break between the bytes only; <wbr> adds nothing to the copied text. -->
      <dt class="text-muted">{t('ssl.fFingerprint')}</dt><dd class="min-w-0 font-mono text-xs select-all">{#if x}{#each x.fingerprint_sha256.split(':') as b, n}{#if n}:<wbr />{/if}{b}{/each}{:else}—{/if}</dd>
    </dl>
    {#if panel?.record}<button class="btn btn-sm" onclick={() => { const r = panel.record; served = null; openCert(r); }}><Icon name="eye" size={13} /> {t('ssl.openRecord', { id: panel.record.id })}</button>{/if}
  {:else}<Skeleton rows={5} />{/if}
  {#snippet footer()}<button class="btn" onclick={() => (served = null)}>{t('common.close')}</button>{/snippet}
</Modal>
