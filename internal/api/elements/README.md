# Stoplight Elements — страница документации API

Источник: `@stoplight/elements@9.0.15` из npm (`web-components.min.js`,
`styles.min.css`). Лицензия Apache-2.0 — см. `LICENSE`.

Файлы вшиты в бинарник и отдаются с origin панели (`/api/v1/docs/…`), а не с
unpkg.com: страница документации живёт там же, где сессия администратора,
поэтому сторонний скрипт на ней недопустим, а её CSP разрешает только `'self'`.

## Как обновить

```sh
ver=9.0.15
curl -sL "https://registry.npmjs.org/@stoplight/elements/-/elements-$ver.tgz" | tar xz
cp package/web-components.min.js package/styles.min.css package/LICENSE internal/api/elements/
```

Хеши в `internal/api/docs_test.go` пинят содержимое: после обновления
замените их (`openssl dgst -sha384 -binary <файл> | base64`) и откройте
`/api/v1/docs` — в консоли браузера не должно быть отказов CSP.
