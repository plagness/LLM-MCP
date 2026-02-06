# Changelog

## [2026.02.3] - 2026-02-06

- Compose naming normalized to short alnum containers:
  - `llmdb`, `llmcore`, `llmworker`, `llmtelemetry`, `llmmcp`.
- Compose labels added (`ns.module`, `ns.component`, `ns.db_owner`).
- Port contract aligned and documented (`5435`, `8080`, `9090`, `3333`).
- `.env.example`, `README.md` and `doc/README.md` synchronized with new service names and MCP routing URL (`http://tgapi:8000`).
- Dockerfiles (`core`, `worker`, `telemetry`, `mcp`) now include OCI labels and `ns.module/ns.component`.

## [2026.02.2]

- Telemetry переведена на gateway-маршрутизацию Telegram:
  - `TELEGRAM_USE_MCP=1` -> отправка/редактирование через `telegram-mcp`.
  - fallback на прямой Telegram при `TELEGRAM_MCP_FALLBACK_DIRECT=1`.
- Добавлен `telemetry/llm_telemetry/telegram_gateway.py`.
- Рефактор `telemetry/llm_telemetry/main.py` под mcp route + direct fallback.
- Добавлены env-переменные `TELEGRAM_MCP_*` в `compose.yml` и `.env.example`.
- Обновлены документация и зависимости (`telegram-api-client` pinned git tag).
