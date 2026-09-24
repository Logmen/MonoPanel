# Сайт MonoPanel

Сайт и документация на Cloudflare Workers — только статика, без кода воркера.
`npm run build` собирает `dist/`: копирует `public/` (главная, стили, шрифты, 404) и
делает страницы документации из `README.md` и `docs/*.md` репозитория, так что
документация на сайте всегда та же, что в репозитории. Разделы README и файлы `docs/`
раскладываются по меню в `build.mjs` (новый файл попадёт в группу «Ещё», пока его туда
не впишут), ссылки между ними и якоря заголовков работают как на GitHub, картинки
берутся из `docs/img/`.

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

Скриншоты Web UI (`docs/img/`, светлая и тёмная версия) снимаются с настоящей панели на
площадке — см. «Как переснять скриншоты» в `docs/09-web-ui.md`.

## Публикация

Workers Builds собирает сайт сам при пуше в `main`, ветки получают превью-ссылки.
Настройки проекта `monopanel` в дашборде Cloudflare:

| Поле | Значение |
|---|---|
| Root directory | `site` |
| Build command | `npm run build` (зависимости Cloudflare ставит сам) |
| Deploy command | `npx wrangler deploy` |
| Build watch paths (Settings → Build) | include `site/*`, `docs/*`, `README.md`, `web/src/lib/components/Icon.svelte` — коммиты в код панели сайт не пересобирают |

Имя проекта в дашборде должно совпадать с `name` в `wrangler.jsonc`.
