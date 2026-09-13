<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Confirm, { type Ask } from '$lib/components/Confirm.svelte';
  import { t, type MsgKey } from '$lib/i18n/index.svelte';
  let fw = $state<any>(null);
  let error = $state('');
  let job = $state<number | null>(null);
  let rule = $state({ kind: 'allow', proto: 'tcp', port: '', source: '', comment: '' });
  let ban = $state('');
  let ask = $state<Ask | null>(null);
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function load() { try { fw = await api('/firewall'); } catch (e: any) { error = e.text || String(e); } }
  onMount(load);
  // Toasts name the result in the person's language; an unknown action falls back to the raw verb.
  const actDone: Record<string, MsgKey> = { apply: 'firewall.applied', enable: 'firewall.enabled', disable: 'firewall.disabled', ban: 'firewall.banned', unban: 'firewall.unbanned' };
  const done = (a: string) => (actDone[a] ? t(actDone[a]) : a);
  async function act(a: string) { error = ''; try { fw = await api(`/firewall/${a}`, { method: 'POST' }); notify(`Firewall: ${done(a)}`); } catch (e) { fail(e); } }
  async function addRule(e: Event) { e.preventDefault(); error = ''; try { await api('/firewall/rules', { method: 'POST', json: rule }); rule.port = ''; rule.source = ''; await load(); } catch (e) { fail(e); } }
  async function rm(id: number) { try { await api(`/firewall/rules/${id}`, { method: 'DELETE' }); await load(); } catch (e) { fail(e); } }
  const askDisable = (): Ask => ({
    title: t('firewall.askDisableTitle'),
    note: t('firewall.askDisableNote'),
    danger: true, action: t('firewall.disable'),
    run: () => act('disable')
  });
  const askRule = (r: any): Ask => ({
    title: t('firewall.askRuleTitle'),
    note: t(r.source ? 'firewall.askRuleNoteSource' : 'firewall.askRuleNote', { kind: t(r.kind === 'allow' ? 'firewall.ruleAllow' : 'firewall.ruleDeny'), proto: r.proto, port: r.port || t('firewall.anyPort'), source: r.source || '' }),
    danger: true, action: t('common.delete'),
    run: () => rm(r.id)
  });
  const askUnban = (ip: string): Ask => ({
    title: t('firewall.askUnbanTitle', { ip }),
    note: t('firewall.askUnbanNote'),
    action: t('firewall.unban'),
    run: () => doBan('unban', ip)
  });
  async function doBan(a: string, ip: string) { error = ''; try { fw = await api(`/firewall/${a}`, { method: 'POST', json: { ip } }); ban = ''; notify(`${ip}: ${done(a)}`); } catch (e) { fail(e); } }
  async function installF2b() { try { const r: any = await api('/stack/install', { method: 'POST', json: { component: 'fail2ban' } }); job = r.job_id; } catch (e) { fail(e); } }
</script>

<PageHead title="Firewall" sub={fw ? t('firewall.sub', { ssh: fw.ssh_ports.join(','), panel: fw.panel_port }) : ''}>
  {#if fw}
    <span class="tag {fw.enabled ? 'tag-ok' : 'tag-warn'}"><span class="dot"></span>{fw.enabled ? t('firewall.enabled') : t('firewall.disabled')}</span>
    {#if fw.enabled}<button class="btn" onclick={() => act('apply')}><Icon name="refresh" size={14} /> {t('firewall.applyBtn')}</button><button class="btn btn-danger" onclick={() => (ask = askDisable())}>{t('firewall.disableBtn')}</button>{:else}<button class="btn btn-primary" onclick={() => act('enable')}>{t('firewall.enableBtn')}</button>{/if}
  {/if}
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if fw}
  <form class="card grid md:grid-cols-6 gap-3 items-end mb-4 rise" onsubmit={addRule}>
    <div><label class="label" for="k">{t('firewall.rule')}</label><select id="k" class="input" bind:value={rule.kind}><option value="allow">allow</option><option value="deny">deny</option></select></div>
    <div><label class="label" for="pr">{t('firewall.proto')}</label><select id="pr" class="input" bind:value={rule.proto}><option value="tcp">tcp</option><option value="udp">udp</option><option value="any">any</option></select></div>
    <div><label class="label" for="po">{t('firewall.port')}</label><input id="po" class="input font-mono" bind:value={rule.port} placeholder={t('firewall.portPlaceholder')} /></div>
    <div><label class="label" for="s">{t('firewall.source')}</label><input id="s" class="input font-mono" bind:value={rule.source} placeholder="IP / CIDR" /></div>
    <div><label class="label" for="c">{t('firewall.comment')}</label><input id="c" class="input" bind:value={rule.comment} /></div>
    <button class="btn btn-primary"><Icon name="plus" size={14} /> {t('common.add')}</button>
  </form>
  <div class="card overflow-x-auto p-0 mb-4 rise" style="--i:1">
    <table class="tbl"><thead><tr><th>ID</th><th>{t('firewall.colKind')}</th><th>Proto</th><th>{t('firewall.port')}</th><th>{t('firewall.source')}</th><th>{t('firewall.comment')}</th><th></th></tr></thead>
      <tbody>{#each fw.rules as r, i}<tr class="rise" style="--i:{i}"><td data-label="ID" class="text-muted">{r.id}</td><td data-label={t('firewall.colKind')}><span class="tag {r.kind === 'allow' ? 'tag-ok' : 'tag-err'}">{r.kind}</span></td><td data-label="Proto">{r.proto}</td><td data-label={t('firewall.port')} class="font-mono">{r.port}</td><td data-label={t('firewall.source')} class="font-mono">{r.source}</td><td data-label={t('firewall.comment')} class="text-muted">{r.comment}</td><td data-label="" class="text-right"><button class="btn btn-danger btn-sm" onclick={() => (ask = askRule(r))}><Icon name="trash" size={13} /></button></td></tr>{/each}
      {#if !fw.rules.length}<Empty text={t('firewall.noRules')} cols={7} />{/if}</tbody></table>
  </div>
  <div class="card rise" style="--i:2">
    <div class="flex flex-wrap items-center gap-3 mb-3"><span class="font-medium">fail2ban</span>
      {#if fw.fail2ban}<span class="tag {fw.fail2ban.running ? 'tag-ok' : 'tag-err'}">{fw.fail2ban.running ? 'running' : 'stopped'}</span>{:else}<button class="btn btn-sm" onclick={installF2b}>{t('firewall.installF2b')}</button>{/if}
      <form class="ml-auto flex gap-2" onsubmit={(e) => { e.preventDefault(); doBan('ban', ban); }}><input class="input w-48 font-mono" bind:value={ban} placeholder={t('firewall.banPlaceholder')} /><button class="btn btn-danger btn-sm">{t('firewall.banBtn')}</button></form>
    </div>
    {#if fw.fail2ban}
      <table class="tbl"><thead><tr><th>Jail</th><th>{t('firewall.colNow')}</th><th>{t('firewall.colTotal')}</th><th>{t('firewall.colIps')}</th></tr></thead><tbody>
        {#each fw.fail2ban.jails as j}<tr><td data-label="Jail" class="font-mono">{j.name}</td><td data-label={t('firewall.colNow')} class="tabular-nums">{j.banned}</td><td data-label={t('firewall.colTotal')} class="tabular-nums">{j.total}</td><td data-label={t('firewall.colIps')} class="font-mono text-xs">{#each j.ips || [] as ip}<button class="tag tag-err mr-1 mb-1 hover:bg-danger hover:text-white transition-colors" onclick={() => (ask = askUnban(ip))} title={t('firewall.unbanTitle')}>{ip} ✕</button>{/each}</td></tr>{/each}
      </tbody></table>
    {/if}
  </div>
  {#if job}<div class="mt-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
{/if}

<Confirm bind:ask />
