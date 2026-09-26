// Сборка сайта в dist/: public/ копируется как есть, документация собирается из README и
// docs/ репозитория в оформлении панели, иконки берутся из Icon.svelte панели. Два языка:
// русская документация (README.md, docs/*.md) — по адресам /docs/…, английская
// (README.en.md, docs/en/*.md) — /en/docs/…; кому какую показывать, решает src/worker.js.
import { execFileSync } from 'node:child_process';
// Cloudflare запускает сборку сам (npm run build) перед wrangler deploy.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { Marked } from 'marked';

const SITE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(SITE, '..');
const OUT = path.join(SITE, 'dist');
const REPO = 'https://github.com/Logmen/MonoPanel';
// Основной адрес сайта: canonical, og:url и sitemap указывают сюда, а не на workers.dev.
const ORIGIN = 'https://monopanel.app';

// Языки: откуда берётся документация, где она живёт на сайте, слова оформления.
const LANGS = {
  ru: {
    prefix: '',
    readme: 'README.md',
    docs: 'docs',
    og: 'ru_RU',
    groups: [['start', 'Начало'], ['guide', 'Руководство'], ['inside', 'Устройство'], ['more', 'Ещё']],
    // Разделы README по заголовку второго уровня; null — раздел, который на сайте заменяют меню и подвал.
    sections: {
      'Установка': ['install', 'Установка', 'Установка одной командой или из пакетов .deb и .rpm: что нужно серверу и откуда панель берёт обновления.'],
      'Быстрый старт': ['quickstart', 'Быстрый старт', 'От mp setup до сайта с базой, почты, бэкапов и переезда с другого сервера — по шагам, командами mp.'],
      'Что умеет': ['features', 'Возможности', 'Что умеет панель по областям: сайты, PHP, базы, TLS, почта, бэкапы, файлы, firewall, переезд, обновления — с командами mp.'],
      'Обновление панели': ['update', 'Обновление', 'Как обновляется панель: релизы с подписью ed25519, установка отдельным юнитом и откат на прежний бинарник, если новая версия не отвечает.'],
      'Переезд': ['migrate', 'Переезд', 'Переезд аккаунта целиком с другой MonoPanel, BitrixVM или FASTPANEL: команды, что приедет и что сделать после переноса.'],
      'Как устроено': ['how', 'Как устроено', 'Как устроена панель: один бинарник в четырёх ролях, состояние в SQLite и конвейер применения конфигов с проверкой и откатом.'],
      'Стек': ['stack', 'Стек', 'Из чего собрана панель: Go, chi и huma, SQLite, Svelte и SvelteKit, Bubble Tea, lego и nfpm.'],
      'Разработка': ['development', 'Разработка', 'Разработка панели: сборка, тесты с фейковым агентом, e2e на живой панели, CI и структура репозитория.'],
      'Документация': null,
      'Безопасность': null,
      'Лицензия': null
    },
    nav: { architecture: 'Архитектура', platforms: 'Платформы', 'web-stack': 'Веб-стек', 'cli-api': 'CLI, TUI и API', roadmap: 'Дорожная карта', mail: 'Почта', migration: 'Перенос между панелями', testbed: 'Тестовая площадка', 'web-ui': 'Web UI' },
    // Описания страниц docs/ для поисковиков и карточек обзора — предложениями, а не списком слов.
    docDesc: {
      '01-architecture.md': 'Как устроена MonoPanel: один бинарник в ролях api, agent и helper, состояние в SQLite, конвейер применения конфигов с проверкой и откатом, безопасность и обновления.',
      '02-platform-matrix.md': 'Поддерживаемые ОС и источники пакетов: nginx.org, Sury и Remi для PHP 5.6–8.5, MySQL и Percona 8.4, SELinux в enforcing, firewall, Valkey на аккаунт.',
      '03-web-stack.md': 'Веб-стек панели: режимы nginx + php-fpm и nginx + Apache, файлы и права, шаблоны конфигов, пул php-fpm на сайт, TLS через lego, логи и установка CMS.',
      '04-cli-tui-api.md': 'Команды mp, меню в терминале и REST API с OpenAPI 3.1: как они связаны, какие есть эндпоинты и как подключить биллинг.',
      '05-roadmap.md': 'Журнал разработки MonoPanel по этапам и датам: что сделано и проверено, что осталось, матрица ОС и риски.',
      '06-mail.md': 'Почтовый сервер MonoPanel: postfix, dovecot и opendkim, путь письма, порты и TLS, нужные DNS-записи, домены «только отправка» и вебпочта Roundcube.',
      '07-migration.md': 'Переезд аккаунта на MonoPanel: с другой MonoPanel по токену, с BitrixVM и FASTPANEL по ssh — что переносится, как идут файлы и пароли, порядок с DNS.',
      '08-testbed.md': 'Тестовая площадка на Proxmox: по машине на каждую из одиннадцати ОС, прогон установки, e2e, CMS и переездов перед релизом и что она нашла.',
      '09-web-ui.md': 'Веб-интерфейс MonoPanel по страницам со скриншотами: дашборд, сайты, PHP, базы, почта, файлы, задачи, firewall, бэкапы, пользователи и настройки.'
    },
    // Абзацы вступления README, которые не нужны на обзоре: значки, язык, состояние.
    introSkip: /^\[!\[|^\[English\]|^Состояние:/,
    t: {
      menu: [['#features', 'Возможности'], ['#how', 'Как устроено'], ['#os', 'Где работает']],
      docs: 'Документация', skip: 'К содержимому', home: 'MonoPanel — на главную', sections: 'Разделы',
      theme: 'Тема', system: ['Как в системе', 'Тема как в системе'], light: ['Светлая', 'Светлая тема'], dark: ['Тёмная', 'Тёмная тема'],
      lang: 'Язык', links: 'Ссылки', releases: 'Релизы', security: 'Безопасность',
      copy: 'Скопировать', done: 'сделано', todo: 'не сделано',
      overview: 'Обзор', section: 'Раздел', onPage: 'На странице', prev: 'Назад', next: 'Дальше', neighbours: 'Соседние страницы',
      source: 'Исходник страницы на GitHub', suffix: 'документация MonoPanel',
      docsTitle: 'Документация MonoPanel', docsDesc: 'Установка, возможности, веб-стек, почта, переезд, CLI и API панели MonoPanel.',
      heroAlt: 'Дашборд панели'
    }
  },
  en: {
    prefix: '/en',
    readme: 'README.en.md',
    docs: 'docs/en',
    og: 'en_US',
    groups: [['start', 'Getting started'], ['guide', 'Guides'], ['inside', 'Internals'], ['more', 'More']],
    sections: {
      'Install': ['install', 'Install', 'Install with one command or from .deb and .rpm packages: what the server needs and where the panel takes updates from.'],
      'Quick start': ['quickstart', 'Quick start', 'From mp setup to a site with a database, mail, backups and a move from another server — step by step, with mp commands.'],
      'What it does': ['features', 'Features', 'What the panel does, area by area: sites, PHP, databases, TLS, mail, backups, files, firewall, migration, updates — with the mp commands.'],
      'Releases and updates': ['update', 'Updates', 'How the panel updates itself: ed25519-signed releases, installation from a separate unit and a rollback to the previous binary if the new one does not answer.'],
      'Moving in': ['migrate', 'Moving in', 'Moving a whole account in from another MonoPanel, BitrixVM or FASTPANEL: the commands, what arrives and what to do after the move.'],
      'How it works': ['how', 'How it works', 'How the panel is built: one binary in four roles, state in SQLite and a config pipeline with validation and rollback.'],
      'Stack': ['stack', 'Stack', 'What the panel is built from: Go, chi and huma, SQLite, Svelte and SvelteKit, Bubble Tea, lego and nfpm.'],
      'Development': ['development', 'Development', 'Developing the panel: builds, tests with a fake agent, e2e against a live panel, CI and the repository layout.'],
      'Documentation': null,
      'Security': null,
      'License': null
    },
    nav: { architecture: 'Architecture', platforms: 'Platforms', 'web-stack': 'Web stack', 'cli-api': 'CLI, TUI and API', roadmap: 'Roadmap', mail: 'Mail', migration: 'Panel-to-panel migration', testbed: 'Testbed', 'web-ui': 'Web UI' },
    docDesc: {
      '01-architecture.md': 'How MonoPanel is built: one binary in the api, agent and helper roles, state in SQLite, a config pipeline with validation and rollback, security and updates.',
      '02-platform-matrix.md': 'Supported operating systems and package sources: nginx.org, Sury and Remi for PHP 5.6–8.5, MySQL and Percona 8.4, SELinux enforcing, firewall, Valkey per account.',
      '03-web-stack.md': 'The web stack: nginx + php-fpm and nginx + Apache modes, files and permissions, config templates, a php-fpm pool per site, TLS via lego, logs and CMS installation.',
      '04-cli-tui-api.md': 'The mp commands, the terminal menu and the REST API with OpenAPI 3.1: how they fit together, which endpoints exist and how to wire up billing.',
      '05-roadmap.md': 'The MonoPanel development log by stage and date: what is done and verified, what remains, the OS matrix and the risks.',
      '06-mail.md': 'The MonoPanel mail server: postfix, dovecot and opendkim, the path of a message, ports and TLS, the DNS records you need, send-only domains and Roundcube webmail.',
      '07-migration.md': 'Moving an account to MonoPanel: from another MonoPanel by token, from BitrixVM and FASTPANEL over ssh — what moves, how files and passwords travel, the DNS order.',
      '08-testbed.md': 'The Proxmox testbed: a machine for each of the eleven OSes, install, e2e, CMS and migration runs before a release, and what it found.',
      '09-web-ui.md': 'The MonoPanel web UI page by page with screenshots: dashboard, sites, PHP, databases, mail, files, jobs, firewall, backups, users and settings.'
    },
    introSkip: /^\[!\[|^English ·|^Status:/,
    t: {
      menu: [['#features', 'Features'], ['#how', 'How it works'], ['#os', 'Where it runs']],
      docs: 'Documentation', skip: 'Skip to content', home: 'MonoPanel — home', sections: 'Sections',
      theme: 'Theme', system: ['System', 'System theme'], light: ['Light', 'Light theme'], dark: ['Dark', 'Dark theme'],
      lang: 'Language', links: 'Links', releases: 'Releases', security: 'Security',
      copy: 'Copy', done: 'done', todo: 'not done',
      overview: 'Overview', section: 'Section', onPage: 'On this page', prev: 'Previous', next: 'Next', neighbours: 'Neighbouring pages',
      source: 'Page source on GitHub', suffix: 'MonoPanel documentation',
      docsTitle: 'MonoPanel documentation', docsDesc: 'Installation, features, web stack, mail, moving in, CLI and API of the MonoPanel control panel.',
      heroAlt: 'The panel dashboard'
    }
  }
};

// Иконка и группа меню у раздела README и файла docs/ — одни на оба языка. Файл docs/ без
// записи здесь всё равно попадёт на сайт, в группу «Ещё».
const META = {
  install: ['download', 'start'], quickstart: ['play', 'start'], features: ['tasks', 'start'], update: ['refresh', 'start'],
  migrate: ['users', 'guide'], how: ['cpu', 'inside'], stack: ['code', 'inside'], development: ['pencil', 'inside'],
  architecture: ['settings', 'inside'], platforms: ['box', 'guide'], 'web-stack': ['globe', 'guide'], 'cli-api': ['terminal', 'guide'],
  roadmap: ['clock', 'inside'], mail: ['mail', 'guide'], migration: ['file', 'inside'], testbed: ['check', 'inside'], 'web-ui': ['monitor', 'start']
};
const DOC_SLUGS = {
  '01-architecture.md': 'architecture', '02-platform-matrix.md': 'platforms', '03-web-stack.md': 'web-stack', '04-cli-tui-api.md': 'cli-api',
  '05-roadmap.md': 'roadmap', '06-mail.md': 'mail', '07-migration.md': 'migration', '08-testbed.md': 'testbed', '09-web-ui.md': 'web-ui'
};
// Порядок внутри групп; чего нет в списке — следом, в порядке исходников.
const ORDER = ['install', 'quickstart', 'web-ui', 'features', 'update', 'web-stack', 'mail', 'migrate', 'cli-api', 'platforms', 'how', 'architecture', 'migration', 'stack', 'roadmap', 'testbed', 'development'];

const read = (f) => fs.readFileSync(path.join(ROOT, f), 'utf8');
const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const icon = (name, cls = 'ic') => `<svg class="${cls}" aria-hidden="true"><use href="/assets/icons.svg#i-${name}"/></svg>`;
const plain = (tokens = []) => tokens.map((t) => (t.tokens ? plain(t.tokens) : (t.text ?? ''))).join('');
const warnings = [];

// ── Даты правок для карты сайта ─────────────────────────────────────────────
// Workers Builds клонирует репозиторий без истории; она дотягивается без содержимого
// файлов (несколько мегабайт), иначе у всех страниц стояла бы дата последней выкладки.
const git = (...args) => execFileSync('git', args, { cwd: ROOT, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();
let history = false;
try {
  if (git('rev-parse', '--is-shallow-repository') === 'true') git('fetch', '--quiet', '--filter=blob:none', '--unshallow');
  history = git('rev-list', '--count', 'HEAD') !== '1';
} catch {
  history = false;
}
if (!history) warnings.push('git: истории нет, карта сайта без lastmod');
const modified = new Map();
function lastmod(file) {
  if (!history) return '';
  if (!modified.has(file)) modified.set(file, git('log', '-1', '--format=%cI', '--', file));
  return modified.get(file);
}

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

// ── Страницы одного языка ───────────────────────────────────────────────────
function collect(lang) {
  const L = LANGS[lang];
  const pages = [];
  const readme = lex(L.readme);
  const intro = [];
  let section = null;
  for (const t of readme) {
    if (t.type === 'heading' && t.depth === 2) {
      const conf = L.sections[t.plain];
      const [slug, nav, desc] = conf ?? [urlSlug(t.plain), t.plain, ''];
      section = { slug, nav, desc, title: t.plain, head: t, source: L.readme, shift: 1, tokens: [] };
      if (conf !== null) pages.push(section);
      continue;
    }
    (section ? section.tokens : intro).push(t);
  }

  // Описания файлов документации — из таблицы в разделе о документации README.
  const docDesc = {};
  for (const m of read(L.readme).matchAll(/^\|\s*\[(?:docs\/)(?:en\/)?([^\]]+)\]\([^)]*\)\s*\|\s*(.+?)\s*\|\s*$/gm)) docDesc[m[1]] = m[2];

  const dir = path.join(ROOT, L.docs);
  const files = fs.existsSync(dir) ? fs.readdirSync(dir).filter((f) => f.endsWith('.md')).sort() : [];
  for (const f of files) {
    const source = `${L.docs}/${f}`;
    const tokens = lex(source);
    const h1 = tokens.findIndex((t) => t.type === 'heading' && t.depth === 1);
    const head = tokens[h1];
    const title = head ? head.plain.replace(/^\d+\.\s*/, '') : f;
    const slug = DOC_SLUGS[f] ?? urlSlug(f.replace(/^\d+-|\.md$/g, ''));
    pages.push({ slug, nav: L.nav[slug] ?? title, desc: L.docDesc?.[f] ?? docDesc[f] ?? '', title, head, source, shift: 0, tokens: tokens.filter((_, i) => i !== h1) });
  }

  const groupIndex = (p) => L.groups.findIndex(([g]) => g === p.group);
  const orderIndex = (p) => (ORDER.includes(p.slug) ? ORDER.indexOf(p.slug) : ORDER.length);
  pages.forEach((p, i) => {
    const [ic, group] = META[p.slug] ?? ['file', 'more'];
    Object.assign(p, { icon: ic, group, url: `${L.prefix}/docs/${p.slug}/`, i, lang });
  });
  pages.sort((a, b) => groupIndex(a) - groupIndex(b) || orderIndex(a) - orderIndex(b) || a.i - b.i);

  // Куда ведут якоря каждого исходника: файл целиком — на свою страницу (README — на
  // обзор документации), заголовок — на страницу, где он оказался.
  const anchors = new Map();
  const mainUrl = new Map([[L.readme, `${L.prefix}/docs/`]]);
  for (const p of pages) {
    if (!anchors.has(p.source)) anchors.set(p.source, new Map());
    const map = anchors.get(p.source);
    if (p.head) map.set(p.head.id, `${p.url}#${p.head.id}`);
    for (const t of p.tokens) if (t.type === 'heading') map.set(t.id, `${p.url}#${t.id}`);
    if (!mainUrl.has(p.source)) mainUrl.set(p.source, p.url);
  }
  return { lang, L, pages, intro, anchors, mainUrl };
}

function decode(s) {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}

// Ссылка из markdown → адрес на сайте: страницы документации и их якоря — сюда,
// картинки из docs/img (docs/en/img) — рядом, остальные файлы репозитория — на GitHub.
// Якорь, которого нет, попадает в предупреждения сборки.
function resolve(href, page, site) {
  if (!href || /^[a-z][a-z0-9+.-]*:/i.test(href)) return href;
  const cut = href.indexOf('#');
  const file = cut < 0 ? href : href.slice(0, cut);
  const hash = cut < 0 ? '' : decode(href.slice(cut + 1));
  if (!file) {
    const url = site.anchors.get(page.source)?.get(hash);
    if (!url) warnings.push(`${page.source}: #${hash} — нет такого заголовка`);
    return url && !url.startsWith(page.url + '#') ? url : `#${hash}`;
  }
  const target = path.posix.normalize(path.posix.join(path.posix.dirname(page.source), decode(file)));
  if (target.startsWith('docs/en/img/')) return '/en/' + target.replace(/^docs\/en\//, 'docs/');
  if (target.startsWith('docs/img/')) return '/' + target;
  if (site.mainUrl.has(target)) {
    if (!hash) return site.mainUrl.get(target);
    const url = site.anchors.get(target)?.get(hash);
    if (!url) warnings.push(`${page.source}: ${target}#${hash} — нет такого заголовка`);
    return url ?? `${REPO}/blob/main/${target}#${hash}`;
  }
  if (/^docs\/.*\.md$/.test(target) || /^README(\.en)?\.md$/.test(target)) warnings.push(`${page.source}: ${href} — документ другого языка или его нет`);
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

// Где картинка сайта лежит в репозитории: /docs/img/x → docs/img/x, /en/docs/img/x → docs/en/img/x.
const imageFile = (u) => path.join(ROOT, u.startsWith('/en/docs/img/') ? 'docs/en/img/' + u.slice('/en/docs/img/'.length) : u.slice(1));
// Английского снимка ещё нет — берётся русский.
const localImage = (u) => (u.startsWith('/en/') && !fs.existsSync(imageFile(u)) ? u.slice(3) : u);

// Скриншот из docs/img: рядом может лежать тёмная версия (x.dark.webp) — тогда
// показывается та, что совпадает с темой сайта. На GitHub видна светлая.
function shot(url, alt) {
  url = localImage(url);
  const variant = (u, cls) => {
    const d = imageSize(imageFile(u));
    const size = d ? ` width="${d.w}" height="${d.h}"` : '';
    return `<a class="shot-frame${cls}" href="${esc(u)}"><img src="${esc(u)}" alt="${esc(alt)}"${size} loading="lazy" decoding="async"></a>`;
  };
  const dark = url.replace(/(\.\w+)$/, '.dark$1');
  const hasDark = fs.existsSync(imageFile(dark));
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
let currentSite; // её язык: якоря, адреса и слова оформления
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
      const copy = currentSite.L.t.copy;
      return `<div class="codeblock"><pre class="code"><code>${highlight(text, l)}</code></pre><button class="btn btn-ghost btn-sm code-copy" type="button" title="${copy}" aria-label="${copy}">${icon('copy')}</button></div>\n`;
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
      const url = resolve(href, current, currentSite);
      return `<a href="${esc(url)}"${title ? ` title="${esc(title)}"` : ''}>${this.parser.parseInline(tokens)}</a>`;
    },
    image({ href, text }) {
      const url = resolve(href, current, currentSite);
      return /^(\/en)?\/docs\/img\//.test(url) ? shot(url, text) : `<img src="${esc(url)}" alt="${esc(text)}" loading="lazy">`;
    },
    checkbox({ checked }) {
      const t = currentSite.L.t;
      return `<span class="task${checked ? ' task-on' : ''}" role="img" aria-label="${checked ? t.done : t.todo}"></span> `;
    }
  }
});

function render(site, page, tokens) {
  current = page;
  currentSite = site;
  page.toc = [];
  const out = md.parser(Object.assign([...tokens], { links: {} }));
  current = null;
  return out;
}

// ── Каркас страниц ──────────────────────────────────────────────────────────
// Та же страница на другом языке: /docs/x/ ↔ /en/docs/x/.
const other = (lang) => (lang === 'ru' ? 'en' : 'ru');
const counterpart = (url, lang) => (lang === 'ru' ? '/en' + url : url.replace(/^\/en/, '') || '/');

// Переключатель языка: ссылка с ?lang= — воркер запомнит выбор и откроет эту страницу.
function langSwitch(lang, url, t) {
  const link = (l) =>
    l === lang
      ? `<a href="${url}" hreflang="${l}" lang="${l}" aria-current="true">${l.toUpperCase()}</a>`
      : `<a href="${counterpart(url, lang)}?lang=${l}" hreflang="${l}" lang="${l}">${l.toUpperCase()}</a>`;
  return `<div class="seg seg-lang" role="group" aria-label="${t.lang}">${link('ru')}${link('en')}</div>`;
}

// Версии страницы для поисковиков: русская, английская и английская по умолчанию.
function alternates(lang, url) {
  const ru = lang === 'ru' ? url : counterpart(url, lang);
  const en = lang === 'en' ? url : counterpart(url, lang);
  return `<link rel="alternate" hreflang="ru" href="${ORIGIN}${ru}">
<link rel="alternate" hreflang="en" href="${ORIGIN}${en}">
<link rel="alternate" hreflang="x-default" href="${ORIGIN}${en}">`;
}

// Структурированные данные для поисковиков; «<» экранируется, чтобы текст не закрыл <script>.
const jsonLd = (data) => `<script type="application/ld+json">${JSON.stringify(data).replace(/</g, '\\u003c')}</script>`;

function layout(site, { title, desc, main, active, toc = '', ld = null }) {
  const { lang, L } = site;
  const t = L.t;
  const home = `${L.prefix}/`;
  const docsUrl = `${L.prefix}/docs/`;
  return `<!doctype html>
<html lang="${lang}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>${esc(title)}</title>
<meta name="description" content="${esc(desc)}">
<link rel="canonical" href="${ORIGIN}${active}">
${alternates(lang, active)}
<meta property="og:type" content="article">
<meta property="og:site_name" content="MonoPanel">
<meta property="og:locale" content="${L.og}">
<meta property="og:url" content="${ORIGIN}${active}">
<meta property="og:title" content="${esc(title)}">
<meta property="og:description" content="${esc(desc)}">
<meta property="og:image" content="${ORIGIN}${localImage(`${L.prefix}/docs/img/dashboard.dark.webp`)}">
${ld ? jsonLd(ld) + '\n' : ''}<meta name="theme-color" content="#eef3f8" media="(prefers-color-scheme: light)">
<meta name="theme-color" content="#05070c" media="(prefers-color-scheme: dark)">
<link rel="icon" href="/favicon.ico" sizes="32x32">
<link rel="icon" href="/favicon.svg?v=2" type="image/svg+xml">
<link rel="stylesheet" href="/assets/fonts.css">
<link rel="stylesheet" href="/assets/site.css">
<script>try{var t=localStorage.getItem("theme");if(t==="dark"||t==="light")document.documentElement.dataset.theme=t}catch(e){}</script>
<script src="/assets/site.js" defer></script>
</head>
<body class="wide">
<a class="skip" href="#doc">${t.skip}</a>
<div class="glow"></div>
<header class="top">
  <div class="wrap">
    <a class="logo" href="${home}" aria-label="${t.home}"><span class="logo-mark" aria-hidden="true">M</span><span class="brand">MonoPanel</span></a>
    <nav class="menu" aria-label="${t.sections}">
      ${[...t.menu.map(([hash, label]) => [home + hash, label]), [docsUrl, t.docs]].map(([href, label]) => `<a class="tab${href === docsUrl ? ' tab-on' : ''}" href="${href}">${label}</a>`).join('\n      ')}
    </nav>
    <div class="top-end">
      ${langSwitch(lang, active, t)}
      <div class="seg" role="group" aria-label="${t.theme}">
        <button type="button" data-theme-mode="system" title="${t.system[0]}" aria-label="${t.system[1]}">${icon('monitor')}</button>
        <button type="button" data-theme-mode="light" title="${t.light[0]}" aria-label="${t.light[1]}">${icon('sun')}</button>
        <button type="button" data-theme-mode="dark" title="${t.dark[0]}" aria-label="${t.dark[1]}">${icon('moon')}</button>
      </div>
      <a class="btn" href="${REPO}">GitHub ${icon('external')}</a>
    </div>
  </div>
</header>
<div class="wrap docs${toc ? '' : ' no-toc'}">
  <aside class="side" aria-label="${t.docs}">${sidebar(site, active)}</aside>
  ${main}
  ${toc}
</div>
<footer class="foot">
  <div class="wrap">
    <a class="logo" href="${home}" aria-label="${t.home}"><span class="logo-mark" aria-hidden="true">M</span><span class="brand">MonoPanel</span></a>
    <nav aria-label="${t.links}">
      <a href="${REPO}">GitHub</a>
      <a href="${REPO}/releases">${t.releases}</a>
      <a href="${docsUrl}">${t.docs}</a>
      <a href="${REPO}/blob/main/SECURITY.md">${t.security}</a>
      <a href="${counterpart(active, lang)}?lang=${other(lang)}" hreflang="${other(lang)}" lang="${other(lang)}">${lang === 'ru' ? 'English' : 'Русский'}</a>
      <a href="${REPO}/blob/main/LICENSE">Apache-2.0</a>
    </nav>
  </div>
</footer>
<div class="toast" id="toast" role="status" aria-live="polite"></div>
</body>
</html>
`;
}

function navLinks(site, active) {
  const item = (url, label, ic) =>
    `<a class="nav-item${url === active ? ' nav-on' : ''}" href="${url}"${url === active ? ' aria-current="page"' : ''}>${icon(ic)}<span>${esc(label)}</span></a>`;
  let html = item(`${site.L.prefix}/docs/`, site.L.t.overview, 'home');
  for (const [g, label] of site.L.groups) {
    const list = site.pages.filter((p) => p.group === g);
    if (list.length) html += `<div class="side-group"><span class="label">${label}</span>${list.map((p) => item(p.url, p.nav, p.icon)).join('')}</div>`;
  }
  return html;
}

function sidebar(site, active) {
  return `<nav class="side-nav">${navLinks(site, active)}</nav>`;
}

// На телефоне боковое меню прячется, вместо него — раскрывающийся список над текстом.
function mobileNav(site, active, label) {
  return `<details class="side-m card"><summary><span class="label">${site.L.t.section}</span><span class="side-m-t">${esc(label)}</span>${icon('chevron', 'ic side-m-ic')}</summary><nav class="side-nav">${navLinks(site, active)}</nav></details>`;
}

function tocHtml(site, page) {
  if (page.toc.length < 2) return '';
  const t = site.L.t;
  return `<nav class="toc" aria-label="${t.onPage}"><span class="label">${t.onPage}</span>${page.toc
    .map((h) => `<a class="toc-${h.level}" href="#${esc(h.id)}">${esc(h.text)}</a>`)
    .join('')}</nav>`;
}

function pager(site, page) {
  const t = site.L.t;
  const i = site.pages.indexOf(page);
  const card = (p, dir) =>
    p ? `<a class="card pager-${dir}" href="${p.url}"><span class="label">${dir === 'prev' ? t.prev : t.next}</span><span class="pager-t">${esc(p.nav)}</span></a>` : '<span></span>';
  return `<nav class="pager" aria-label="${t.neighbours}">${card(site.pages[i - 1], 'prev')}${card(site.pages[i + 1], 'next')}</nav>`;
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

// Хлебные крошки для поисковиков: сайт → документация → страница.
function crumbsLd(site, ...tail) {
  const items = [['MonoPanel', `${site.L.prefix}/`], [site.L.t.docs, `${site.L.prefix}/docs/`], ...tail];
  return {
    '@context': 'https://schema.org',
    '@type': 'BreadcrumbList',
    itemListElement: items.map(([name, url], i) => ({ '@type': 'ListItem', position: i + 1, name, item: `${ORIGIN}${url}` }))
  };
}

function buildDocs(site) {
  const { L } = site;
  const t = L.t;
  for (const page of site.pages) {
    const body = render(site, page, page.tokens);
    const group = L.groups.find(([g]) => g === page.group)[1];
    const main = `<main class="doc" id="doc">
    ${mobileNav(site, page.url, page.nav)}
    <div class="crumbs"><a href="${L.prefix}/docs/">${t.docs}</a>${icon('chevron')}<span>${group}</span></div>
    <h1 class="page-title"${page.head ? ` id="${esc(page.head.id)}"` : ''}>${esc(page.title)}</h1>
    <div class="prose">${body}</div>
    ${pager(site, page)}
    <a class="edit" href="${REPO}/blob/main/${page.source}${page.source === L.readme ? '#' + page.head.id : ''}">${icon('pencil')}${t.source}</a>
  </main>`;
    write(`${L.prefix.slice(1)}/docs/${page.slug}/index.html`.replace(/^\//, ''), layout(site, { title: `${page.title} — ${t.suffix}`, desc: page.desc || page.title, main, active: page.url, toc: tocHtml(site, page), ld: crumbsLd(site, [page.nav, page.url]) }));
  }

  // Обзор: вступление README и карточки разделов.
  const docs = `${L.prefix}/docs/`;
  const home = { source: L.readme, url: docs, shift: 1 };
  const introHtml = render(site, home, site.intro.filter((tk) => tk.type === 'paragraph' && !L.introSkip.test(tk.raw)).slice(0, 2));
  const heroShot = fs.existsSync(path.join(ROOT, 'docs/img/dashboard.webp')) ? shot(`${L.prefix}/docs/img/dashboard.webp`, t.heroAlt) : '';
  const cards = L.groups
    .map(([g, label]) => {
      const list = site.pages.filter((p) => p.group === g);
      if (!list.length) return '';
      return `<section class="home-group"><h2 class="label">${label}</h2><div class="grid grid-3">${list
        .map((p) => `<a class="card feat doc-card" href="${p.url}"><div class="feat-h"><span class="feat-ic">${icon(p.icon)}</span><h3>${esc(p.nav)}</h3></div><p>${esc(p.desc)}</p></a>`)
        .join('')}</div></section>`;
    })
    .join('\n');
  write(
    `${L.prefix.slice(1)}/docs/index.html`.replace(/^\//, ''),
    layout(site, {
      title: t.docsTitle,
      desc: t.docsDesc,
      active: docs,
      ld: crumbsLd(site),
      main: `<main class="doc doc-home" id="doc">
    ${mobileNav(site, docs, t.overview)}
    <h1 class="page-title">${t.docs}</h1>
    <div class="prose lead-prose">${introHtml}</div>
    ${heroShot}
    ${cards}
  </main>`
    })
  );
}

fs.rmSync(OUT, { recursive: true, force: true });
fs.cpSync(path.join(SITE, 'public'), OUT, { recursive: true });
write('assets/icons.svg', iconSprite());
if (fs.existsSync(path.join(ROOT, 'docs/img'))) fs.cpSync(path.join(ROOT, 'docs/img'), path.join(OUT, 'docs/img'), { recursive: true });
if (fs.existsSync(path.join(ROOT, 'docs/en/img'))) fs.cpSync(path.join(ROOT, 'docs/en/img'), path.join(OUT, 'en/docs/img'), { recursive: true });

const sites = Object.keys(LANGS).map(collect);
for (const site of sites) buildDocs(site);

// Структурированные данные главной: название сайта и карточка программы. Главная лежит в
// public/ готовой, поэтому блок вписывается в её копию в dist/.
const APP = {
  ru: {
    kind: 'Панель управления веб-хостингом',
    os: 'Linux: Debian 12/13, Ubuntu 22.04/24.04/26.04, AlmaLinux, Rocky Linux и Oracle Linux 9/10',
    features: ['Сайты: nginx + php-fpm, nginx + Apache или прокси на своё приложение', 'PHP 5.6–8.5 параллельно, пул php-fpm на сайт', 'MySQL и Percona Server 8.4', "Let's Encrypt по HTTP-01 и DNS-01", 'Почта: postfix, dovecot, DKIM, Roundcube', 'Бэкапы restic: local, SFTP, S3, B2, REST', 'Firewall nftables и fail2ban', 'Переезд с другой MonoPanel, FASTPANEL и BitrixVM', 'Web UI, CLI, TUI и REST API']
  },
  en: {
    kind: 'Web hosting control panel',
    os: 'Linux: Debian 12/13, Ubuntu 22.04/24.04/26.04, AlmaLinux, Rocky Linux and Oracle Linux 9/10',
    features: ['Sites: nginx + php-fpm, nginx + Apache or a proxy to your own application', 'PHP 5.6–8.5 side by side, a php-fpm pool per site', 'MySQL and Percona Server 8.4', "Let's Encrypt over HTTP-01 and DNS-01", 'Mail: postfix, dovecot, DKIM, Roundcube', 'restic backups: local, SFTP, S3, B2, REST', 'nftables firewall and fail2ban', 'Migration from another MonoPanel, FASTPANEL and BitrixVM', 'Web UI, CLI, TUI and REST API']
  }
};
const version = /^Status: \*\*([0-9.]+)\*\*/m.exec(read('README.en.md'))?.[1];
for (const { lang, L } of sites) {
  const file = path.join(OUT, L.prefix.slice(1), 'index.html');
  const html = fs.readFileSync(file, 'utf8');
  const desc = (/<meta name="description" content="([^"]*)"/.exec(html)?.[1] ?? '').replace(/&quot;/g, '"').replace(/&amp;/g, '&');
  const app = {
    '@type': 'SoftwareApplication',
    name: 'MonoPanel',
    url: `${ORIGIN}${L.prefix}/`,
    description: desc,
    applicationCategory: 'DeveloperApplication',
    applicationSubCategory: APP[lang].kind,
    operatingSystem: APP[lang].os,
    ...(version ? { softwareVersion: version } : {}),
    license: 'https://www.apache.org/licenses/LICENSE-2.0',
    isAccessibleForFree: true,
    offers: { '@type': 'Offer', price: '0', priceCurrency: 'USD' },
    downloadUrl: `${REPO}/releases`,
    installUrl: `${ORIGIN}${L.prefix}/docs/install/`,
    screenshot: `${ORIGIN}/docs/img/dashboard.webp`,
    image: `${ORIGIN}/docs/img/dashboard.dark.webp`,
    sameAs: [REPO],
    inLanguage: ['ru', 'en'],
    featureList: APP[lang].features
  };
  const site = { '@type': 'WebSite', name: 'MonoPanel', url: `${ORIGIN}/`, inLanguage: ['ru', 'en'] };
  fs.writeFileSync(file, html.replace('</head>', `${jsonLd({ '@context': 'https://schema.org', '@graph': [site, app] })}\n</head>`));
}

// Карта сайта для поисковиков: главная, обзор документации и все её страницы на обоих
// языках, у каждой — ссылки на версии и дата последней правки исходника.
const modOf = new Map();
for (const s of sites) {
  modOf.set(`${s.L.prefix}/`, lastmod(`site/public${s.L.prefix}/index.html`));
  modOf.set(`${s.L.prefix}/docs/`, lastmod(s.L.readme));
  for (const p of s.pages) modOf.set(p.url, lastmod(p.source));
}
const urlTag = (u) => `<url><loc>${ORIGIN}${u}</loc>${modOf.get(u) ? `<lastmod>${modOf.get(u)}</lastmod>` : ''}`;
const ruUrls = ['/', '/docs/', ...sites[0].pages.map((p) => p.url)];
const entries = ruUrls.flatMap((ru) => {
  const en = counterpart(ru, 'ru');
  const alt = `\n    <xhtml:link rel="alternate" hreflang="ru" href="${ORIGIN}${ru}"/>\n    <xhtml:link rel="alternate" hreflang="en" href="${ORIGIN}${en}"/>\n    <xhtml:link rel="alternate" hreflang="x-default" href="${ORIGIN}${en}"/>\n  `;
  return [`  ${urlTag(ru)}${alt}</url>`, `  ${urlTag(en)}${alt}</url>`];
});
write('sitemap.xml', `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">\n${entries.join('\n')}\n</urlset>\n`);

// Страница, которой нет на другом языке: переключатель и hreflang вели бы в 404.
const slugs = (s) => new Set(s.pages.map((p) => p.slug));
for (const [a, b] of [[sites[0], sites[1]], [sites[1], sites[0]]]) {
  for (const slug of slugs(a)) if (!slugs(b).has(slug)) warnings.push(`${a.lang}: /docs/${slug}/ — нет в ${b.lang}`);
}
for (const w of new Set(warnings)) console.warn(`предупреждение: ${w}`);
console.log(`dist: ${sites.map((s) => `${s.lang} — ${s.pages.length} страниц документации + обзор`).join(', ')}`);
