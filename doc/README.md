# Документация llm-mcp

## Содержание
- [Быстрый старт](#быстрый-старт)
- [Компоненты и роль каждого](#компоненты-и-роль-каждого)
- [Основной сценарий использования](#основной-сценарий-использования)
- [HTTP API (core)](#http-api-core)
- [Типы задач и payload](#типы-задач-и-payload)
- [MCP‑адаптер (HTTP‑мост)](#mcp-адаптер-http-мост)
- [gRPC (внутренний транспорт)](#grpc-внутренний-транспорт)
- [Умный роутинг](#умный-роутинг)
- [Бенчмарки](#бенчмарки)
- [Дискавери устройств (Tailscale)](#дискавери-устройств-tailscale)
- [База данных](#база-данных)
- [Масштабирование и параллель](#масштабирование-и-параллель)
- [Логи, мониторинг, отладка](#логи-мониторинг-отладка)
- [FAQ / устранение проблем](#faq--устранение-проблем)
- [Интеграция в другие сервисы](#интеграция-в-другие-сервисы)

---

## Быстрый старт
1) Скопировать env:
```bash
cp .env.example .env
```
2) Запустить:
```bash
docker compose -f compose.yml --env-file .env up -d
```

Проверка:
- `GET http://127.0.0.1:8080/health` (core)
- `GET http://127.0.0.1:3333/health` (mcp‑мост)

### Важно про Ollama
По умолчанию воркер ходит в Ollama на хосте через `host.docker.internal`:
```
OLLAMA_BASE_URL=http://host.docker.internal:11434
```
Если Ollama находится на другом устройстве, укажите соответствующий URL в `.env`.

### Основные переменные окружения
- `OPENAI_API_KEY`, `OPENROUTER_API_KEY` — ключи облачных провайдеров.
- `OPENAI_MODEL`, `OPENROUTER_MODEL` — модель по умолчанию для облака.
- `OLLAMA_MODEL`, `OLLAMA_EMBED_MODEL` — модели по умолчанию для Ollama.
- `WORKER_KINDS` — ограничение типов задач у воркера (через запятую).
- `WORKER_LEASE_SECONDS` — время аренды задачи воркером.
- `DISCOVERY_INTERVAL` — период автодискавери (сек), `0` = выкл.
- `OLLAMA_PORTS` — порты для мультипортового сканирования Ollama (по умолчанию `"11434"`). Через запятую: `"11434,11435"`. Каждый порт создаёт отдельную ноду (port device) привязанную к base host.
- `DEVICE_MAX_CONCURRENCY` — лимит параллельных задач на одно устройство (по умолчанию 1).
- `STRICT_MODEL_LIMITS` — строгая отсечка (если лимиты заданы, неизвестные метрики блокируются).
- `DEVICE_LIMITS_JSON` — JSON с лимитами по устройствам (см. пример ниже).
- `DEVICE_LIMITS_FILE` — путь до JSON файла с лимитами.
- `DEVICE_LIMITS_INTERVAL` — период авто‑применения лимитов (сек), `0` = выкл.

---

## Компоненты и роль каждого
- **core (Go)** — очередь, API, умный роутинг, дискавери, хранение результатов.
- **worker (Python)** — выполнение задач (Ollama/OpenAI/OpenRouter), бенчмарки.
- **mcp (TypeScript)** — HTTP‑мост для MCP‑сценариев, общается с core по gRPC.
- **postgres** — задачи, устройства, бенчмарки, каталог моделей, стоимости.

---

## Основной сценарий использования
1) Сервис отправляет «умный» запрос в `POST /v1/llm/request`.
2) Core выбирает провайдера и ставит задачу в очередь.
3) Worker забирает задачу, выполняет запрос и сохраняет результат.
4) Клиент читает статус и результат через `GET /v1/jobs/{id}` или SSE‑стрим.

Мини‑пример:
```bash
curl -X POST http://127.0.0.1:8080/v1/llm/request \
  -H 'content-type: application/json' \
  -d '{
    "task": "chat",
    "provider": "auto",
    "prompt": "Сформируй теги",
    "temperature": 0.2,
    "max_tokens": 200,
    "constraints": {"prefer_local": true}
  }'
```

---

## HTTP API (core)

### 1) Очередь
**POST /v1/jobs** — поставить задачу
```json
{
  "kind": "ollama.generate",
  "payload": {"prompt": "Привет"},
  "priority": 1,
  "source": "my-service",
  "max_attempts": 3,
  "deadline_at": "2026-02-05T12:00:00Z"
}
```
Ответ:
```json
{"job_id":"..."}
```

**GET /v1/jobs/{id}** — статус задачи

**GET /v1/jobs/{id}/stream** — SSE‑стрим статусов
```bash
curl -N http://127.0.0.1:8080/v1/jobs/{id}/stream
```

---

### 2) Умный запрос (роутинг)
**POST /v1/llm/request**
```json
{
  "task": "chat",
  "provider": "auto",
  "model": "gpt-4o-mini",
  "prompt": "Суммируй текст",
  "messages": [{"role":"user","content":"..."}],
  "temperature": 0.2,
  "max_tokens": 200,
  "priority": 1,
  "source": "channel-mcp",
  "constraints": {
    "prefer_local": true,
    "force_cloud": false,
    "max_latency_ms": 3000
  }
}
```
Ответ:
```json
{"job_id":"...","provider":"ollama","kind":"ollama.generate"}
```

Если `payload` указан явно — core не собирает тело запроса, а кладёт payload как есть.

---

### 3) Dashboard и мониторинг

**GET /v1/dashboard** — полный snapshot системы
```json
{
  "jobs":           {"queued": 0, "running": 1, "done": 180, "error": 2},
  "hosts":          [{"id": "host-1", "name": "host-1", "status": "online", "orchestration": "docker", "nodes": [...], "total_models": 3, "total_running": 1, "total_slots": 4}],
  "devices":        [...],
  "workers_online": 2,
  "issues":         ["Host 'host-2' offline (last seen 2h ago)"],
  "running_jobs":   [...],
  "costs":          {"today": 0.0012, "week": 0.0084, "month": 0.0312},
  "models_count":   33,
  "updated_at":     "2026-02-14T12:00:00Z"
}
```

`hosts[]` — иерархия Host→Node (физическая машина → Ollama-инстансы на портах).
`devices[]` — плоский список (backward compatibility для роутера и т.д.).
`issues[]` — проблемы на человеческом языке.

**GET /v1/costs/summary?period=day|week|month** — расходы по провайдерам
```json
{
  "period": "week",
  "total_cost_usd": 0.0084,
  "total_requests": 42,
  "by_provider": [
    {"provider": "openrouter", "cost_usd": 0.0082, "jobs": 3, "tokens_in": 500, "tokens_out": 1200},
    {"provider": "openai", "cost_usd": 0.0002, "jobs": 1, "tokens_in": 100, "tokens_out": 300}
  ]
}
```

---

### 4) Debug endpoints (диагностика для разработчика)

**GET /v1/debug/health** — глубокая диагностика всех компонентов
```json
{
  "status": "warning",
  "version": "2026.02.12",
  "checks": {
    "database": {"status": "ok", "latency_ms": 3, "connections": {"active": 2, "max": 10}},
    "queue":    {"status": "ok", "queued": 2, "running": 1, "stuck": 0},
    "hosts":    {"status": "warning", "total": 4, "online": 3, "offline": 1},
    "workers":  {"status": "ok", "online": 2, "capacity": 4}
  },
  "issues": ["Host 'host-2' offline (last seen 2h ago)"]
}
```
`status` = worst of all checks (ok < warning < error).

**GET /v1/debug/actions** — каталог всех API endpoint'ов с описаниями и curl-примерами.
Удобен для нового разработчика: "какие endpoint'ы есть и что они делают?"

**GET /v1/debug/capacity** — мощности кластера
```json
{
  "total_slots": 10,
  "used_slots": 3,
  "free_slots": 7,
  "utilization_pct": 30.0,
  "hosts": [{"name": "host-1", "status": "online", "slots": 4, "used": 2, "free": 2, "utilization_pct": 50.0}],
  "workers": {"online": 2, "total_capacity": 4}
}
```
Slots = `DEVICE_MAX_CONCURRENCY × количество нод на хосте`.

**POST /v1/debug/test** — smoke test всего pipeline
```json
{
  "status": "pass",
  "duration_ms": 1250,
  "results": [
    {"name": "db_ping", "status": "pass", "ms": 3, "detail": "PostgreSQL connected"},
    {"name": "db_read", "status": "pass", "ms": 5, "detail": "Read 4 devices, 24 models"},
    {"name": "ollama_ping", "status": "pass", "ms": 62, "detail": "3/4 hosts reachable"},
    {"name": "job_create", "status": "pass", "ms": 8, "detail": "Job abc12345 created and cleaned"}
  ],
  "issues": ["Host 'host-2' unreachable (timeout 2s)"]
}
```

---

### 5) Воркеры (для кастомных исполнителей)
**POST /v1/workers/register**
```json
{"worker":{"id":"worker-1","name":"my-worker","platform":"linux","arch":"arm64","host":"host","tags":{}}}
```

**POST /v1/workers/claim**
```json
{"worker_id":"worker-1","kinds":["ollama.generate"],"lease_seconds":60}
```

**POST /v1/workers/complete**
```json
{"worker_id":"worker-1","job_id":"...","result":{},"metrics":{}}
```

**POST /v1/workers/fail**
```json
{"worker_id":"worker-1","job_id":"...","error":"...","metrics":{}}
```

**POST /v1/workers/heartbeat**
```json
{"worker_id":"worker-1","job_id":"...","extend_seconds":60}
```

---

### 6) Бенчмарки
**POST /v1/benchmarks/run**
```json
{
  "provider": "ollama",
  "model": "llama3.2:1b",
  "task_type": "generate",
  "prompt": "test",
  "runs": 1,
  "device_id": "your-device.ts.net."
}
```

**GET /v1/benchmarks?limit=20** — последние результаты

---

### 7) Дискавери
**POST /v1/discovery/run** — разовый запуск

**GET /v1/discovery/last** — время последнего запуска

---

### 8) Устройства (сигналы от воркеров)
**POST /v1/devices/offline** — пометить устройство как offline (например, при ошибке сети)
```json
{"device_id":"your-device.ts.net.","reason":"connection refused"}
```

---

## Типы задач и payload

### `ollama.generate`
Payload:
```json
{"model":"llama3.2:1b","prompt":"...","options":{},"ollama_addr":"100.x.x.x","ollama_host":"your-host.ts.net"}
```
Результат: `result.data` содержит raw‑ответ Ollama `/api/generate`.

### `ollama.embed`
Payload:
```json
{"model":"nomic-embed-text","prompt":"...","ollama_addr":"100.x.x.x","ollama_host":"your-host.ts.net"}
```
Результат: `result.data.embedding` (формат Ollama `/api/embeddings`).

### `openai.chat`
Payload:
```json
{"model":"gpt-4o-mini","messages":[{"role":"user","content":"..."}],"temperature":0.2,"max_tokens":200}
```

### `openrouter.chat`
Payload:
```json
{"model":"google/gemini-2.0-flash","messages":[...],"temperature":0.3,"max_tokens":200}
```

### `benchmark.ollama.generate` / `benchmark.ollama.embed`
Payload идентичен соответствующей Ollama‑задаче.
Результат: `tokens_in`, `tokens_out`, `latency_ms`, `tps`.

### `echo`
Payload возвращается как есть.

---

## MCP‑адаптер (HTTP‑мост)
Пока MCP реализован как HTTP‑мост, который проксирует запросы в gRPC core:

- `POST /submit` — поставить задачу
- `GET /jobs/{id}` — получить статус
- `GET /jobs/{id}/stream` — SSE‑стрим

Пример:
```bash
curl -X POST http://127.0.0.1:3333/submit \
  -H 'content-type: application/json' \
  -d '{"kind":"echo","payload":{"via":"mcp"},"priority":2}'
```

Этот мост удобно использовать как инструмент для других LLM: модель может отправлять JSON‑payload в `/submit` и читать результат через `/jobs/{id}`.

---

## gRPC (внутренний транспорт)
Контракт находится в `proto/llm.proto`.
Основные методы: `SubmitJob`, `GetJob`, `StreamJob`, `RegisterWorker`, `ClaimJob`, `CompleteJob`, `FailJob`, `Heartbeat`, `ReportMetrics`, `ReportBenchmark`.

gRPC-сервер поддерживает reflection — можно использовать `grpcurl`:
```bash
grpcurl -plaintext localhost:9090 list
grpcurl -plaintext localhost:9090 describe llmmcp.v1.Core
grpcurl -plaintext -d '{"job_id":"abc"}' localhost:9090 llmmcp.v1.Core/GetJob
```

Псевдокод (Python):
```python
channel = grpc.insecure_channel("llmcore:9090")
stub = llm_pb2_grpc.CoreStub(channel)
resp = stub.SubmitJob(llm_pb2.SubmitJobRequest(
  kind="ollama.generate",
  payload_json='{"prompt":"Привет"}',
  priority=1
))
```

---

## Умный роутинг

### Smart Routing по quality (v3)

Разработчику не нужно знать имена моделей. Единственный параметр `quality` определяет модель и провайдера:

```json
POST /v1/llm/request
{
  "task": "chat",
  "quality": "standard",
  "prompt": "Напиши функцию сортировки",
  "source": "my-service"
}
```

#### 6 уровней quality

| # | quality | Модели (2026) | Таймаут |
|---|---------|--------------|---------|
| 1 | `turbo` | tiny local ≤1B | 15с |
| 2 | `economy` | small local 1.5-2B | 30с |
| 3 | `standard` | medium local 3-4B | 60с |
| 4 | `premium` | large local 7-8B или cloud fast | 90с |
| 5 | `ultra` | xl local 14B+ или cloud fast | 120с |
| 6 | `max` | лучший cloud (Claude/GPT) | 180с |

#### Контекстно-зависимый роутинг (3 бакета)

Router адаптирует tier в зависимости от длины промпта (оценка через `len(prompt)/4`):

| quality | ≤4K tokens | 4K-32K tokens | >32K tokens |
|---------|-----------|--------------|------------|
| `turbo` | tiny | tiny, small | cloud |
| `economy` | tiny, small | small, medium | cloud |
| `standard` | small, medium | medium, large | cloud |
| `premium` | medium, large | large, xl | cloud |
| `ultra` | large, xl | xl | cloud |
| `max` | cloud | cloud | cloud |

Если tier'ов нет (>32K или `max`) — fallback в облако:

| quality | Cloud fallback tiers |
|---------|---------------------|
| `turbo`/`economy`/`standard` | cloud_economy |
| `premium` | cloud_economy, cloud_premium |
| `ultra`/`max` | cloud_premium, cloud_economy |

#### Thinking-модели

```json
{ "task": "reason", "quality": "premium", "thinking": true, "prompt": "Почему небо голубое?" }
```

`thinking` (bool): `null` (auto) = router решает по модели, `true` = ответ + цепочка рассуждений, `false` = только ответ.

Модели с поддержкой thinking (распознаются автоматически по имени):
- `qwen3` — все размеры
- `deepseek-r1` — все размеры
- `phi4-reasoning`
- `lfm2.5-thinking`
- `deepscaler`
- `exaone-deep`

Thinking-парсинг: worker ищет `<think>...</think>` теги в ответе Ollama, разделяя на `response` (основной ответ) и `thinking` (рассуждения).

#### Формат ответа — раздельные блоки + стоимость

```json
{
  "job_id": "abc-123",
  "status": "done",
  "result": {
    "response": "Небо голубое из-за рэлеевского рассеяния.",
    "thinking": "Давайте разберёмся...",
    "model": "qwen3:1.7b",
    "provider": "ollama",
    "device_id": "host-1",
    "tier": "small",
    "tokens_in": 12,
    "tokens_out": 85,
    "cost": "0.00000$"
  }
}
```

- `result.response` — всегда, `result.thinking` — только если `thinking: true` и модель поддерживает.
- `cost` — строка формата `"X.XXXXX$"` (точность $0.00001). Для local моделей = `"0.00000$"`.

#### Расчёт стоимости

Worker рассчитывает cost из полей `_price_in_1m` и `_price_out_1m` в payload (цена за 1M токенов). Core заполняет эти поля из таблицы `model_pricing` при создании job. Стоимость возвращается в `result.cost`. Cloud-расходы записываются в таблицу `llm_costs` через `RecordCost` handler.

#### Circuit Breaker (local → cloud fallback)

Если локальная модель 3 раза подряд не выполнила задачу (таймаут / ошибка):
1. Устройство помечается как "degraded" на 5 минут.
2. Задачи перенаправляются в облако.
3. Через 5 минут одна задача идёт на probe — если ОК → устройство снова active.

Dashboard показывает circuit status каждого устройства.

#### Рейтинг устройств

Dashboard включает статистику за 7 дней для каждого устройства:
- `success_rate` — % успешных задач
- `avg_latency_ms` — среднее время выполнения
- `total_jobs_7d`, `done_jobs_7d` — количество задач

#### Обратная совместимость

Старый формат `{"model":"qwen3:1.7b","provider":"ollama",...}` работает без изменений.
`cost` добавляется в ответ автоматически.

### Классический роутинг (provider=auto)

Если `quality` не указан, используется классическая логика:
1) `task=embed` → `ollama`.
2) `force_cloud=true` → OpenRouter → OpenAI → Ollama.
3) `prefer_local=true` + есть локальная Ollama → Ollama.
4) Иначе: OpenRouter → OpenAI → Ollama.
5) Если задан `max_latency_ms` и есть бенчмарки — Ollama используется только если укладывается в лимит.

Если `provider` задан явно — роутинг не меняет выбор.

Для `ollama` core может выбрать устройство автоматически:
если указан `model`, выбирается онлайн‑устройство с лучшим `tps`, и в payload
добавляются `ollama_addr` и `ollama_host`.

---

## Бенчмарки
Worker автоматически сохраняет метрики:
- `latency_ms`
- `tokens_in`, `tokens_out`
- `tps` (tokens per second)

Бенчмарки нужны для маршрутизации и оценки производительности узлов.

---

## Дискавери устройств (Tailscale)
Core выполняет `tailscale status --json`, обновляет таблицу `devices` и пытается определить наличие Ollama на узлах.

### Мультипортовое сканирование

Переменная `OLLAMA_PORTS` (по умолчанию `"11434"`) задаёт список портов через запятую. Для каждого нестандартного порта создаётся отдельный port device:

```
host-1 (base device)
├── host-1:11434 (port device, tags: {port_device: true, base_device: "host-1", ollama_port: "11434"})
└── host-1:11435 (port device, tags: {port_device: true, base_device: "host-1", ollama_port: "11435"})
```

Каждый port device имеет свой набор моделей, latency и circuit breaker. Router работает на уровне port device — т.е. выбирает конкретный порт на конкретном хосте.

### Автоопределение tier/thinking/context

При discovery каждая модель автоматически получает:
- `tier` — по размеру параметров (tiny ≤1B, small ≤3B, medium ≤7B, large ≤13B, xl >13B)
- `thinking` — true если имя содержит qwen3, deepseek-r1, phi4-reasoning и др.
- `context_k` — по модели (4K, 8K, 32K, 128K)
- `kind` — chat/embed по имени модели

В `compose.yml` уже добавлены монтирования:
- `/usr/bin/tailscale` (binary)
- `/var/run/tailscale/tailscaled.sock` (socket)

Если путь бинарника/сокета отличается — измените `compose.yml`.

---

## База данных

### Основные таблицы

| Таблица | Назначение |
|---------|-----------|
| `jobs` | Очередь задач (status, payload, result, attempts) |
| `job_attempts` | История попыток выполнения (worker_id, started_at, metrics) |
| `devices` | Устройства/workers (id, name, platform, tags JSONB, status) |
| `device_models` | Связь устройство↔модель (device_id, model_id, available) |
| `device_limits` | Лимиты по устройствам (ram_gb, max_params_b, allow_models) |
| `models` | Каталог моделей (id, provider, tier, thinking, context_k, kind) |
| `model_pricing` | Цены за 1M токенов (provider, model_id, price_in_1m, price_out_1m) |
| `benchmarks` | Результаты бенчмарков (device_id, model_id, latency_ms, tps) |
| `llm_costs` | Расходы по запросам (model_id, provider, tokens_in/out, cost_usd) |

### SQL Views

**`v_device_stats`** — статистика устройств за 7 дней:
```sql
SELECT device_id, total_jobs_7d, done_jobs_7d, success_rate, avg_latency_ms
FROM v_device_stats;
```

**`v_cost_stats`** — дневная сводка расходов:
```sql
SELECT date, model_id, provider, requests, total_tokens_in, total_tokens_out, total_cost_usd
FROM v_cost_stats;
```

---

## Масштабирование и параллель
- Можно запускать несколько воркеров: `docker compose up -d --scale llmworker=3`.
- Воркеры можно запускать на других машинах, указав `CORE_GRPC_ADDR`.
- `WORKER_KINDS` позволяет разделить роли (например, один воркер только для embed).
- `DEVICE_MAX_CONCURRENCY` ограничивает параллельные задачи на устройство (по умолчанию 1). Router не отправит на устройство больше задач, чем разрешено.

Очередь устойчива к рестартам: задача вернётся в очередь после истечения lease.

---

## Логи, мониторинг, отладка

### Логи
```bash
docker compose logs -f llmcore llmworker llmmcp
# K8s:
kubectl logs -n ns-llm -l app=llmcore -f
kubectl logs -n ns-llm -l app=llmworker -f
```

### Мониторинг
- **Dashboard**: `GET /v1/dashboard` — полный snapshot (jobs, hosts, costs, issues)
- **SSE‑стрим**: `GET /v1/jobs/{id}/stream`
- **Deep health**: `GET /v1/debug/health` — проверка БД, очереди, хостов, workers
- **Capacity**: `GET /v1/debug/capacity` — слоты, загрузка, утилизация
- **Smoke test**: `POST /v1/debug/test` — автоматическая проверка всего pipeline
- **API каталог**: `GET /v1/debug/actions` — все endpoint'ы с описаниями и примерами

### Телеметрия (llmtelemetry)
Сервис `llmtelemetry` работает в alert-only режиме: проверяет состояние системы каждые 30 секунд и отправляет в Telegram только при обнаружении проблем:
- Устройство перешло offline / восстановилось
- Очередь застряла (queued > 0, running = 0)
- Задача провалилась N раз (порог `ALERT_FAIL_THRESHOLD`, по умолчанию 3)

Конфигурация:
- `TELEMETRY_CHECK_INTERVAL` — интервал проверки в секундах (по умолчанию 30).
- `ALERT_FAIL_THRESHOLD` — порог ошибок перед алертом (по умолчанию 3).
- Маршрут: через `telegram-mcp` (`TELEGRAM_USE_MCP=1`, `TELEGRAM_MCP_BASE_URL`, `TELEGRAM_MCP_BOT_ID`, `TELEGRAM_MCP_CHAT_ID`).
- Fallback: прямой Telegram (`TELEGRAM_MCP_FALLBACK_DIRECT=1`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID`).

### Быстрые команды для отладки
```bash
# Полный снимок
curl http://localhost:8080/v1/dashboard | jq .

# Что сломалось?
curl http://localhost:8080/v1/debug/health | jq .issues

# Мощности кластера
curl http://localhost:8080/v1/debug/capacity | jq .

# Smoke test
curl -X POST http://localhost:8080/v1/debug/test | jq .results

# Все доступные API
curl http://localhost:8080/v1/debug/actions | jq '.endpoints[].path'

# Принудительный discovery
curl -X POST http://localhost:8080/v1/discovery/run

# Расходы за неделю
curl 'http://localhost:8080/v1/costs/summary?period=week' | jq .

# gRPC reflection
grpcurl -plaintext localhost:9090 list llmmcp.v1.Core
```

---

## FAQ / устранение проблем
**Нужно строго блокировать тяжёлые модели на слабом железе**
- Используйте таблицу `device_limits` (см. `db/init/01_core.sql`).
- Лимиты задаются на устройство: `max_params_b`, `max_size_gb`, `max_context_k`.
- Можно сделать allow/deny список: `allow_models`, `deny_models` (JSON массив строк).
- При постановке задач на конкретное устройство будет проверка этих лимитов.
- Можно применять лимиты автоматически через `DEVICE_LIMITS_JSON` или `DEVICE_LIMITS_FILE`.

Пример `DEVICE_LIMITS_JSON`:
```json
{
  "*": {"ram_gb": 16},
  "device-a.ts.net.": {"ram_gb": 8},
  "your-device.ts.net.": {"vram_gb": 16, "allow_models": ["llama3.2:3b", "qwen2.5:3b"]},
  "device-c.ts.net.": {"ram_gb": 16, "deny_models": ["qwen3-vl:latest"]}
}
```
Правило авто‑подбора:
- 8GB → max_params_b=5, max_context_k=4096
- 16GB → max_params_b=12, max_context_k=8192
- >16GB → max_params_b≈0.75*mem, max_context_k=16384

**Ollama не доступна из воркера**
- Убедитесь, что `OLLAMA_BASE_URL` доступен из контейнера.
- Для хоста используйте `http://host.docker.internal:11434`.

**Discovery не работает**
- Проверьте наличие `/var/run/tailscale/tailscaled.sock`.
- Убедитесь, что бинарник `tailscale` доступен внутри контейнера.

**Задачи зависли в running**
- Воркеры могли упасть. После истечения lease задача вернётся в очередь.
- Быстрая диагностика: `curl http://localhost:8080/v1/debug/health | jq .checks.queue.stuck`

**Dashboard показывает много устройств**
- Каждый Ollama-порт отображается как отдельная нода внутри хоста.
- Workers не отображаются в Fleet — видны отдельно как "Workers: N online".
- Используйте `GET /v1/debug/capacity` для понимания реальных мощностей.

---

## Roadmap

### P2P Model Distribution (планируется)
Система торрент-подобной подкачки моделей между хостами кластера:

- **Hub-нода** (pi5) хранит все модели, включая тяжёлые
- При появлении нового VDS — запрос на модель отправляется в систему, и скачивание идёт с хостов, где модель уже есть (параллельно с нескольких нод)
- **Auto-eviction**: если модель долго не используется (по статистике задач, процентному соотношению), она выгружается с VDS. Hub-нода всегда хранит полную копию
- Преимущество: не зависим от внешних реестров (ollama.com), трафик идёт по Tailscale mesh

### Auto-scaling моделей
- Мониторинг утилизации: если модель загружена >80% на всех нодах, автоматическая подкачка на свободные VDS
- Если модель не используется >7 дней и не на hub-ноде — выгрузка для освобождения RAM

---

## Интеграция в другие сервисы
См. подробный гайд: `doc/integration_channel_mcp.md`
