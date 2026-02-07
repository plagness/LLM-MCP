# Changelog

## [2026.02.6] - 2026-02-07

- `telemetry/llm_telemetry/telegram_gateway.py`:
  - default `TELEGRAM_MCP_BASE_URL` переключен на `http://tgapi:8000`;
  - добавлен compat fallback на legacy `http://telegram-api:8000` при неявной конфигурации.
- `README.md` и `doc/README.md` синхронизированы с новым routing default и compat-window.
- Добавлены governance-файлы публичного репозитория:
  - `LICENSE` (MIT), `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`;
  - `.github/ISSUE_TEMPLATE/*`, `.github/pull_request_template.md`, `.github/CODEOWNERS`.
- Обновлены `VERSION` и `.env.example` до `2026.02.6`.
- Добавлен pragmatic CI: `.github/workflows/ci.yml` (compose config, markdown links, Python compile, Go test, MCP TS build).


## [2026.02.5] - 2026-02-07

- Синхронизирована версия модуля в `.env.example`:
  - `LLM_MCP_VERSION=2026.02.5`.
- Обновлены `VERSION` и version badge в `README.md` до `2026.02.5`.
- Исправлен дрейф версий между docs/env и интеграционным root compose-контуром.

## [2026.02.4] - 2026-02-07

- `README.md` приведён к единому визуальному стилю NeuronSwarm:
  - badges, кнопки-навигации и emoji-структура;
  - обновлены блоки архитектуры, quick start, API и env-контрактов.
- Добавлен раздел `Public Git Standards`:
  - версия в формате `YYYY.MM.x`;
  - обязательный `CHANGELOG.md`;
  - исключение секретов из git и smoke-проверка перед merge.
- Обновлён `VERSION` до `2026.02.4`.

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
