# Сайт MonoPanel

Сайт и документация на Cloudflare Workers: статика и маленький воркер, который выбирает
язык. `npm run build` собирает `dist/`: копирует `public/` (главная, стили, шрифты, 404) и
делает страницы документации из `README.md` и `docs/*.md` репозитория, так что
документация на сайте всегда та же, что в репозитории. Разделы README и файлы `docs/`
раскладываются по меню в `build.mjs` (новый файл попадёт в группу «Ещё», пока его туда
не впишут), ссылки между ними и якоря заголовков работают как на GitHub, картинки
берутся из `docs/img/`.

Сайт на двух языках. Русская версия — по прежним адресам (`/`, `/docs/…`), английская —
под `/en/`: `public/en/` и документация из `README.en.md` и `docs/en/*.md` с картинками
из `docs/en/img/` (английского снимка нет — берётся русский). Сборка предупреждает о
ссылке на несуществующий заголовок и о странице, которой нет на другом языке.
`src/worker.js` отправляет посетителя на его версию по тому же правилу, что Web UI
панели: первый язык браузера из стран СНГ — русский, любой другой — английский.
Переключатель RU/EN в шапке запоминает выбор в cookie `lang`, и он главнее языка
браузера; поисковые роботы не перенаправляются, версии связаны через hreflang и
`sitemap.xml`. Воркер запускается только для страниц (`run_worker_first` в
`wrangler.jsonc`), стили, картинки и `install.sh` отдаются мимо него.

- `npm install` — один раз; `npm run build` — сборка в `dist/`.
- `npm run dev` — сборка и локальный просмотр на http://localhost:8787 (после правок
  markdown — пересобрать и перезапустить: wrangler dev читает список файлов при старте).
- `npx wrangler deploy --dry-run` — проверить конфигурацию, ничего не публикуя.

Оформление повторяет панель: токены светлой и тёмной темы, компоненты (`.card`, `.btn`,
`.tag`, `.kpi`, `.tbl`, `.tabs`, пункты меню) и фон в `public/assets/site.css`
переписаны с `web/src/app.css` без Tailwind — правки темы панели переносятся сюда вручную.
Иконки при сборке берутся из `web/src/lib/components/Icon.svelte`, шрифты Play, Exo 2 и
JetBrains Mono лежат в `public/fonts/` (latin и cyrillic из `@fontsource`, лицензия OFL
рядом). `public/_headers` задаёт кэш шрифтов и заголовки безопасности, `public/404.html`
отдаётся на неизвестные адреса.

Скриншоты Web UI (`docs/img/` и `docs/en/img/`, светлая и тёмная версия) снимаются с
настоящей панели на площадке — см. «Как переснять скриншоты» в `docs/09-web-ui.md`.

## Публикация

Workers Builds собирает сайт сам при пуше в `main`, ветки получают превью-ссылки.
Настройки проекта `monopanel` в дашборде Cloudflare:

| Поле | Значение |
|---|---|
| Root directory | `site` |
| Build command | `npm run build` (зависимости Cloudflare ставит сам) |
| Deploy command | `npx wrangler deploy` |
| Build watch paths (Settings → Build) | include `site/*`, `docs/*`, `README.md`, `README.en.md`, `web/src/lib/components/Icon.svelte` — коммиты в код панели сайт не пересобирают |

Имя проекта в дашборде должно совпадать с `name` в `wrangler.jsonc`.

## Домен

Сайт живёт на https://monopanel.app — custom domain воркера `monopanel`; canonical,
`og:url` и `sitemap.xml` указывают туда (`ORIGIN` в `build.mjs`), так что копия на
workers.dev поисковикам не мешает. В зоне `monopanel.app` (Cloudflare, тариф Free):

- `www.monopanel.app` — запись AAAA `100::` через прокси и Single Redirect 301 на
  `https://monopanel.app` с тем же путём и query;
- Always Use HTTPS включён, минимальная версия TLS — 1.2 (домены `.app` браузеры и так
  открывают только по HTTPS).
