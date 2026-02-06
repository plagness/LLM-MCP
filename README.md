# llm-mcp

Единый роутер нейросетей и MCP‑набор инструментов с локальным и облачным выполнением,
устойчивой очередью, авто‑дискавери железа и богатой телеметрией.

Версионирование: **[год].[месяц].[версия]** (пример: `2026.02.1`).

## Зачем
- Привести все вызовы LLM к одному стандарту.
- Распределять задачи между локальными машинами (Ollama) и облаком (OpenRouter/OpenAI).
- Иметь устойчивую очередь с ретраями и продолжением после перезапуска.
- Считать стоимость, задержки, скорость, состояние железа.
- Дать MCP‑инструменты, чтобы другие LLM могли использовать локальные мощности.

## Архитектура (вариант A)
- **core (Go)** — API, очередь, маршрутизация, дискавери, стоимость/ETA, планировщик.
- **worker (Python)** — локальные вычисления, адаптеры, бенчмарки.
- **mcp (TypeScript)** — MCP‑адаптер (инструменты) → core API.
- **postgres** — единое хранилище очереди, устройств, моделей и результатов.

## Транспорт
- Внутри: **gRPC** (core ↔ worker, core ↔ mcp) — контракты уже в `proto/`, реализация в процессе.
- Снаружи: **HTTP/JSON + SSE** (стриминг статуса и результата).

## Очередь (почему Postgres)
- Задачи хранятся в БД и берутся через `FOR UPDATE SKIP LOCKED`.
- Воркеры берут lease на задачу; при падении lease истекает и задача переходит обратно в очередь.
- Работает без дополнительных брокеров и переживает рестарты.

## Дискавери и бенчмарки
- Дискавери через `tailscale status --json` (без изменения конфигурации).
- Проверяем доступность Ollama на узлах.
- Бенчмарки формируют профиль устройства и модели (latency, tps, стабильность).
> Для работы дискавери внутри контейнера нужен доступ к бинарнику `tailscale`.
> Если `tailscale` доступен только на хосте — запускайте дискавери с хоста или
> пробрасывайте бинарник внутрь контейнера.

## HTTP API (минимум, сейчас)
- `POST /v1/jobs` — поставить задачу в очередь.
- `GET /v1/jobs/{id}` — получить статус задачи.
- `GET /v1/jobs/{id}/stream` — SSE‑стрим по статусу.
- `POST /v1/workers/register` — регистрация воркера.
- `POST /v1/workers/claim` — получить задачу в работу.
- `POST /v1/workers/complete` — завершить задачу.
- `POST /v1/workers/fail` — ошибка выполнения.
- `POST /v1/workers/heartbeat` — продлить lease.
- `POST /v1/discovery/run` — разовый запуск дискавери через Tailscale.
- `GET /v1/discovery/last` — время последнего дискавери.

## gRPC (внутренний транспорт)
Методы совпадают по смыслу с HTTP, основной контракт в `proto/llm.proto`.

## Типы задач (MVP)
- `ollama.generate` — локальная генерация (Ollama `/api/generate`).
- `ollama.embed` — локальные эмбеддинги (Ollama `/api/embeddings`).
- `openai.chat` — Chat Completions (OpenAI).
- `openrouter.chat` — Chat Completions (OpenRouter).
- `echo` — технич. режим (возвращает payload).

## MCP‑адаптер (HTTP‑мост, пока)
MCP‑контейнер поднимает простой HTTP‑мост, который общается с core по gRPC:
- `POST /submit` — поставить задачу.
- `GET /jobs/{id}` — статус задачи.
- `GET /jobs/{id}/stream` — SSE‑стрим по статусу.

## Структура проекта
```
llm-mcp/
  core/         # Go API + router + queue
  worker/       # Python execution + adapters
  mcp/          # TypeScript MCP adapter
  db/init/      # SQL-инициализация
  proto/        # gRPC контракты
  compose.yml   # docker compose (вариант A)
```

## Быстрый старт (dev)
```
cd llm-mcp
cp .env.example .env
docker compose -f compose.yml --env-file .env up -d --build
```

Полезные env:
- `DISCOVERY_INTERVAL=0` — период автодискавери (секунды), 0 = выкл.
- `TELEGRAM_USE_MCP=1` — отправка телеметрии через `telegram-mcp`.
- `TELEGRAM_MCP_BASE_URL=http://tgapi:8000` — URL `tgapi`.
- `TELEGRAM_MCP_BOT_ID=<bot_id>` — явный мультибот-контекст.
- `TELEGRAM_MCP_CHAT_ID=<chat_id>` — целевой чат для телеметрии через `telegram-mcp`.
- `TELEGRAM_MCP_FALLBACK_DIRECT=1` — fallback на прямой Telegram при ошибке маршрута `telegram-mcp`.

## Roadmap (первые этапы)
1. Core API + устойчивая очередь.
2. Регистрация воркеров + claim/complete.
3. Адаптеры Ollama + OpenRouter + OpenAI.
4. Стриминг статусов + MCP‑инструменты.
5. Дискавери + бенчмарки + умная маршрутизация.

## Документация
См. `doc/README.md` и `doc/integration_channel_mcp.md`.
