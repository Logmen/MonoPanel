<script lang="ts">
  import { api, apiText, apiPutRaw, ApiError, bytes } from '$lib/api';
  import { notify } from '$lib/state.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';

  let { user, start = '/', sites = [] }: { user: string; start?: string; sites?: { domain: string; docroot?: string }[] } = $props();

  type Entry = { name: string; type: string; size: number; mode: string; mtime: string; target?: string };

  let cwd = $state(start);
  let entries = $state<Entry[]>([]);
  let loading = $state(false);
  let error = $state('');
  let selected = $state<string[]>([]);

  // Редактор
  let editing = $state<string | null>(null);
  let text = $state('');
  let original = $state('');
  let saving = $state(false);
  let area = $state<HTMLTextAreaElement | null>(null);
  let gutter = $state<HTMLDivElement | null>(null);
  const dirty = $derived(editing !== null && text !== original);
  const lines = $derived(text.split('\n').length);

  // Диалог одного поля: отдельные переменные, а не объект — привязка к
  // свойству объекта, который целиком переприсваивается, значение теряет.
  let askOpen = $state(false);
  let askTitle = $state('');
  let askLabel = $state('');
  let askValue = $state('');
  let askRun: (v: string) => Promise<void> = async () => {};

  // Расширения, которые точно не текст: не тратим запрос на попытку открыть.
  const binaryExt = /\.(png|jpe?g|gif|webp|avif|ico|bmp|tiff?|svgz|pdf|zip|gz|tgz|bz2|xz|7z|rar|tar|mp[34]|m4a|mov|avi|mkv|webm|woff2?|ttf|eot|otf|so|bin|exe|dll|class|jar|db|sqlite3?|psd)$/i;
  const archiveExt = /\.(zip|tar|tgz|gz|bz2|xz)$/i;
  const editLimit = 1000000;

  const clean = (p: string) => '/' + p.split('/').filter(Boolean).join('/');
  const join = (dir: string, name: string) => clean(dir + '/' + name);
  const parent = (p: string) => clean(p.split('/').filter(Boolean).slice(0, -1).join('/'));
  const crumbs = $derived(
    cwd.split('/').filter(Boolean).map((part, i, all) => ({ name: part, path: clean(all.slice(0, i + 1).join('/')) }))
  );
  const q = (p: string) => `user=${encodeURIComponent(user)}&path=${encodeURIComponent(p)}`;
  const fail = (e: unknown) => { error = e instanceof ApiError ? e.text : String(e); notify(error, 'err'); };

  async function load() {
    loading = true;
    error = '';
    selected = [];
    try {
      const res: any = await api(`/files?${q(cwd)}`);
      const list: Entry[] = res.entries || [];
      // Каталоги сверху, дальше по алфавиту.
      entries = list.sort((a, b) =>
        (a.type === 'dir' ? 0 : 1) - (b.type === 'dir' ? 0 : 1) || a.name.localeCompare(b.name, 'ru')
      );
    } catch (e) {
      entries = [];
      fail(e);
    } finally {
      loading = false;
    }
  }

  $effect(() => { user; cwd; load(); });

  // Уход из редактора с несохранёнными правками требует подтверждения.
  function leaveEditor(): boolean {
    if (dirty && !confirm('Изменения не сохранены. Закрыть файл?')) return false;
    editing = null;
    text = original = '';
    return true;
  }

  function go(path: string) {
    if (!leaveEditor()) return;
    cwd = clean(path);
  }

  async function open(e: Entry) {
    if (e.type === 'dir') { go(join(cwd, e.name)); return; }
    if (binaryExt.test(e.name)) { notify('двоичный файл — доступно скачивание', 'err'); return; }
    if (e.size > editLimit) { notify(`файл больше ${bytes(editLimit)} — доступно скачивание`, 'err'); return; }
    if (!leaveEditor()) return;
    try {
      const body = await apiText(`/files/content?${q(join(cwd, e.name))}`);
      // Нулевой байт — верный признак, что это не текст.
      if (body.indexOf('\u0000') >= 0) { notify('двоичный файл — доступно скачивание', 'err'); return; }
      original = text = body;
      editing = e.name;
    } catch (err) { fail(err); }
  }

  async function save() {
    if (editing === null || saving) return;
    saving = true;
    try {
      if (text === '') {
        // Пустое тело PUT не принимает — опустошение файла идёт отдельной операцией.
        await api('/files/op', { method: 'POST', json: { user, op: 'touch', path: join(cwd, editing), force: true } });
      } else {
        await apiPutRaw(`/files/content?${q(join(cwd, editing))}`, text);
      }
      original = text;
      notify(`${editing} сохранён`);
      await load();
    } catch (e) { fail(e); } finally { saving = false; }
  }

  function editorKeys(e: KeyboardEvent) {
    if ((e.ctrlKey || e.metaKey) && e.key === 's') { e.preventDefault(); save(); return; }
    if (e.key === 'Escape') { leaveEditor(); return; }
    if (e.key === 'Tab') {
      e.preventDefault();
      const el = e.currentTarget as HTMLTextAreaElement;
      const at = el.selectionStart;
      text = text.slice(0, at) + '  ' + text.slice(el.selectionEnd);
      queueMicrotask(() => el.setSelectionRange(at + 2, at + 2));
    }
  }

  function syncGutter() { if (gutter && area) gutter.scrollTop = area.scrollTop; }

  async function op(body: Record<string, unknown>, done: string) {
    try {
      await api('/files/op', { method: 'POST', json: { user, ...body } });
      notify(done);
      await load();
    } catch (e) { fail(e); }
  }

  function request(title: string, label: string, value: string, run: (v: string) => Promise<void>) {
    askTitle = title;
    askLabel = label;
    askValue = value;
    askRun = run;
    askOpen = true;
  }
  async function askSubmit(e: Event) {
    e.preventDefault();
    const value = askValue.trim();
    if (!value) return;
    askOpen = false;
    await askRun(value);
  }

  const mkdir = () => request('Новая папка', 'Название', '', (v) => op({ op: 'mkdir', path: join(cwd, v) }, `папка ${v} создана`));
  const touch = () => request('Новый файл', 'Название', '', (v) => op({ op: 'touch', path: join(cwd, v) }, `файл ${v} создан`));
  const rename = (e: Entry) => request('Переименовать', 'Новое имя или путь', e.name, (v) =>
    op({ op: 'mv', path: join(cwd, e.name), dest: v.includes('/') ? clean(v) : join(cwd, v) }, 'переименовано'));
  const chmod = (e: Entry) => request(`Права на ${e.name}`, 'Восьмеричные права, например 644', modeOctal(e.mode), (v) =>
    op({ op: 'chmod', path: join(cwd, e.name), mode: v }, 'права изменены'));
  const extract = (e: Entry) => request(`Распаковать ${e.name}`, 'Куда распаковать', cwd, (v) =>
    op({ op: 'extract', path: join(cwd, e.name), dest: clean(v) }, 'архив распакован'));

  function remove(names: string[]) {
    if (!names.length) return;
    const what = names.length === 1 ? names[0] : `${names.length} объект(ов)`;
    if (!confirm(`Удалить ${what}? Действие необратимо.`)) return;
    const paths = names.map((n) => join(cwd, n));
    op({ op: 'rm', path: paths[0], paths: paths.slice(1) }, 'удалено');
  }

  function download(e: Entry) {
    // Прямая ссылка: файл придёт с Content-Disposition от API.
    window.location.href = `/api/v1/files/content?${q(join(cwd, e.name))}`;
  }

  async function upload(files: FileList | null) {
    if (!files || !files.length) return;
    for (const f of Array.from(files)) {
      try {
        await apiPutRaw(`/files/content?${q(join(cwd, f.name))}`, f);
        notify(`${f.name} загружен (${bytes(f.size)})`);
      } catch (e) { fail(e); }
    }
    await load();
  }

  function pick() {
    const input = document.createElement('input');
    input.type = 'file';
    input.multiple = true;
    input.onchange = () => upload(input.files);
    input.click();
  }

  let askInput = $state<HTMLInputElement | null>(null);
  // Диалог открывается пустым или с текущим значением — курсор сразу в поле.
  $effect(() => { if (askOpen) queueMicrotask(() => askInput?.select()); });

  let dragging = $state(false);
  function drop(e: DragEvent) {
    e.preventDefault();
    dragging = false;
    upload(e.dataTransfer?.files ?? null);
  }

  // Права приходят строкой вида "-rw-r--r--"; для chmod нужно восьмеричное.
  function modeOctal(mode: string): string {
    const m = mode.slice(-9);
    if (m.length !== 9) return '644';
    let out = '';
    for (let i = 0; i < 9; i += 3) {
      out += String((m[i] !== '-' ? 4 : 0) + (m[i + 1] !== '-' ? 2 : 0) + (m[i + 2] !== '-' ? 1 : 0));
    }
    return out;
  }

  const icon = (e: Entry) =>
    e.type === 'dir' ? 'box' : archiveExt.test(e.name) ? 'archive' : binaryExt.test(e.name) ? 'file' : 'code';
  const toggle = (name: string) =>
    (selected = selected.includes(name) ? selected.filter((n) => n !== name) : [...selected, name]);
</script>

<div class="card p-0 overflow-hidden">
  <div class="flex flex-wrap items-center gap-2 p-3 border-b border-line bg-surface-2">
    <div class="flex items-center gap-1 text-sm min-w-0 flex-1 flex-wrap">
      <button class="btn btn-ghost btn-sm" onclick={() => go('/')} title="домашний каталог"><Icon name="home" size={14} /></button>
      {#if cwd !== '/'}<button class="btn btn-ghost btn-sm" onclick={() => go(parent(cwd))} title="вверх">..</button>{/if}
      <span class="text-muted font-mono text-xs">/var/www/{user}</span>
      {#each crumbs as c}
        <span class="text-muted">/</span>
        <button class="font-mono text-xs hover:text-accent-ink transition-colors" onclick={() => go(c.path)}>{c.name}</button>
      {/each}
    </div>
    {#if sites.length}
      <select
        class="input py-1 text-xs w-auto"
        onchange={(e) => {
          const el = e.currentTarget as HTMLSelectElement;
          if (el.value) go(el.value);
          el.value = '';
        }}
      >
        <option value="">перейти к сайту…</option>
        {#each sites as s}<option value={'/data/www/' + s.domain + (s.docroot ? '/' + s.docroot : '')}>{s.domain}</option>{/each}
      </select>
    {/if}
    <div class="flex gap-1">
      <button class="btn btn-sm" onclick={mkdir}><Icon name="plus" size={13} /> папка</button>
      <button class="btn btn-sm" onclick={touch}><Icon name="file" size={13} /> файл</button>
      <button class="btn btn-sm" onclick={pick}><Icon name="archive" size={13} /> загрузить</button>
      <button class="btn btn-ghost btn-sm" onclick={load} title="обновить"><Icon name="refresh" size={14} /></button>
    </div>
  </div>

  {#if selected.length}
    <div class="flex items-center gap-3 px-3 py-2 border-b border-line bg-accent-soft text-sm">
      <span>выбрано: {selected.length}</span>
      <button class="btn btn-danger btn-sm" onclick={() => remove(selected)}><Icon name="trash" size={13} /> удалить</button>
      <button class="btn btn-ghost btn-sm" onclick={() => (selected = [])}>снять</button>
    </div>
  {/if}

  {#if error}<p class="text-danger text-sm px-3 py-2 flex items-center gap-1.5"><Icon name="alert" size={15} /> {error}</p>{/if}

  <div
    class="relative {dragging ? 'ring-2 ring-accent ring-inset' : ''}"
    role="region"
    aria-label="файлы"
    ondragover={(e) => { e.preventDefault(); dragging = true; }}
    ondragleave={() => (dragging = false)}
    ondrop={drop}
  >
    {#if loading && !entries.length}
      <p class="text-sm text-muted p-4">загрузка…</p>
    {:else if !entries.length}
      <p class="text-sm text-muted p-6 text-center">Пусто. Перетащите файлы сюда или создайте новый.</p>
    {:else}
      <table class="w-full text-sm">
        <thead class="text-xs text-muted">
          <tr class="border-b border-line">
            <th class="w-8 py-2"></th>
            <th class="text-left font-medium py-2 px-2">Имя</th>
            <th class="text-right font-medium py-2 px-3 w-24">Размер</th>
            <th class="text-left font-medium py-2 px-3 w-28 hidden sm:table-cell">Права</th>
            <th class="text-left font-medium py-2 px-3 w-40 hidden md:table-cell">Изменён</th>
            <th class="w-40"></th>
          </tr>
        </thead>
        <tbody>
          {#each entries as e (e.name)}
            <tr class="border-b border-line last:border-0 hover:bg-surface-2 transition-colors {editing === e.name ? 'bg-accent-soft' : ''}">
              <td class="text-center">
                <input type="checkbox" checked={selected.includes(e.name)} onchange={() => toggle(e.name)} aria-label={'выбрать ' + e.name} />
              </td>
              <td class="py-1.5 px-2">
                <button class="inline-flex items-center gap-2 text-left max-w-full" onclick={() => open(e)}>
                  <Icon name={icon(e)} size={15} class="shrink-0 text-muted" />
                  <span class="truncate {e.type === 'dir' ? 'font-medium' : ''}">{e.name}</span>
                  {#if e.type === 'link'}<span class="text-xs text-muted">→ {e.target}</span>{/if}
                </button>
              </td>
              <td class="text-right text-muted tabular-nums px-3 whitespace-nowrap">{e.type === 'dir' ? '—' : bytes(e.size)}</td>
              <td class="font-mono text-xs text-muted hidden sm:table-cell px-3">{e.mode}</td>
              <td class="text-muted text-xs hidden md:table-cell px-3 whitespace-nowrap">{(e.mtime || '').replace('T', ' ').slice(0, 16)}</td>
              <td class="text-right whitespace-nowrap pr-2">
                {#if e.type !== 'dir'}
                  <button class="btn btn-ghost btn-sm" onclick={() => download(e)} title="скачать"><Icon name="download" size={13} /></button>
                  {#if archiveExt.test(e.name)}<button class="btn btn-ghost btn-sm" onclick={() => extract(e)} title="распаковать"><Icon name="archive" size={13} /></button>{/if}
                {/if}
                <button class="btn btn-ghost btn-sm" onclick={() => rename(e)} title="переименовать"><Icon name="pencil" size={13} /></button>
                <button class="btn btn-ghost btn-sm" onclick={() => chmod(e)} title="права"><Icon name="lock" size={13} /></button>
                <button class="btn btn-ghost btn-sm text-danger" onclick={() => remove([e.name])} title="удалить"><Icon name="trash" size={13} /></button>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>
</div>

{#if editing !== null}
  <div class="card p-0 mt-4 overflow-hidden">
    <div class="flex flex-wrap items-center gap-2 p-3 border-b border-line bg-surface-2">
      <Icon name="code" size={15} class="text-muted" />
      <span class="font-mono text-sm truncate">{join(cwd, editing)}</span>
      {#if dirty}<span class="tag">не сохранено</span>{/if}
      <span class="text-xs text-muted ml-auto">{lines} строк · {bytes(new Blob([text]).size)}</span>
      <button class="btn btn-primary btn-sm" disabled={!dirty || saving} onclick={save}>
        <Icon name="save" size={13} /> {saving ? 'сохраняю…' : 'Сохранить'}
      </button>
      <button class="btn btn-sm" onclick={leaveEditor}>Закрыть</button>
    </div>
    <div class="flex font-mono text-[13px] leading-[1.5]">
      <div bind:this={gutter} class="select-none text-right text-muted bg-surface-2 py-3 px-2 overflow-hidden" style="max-height:60vh">
        {#each Array.from({ length: lines }) as _, i}<div>{i + 1}</div>{/each}
      </div>
      <textarea
        bind:this={area}
        bind:value={text}
        onkeydown={editorKeys}
        onscroll={syncGutter}
        spellcheck="false"
        autocomplete="off"
        autocapitalize="off"
        class="flex-1 bg-transparent p-3 outline-none resize-none font-mono text-[13px] leading-[1.5]"
        style="min-height:40vh;max-height:60vh"
        aria-label="содержимое файла"
      ></textarea>
    </div>
    <div class="px-3 py-2 border-t border-line text-xs text-muted">
      Ctrl+S — сохранить, Esc — закрыть. Файл пишется от имени {user}; nginx и php-fpm подхватывают изменения сразу.
    </div>
  </div>
{/if}

<Modal bind:open={askOpen} title={askTitle}>
  <form id="fm-ask" onsubmit={askSubmit}>
    <label class="label" for="fm-value">{askLabel}</label>
    <!-- Кнопка «Готово» живёт в подвале модалки, вне формы, поэтому Enter
         обрабатывается здесь, а не неявной отправкой формы. -->
    <input
      id="fm-value"
      class="input font-mono"
      bind:this={askInput}
      bind:value={askValue}
      onkeydown={(e) => { if (e.key === 'Enter') askSubmit(e); }}
    />
  </form>
  {#snippet footer()}
    <button class="btn" onclick={() => (askOpen = false)}>Отмена</button>
    <button class="btn btn-primary" form="fm-ask" type="submit">Готово</button>
  {/snippet}
</Modal>
