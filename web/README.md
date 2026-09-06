# MonoPanel Web UI

SvelteKit 2 + Svelte 5 + Tailwind 4, собирается статически в `build/` и вшивается в бинарник Go (`web/embed.go`).

- `pnpm install && pnpm build` — сборка в `build/` (Node 24 LTS, pnpm 10).
- `pnpm dev` — dev-сервер с прокси `/api` на локальную панель.
- `pnpm api:types` — генерация TypeScript-клиента из OpenAPI панели.

Страницы: дашборд (статус, метрики, doctor), сайты (+ карточка с настройками и логами), пользователи (+ cron), PHP, базы данных, SSL (+ DNS-провайдеры), задачи (живой лог через SSE), firewall (+ fail2ban), бэкапы, настройки (2FA с QR, API-токены, webhooks).
Без Node `go build` использует то, что лежит в `build/`; в репозитории хранится только заглушка `build/index.html`, CI собирает полный UI.
