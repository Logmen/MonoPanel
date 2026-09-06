<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import JobLog from '$lib/components/JobLog.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Empty from '$lib/components/Empty.svelte';
  import Icon from '$lib/components/Icon.svelte';
  let fw = $state<any>(null);
  let error = $state('');
  let job = $state<number | null>(null);
  let rule = $state({ kind: 'allow', proto: 'tcp', port: '', source: '', comment: '' });
  let ban = $state('');
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };
  async function load() { try { fw = await api('/firewall'); } catch (e: any) { error = e.text || String(e); } }
  onMount(load);
  async function act(a: string) { error = ''; try { fw = await api(`/firewall/${a}`, { method: 'POST' }); notify(`Firewall: ${a}`); } catch (e) { fail(e); } }
  async function addRule(e: Event) { e.preventDefault(); error = ''; try { await api('/firewall/rules', { method: 'POST', json: rule }); rule.port = ''; rule.source = ''; await load(); } catch (e) { fail(e); } }
  async function rm(id: number) { try { await api(`/firewall/rules/${id}`, { method: 'DELETE' }); await load(); } catch (e) { fail(e); } }
  async function doBan(a: string, ip: string) { error = ''; try { fw = await api(`/firewall/${a}`, { method: 'POST', json: { ip } }); ban = ''; notify(`${ip}: ${a}`); } catch (e) { fail(e); } }
  async function installF2b() { try { const r: any = await api('/stack/install', { method: 'POST', json: { component: 'fail2ban' } }); job = r.job_id; } catch (e) { fail(e); } }
</script>

<PageHead title="Firewall" sub={fw ? `nftables inet monopanel · policy drop · всегда открыты SSH ${fw.ssh_ports.join(',')}, 80, 443, панель ${fw.panel_port}` : ''}>
  {#if fw}
    <span class="tag {fw.enabled ? 'tag-ok' : 'tag-warn'}"><span class="dot"></span>{fw.enabled ? 'включён' : 'выключен'}</span>
    {#if fw.enabled}<button class="btn" onclick={() => act('apply')}><Icon name="refresh" size={14} /> применить</button><button class="btn btn-danger" onclick={() => confirm('Выключить firewall?') && act('disable')}>выключить</button>{:else}<button class="btn btn-primary" onclick={() => act('enable')}>включить</button>{/if}
  {/if}
</PageHead>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
{#if fw}
  <form class="card grid md:grid-cols-6 gap-3 items-end mb-4 rise" onsubmit={addRule}>
    <div><label class="label" for="k">Правило</label><select id="k" class="input" bind:value={rule.kind}><option value="allow">allow</option><option value="deny">deny</option></select></div>
    <div><label class="label" for="pr">Протокол</label><select id="pr" class="input" bind:value={rule.proto}><option value="tcp">tcp</option><option value="udp">udp</option><option value="any">any</option></select></div>
    <div><label class="label" for="po">Порт</label><input id="po" class="input font-mono" bind:value={rule.port} placeholder="8081 или 8000-8100" /></div>
    <div><label class="label" for="s">Источник</label><input id="s" class="input font-mono" bind:value={rule.source} placeholder="IP / CIDR" /></div>
    <div><label class="label" for="c">Комментарий</label><input id="c" class="input" bind:value={rule.comment} /></div>
    <button class="btn btn-primary"><Icon name="plus" size={14} /> Добавить</button>
  </form>
  <div class="card overflow-x-auto p-0 mb-4 rise" style="--i:1">
    <table class="tbl"><thead><tr><th>ID</th><th>Тип</th><th>Proto</th><th>Порт</th><th>Источник</th><th>Комментарий</th><th></th></tr></thead>
      <tbody>{#each fw.rules as r, i}<tr class="rise" style="--i:{i}"><td data-label="ID" class="text-muted">{r.id}</td><td data-label="Тип"><span class="tag {r.kind === 'allow' ? 'tag-ok' : 'tag-err'}">{r.kind}</span></td><td data-label="Proto">{r.proto}</td><td data-label="Порт" class="font-mono">{r.port}</td><td data-label="Источник" class="font-mono">{r.source}</td><td data-label="Комментарий" class="text-muted">{r.comment}</td><td data-label="" class="text-right"><button class="btn btn-danger btn-sm" onclick={() => rm(r.id)}><Icon name="trash" size={13} /></button></td></tr>{/each}
      {#if !fw.rules.length}<Empty text="Пользовательских правил нет." cols={7} />{/if}</tbody></table>
  </div>
  <div class="card rise" style="--i:2">
    <div class="flex flex-wrap items-center gap-3 mb-3"><span class="font-medium">fail2ban</span>
      {#if fw.fail2ban}<span class="tag {fw.fail2ban.running ? 'tag-ok' : 'tag-err'}">{fw.fail2ban.running ? 'running' : 'stopped'}</span>{:else}<button class="btn btn-sm" onclick={installF2b}>установить (sshd, nginx, панель)</button>{/if}
      <form class="ml-auto flex gap-2" onsubmit={(e) => { e.preventDefault(); doBan('ban', ban); }}><input class="input w-48 font-mono" bind:value={ban} placeholder="IP для бана" /><button class="btn btn-danger btn-sm">забанить</button></form>
    </div>
    {#if fw.fail2ban}
      <table class="tbl"><thead><tr><th>Jail</th><th>Сейчас</th><th>Всего</th><th>IP (клик — разбанить)</th></tr></thead><tbody>
        {#each fw.fail2ban.jails as j}<tr><td data-label="Jail" class="font-mono">{j.name}</td><td data-label="Сейчас" class="tabular-nums">{j.banned}</td><td data-label="Всего" class="tabular-nums">{j.total}</td><td data-label="IP (клик — разбанить)" class="font-mono text-xs">{#each j.ips || [] as ip}<button class="tag tag-err mr-1 mb-1 hover:bg-danger hover:text-white transition-colors" onclick={() => doBan('unban', ip)} title="разбанить">{ip} ✕</button>{/each}</td></tr>{/each}
      </tbody></table>
    {/if}
  </div>
  {#if job}<div class="mt-4"><JobLog jobId={job} onfinish={() => load()} /></div>{/if}
{/if}
