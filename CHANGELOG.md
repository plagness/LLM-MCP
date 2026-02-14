# Changelog

## [2026.02.12] - 2026-02-14

### Smart Routing
- Контекстно-зависимый маппинг quality→tiers (3 бакета контекста: ≤4K, 4K-32K, >32K)
- Каскадный fallback: local ollama → openrouter → openai
- Поддержка thinking-моделей (qwen3, deepseek-r1, phi4-reasoning, exaone-deep, lfm2.5-thinking, deepscaler)
- Балансировка нагрузки: учёт queued+running задач при batch-отправке

### Dashboard Host→Node Hierarchy
- Иерархическая структура: Host → Node (физическая машина → Ollama-инстансы на портах)
- Автоопределение orchestration типа (docker/native)
- Подсчёт уникальных моделей (не дубликатов по устройствам)
- Issues на человеческом языке (offline хосты, low success rate, stuck queue)

### Developer Tools — 4 новых endpoint'а
- `GET /v1/debug/health` — глубокая диагностика (БД, очередь, хосты, workers)
- `GET /v1/debug/actions` — каталог API с curl-примерами
- `GET /v1/debug/capacity` — мощности кластера (слоты, утилизация)
- `POST /v1/debug/test` — smoke test pipeline

### Discovery
- Автоопределение tier, thinking, context_k для Ollama-моделей

### Worker
- Поддержка thinking-ответов (`<think>` tag parsing)
- Cost-расчёт из `_price_in_1m`/`_price_out_1m`

### Исправления
- UUID-валидация в HandleJobByID (500→404)
- Graceful HandleCostsSummary при отсутствии llm_costs
- Очистка документации от реальных хостнеймов и IP

### Миграции
- `04_smart_routing.sql` — таблицы `model_pricing`, `llm_costs`, view `v_device_stats`

### Новые env-переменные
- `OLLAMA_PORTS` — мультипортовое discovery (default: `11434`)
- `DEVICE_MAX_CONCURRENCY` — макс задач на устройство (default: `1`)

### Прочее
- VERSION: `2026.02.11` → `2026.02.12`
- Roadmap: P2P model distribution, auto-scaling моделей

---

## [2026.02.11] - 2026-02-10

### Dashboard API (расширение для Mini App)

- **DeviceInfo**: добавлены поля `platform`, `arch`, `host`, `model_names`, `last_seen` в `/v1/dashboard`
- Расширен SQL-запрос devices: JSON-массив доступных моделей, метаданные устройства
- Обратная совместимость: llm.html игнорирует новые поля

### Прочее
- VERSION: `2026.02.10` → `2026.02.11`

---

## [2026.02.9] - 2026-02-10

### Multi-port Discovery
- `feat(discovery): multi-port Ollama + fix pgx type inference`
- Корректный multi-port probe с explicit `::text` cast для pgx v5

---

## [2026.02.8] - 2026-02-10

### Multi-Ollama Discovery
- **OLLAMA_PORTS** env var: discovery пробирует несколько портов на каждом устройстве (default: 11434).
- `probeOllamaPort()` — probe конкретного порта, `ensurePortDevice()` — создание device entry для non-default портов.
- `ollama_addr` хранит полный `ip:port` формат для всех портов.
- Subnet scan с multi-port поддержкой.

### Worker addr:port
- `_split_host_port()` — корректное разделение addr на host и port (IPv4/IPv6).
- `_resolve_ollama_base()` — автоматическое определение порта из `ollama_addr`.
- Backward compat: addr без порта → default 11434.

### Compose Ollama profiles
- Profile `ollama` — одиночный инстанс (:11434).
- Profile `ollama-multi` — 3 инстанса (:11434, :11435, :11436).
- Изолированные named volumes для каждого инстанса.
- `OLLAMA_PORTS` env передаётся в llmcore.

### Kubernetes манифесты
- Полный набор K8s манифестов: namespace, secret, configmap, PVC, StatefulSet, Deployments, Services.
- `ollama-deployment.yaml` — Deployment с nodeSelector, health probes, resource limits.
- `ollama-service.yaml` — NodePort 31434.
- `kustomization.yaml` с общими метками и аннотациями.
- `k8s/README.md` — документация развёртывания.

### Документация и инструменты
- `TODO.md` — план multi-Ollama + distributed compute.
- `CHANGELOG_V2.md`, `INTEGRATION_GUIDE_V2.md`, `V2_RELEASE_SUMMARY.md` — полная документация v2.
- `scripts/sync_openrouter_models.py`, `scripts/probe_openrouter_models.py` — инструменты синхронизации моделей.
- `config/curated_openrouter_models.yaml` — курированный список моделей.

### Прочее
- Санитизация PII из всех документов и примеров.
- Синхронизация версий: VERSION, README badge, .env.example, mcp/package.json.

## [2026.02.7] - 2026-02-09

### Tier 1 — Новые эндпоинты и запись расходов

- **Запись actual cost**: worker парсит `usage` (prompt_tokens/completion_tokens) из ответов OpenAI/OpenRouter; core записывает стоимость в `llm_costs` через SQL-функцию `calculate_job_cost()`.
- **`GET /v1/costs/summary?period=day|week|month`**: агрегированная статистика расходов с группировкой по провайдерам.
- **`GET /v1/dashboard`**: полный snapshot системы в одном JSON — jobs, costs, devices, running_jobs, models_count, benchmarks.
- **MCP proxy**: добавлены 5 HTTP-прокси через llmmcp → llmcore: `/llm/request`, `/dashboard`, `/costs/summary`, `/benchmarks`, `/discovery`.

### Tier 2 — Рефакторинг и Web UI

- **Рефакторинг core**: `main.go` (1856 строк) разбит на 5 пакетов:
  - `internal/config` — конфигурация и env-хелперы
  - `internal/models` — все типы данных
  - `internal/routing` — маршрутизация LLM-запросов, выбор устройств
  - `internal/limits` — лимиты устройств, валидация моделей
  - `internal/api` — HTTP-хендлеры, SSE, helpers
  - `cmd/core/main.go` — только инициализация (~115 строк)
- **Retry с backoff в worker**: `_post_json()` поддерживает 3 попытки с exponential backoff (1s/2s/4s), не ретраит 4xx (кроме 429).
- **LLM дашборд в telegram-mcp web UI**: новый тип страницы `llm` (`/p/llm`) — Job Queue, Costs, Fleet, Running Jobs, Models; автообновление каждые 5 сек; server-side fetch из llmcore.
- **Telemetry → alert-only**: вместо полного ASCII-дашборда каждые 2 сек → проверка каждые 30 сек, уведомления только при проблемах (device offline/recovery, queue stuck, job failed). Дедупликация алертов.

### Tier 3 — Производительность

- **LISTEN/NOTIFY для SSE**: PostgreSQL trigger `trg_job_status_notify` уведомляет при смене статуса задачи; `handleJobStream` использует `LISTEN job_update` + `WaitForNotification` вместо polling 1/сек. Fallback poll 15 сек на случай пропущенного notify.
- **CTE в handleWorkerClaim**: заменён correlated subquery на CTE `running_per_device` с LEFT JOIN — O(1) агрегация вместо O(N×M).
- **Discovery graceful fallback**: `tailscaleAvailable()` проверяет наличие binary через `LookPath`; при отсутствии Tailscale — warning + продолжение (subnet scan работает независимо). Лог `tailscale=true/false`.

### Исправления

- Дефолт `CORE_HTTP_URL` в MCP-адаптере: `llm-core` → `llmcore` (соответствует имени контейнера).
- `CORE_HTTP_URL` добавлен в compose.yml для llmmcp.
- Миграция `03_notify_trigger.sql` с триггером и функцией `notify_job_status_change()`.

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

- `README.md` приведён к единому визуальному стилю:
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
