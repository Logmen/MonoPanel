<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError, when } from '$lib/api';
  import { auth } from '$lib/state.svelte';
  import { t, type MsgKey } from '$lib/i18n/index.svelte';
  import PageHead from '$lib/components/PageHead.svelte';
  import Skeleton from '$lib/components/Skeleton.svelte';
  const admin = $derived(auth.me?.role === 'admin');
  let entries = $state<any[] | null>(null);
  let error = $state('');
  // Сервер фильтрует по аккаунту (точное имя) и префиксу действия и отдаёт
  // не больше limit записей — как mp audit --actor --action -n; «только
  // отказы» — по результату здесь. Аккаунт без прав администратора сервер
  // и так ограничивает его собственными записями.
  let actor = $state('');
  let action = $state('');
  let limit = $state(200);
  let denied = $state(false);
  const limits = [50, 100, 200, 500, 1000];
  const quick: [string, MsgKey][] = [['auth.', 'audit.filterAuth'], ['token.', 'audit.filterTokens']];
  // Поле ввода ждёт паузы, а не дёргает сервер на каждую букву; кнопки и
  // клики по ячейкам применяются сразу.
  let actorQ = $state('');
  let actionQ = $state('');
  $effect(() => {
    const a = actor.trim(), b = action.trim();
    const id = setTimeout(() => { actorQ = a; actionQ = b; }, 350);
    return () => clearTimeout(id);
  });
  function setFilter(a: string, b: string) { actor = actorQ = a; action = actionQ = b; }

  let seq = 0;
  async function load() {
    const n = ++seq;
    const q = new URLSearchParams({ limit: String(limit) });
    if (admin && actorQ) q.set('actor', actorQ);
    if (actionQ) q.set('action', actionQ);
    try {
      const list = await api<any[]>('/system/audit?' + q);
      if (n === seq) { entries = list; error = ''; }
    } catch (e) {
      if (n === seq) error = e instanceof ApiError ? e.text : String(e);
    }
  }
  $effect(() => {
    limit;
    actorQ;
    actionQ;
    load();
    const timer = setInterval(load, 10000);
    return () => clearInterval(timer);
  });

  // Подсказки для полей: аккаунты панели и действия, уже встречавшиеся в журнале.
  let logins = $state<string[]>([]);
  onMount(async () => {
    if (!admin) return;
    try { logins = ((await api<any[]>('/users')) || []).map((u) => u.login); } catch { /* the field works without hints */ }
  });
  const actorHints = $derived([...new Set([...logins, ...(entries || []).map((e) => e.actor).filter(Boolean)])].sort());
  const actionHints = $derived([...new Set((entries || []).flatMap((e) => (e.action.includes('.') ? [e.action.slice(0, e.action.indexOf('.') + 1), e.action] : [e.action])))].sort());

  const shown = $derived((entries || []).filter((e) => !denied || e.result !== 'ok'));
  const fmt = (v: unknown) => (v !== null && typeof v === 'object' ? JSON.stringify(v) : String(v));
  const details = (d: Record<string, unknown> | undefined) => d ? Object.keys(d).sort().map((k) => `${k}=${fmt(d[k])}`).join(' ') : '';
  let expanded = $state<number[]>([]);
  const toggle = (id: number) => (expanded = expanded.includes(id) ? expanded.filter((x) => x !== id) : [...expanded, id]);
  const filtered = $derived(!!(actorQ || actionQ || denied));
</script>

<PageHead title={t('audit.title')} sub={t('audit.sub')}>
  <div class="flex flex-wrap gap-1">
    <button class="btn btn-sm {!actor && !action && !denied ? 'btn-primary' : 'btn-ghost'}" onclick={() => { setFilter('', ''); denied = false; }}>{t('audit.filterAll')}</button>
    {#each quick as [v, k]}<button class="btn btn-sm {action === v ? 'btn-primary' : 'btn-ghost'}" aria-pressed={action === v} onclick={() => setFilter(actor.trim(), action === v ? '' : v)}>{t(k)}</button>{/each}
    <button class="btn btn-sm {denied ? 'btn-primary' : 'btn-ghost'}" aria-pressed={denied} onclick={() => (denied = !denied)}>{t('audit.filterDenied')}</button>
  </div>
</PageHead>
<form class="card grid sm:grid-cols-2 md:grid-cols-4 gap-3 items-end mb-4 rise" onsubmit={(e) => { e.preventDefault(); setFilter(actor.trim(), action.trim()); }}>
  {#if admin}<div><label class="label" for="au-actor">{t('audit.actor')}</label><input id="au-actor" class="input" list="au-actors" bind:value={actor} placeholder={t('audit.actorPlaceholder')} autocomplete="off" /></div>{/if}
  <div><label class="label" for="au-action">{t('audit.action')}</label><input id="au-action" class="input font-mono" list="au-actions" bind:value={action} placeholder="site." autocomplete="off" /></div>
  <div><label class="label" for="au-limit">{t('audit.limit')}</label><select id="au-limit" class="input" bind:value={limit}>{#each limits as n}<option value={n}>{n}</option>{/each}</select></div>
  <label class="flex items-center gap-2 text-sm pb-1.5"><input type="checkbox" bind:checked={denied} /> {t('audit.onlyDenied')}</label>
  <p class="sm:col-span-2 md:col-span-4 text-xs text-muted m-0">{admin ? t('audit.filterHint') : t('audit.filterHintOwn')}</p>
  <datalist id="au-actors">{#each actorHints as a}<option value={a}></option>{/each}</datalist>
  <datalist id="au-actions">{#each actionHints as a}<option value={a}></option>{/each}</datalist>
</form>
{#if error}<p class="text-danger text-sm mb-3">{error}</p>{/if}
<div class="card overflow-x-auto p-0 rise">
  {#if !entries}<Skeleton rows={6} />{:else}
  <table class="tbl"><thead><tr><th>{t('audit.colTime')}</th><th>{t('audit.colWho')}</th><th>{t('audit.colAction')}</th><th>{t('audit.colTarget')}</th><th>{t('audit.colIP')}</th><th>{t('audit.colResult')}</th><th>{t('audit.colDetails')}</th></tr></thead>
    <tbody>
      {#each shown as e, i (e.id)}
        {@const d = details(e.details)}
        <tr class="rise" style="--i:{Math.min(i, 12)}">
          <td data-label={t('audit.colTime')} class="text-xs text-muted whitespace-nowrap">{when(e.ts)}</td>
          <td data-label={t('audit.colWho')}>{#if admin && e.actor}<button class="text-left hover:text-accent-ink transition-colors" title={t('audit.byActor', { actor: e.actor })} onclick={() => setFilter(e.actor, action.trim())}>{e.actor}</button>{:else}{e.actor}{/if}</td>
          <td data-label={t('audit.colAction')} class="font-mono"><button class="text-left hover:text-accent-ink transition-colors" title={t('audit.byAction', { action: e.action })} onclick={() => setFilter(actor.trim(), e.action)}>{e.action}</button></td>
          <td data-label={t('audit.colTarget')} class="font-mono text-muted break-words">{e.target || ''}</td>
          <td data-label={t('audit.colIP')} class="font-mono text-muted">{e.ip || ''}</td>
          <td data-label={t('audit.colResult')}><span class="tag {e.result === 'ok' ? 'tag-ok' : 'tag-err'}">{e.result}</span></td>
          <td data-label={t('audit.colDetails')} class="text-xs text-muted max-w-md">{#if d}<button class="text-left w-full max-w-md {expanded.includes(e.id) ? 'whitespace-pre-wrap break-all' : 'truncate block'}" title={expanded.includes(e.id) ? t('audit.collapse') : d} onclick={() => toggle(e.id)}>{d}</button>{/if}</td>
        </tr>
      {/each}
      {#if !shown.length}<tr><td colspan="7" class="text-muted text-center py-6">{filtered ? t('audit.emptyFiltered') : t('audit.empty')}</td></tr>{/if}
    </tbody></table>
  {/if}
</div>
{#if entries?.length}<p class="text-xs text-muted mt-3">{entries.length >= limit ? t('audit.shownLimit', { n: entries.length }) : t('audit.shown', { n: entries.length })}{#if denied}{' ' + t('audit.shownDenied', { n: shown.length })}{/if}</p>{/if}
<p class="text-xs text-muted mt-1">{t('audit.file')}</p>
