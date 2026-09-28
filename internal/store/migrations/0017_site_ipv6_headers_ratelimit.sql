-- IPv6-адрес сайта (пусто — только IPv4), заголовки безопасности в ответах
-- nginx и ограничение частоты запросов с одного адреса (0 — выключено).
ALTER TABLE sites ADD COLUMN ipv6 TEXT NOT NULL DEFAULT '';
ALTER TABLE sites ADD COLUMN security_headers INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sites ADD COLUMN rate_limit INTEGER NOT NULL DEFAULT 0;
