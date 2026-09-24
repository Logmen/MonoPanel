// Сборка сайта в dist/: public/ копируется как есть, документация собирается из README.md
// и docs/*.md репозитория в оформлении панели, иконки берутся из Icon.svelte панели.
// Cloudflare запускает её сам (npm run build) перед wrangler deploy.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { Marked } from 'marked';

const SITE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(SITE, '..');
const OUT = path.join(SITE, 'dist');
const REPO = 'https://github.com/Logmen/MonoPanel';

// Группы меню документации, по порядку.
const GROUPS = [
  ['start', 'Начало'],
  ['guide', 'Руководство'],
  ['inside', 'Устройство'],
  ['more', 'Ещё']
];

// Страницы — разделы README (по заголовку второго уровня) и файлы docs/. Раздел или
// файл без записи здесь всё равно попадёт на сайт, в группу «Ещё»; skip — разделы,
// которые на сайте заменяют меню и подвал. Описание файла docs/ берётся из таблицы
// «Документация» в README, если его нет здесь.
const README_SECTIONS = {
  'Установка': { slug: 'install', nav: 'Установка', icon: 'download', group: 'start', desc: 'Скрипт установки, пакеты .deb и .rpm, требования к серверу.' },
  'Быстрый старт': { slug: 'quickstart', nav: 'Быстрый старт', icon: 'play', group: 'start', desc: 'От mp setup до сайта с базой, почты, бэкапов и переезда — командами CLI.' },
  'Что умеет': { slug: 'features', nav: 'Возможности', icon: 'tasks', group: 'start', desc: 'Все области панели: команды, как они устроены и чего пока нет.' },
  'Обновление панели': { slug: 'update', nav: 'Обновление', icon: 'refresh', group: 'start', desc: 'Релиз тегом, подпись ed25519, обновление с откатом на прежний бинарник.' },
  'Переезд': { slug: 'migrate', nav: 'Переезд', icon: 'users', group: 'guide', desc: 'Аккаунт целиком — с другой MonoPanel, BitrixVM или FASTPANEL.' },
  'Как устроено': { slug: 'how', nav: 'Как устроено', icon: 'cpu', group: 'inside', desc: 'Один бинарник в четырёх ролях и конвейер применения конфигов.' },
  'Стек': { slug: 'stack', nav: 'Стек', icon: 'code', group: 'inside', desc: 'Из чего собрана панель: Go, SQLite, SvelteKit, lego, nfpm.' },
  'Разработка': { slug: 'development', nav: 'Разработка', icon: 'pencil', group: 'inside', desc: 'Сборка, тесты, e2e, CI и структура репозитория.' },
  'Документация': { skip: true },
  'Безопасность': { skip: true },
  'Лицензия': { skip: true }
};
const DOC_FILES = {
  '01-architecture.md': { slug: 'architecture', nav: 'Архитектура', icon: 'settings', group: 'inside' },
  '02-platform-matrix.md': { slug: 'platforms', nav: 'Платформы', icon: 'box', group: 'guide' },
  '03-web-stack.md': { slug: 'web-stack', nav: 'Веб-стек', icon: 'globe', group: 'guide' },
  '04-cli-tui-api.md': { slug: 'cli-api', nav: 'CLI, TUI и API', icon: 'terminal', group: 'guide' },
  '05-roadmap.md': { slug: 'roadmap', nav: 'Дорожная карта', icon: 'clock', group: 'inside' },
  '06-mail.md': { slug: 'mail', nav: 'Почта', icon: 'mail', group: 'guide' },
  '07-migration.md': { slug: 'migration', nav: 'Перенос между панелями', icon: 'file', group: 'inside' },
  '08-testbed.md': { slug: 'testbed', nav: 'Тестовая площадка', icon: 'check', group: 'inside' },
  '09-web-ui.md': { slug: 'web-ui', nav: 'Web UI', icon: 'monitor', group: 'start' }
};
// Порядок внутри групп; чего нет в списке — следом, в порядке исходников.
const ORDER = ['install', 'quickstart', 'web-ui', 'features', 'update', 'web-stack', 'mail', 'migrate', 'cli-api', 'platforms', 'how', 'architecture', 'migration', 'stack', 'roadmap', 'testbed', 'development'];

const read = (f) => fs.readFileSync(path.join(ROOT, f), 'utf8');
const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const icon = (name, cls = 'ic') => `<svg class="${cls}" aria-hidden="true"><use href="/assets/icons.svg#i-${name}"/></svg>`;
const plain = (tokens = []) => tokens.map((t) => (t.tokens ? plain(t.tokens) : (t.text ?? ''))).join('');

// Якоря как у GitHub: ссылки вида 07-migration.md#6-чужие-панели… из markdown
// должны вести в то же место и на сайте.
function slugger() {
  const seen = new Map();
  return (text) => {
    const base = text.toLowerCase().replace(/[^\p{L}\p{M}\p{N}\p{Pc}\- ]/gu, '').replace(/ /g, '-');
    let slug = base;
    while (seen.has(slug)) {
      const n = seen.get(base) + 1;
      seen.set(base, n);
      slug = `${base}-${n}`;
    }
    seen.set(slug, 0);
    return slug;
  };
}

const TRANSLIT = { а: 'a', б: 'b', в: 'v', г: 'g', д: 'd', е: 'e', ё: 'e', ж: 'zh', з: 'z', и: 'i', й: 'y', к: 'k', л: 'l', м: 'm', н: 'n', о: 'o', п: 'p', р: 'r', с: 's', т: 't', у: 'u', ф: 'f', х: 'h', ц: 'c', ч: 'ch', ш: 'sh', щ: 'sch', ы: 'y', э: 'e', ю: 'yu', я: 'ya' };
const urlSlug = (s) => [...s.toLowerCase()].map((c) => TRANSLIT[c] ?? c).join('').replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');

const lexer = new Marked({ gfm: true });
function lex(file) {
  const tokens = lexer.lexer(read(file));
  const slug = slugger();
  for (const t of tokens) {
    if (t.type !== 'heading') continue;
    t.plain = plain(t.tokens).trim();
    t.id = slug(t.plain);
  }
  return tokens;
}

// ── Страницы ────────────────────────────────────────────────────────────────
const pages = [];
const readme = lex('README.md');
const intro = [];
let section = null;
for (const t of readme) {
  if (t.type === 'heading' && t.depth === 2) {
    const conf = README_SECTIONS[t.plain] ?? { slug: urlSlug(t.plain), nav: t.plain, icon: 'file', group: 'more' };
    section = { ...conf, title: t.plain, head: t, source: 'README.md', shift: 1, tokens: [] };
    if (!conf.skip) pages.push(section);
    continue;
  }
  (section ? section.tokens : intro).push(t);
}

// Описания файлов docs/ — из таблицы «Документация» в README.
const docDesc = {};
for (const m of read('README.md').matchAll(/^\|\s*\[docs\/([^\]]+)\]\([^)]*\)\s*\|\s*(.+?)\s*\|\s*$/gm)) docDesc[m[1]] = m[2];

for (const f of fs.readdirSync(path.join(ROOT, 'docs')).filter((f) => f.endsWith('.md')).sort()) {
  const tokens = lex('docs/' + f);
  const h1 = tokens.findIndex((t) => t.type === 'heading' && t.depth === 1);
  const head = tokens[h1];
  const title = head ? head.plain.replace(/^\d+\.\s*/, '') : f;
  const conf = DOC_FILES[f] ?? { slug: urlSlug(f.replace(/^\d+-|\.md$/g, '')), nav: title, icon: 'file', group: 'more' };
  pages.push({ desc: docDesc[f] ?? '', ...conf, title, head, source: 'docs/' + f, shift: 0, tokens: tokens.filter((_, i) => i !== h1) });
}

const groupIndex = (p) => GROUPS.findIndex(([g]) => g === p.group);
const orderIndex = (p) => (ORDER.includes(p.slug) ? ORDER.indexOf(p.slug) : ORDER.length);
pages.forEach((p, i) => { p.url = `/docs/${p.slug}/`; p.i = i; });
pages.sort((a, b) => groupIndex(a) - groupIndex(b) || orderIndex(a) - orderIndex(b) || a.i - b.i);

// Куда ведут якоря каждого исходника: файл целиком — на свою страницу (README — на
// обзор документации), заголовок — на страницу, где он оказался.
const anchors = new Map();
const mainUrl = new Map([['README.md', '/docs/']]);
for (const p of pages) {
  if (!anchors.has(p.source)) anchors.set(p.source, new Map());
  const map = anchors.get(p.source);
  if (p.head) map.set(p.head.id, `${p.url}#${p.head.id}`);
  for (const t of p.tokens) if (t.type === 'heading') map.set(t.id, `${p.url}#${t.id}`);
  if (!mainUrl.has(p.source)) mainUrl.set(p.source, p.url);
}

function decode(s) {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}

// Ссылка из markdown → адрес на сайте: страницы документации и их якоря — сюда,
// картинки из docs/img — рядом, остальные файлы репозитория — на GitHub.
function resolve(href, page) {
  if (!href || /^[a-z][a-z0-9+.-]*:/i.test(href)) return href;
  const cut = href.indexOf('#');
  const file = cut < 0 ? href : href.slice(0, cut);
  const hash = cut < 0 ? '' : decode(href.slice(cut + 1));
  if (!file) {
    const url = anchors.get(page.source)?.get(hash);
    return url && !url.startsWith(page.url + '#') ? url : `#${hash}`;
  }
  const target = path.posix.normalize(path.posix.join(path.posix.dirname(page.source), decode(file)));
  if (target.startsWith('docs/img/')) return '/' + target;
  if (mainUrl.has(target)) {
    if (!hash) return mainUrl.get(target);
    return anchors.get(target)?.get(hash) ?? `${REPO}/blob/main/${target}#${hash}`;
  }
  return `${REPO}/blob/main/${target}${hash ? '#' + hash : ''}`;
}

// ── Картинки ────────────────────────────────────────────────────────────────
function imageSize(file) {
  if (!fs.existsSync(file)) return null;
  const b = fs.readFileSync(file);
  if (b.toString('ascii', 1, 4) === 'PNG') return { w: b.readUInt32BE(16), h: b.readUInt32BE(20) };
  if (b.toString('ascii', 0, 4) === 'RIFF' && b.toString('ascii', 8, 12) === 'WEBP') {
    const chunk = b.toString('ascii', 12, 16);
    if (chunk === 'VP8X') return { w: 1 + b.readUIntLE(24, 3), h: 1 + b.readUIntLE(27, 3) };
    if (chunk === 'VP8 ') return { w: b.readUInt16LE(26) & 0x3fff, h: b.readUInt16LE(28) & 0x3fff };
    if (chunk === 'VP8L') {
      const bits = b.readUInt32LE(21);
      return { w: 1 + (bits & 0x3fff), h: 1 + ((bits >> 14) & 0x3fff) };
    }
  }
  return null;
}

// Скриншот из docs/img: рядом может лежать тёмная версия (x.dark.webp) — тогда
// показывается та, что совпадает с темой сайта. На GitHub видна светлая.
function shot(url, alt) {
  const variant = (u, cls) => {
    const d = imageSize(path.join(ROOT, u.slice(1)));
    const size = d ? ` width="${d.w}" height="${d.h}"` : '';
    return `<a class="shot-frame${cls}" href="${esc(u)}"><img src="${esc(u)}" alt="${esc(alt)}"${size} loading="lazy" decoding="async"></a>`;
  };
  const dark = url.replace(/(\.\w+)$/, '.dark$1');
  const hasDark = fs.existsSync(path.join(ROOT, dark.slice(1)));
  const imgs = hasDark ? variant(url, ' only-light') + variant(dark, ' only-dark') : variant(url, '');
  return `<figure class="shot">${imgs}${alt ? `<figcaption>${esc(alt)}</figcaption>` : ''}</figure>\n`;
}

// ── Markdown → HTML в стиле панели ──────────────────────────────────────────
const COMMENTED = new Set(['', 'bash', 'sh', 'shell', 'console', 'nginx', 'ini', 'yaml', 'yml', 'conf', 'text', 'toml', 'env', 'apache', 'systemd']);
function highlight(text, lang) {
  const html = esc(text);
  return COMMENTED.has(lang) ? html.replace(/(^|[ \t])(#(?!!).*)$/gm, '$1<span class="c">$2</span>') : html;
}
const align = (a) => (a ? ` style="text-align:${a}"` : '');

let current; // страница, которую сейчас рендерим: от неё считаются относительные ссылки
const md = new Marked({ gfm: true });
md.use({
  renderer: {
    heading(token) {
      const level = Math.min(6, Math.max(2, token.depth - current.shift));
      if (level <= 3) current.toc.push({ level, id: token.id, text: token.plain });
      const id = esc(token.id);
      return `<h${level} id="${id}">${this.parser.parseInline(token.tokens)}<a class="anchor" href="#${id}" aria-hidden="true" tabindex="-1">#</a></h${level}>\n`;
    },
    paragraph({ tokens }) {
      const content = tokens.filter((t) => !(t.type === 'text' && !t.text.trim()));
      if (content.length === 1 && content[0].type === 'image') return this.parser.parseInline(content);
      return `<p>${this.parser.parseInline(tokens)}</p>\n`;
    },
    code({ text, lang }) {
      const l = (lang || '').split(/\s/)[0].toLowerCase();
      return `<div class="codeblock"><pre class="code"><code>${highlight(text, l)}</code></pre><button class="btn btn-ghost btn-sm code-copy" type="button" title="Скопировать" aria-label="Скопировать">${icon('copy')}</button></div>\n`;
    },
    blockquote({ tokens }) {
      return `<blockquote class="note">${this.parser.parse(tokens)}</blockquote>\n`;
    },
    table(token) {
      const labels = token.header.map((c) => esc(plain(c.tokens)));
      const head = token.header.map((c, i) => `<th${align(token.align[i])}>${this.parser.parseInline(c.tokens)}</th>`).join('');
      const rows = token.rows
        .map((r) => `<tr>${r.map((c, i) => `<td data-label="${labels[i]}"${align(token.align[i])}><span>${this.parser.parseInline(c.tokens)}</span></td>`).join('')}</tr>`)
        .join('\n');
      return `<div class="card tblcard"><table class="tbl"><thead><tr>${head}</tr></thead><tbody>\n${rows}\n</tbody></table></div>\n`;
    },
    link({ href, title, tokens }) {
      const url = resolve(href, current);
      return `<a href="${esc(url)}"${title ? ` title="${esc(title)}"` : ''}>${this.parser.parseInline(tokens)}</a>`;
    },
    image({ href, text }) {
      const url = resolve(href, current);
      return url.startsWith('/docs/img/') ? shot(url, text) : `<img src="${esc(url)}" alt="${esc(text)}" loading="lazy">`;
    },
    checkbox({ checked }) {
      return `<span class="task${checked ? ' task-on' : ''}" role="img" aria-label="${checked ? 'сделано' : 'не сделано'}"></span> `;
    }
  }
});

function render(page, tokens) {
  current = page;
  page.toc = [];
  const out = md.parser(Object.assign([...tokens], { links: {} }));
  current = null;
  return out;
}

// ── Каркас страниц ──────────────────────────────────────────────────────────
const MENU = [
  ['/#features', 'Возможности'],
  ['/#how', 'Как устроено'],
  ['/#os', 'Где работает'],
  ['/docs/', 'Документация']
];

function layout({ title, desc, main, active, toc = '' }) {
  return `<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>${esc(title)}</title>
<meta name="description" content="${esc(desc)}">
<meta name="theme-color" content="#eef3f8" media="(prefers-color-scheme: light)">
<meta name="theme-color" content="#05070c" media="(prefers-color-scheme: dark)">
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<link rel="stylesheet" href="/assets/fonts.css">
<link rel="stylesheet" href="/assets/site.css">
<script>try{var t=localStorage.getItem("theme");if(t==="dark"||t==="light")document.documentElement.dataset.theme=t}catch(e){}</script>
<script src="/assets/site.js" defer></script>
</head>
<body class="wide">
<a class="skip" href="#doc">К содержимому</a>
<div class="glow"></div>
<header class="top">
  <div class="wrap">
    <a class="logo" href="/" aria-label="MonoPanel — на главную"><span class="logo-mark" aria-hidden="true">M</span><span class="brand">MonoPanel</span></a>
    <nav class="menu" aria-label="Разделы">
      ${MENU.map(([href, label]) => `<a class="tab${href === '/docs/' ? ' tab-on' : ''}" href="${href}">${label}</a>`).join('\n      ')}
    </nav>
    <div class="top-end">
      <div class="seg" role="group" aria-label="Тема">
        <button type="button" data-theme-mode="system" title="Как в системе" aria-label="Тема как в системе">${icon('monitor')}</button>
        <button type="button" data-theme-mode="light" title="Светлая" aria-label="Светлая тема">${icon('sun')}</button>
        <button type="button" data-theme-mode="dark" title="Тёмная" aria-label="Тёмная тема">${icon('moon')}</button>
      </div>
      <a class="btn" href="${REPO}">GitHub ${icon('external')}</a>
    </div>
  </div>
</header>
<div class="wrap docs${toc ? '' : ' no-toc'}">
  <aside class="side" aria-label="Документация">${sidebar(active)}</aside>
  ${main}
  ${toc}
</div>
<footer class="foot">
  <div class="wrap">
    <a class="logo" href="/" aria-label="MonoPanel — на главную"><span class="logo-mark" aria-hidden="true">M</span><span class="brand">MonoPanel</span></a>
    <nav aria-label="Ссылки">
      <a href="${REPO}">GitHub</a>
      <a href="${REPO}/releases">Релизы</a>
      <a href="/docs/">Документация</a>
      <a href="${REPO}/blob/main/SECURITY.md">Безопасность</a>
      <a href="${REPO}/blob/main/README.en.md">English</a>
      <a href="${REPO}/blob/main/LICENSE">Apache-2.0</a>
    </nav>
  </div>
</footer>
<div class="toast" id="toast" role="status" aria-live="polite"></div>
</body>
</html>
`;
}

function navLinks(active) {
  const item = (url, label, ic) =>
    `<a class="nav-item${url === active ? ' nav-on' : ''}" href="${url}"${url === active ? ' aria-current="page"' : ''}>${icon(ic)}<span>${esc(label)}</span></a>`;
  let html = item('/docs/', 'Обзор', 'home');
  for (const [g, label] of GROUPS) {
    const list = pages.filter((p) => p.group === g);
    if (list.length) html += `<div class="side-group"><span class="label">${label}</span>${list.map((p) => item(p.url, p.nav, p.icon)).join('')}</div>`;
  }
  return html;
}

function sidebar(active) {
  return `<nav class="side-nav">${navLinks(active)}</nav>`;
}

// На телефоне боковое меню прячется, вместо него — раскрывающийся список над текстом.
function mobileNav(active, label) {
  return `<details class="side-m card"><summary><span class="label">Раздел</span><span class="side-m-t">${esc(label)}</span>${icon('chevron', 'ic side-m-ic')}</summary><nav class="side-nav">${navLinks(active)}</nav></details>`;
}

function tocHtml(page) {
  if (page.toc.length < 2) return '';
  return `<nav class="toc" aria-label="На странице"><span class="label">На странице</span>${page.toc
    .map((h) => `<a class="toc-${h.level}" href="#${esc(h.id)}">${esc(h.text)}</a>`)
    .join('')}</nav>`;
}

function pager(page) {
  const i = pages.indexOf(page);
  const card = (p, dir) =>
    p ? `<a class="card pager-${dir}" href="${p.url}"><span class="label">${dir === 'prev' ? 'Назад' : 'Дальше'}</span><span class="pager-t">${esc(p.nav)}</span></a>` : '<span></span>';
  return `<nav class="pager" aria-label="Соседние страницы">${card(pages[i - 1], 'prev')}${card(pages[i + 1], 'next')}</nav>`;
}

// ── Иконки ──────────────────────────────────────────────────────────────────
// Те же пути, что в web/src/lib/components/Icon.svelte, плюс то, чего в панели нет.
function iconSprite() {
  const src = read('web/src/lib/components/Icon.svelte');
  const found = [...src.matchAll(/^\s*([a-z]+):\s*'([^']+)'/gm)].map((m) => [m[1], m[2]]);
  if (found.length < 20) throw new Error('Icon.svelte: не нашёл пути иконок — поменялся формат файла?');
  const extra = [['copy', 'M9 9h12v12H9zM15 9V3H3v12h6']];
  const symbols = [...found, ...extra].map(([n, d]) => `<symbol id="i-${n}" viewBox="0 0 24 24"><path d="${d}"/></symbol>`);
  return `<svg xmlns="http://www.w3.org/2000/svg">${symbols.join('')}</svg>\n`;
}

// ── Сборка ──────────────────────────────────────────────────────────────────
function write(rel, content) {
  const file = path.join(OUT, rel);
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, content);
}

fs.rmSync(OUT, { recursive: true, force: true });
fs.cpSync(path.join(SITE, 'public'), OUT, { recursive: true });
write('assets/icons.svg', iconSprite());
if (fs.existsSync(path.join(ROOT, 'docs/img'))) fs.cpSync(path.join(ROOT, 'docs/img'), path.join(OUT, 'docs/img'), { recursive: true });

for (const page of pages) {
  const body = render(page, page.tokens);
  const group = GROUPS.find(([g]) => g === page.group)[1];
  const main = `<main class="doc" id="doc">
    ${mobileNav(page.url, page.nav)}
    <div class="crumbs"><a href="/docs/">Документация</a>${icon('chevron')}<span>${group}</span></div>
    <h1 class="page-title"${page.head ? ` id="${esc(page.head.id)}"` : ''}>${esc(page.title)}</h1>
    <div class="prose">${body}</div>
    ${pager(page)}
    <a class="edit" href="${REPO}/blob/main/${page.source}${page.source === 'README.md' ? '#' + page.head.id : ''}">${icon('pencil')}Исходник страницы на GitHub</a>
  </main>`;
  write(`docs/${page.slug}/index.html`, layout({ title: `${page.title} — документация MonoPanel`, desc: page.desc || page.title, main, active: page.url, toc: tocHtml(page) }));
}

// Обзор: вступление README и карточки разделов.
const home = { source: 'README.md', url: '/docs/', shift: 1 };
const introHtml = render(
  home,
  intro.filter((t) => t.type === 'paragraph' && !/^\[!\[|^\[English\]|^Состояние:/.test(t.raw)).slice(0, 2)
);
const heroShot = fs.existsSync(path.join(ROOT, 'docs/img/dashboard.webp')) ? shot('/docs/img/dashboard.webp', 'Дашборд панели') : '';
const cards = GROUPS.map(([g, label]) => {
  const list = pages.filter((p) => p.group === g);
  if (!list.length) return '';
  return `<section class="home-group"><h2 class="label">${label}</h2><div class="grid grid-3">${list
    .map((p) => `<a class="card feat doc-card" href="${p.url}"><div class="feat-h"><span class="feat-ic">${icon(p.icon)}</span><h3>${esc(p.nav)}</h3></div><p>${esc(p.desc)}</p></a>`)
    .join('')}</div></section>`;
}).join('\n');
write(
  'docs/index.html',
  layout({
    title: 'Документация MonoPanel',
    desc: 'Установка, возможности, веб-стек, почта, переезд, CLI и API панели MonoPanel.',
    active: '/docs/',
    main: `<main class="doc doc-home" id="doc">
    ${mobileNav('/docs/', 'Обзор')}
    <h1 class="page-title">Документация</h1>
    <div class="prose lead-prose">${introHtml}</div>
    ${heroShot}
    ${cards}
  </main>`
  })
);

console.log(`dist: ${pages.length} страниц документации + обзор`);
