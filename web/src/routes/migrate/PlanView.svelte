<script lang="ts">
  import type { Snippet } from 'svelte';
  import { bytes, when, daysLeft } from '$lib/api';
  import { t, type MsgKey } from '$lib/i18n/index.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Modal from '$lib/components/Modal.svelte';
  import type { Issue, Plan, PlanApp, PlanSite } from './types';

  // The answer of POST /migrate/plan, before anything is created: the account
  // as the source describes it, what blocks the move (conflicts), what only
  // complicates it (warnings) and what the source says will not move (notes).
  // Server texts are English by design; the page names their kind and target
  // and links to the place where the fix is done.
  let { plan, hint = '', hintTone = 'muted', onlogin, actions }: { plan: Plan; hint?: string; hintTone?: 'muted' | 'danger'; onlogin?: () => void; actions?: Snippet } = $props();

  const b = $derived(plan.bundle);
  const sites = $derived(b.sites ?? []);
  const dbs = $derived(b.databases ?? []);
  const cron = $derived(b.cron ?? []);
  const apps = $derived(b.apps ?? []);
  const valkey = $derived(b.valkey ?? []);
  const domains = $derived(b.mail_domains ?? []);
  const boxes = $derived(b.mailboxes ?? []);
  const aliases = $derived(b.mail_aliases ?? []);
  const certs = $derived(b.certificates ?? []);
  const conflicts = $derived(plan.conflicts ?? []);
  const warnings = $derived(plan.warnings ?? []);
  const dbSize = (name: string, own: number) => b.sizes?.databases?.[name] ?? own ?? 0;
  const dbTotal = $derived(dbs.reduce((n, d) => n + dbSize(d.name, d.size_bytes), 0));

  // The ssh adapters put the source's host key fingerprint among the notes:
  // nothing can check it automatically, a person compares it with the server.
  const fpPrefix = 'source ssh host key fingerprint: ';
  const fingerprint = $derived((b.notes ?? []).find((n) => n.startsWith(fpPrefix))?.slice(fpPrefix.length) ?? '');
  const notes = $derived((b.notes ?? []).filter((n) => !n.startsWith(fpPrefix)));

  const kinds: Record<string, MsgKey> = { user: 'migrate.kindUser', site: 'migrate.kindSite', database: 'migrate.kindDatabase', php: 'migrate.kindPhp', mail: 'migrate.kindMail', disk: 'migrate.kindDisk', cert: 'migrate.kindCert' };
  const kind = (k: string) => (kinds[k] ? t(kinds[k]) : k);

  // Where the fix is done in the panel: the API words its hint for the
  // terminal (mp …), the link leads to the page with the same control.
  function fixPage(i: Issue): [string, MsgKey] | null {
    const f = i.fix ?? '';
    if (f.includes('mp stack install')) return ['/stack', 'shell.nav.stack'];
    if (f.includes('mp php install')) return ['/php', 'shell.nav.php'];
    if (f.includes('mp mail install')) return ['/mail', 'shell.nav.mail'];
    if (f.includes('mp ssl issue')) return ['/ssl', 'shell.nav.ssl'];
    if (f.includes('mp app start')) return ['/users', 'shell.nav.users'];
    if (i.kind === 'site' && i.target) return ['/sites/' + encodeURIComponent(i.target), 'shell.nav.sites'];
    if (i.kind === 'database' && i.target) return ['/databases', 'shell.nav.databases'];
    if (i.kind === 'mail' && i.target) return ['/mail', 'shell.nav.mail'];
    return null;
  }

  type Section = 'sites' | 'databases' | 'mail' | 'certs' | 'cron' | 'apps' | 'valkey' | 'account';
  const sections = $derived(
    ([
      ['sites', 'migrate.sites', sites.length],
      ['databases', 'migrate.databases', dbs.length],
      ['mail', 'migrate.mailDomains', domains.length],
      ['certs', 'migrate.certs', certs.length],
      ['cron', 'migrate.cron', cron.length],
      ['apps', 'migrate.apps', apps.length],
      ['valkey', 'migrate.valkey', valkey.length]
    ] as [Section, MsgKey, number][]).filter(([, , n]) => n > 0)
  );
  // A section picked in an earlier plan may be empty in this one.
  let picked = $state<Section | ''>('');
  const section = $derived<Section>(picked === 'account' || sections.some(([id]) => id === picked) ? (picked as Section) : (sections[0]?.[0] ?? 'account'));

  const tiles = $derived(
    [
      ['migrate.sites', String(sites.length), ''],
      ['migrate.databases', String(dbs.length), dbs.length ? bytes(dbTotal) : ''],
      ['migrate.mailDomains', String(domains.length), domains.length ? t('migrate.subMail', { boxes: boxes.length, aliases: aliases.length }) : ''],
      ['migrate.certs', String(certs.length), certs.length ? t('migrate.subCerts', { n: certs.filter((c) => c.files).length }) : ''],
      ['migrate.cron', String(cron.length), cron.some((j) => !j.enabled) ? t('migrate.subCronOff', { n: cron.filter((j) => !j.enabled).length }) : ''],
      ['migrate.apps', String(apps.length), apps.length ? t('migrate.subApps') : ''],
      ['migrate.valkey', String(valkey.length), valkey.length ? t('migrate.subValkey') : ''],
      ['migrate.files', bytes(b.sizes?.files_bytes), t('migrate.subMailSize', { size: bytes(b.sizes?.mail_bytes) })]
    ] as [MsgKey, string, string][]
  );

  const modeName = (s: PlanSite) =>
    s.mode === 'proxy' ? 'proxy → ' + (s.backend || '—') : (s.mode === 'apache' ? 'nginx + Apache' : 'nginx + php-fpm') + (s.php_version ? ' · PHP ' + s.php_version : '');
  // Variable names only: the values (keys, passwords) are not needed to decide.
  const envNames = (a: PlanApp) => (a.env ?? []).map((e) => e.split('=')[0]).filter(Boolean).join(', ');
  const mb = (n: number | null | undefined) => (n ? t('migrate.mb', { n }) : t('migrate.noLimit'));
  // The API marks the jobs it switched off itself as a planted backdoor.
  const suspect = (comment: string | undefined) => !!comment && comment.includes('backdoor');

  let nginxOf = $state('');
</script>

{#snippet issue(i: Issue, conflict: boolean)}
  {@const to = fixPage(i)}
  <li class="text-sm flex gap-2 items-start min-w-0">
    <Icon name={conflict ? 'x' : 'alert'} size={15} class="{conflict ? 'text-danger' : 'text-warn'} shrink-0 mt-0.5" />
    <div class="min-w-0">
      <div class="break-words"><span class="tag tag-muted mr-1">{kind(i.kind)}</span>{#if i.target}<span class="font-mono">{i.target}</span>{' — '}{/if}{i.text}</div>
      {#if conflict && i.kind === 'user'}
        <div class="text-xs text-muted mt-1 flex flex-wrap items-center gap-2">{t('migrate.fixLogin')}{#if onlogin}<button type="button" class="btn btn-sm" onclick={onlogin}>{t('migrate.chooseLogin')}</button>{/if}</div>
      {:else if i.fix || to}
        <div class="text-xs text-muted mt-1 break-words">{#if i.fix}{'→ ' + i.fix}{/if}{#if to}{#if i.fix}{' · '}{/if}<a class="link" href={to[0]}>{t(to[1])}</a>{/if}</div>
      {/if}
    </div>
  </li>
{/snippet}

<div class="card mb-4 rise">
  <div class="flex flex-wrap items-start justify-between gap-3 mb-3">
    <div class="min-w-0">
      <div class="font-medium">{t('migrate.planTitle')}: <span class="font-mono">{b.user.login}</span> → <span class="font-mono">{plan.login}</span></div>
      <div class="text-sm text-muted mt-0.5 break-words">{t('migrate.planFrom', { host: b.hostname || '—', panel: b.panel || '—', family: b.family || '—' })} · <span class="font-mono">{b.scope}</span> · {t('migrate.planRead', { when: when(b.generated) })}</div>
    </div>
    <div class="flex flex-wrap items-center gap-2">
      <span class="tag {plan.ok ? 'tag-ok' : 'tag-err'}">{plan.ok ? t('migrate.ok') : t('migrate.blocked')}</span>
      {@render actions?.()}
    </div>
  </div>
  {#if hint}<p class="text-sm mb-3 {hintTone === 'danger' ? 'text-danger' : 'text-muted'}">{hint}</p>{/if}

  {#if conflicts.length}
    <div class="rounded-lg border border-danger/40 bg-danger-soft p-3 mb-3">
      <div class="label text-danger">{t('migrate.conflicts')}</div>
      <ul class="space-y-2">{#each conflicts as i}{@render issue(i, true)}{/each}</ul>
    </div>
  {/if}
  {#if warnings.length}
    <div class="rounded-lg border border-warn/40 bg-warn-soft p-3 mb-3">
      <div class="label text-warn">{t('migrate.warnings')}</div>
      <ul class="space-y-2">{#each warnings as i}{@render issue(i, false)}{/each}</ul>
    </div>
  {/if}
  {#if fingerprint}
    <div class="rounded-lg border border-accent/40 bg-accent-soft p-3 mb-3 text-sm">
      <div class="font-medium break-words">{t('migrate.fingerprint')}: <span class="font-mono select-all">{fingerprint}</span></div>
      <div class="text-xs text-muted mt-1">{t('migrate.fingerprintHint')}</div>
      <code class="block text-xs font-mono mt-1 break-all select-all">for f in /etc/ssh/ssh_host_*_key.pub; do ssh-keygen -lf "$f"; done</code>
    </div>
  {/if}
  {#if notes.length}
    <div class="mb-3">
      <div class="label">{t('migrate.notes')}</div>
      <ul class="text-sm text-muted space-y-1">{#each notes as n}<li class="flex gap-2 min-w-0"><span class="shrink-0">·</span><span class="whitespace-pre-wrap break-words min-w-0">{n}</span></li>{/each}</ul>
    </div>
  {/if}

  <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 mb-4">
    {#each tiles as [key, value, sub]}
      <div class="rounded-lg border border-line px-3 py-2 min-w-0">
        <div class="label mb-0.5 truncate">{t(key)}</div>
        <div class="font-mono tabular-nums truncate">{value}</div>
        {#if sub}<div class="text-[11px] text-muted truncate" title={sub}>{sub}</div>{/if}
      </div>
    {/each}
  </div>

  <div class="tabs">
    {#each sections as [id, key, n]}<button type="button" class="tab {section === id ? 'tab-on' : ''}" onclick={() => (picked = id)}>{t(key)} <span class="tabular-nums opacity-60">{n}</span></button>{/each}
    <button type="button" class="tab {section === 'account' ? 'tab-on' : ''}" onclick={() => (picked = 'account')}>{t('migrate.account')}</button>
  </div>

  {#if section === 'sites'}
    <div class="overflow-x-auto">
      <table class="tbl"><thead><tr><th>{t('migrate.colDomain')}</th><th>{t('migrate.colMode')}</th><th>Docroot</th><th>SSL</th><th>{t('migrate.colCms')}</th><th>{t('migrate.colMore')}</th></tr></thead><tbody>
        {#each sites as s}
          <tr>
            <td data-label={t('migrate.colDomain')}><div class="min-w-0"><div class="font-mono">{s.domain}</div>{#if s.aliases?.length}<div class="text-xs text-muted font-mono [overflow-wrap:anywhere]">{s.aliases.join(', ')}</div>{/if}</div></td>
            <td data-label={t('migrate.colMode')} class="text-xs">{modeName(s)}</td>
            <td data-label="Docroot" class="font-mono text-xs">{s.docroot || '—'}</td>
            <td data-label="SSL" class="text-xs"><span>{s.ssl || '—'}{#if s.redirect_https}<span class="tag tag-muted ml-1">→ HTTPS</span>{/if}</span></td>
            <td data-label={t('migrate.colCms')} class="text-xs">{s.cms ? s.cms + (s.cms_version ? ' ' + s.cms_version : '') : s.preset || '—'}</td>
            <td data-label={t('migrate.colMore')}><div class="flex flex-wrap gap-1">
              {#if b.site_nginx?.[s.domain]}<button type="button" class="btn btn-sm" onclick={() => (nginxOf = s.domain)}><Icon name="file" size={12} /> {t('migrate.ownNginx')}</button>{/if}
              {#if s.allow_from?.length}<span class="tag tag-muted" title={s.allow_from.join(', ')}>{t('migrate.allowList', { n: s.allow_from.length })}</span>{/if}
              {#if s.rate_limit}<span class="tag tag-muted">{t('migrate.rateLimit', { n: s.rate_limit })}</span>{/if}
              {#if s.session_store === 'valkey'}<span class="tag tag-muted">{t('migrate.sessionsValkey')}</span>{/if}
              {#if s.http3}<span class="tag tag-muted">HTTP/3</span>{/if}
            </div></td>
          </tr>
        {/each}
      </tbody></table>
    </div>
    <p class="text-xs text-muted mt-2">{t('migrate.sitesNote')}</p>
  {:else if section === 'databases'}
    <div class="overflow-x-auto">
      <table class="tbl"><thead><tr><th>{t('migrate.colDb')}</th><th>{t('migrate.colCharset')}</th><th>{t('migrate.colSize')}</th><th>{t('migrate.colDbUsers')}</th></tr></thead><tbody>
        {#each dbs as d}
          <tr>
            <td data-label={t('migrate.colDb')} class="font-mono">{d.name}</td>
            <td data-label={t('migrate.colCharset')} class="font-mono text-xs">{d.charset || '—'}{#if d.collation}{' / ' + d.collation}{/if}</td>
            <td data-label={t('migrate.colSize')} class="tabular-nums">{bytes(dbSize(d.name, d.size_bytes))}</td>
            <td data-label={t('migrate.colDbUsers')} class="font-mono text-xs"><div class="min-w-0">{#each d.users ?? [] as u}<div>{u.name}@{u.host}{#if u.auth_plugin}<span class="text-muted ml-1">({u.auth_plugin})</span>{/if}</div>{:else}—{/each}</div></td>
          </tr>
        {/each}
      </tbody></table>
    </div>
    <p class="text-xs text-muted mt-2">{t('migrate.dbNote')}</p>
  {:else if section === 'mail'}
    <div class="overflow-x-auto">
      <table class="tbl"><thead><tr><th>{t('migrate.colDomain')}</th><th>{t('migrate.colBoxes')}</th><th>{t('migrate.colAliases')}</th><th>DKIM</th><th></th></tr></thead><tbody>
        {#each domains as d}
          <tr>
            <td data-label={t('migrate.colDomain')} class="font-mono">{d.name}</td>
            <td data-label={t('migrate.colBoxes')} class="tabular-nums">{boxes.filter((x) => x.domain_id === d.id).length}</td>
            <td data-label={t('migrate.colAliases')} class="tabular-nums">{aliases.filter((x) => x.domain_id === d.id).length}</td>
            <td data-label="DKIM" class="font-mono text-xs">{d.dkim_selector || '—'}</td>
            <td data-label=""><div class="flex flex-wrap gap-1 justify-end">
              {#if !d.active}<span class="tag tag-warn">{t('common.off')}</span>{/if}
              {#if d.send_only}<span class="tag tag-accent">{t('migrate.sendOnly')}</span>{/if}
              {#if d.lenient}<span class="tag tag-warn">{t('migrate.lenient')}</span>{/if}
            </div></td>
          </tr>
        {/each}
      </tbody></table>
    </div>
    {#if boxes.length}
      <div class="label mt-4">{t('migrate.mailboxes')}</div>
      <div class="overflow-x-auto">
        <table class="tbl"><thead><tr><th>{t('migrate.colAddress')}</th><th>{t('common.name')}</th><th>{t('migrate.colQuota')}</th><th>{t('migrate.colSize')}</th><th>{t('common.status')}</th></tr></thead><tbody>
          {#each boxes as x}
            <tr>
              <td data-label={t('migrate.colAddress')} class="font-mono">{x.address}</td>
              <td data-label={t('common.name')} class="text-muted">{x.name || '—'}</td>
              <td data-label={t('migrate.colQuota')} class="tabular-nums">{mb(x.quota_mb)}</td>
              <td data-label={t('migrate.colSize')} class="tabular-nums">{bytes(x.size_bytes)}</td>
              <td data-label={t('common.status')}><span class={x.active ? 'text-ok' : 'text-muted'}>{x.active ? t('migrate.active') : t('common.off')}</span></td>
            </tr>
          {/each}
        </tbody></table>
      </div>
    {/if}
    {#if aliases.length}
      <div class="label mt-4">{t('migrate.aliases')}</div>
      <div class="overflow-x-auto">
        <table class="tbl"><thead><tr><th>{t('migrate.colAddress')}</th><th>{t('migrate.colDestination')}</th><th>{t('common.status')}</th></tr></thead><tbody>
          {#each aliases as a}
            <tr>
              <td data-label={t('migrate.colAddress')} class="font-mono">{a.address}</td>
              <td data-label={t('migrate.colDestination')} class="font-mono text-xs break-all">{a.destination}</td>
              <td data-label={t('common.status')}><span class={a.active ? 'text-ok' : 'text-muted'}>{a.active ? t('migrate.active') : t('common.off')}</span></td>
            </tr>
          {/each}
        </tbody></table>
      </div>
    {/if}
    <p class="text-xs text-muted mt-2">{t('migrate.mailNote')}</p>
  {:else if section === 'certs'}
    <div class="overflow-x-auto">
      <table class="tbl"><thead><tr><th>{t('migrate.colCert')}</th><th>{t('migrate.colNames')}</th><th>{t('migrate.colKind')}</th><th>{t('migrate.colUntil')}</th><th>{t('migrate.colFiles')}</th></tr></thead><tbody>
        {#each certs as c}
          <tr>
            <td data-label={t('migrate.colCert')} class="font-mono">{c.name}</td>
            <td data-label={t('migrate.colNames')} class="font-mono text-xs [overflow-wrap:anywhere]">{(c.names ?? []).join(', ') || '—'}</td>
            <td data-label={t('migrate.colKind')} class="text-xs">{c.kind === 'acme' ? "Let's Encrypt / ACME" : c.kind === 'custom' ? t('migrate.certCustom') : c.kind}{#if c.auto_renew}{' · ' + t('migrate.autoRenew')}{/if}</td>
            <td data-label={t('migrate.colUntil')} class="text-xs">{daysLeft(c.not_after)}</td>
            <td data-label={t('migrate.colFiles')}>{#if c.files}<span class="tag tag-ok">{t('migrate.certFiles')}</span>{:else}<span class="tag tag-warn">{t('migrate.certNoFiles')}</span>{/if}</td>
          </tr>
        {/each}
      </tbody></table>
    </div>
    <p class="text-xs text-muted mt-2">{t('migrate.certsNote')}</p>
  {:else if section === 'cron'}
    <div class="overflow-x-auto">
      <table class="tbl"><thead><tr><th>{t('migrate.colSchedule')}</th><th>{t('migrate.colCommand')}</th><th>{t('migrate.colNote')}</th></tr></thead><tbody>
        {#each cron as j}
          <tr>
            <td data-label={t('migrate.colSchedule')} class="font-mono whitespace-nowrap">{j.schedule}</td>
            <td data-label={t('migrate.colCommand')} class="font-mono text-xs break-all"><span>{j.command}{#if j.php_version}<span class="tag tag-muted ml-1">PHP {j.php_version}</span>{/if}</span></td>
            <td data-label={t('migrate.colNote')} class="text-xs"><span>{#if !j.enabled}<span class="tag tag-warn mr-1">{t('common.off')}</span>{/if}<span class={suspect(j.comment) ? 'text-danger' : 'text-muted'}>{j.comment || ''}</span></span></td>
          </tr>
        {/each}
      </tbody></table>
    </div>
    <p class="text-xs text-muted mt-2">{t('migrate.cronNote')}</p>
    {#if cron.some((j) => suspect(j.comment))}<p class="text-xs text-danger mt-1">{t('migrate.cronSuspect')}</p>{/if}
  {:else if section === 'apps'}
    <div class="overflow-x-auto">
      <table class="tbl"><thead><tr><th>{t('common.name')}</th><th>{t('migrate.colCommand')}</th><th>{t('migrate.colWorkdir')}</th><th>{t('migrate.colRestart')}</th><th>{t('migrate.colEnv')}</th></tr></thead><tbody>
        {#each apps as a}
          <tr>
            <td data-label={t('common.name')}><div class="min-w-0"><div class="font-mono">{a.name}</div>{#if a.description}<div class="text-xs text-muted">{a.description}</div>{/if}</div></td>
            <td data-label={t('migrate.colCommand')} class="font-mono text-xs break-all">{a.command}</td>
            <td data-label={t('migrate.colWorkdir')} class="font-mono text-xs">{a.workdir || '—'}</td>
            <td data-label={t('migrate.colRestart')} class="text-xs">{a.restart || '—'}</td>
            <td data-label={t('migrate.colEnv')} class="font-mono text-xs [overflow-wrap:anywhere]"><div class="min-w-0">{envNames(a) || '—'}{#if a.env_file}<div class="text-muted">{a.env_file}</div>{/if}</div></td>
          </tr>
        {/each}
      </tbody></table>
    </div>
    <p class="text-xs text-muted mt-2">{t('migrate.appsNote')}</p>
  {:else if section === 'valkey'}
    <div class="overflow-x-auto">
      <table class="tbl"><thead><tr><th>{t('migrate.colPurpose')}</th><th>{t('migrate.colMemory')}</th></tr></thead><tbody>
        {#each valkey as v}<tr><td data-label={t('migrate.colPurpose')} class="font-mono">{v.purpose}</td><td data-label={t('migrate.colMemory')} class="tabular-nums">{t('migrate.mb', { n: v.memory_mb })}</td></tr>{/each}
      </tbody></table>
    </div>
    <p class="text-xs text-muted mt-2">{t('migrate.valkeyNote')}</p>
  {:else}
    <dl class="grid sm:grid-cols-2 gap-x-6 gap-y-3 text-sm">
      <div><dt class="label">{t('migrate.accSource')}</dt><dd class="font-mono">{b.user.login}</dd></div>
      <div><dt class="label">{t('migrate.as')}</dt><dd class="font-mono">{plan.login}</dd></div>
      <div><dt class="label">E-mail</dt><dd>{b.user.email || '—'}</dd></div>
      <div><dt class="label">{t('migrate.accAccess')}</dt><dd>{b.user.shell ? 'SSH shell' : 'SFTP-only'}</dd></div>
      <div><dt class="label">{t('migrate.colQuota')}</dt><dd>{mb(b.user.quota_mb)}</dd></div>
      <div><dt class="label">{t('migrate.accHome')}</dt><dd class="font-mono text-xs break-all">{b.user.home || '—'}</dd></div>
    </dl>
    <p class="text-xs text-muted mt-3">{t('migrate.accNote')}</p>
  {/if}

  <details class="mt-4 text-xs">
    <summary class="cursor-pointer select-none text-muted hover:text-ink transition-colors">JSON</summary>
    <pre class="code mt-2 max-h-96">{JSON.stringify(plan, null, 2)}</pre>
  </details>
</div>

<Modal open={!!nginxOf} title={t('migrate.ownNginxTitle', { domain: nginxOf })} onclose={() => (nginxOf = '')}>
  <pre class="code max-h-96">{b.site_nginx?.[nginxOf] ?? ''}</pre>
  <p class="text-xs text-muted">{t('migrate.ownNginxNote')}</p>
</Modal>
