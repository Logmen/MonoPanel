<script lang="ts" module>
  // AMD-загрузчик Monaco: один на страницу, общий для всех экземпляров
  // менеджера (страница файлов и вкладка сайта). Неудача не запоминается:
  // следующий вызов пробует снова — панель могла просто перезапускаться.
  let monacoPromise: Promise<any> | null = null;
  export function loadMonaco(): Promise<any> {
    const w = window as any;
    if (w.monaco) return Promise.resolve(w.monaco);
    if (monacoPromise) return monacoPromise;
    const script = document.createElement('script');
    monacoPromise = new Promise((resolve, reject) => {
      script.src = '/monaco/vs/loader.js';
      script.onerror = () => reject(new Error('monaco loader'));
      script.onload = () => {
        w.require.config({ paths: { vs: '/monaco/vs' } });
        w.require(['vs/editor/editor.main'], () => resolve(w.monaco), reject);
      };
      document.head.appendChild(script);
    }).catch((e) => {
      monacoPromise = null;
      script.remove();
      throw e;
    });
    return monacoPromise;
  }
</script>

<script lang="ts">
  import { tick, onMount } from 'svelte';
  import { api, apiText, apiPutRaw, ApiError, bytes } from '$lib/api';
  import { notify, theme } from '$lib/state.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';
  import Confirm, { type Ask } from './Confirm.svelte';
  import { t, tn, lang } from '$lib/i18n/index.svelte';

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
  let monacoBox = $state<HTMLDivElement | null>(null);
  // Редактор VS Code грузится лениво: 4,7 МБ статики нужны только тому, кто
  // открыл файл. Если не загрузится — остаётся простое поле ввода.
  let editor: any = null;
  let monacoFailed = $state(false);
  let editorReady = $state(false);
  const dirty = $derived(editing !== null && text !== original);
  const lines = $derived(text.split('\n').length);

  // Диалог одного поля: отдельные переменные, а не объект — привязка к
  // свойству объекта, который целиком переприсваивается, значение теряет.
  let askOpen = $state(false);
  let askTitle = $state('');
  let askLabel = $state('');
  let askValue = $state('');
  let askRun: (v: string) => Promise<void> = async () => {};

  // Подтверждения — тем же модальным окном, что и в остальной панели.
  let ask = $state<Ask | null>(null);

  function askConfirm(title: string, text: string, label: string, run: () => void) {
    ask = { title, note: text, action: label, danger: true, run };
  }

  // Режимы подсветки, которые лежат в web/static/monaco; всё остальное —
  // обычный текст.
  const languages: Record<string, string> = {
    php: 'php', phtml: 'php', inc: 'php',
    html: 'html', htm: 'html', twig: 'html', tpl: 'html', vue: 'html',
    css: 'css', scss: 'css', less: 'css',
    js: 'javascript', mjs: 'javascript', cjs: 'javascript',
    ts: 'typescript', json: 'json', map: 'json', lock: 'json',
    xml: 'xml', svg: 'xml', xsl: 'xml',
    yml: 'yaml', yaml: 'yaml',
    md: 'markdown', markdown: 'markdown',
    sql: 'sql', sh: 'shell', bash: 'shell', zsh: 'shell', py: 'python',
    ini: 'ini', conf: 'ini', cfg: 'ini', env: 'ini', htaccess: 'ini', htpasswd: 'ini',
    dockerfile: 'dockerfile'
  };
  function languageFor(name: string): string {
    const lower = name.toLowerCase();
    if (lower === 'dockerfile') return 'dockerfile';
    const ext = lower.includes('.') ? lower.slice(lower.lastIndexOf('.') + 1) : lower.replace(/^\./, '');
    return languages[ext] ?? 'plaintext';
  }
  const darkTheme = () =>
    theme.mode === 'dark' ||
    (theme.mode === 'system' && typeof matchMedia === 'function' && matchMedia('(prefers-color-scheme: dark)').matches);

  // Пока человек выбирает файл, редактор уже едет: с прогретым кэшем
  // браузера первое открытие не ждёт 4,7 МБ.
  onMount(() => {
    const w = window as any;
    const run = () => { if (!monacoFailed) loadMonaco().catch(() => {}); };
    if (w.requestIdleCallback) {
      const id = w.requestIdleCallback(run, { timeout: 2000 });
      return () => w.cancelIdleCallback?.(id);
    }
    const id = setTimeout(run, 300);
    return () => clearTimeout(id);
  });

  async function mountEditor(name: string) {
    if (monacoFailed) return;
    await tick();
    if (!monacoBox) return;
    try {
      const monaco = await loadMonaco();
      editor?.dispose();
      editor = monaco.editor.create(monacoBox, {
        value: text,
        language: languageFor(name),
        theme: darkTheme() ? 'vs-dark' : 'vs',
        automaticLayout: true,
        minimap: { enabled: false },
        fontSize: 13,
        tabSize: 2,
        renderWhitespace: 'selection',
        scrollBeyondLastLine: false,
        fixedOverflowWidgets: true
      });
      editor.onDidChangeModelContent(() => (text = editor.getValue()));
      // Высота под содержимое: короткий .htaccess не должен занимать пол-экрана.
      editor.onDidContentSizeChange(() => {
        if (!monacoBox) return;
        const fit = Math.min(Math.max(editor.getContentHeight() + 24, 240), window.innerHeight * 0.6);
        monacoBox.style.height = fit + 'px';
      });
      editor.focus();
      editorReady = true;
    } catch (e) {
      monacoFailed = true;
      notify(t('files.monacoFailed'), 'err');
    }
  }

  // Тема редактора следует за темой панели.
  $effect(() => {
    const dark = darkTheme();
    const w = window as any;
    if (editor && w.monaco) w.monaco.editor.setTheme(dark ? 'vs-dark' : 'vs');
  });

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
        (a.type === 'dir' ? 0 : 1) - (b.type === 'dir' ? 0 : 1) || a.name.localeCompare(b.name, lang.locale)
      );
    } catch (e) {
      entries = [];
      fail(e);
    } finally {
      loading = false;
    }
  }

  $effect(() => { user; cwd; load(); });

  function closeEditor() {
    editor?.dispose();
    editor = null;
    editorReady = false;
    editing = null;
    text = original = '';
  }

  // Уход из редактора с несохранёнными правками требует подтверждения.
  function leaveEditor(then?: () => void): boolean {
    if (dirty) {
      askConfirm(t('files.unsavedTitle'), t('files.unsavedNote', { name: editing ?? '' }), t('files.discard'), () => {
        closeEditor();
        then?.();
      });
      return false;
    }
    closeEditor();
    then?.();
    return true;
  }

  function go(path: string) {
    leaveEditor(() => (cwd = clean(path)));
  }

  async function open(e: Entry) {
    if (e.type === 'dir') { go(join(cwd, e.name)); return; }
    if (dirty) { leaveEditor(() => open(e)); return; }
    if (binaryExt.test(e.name)) { notify(t('files.binaryFile'), 'err'); return; }
    if (e.size > editLimit) { notify(t('files.tooLarge', { size: bytes(editLimit) }), 'err'); return; }
    closeEditor();
    try {
      const body = await apiText(`/files/content?${q(join(cwd, e.name))}`);
      // Нулевой байт — верный признак, что это не текст.
      if (body.indexOf('\u0000') >= 0) { notify(t('files.binaryFile'), 'err'); return; }
      original = text = body;
      editing = e.name;
      mountEditor(e.name);
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
      notify(t('files.saved', { name: editing }));
      await load();
    } catch (e) { fail(e); } finally { saving = false; }
  }

  // Ctrl+S ловится на окне: внутри Monaco свои привязки, и до textarea-обработчика
  // событие не доходит.
  function windowKeys(e: KeyboardEvent) {
    if (editing !== null && (e.ctrlKey || e.metaKey) && (e.code === 'KeyS' || e.key === 's')) {
      e.preventDefault();
      save();
    }
  }

  function editorKeys(e: KeyboardEvent) {
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

  const mkdir = () => request(t('files.newFolder'), t('files.nameLabel'), '', (v) => op({ op: 'mkdir', path: join(cwd, v) }, t('files.folderCreated', { name: v })));
  const touch = () => request(t('files.newFile'), t('files.nameLabel'), '', (v) => op({ op: 'touch', path: join(cwd, v) }, t('files.fileCreated', { name: v })));
  const rename = (e: Entry) => request(t('files.rename'), t('files.renameLabel'), e.name, (v) => {
    if (editing === e.name) closeEditor();
    return op({ op: 'mv', path: join(cwd, e.name), dest: v.includes('/') ? clean(v) : join(cwd, v) }, t('files.renamed'));
  });
  const chmod = (e: Entry) => request(t('files.permsTitle', { name: e.name }), t('files.permsLabel'), modeOctal(e.mode), (v) =>
    op({ op: 'chmod', path: join(cwd, e.name), mode: v }, t('files.permsChanged')));
  const extract = (e: Entry) => request(t('files.extractTitle', { name: e.name }), t('files.extractLabel'), cwd, (v) =>
    op({ op: 'extract', path: join(cwd, e.name), dest: clean(v) }, t('files.extracted')));

  function remove(names: string[]) {
    if (!names.length) return;
    const what = names.length === 1 ? names[0] : tn('files.items', names.length);
    askConfirm(t('files.deleteTitle'), t('files.deleteNote', { what }), t('common.delete'), () => {
      // Открытый в редакторе файл после удаления показывать нечего.
      if (editing !== null && names.includes(editing)) closeEditor();
      const paths = names.map((n) => join(cwd, n));
      op({ op: 'rm', path: paths[0], paths: paths.slice(1) }, t('files.deleted'));
    });
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
        notify(t('files.uploaded', { name: f.name, size: bytes(f.size) }));
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

<svelte:window onkeydown={windowKeys} />

<div class="card p-0 overflow-hidden">
  <div class="flex flex-wrap items-center gap-2 p-3 border-b border-line bg-surface-2">
    <div class="flex items-center gap-1 text-sm min-w-0 w-full sm:w-auto sm:flex-1 flex-wrap">
      <button class="btn btn-ghost btn-sm" onclick={() => go('/')} title={t('files.homeDir')}><Icon name="home" size={14} /></button>
      {#if cwd !== '/'}<button class="btn btn-ghost btn-sm" onclick={() => go(parent(cwd))} title={t('files.up')}>..</button>{/if}
      <span class="text-muted font-mono text-xs">/var/www/{user}</span>
      {#each crumbs as c}
        <span class="text-muted">/</span>
        <button class="font-mono text-xs hover:text-accent-ink transition-colors" onclick={() => go(c.path)}>{c.name}</button>
      {/each}
    </div>
    {#if sites.length}
      <select
        class="input py-1 text-xs w-full sm:w-auto"
        onchange={(e) => {
          const el = e.currentTarget as HTMLSelectElement;
          if (el.value) go(el.value);
          el.value = '';
        }}
      >
        <option value="">{t('files.goToSite')}</option>
        {#each sites as s}<option value={'/data/www/' + s.domain + (s.docroot ? '/' + s.docroot : '')}>{s.domain}</option>{/each}
      </select>
    {/if}
    <div class="flex gap-1">
      <button class="btn btn-sm" onclick={mkdir}><Icon name="plus" size={13} /> {t('files.folderBtn')}</button>
      <button class="btn btn-sm" onclick={touch}><Icon name="file" size={13} /> {t('files.fileBtn')}</button>
      <button class="btn btn-sm" onclick={pick}><Icon name="archive" size={13} /> {t('files.upload')}</button>
      <button class="btn btn-ghost btn-sm" onclick={load} title={t('files.refresh')}><Icon name="refresh" size={14} /></button>
    </div>
  </div>

  {#if selected.length}
    <div class="flex items-center gap-3 px-3 py-2 border-b border-line bg-accent-soft text-sm">
      <span>{t('files.selectedCount', { n: selected.length })}</span>
      <button class="btn btn-danger btn-sm" onclick={() => remove(selected)}><Icon name="trash" size={13} /> {t('files.deleteBtn')}</button>
      <button class="btn btn-ghost btn-sm" onclick={() => (selected = [])}>{t('files.deselect')}</button>
    </div>
  {/if}

  {#if error}<p class="text-danger text-sm px-3 py-2 flex items-center gap-1.5"><Icon name="alert" size={15} /> {error}</p>{/if}

  <div
    class="relative {dragging ? 'ring-2 ring-accent ring-inset' : ''}"
    role="region"
    aria-label={t('files.regionLabel')}
    ondragover={(e) => { e.preventDefault(); dragging = true; }}
    ondragleave={() => (dragging = false)}
    ondrop={drop}
  >
    {#if loading && !entries.length}
      <p class="text-sm text-muted p-4">{t('common.loading')}</p>
    {:else if !entries.length}
      <p class="text-sm text-muted p-6 text-center">{t('files.empty')}</p>
    {:else}
      <table class="tbl">
        <thead>
          <tr>
            <th class="w-8"></th>
            <th>{t('common.name')}</th>
            <th class="text-right w-24">{t('files.colSize')}</th>
            <th class="w-28 hidden sm:table-cell">{t('files.colMode')}</th>
            <th class="w-40 hidden md:table-cell">{t('files.colModified')}</th>
            <th class="w-40"></th>
          </tr>
        </thead>
        <tbody>
          {#each entries as e (e.name)}
            <tr class={editing === e.name ? 'bg-accent-soft' : ''}>
              <td data-label="" class="text-center">
                <input type="checkbox" checked={selected.includes(e.name)} onchange={() => toggle(e.name)} aria-label={t('files.selectEntry', { name: e.name })} />
              </td>
              <td data-label={t('common.name')}>
                <button class="inline-flex items-center gap-2 text-left max-w-full" onclick={() => open(e)}>
                  <Icon name={icon(e)} size={15} class="shrink-0 text-muted" />
                  <span class="truncate {e.type === 'dir' ? 'font-medium' : ''}">{e.name}</span>
                  {#if e.type === 'link'}<span class="text-xs text-muted">→ {e.target}</span>{/if}
                </button>
              </td>
              <td data-label={t('files.colSize')} class="text-right text-muted tabular-nums whitespace-nowrap">{e.type === 'dir' ? '—' : bytes(e.size)}</td>
              <td data-label={t('files.colMode')} class="font-mono text-xs text-muted hidden sm:table-cell">{e.mode}</td>
              <td data-label={t('files.colModified')} class="text-muted text-xs hidden md:table-cell whitespace-nowrap">{(e.mtime || '').replace('T', ' ').slice(0, 16)}</td>
              <td data-label="" class="text-right whitespace-nowrap">
                {#if e.type !== 'dir'}
                  <button class="btn btn-ghost btn-sm" onclick={() => download(e)} title={t('files.download')}><Icon name="download" size={13} /></button>
                  {#if archiveExt.test(e.name)}<button class="btn btn-ghost btn-sm" onclick={() => extract(e)} title={t('files.extractBtn')}><Icon name="archive" size={13} /></button>{/if}
                {/if}
                <button class="btn btn-ghost btn-sm" onclick={() => rename(e)} title={t('files.renameBtn')}><Icon name="pencil" size={13} /></button>
                <button class="btn btn-ghost btn-sm" onclick={() => chmod(e)} title={t('files.permsBtn')}><Icon name="lock" size={13} /></button>
                <button class="btn btn-ghost btn-sm text-danger" onclick={() => remove([e.name])} title={t('files.deleteBtn')}><Icon name="trash" size={13} /></button>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>
</div>

{#if editing !== null}
  <div class="card no-cut p-0 mt-4 overflow-hidden">
    <div class="flex flex-wrap items-center gap-2 p-3 border-b border-line bg-surface-2">
      <Icon name="code" size={15} class="text-muted" />
      <span class="font-mono text-sm truncate">{join(cwd, editing)}</span>
      {#if dirty}<span class="tag">{t('files.unsavedTag')}</span>{/if}
      <span class="text-xs text-muted ml-auto">{tn('files.lines', lines)} · {bytes(new Blob([text]).size)}</span>
      <button class="btn btn-primary btn-sm" disabled={!dirty || saving} onclick={save}>
        <Icon name="save" size={13} /> {saving ? t('files.saving') : t('common.save')}
      </button>
      <button class="btn btn-sm" onclick={() => leaveEditor()}>{t('common.close')}</button>
    </div>
    {#if monacoFailed}
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
          aria-label={t('files.contentLabel')}
        ></textarea>
      </div>
    {:else}
      {#if !editorReady}<div class="px-3 pt-2 text-xs text-muted">{t('files.editorLoading')}</div>{/if}
      <div bind:this={monacoBox} style="height:240px" aria-label={t('files.contentLabel')}></div>
    {/if}
    <div class="px-3 py-2 border-t border-line text-xs text-muted">
      {monacoFailed ? t('files.hintSave') : t('files.hintSaveF1')} {t('files.hintWrites', { user })}
    </div>
  </div>
{/if}

<Confirm bind:ask />

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
    <button class="btn" onclick={() => (askOpen = false)}>{t('common.cancel')}</button>
    <button class="btn btn-primary" form="fm-ask" type="submit">{t('files.done')}</button>
  {/snippet}
</Modal>
