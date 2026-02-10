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
- **postgres** — задачи, устройства, бенчмарки, каталог моделей.

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

### 3) Воркеры (для кастомных исполнителей)
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

### 4) Бенчмарки
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

### 5) Дискавери
**POST /v1/discovery/run** — разовый запуск

**GET /v1/discovery/last** — время последнего запуска

---

### 6) Устройства (сигналы от воркеров)
**POST /v1/devices/offline** — пометить устройство как offline (например, при ошибке сети)
```json
{"device_id":"your-device.ts.net.","reason":"connection refused"}
```

---

## Типы задач и payload

### `ollama.generate`
Payload:
```json
{"model":"llama3.2:1b","prompt":"...","options":{},"ollama_addr":"100.64.0.1","ollama_host":"your-device.ts.net"}
```
Результат: `result.data` содержит raw‑ответ Ollama `/api/generate`.

### `ollama.embed`
Payload:
```json
{"model":"nomic-embed-text","prompt":"...","ollama_addr":"100.64.0.1","ollama_host":"your-device.ts.net"}
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
Текущая логика `provider=auto`:
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

В `compose.yml` уже добавлены монтирования:
- `/usr/bin/tailscale` (binary)
- `/var/run/tailscale/tailscaled.sock` (socket)

Если путь бинарника/сокета отличается — измените `compose.yml`.

---

## Масштабирование и параллель
- Можно запускать несколько воркеров: `docker compose up -d --scale llmworker=3`.
- Воркеры можно запускать на других машинах, указав `CORE_GRPC_ADDR`.
- `WORKER_KINDS` позволяет разделить роли (например, один воркер только для embed).

Очередь устойчива к рестартам: задача вернётся в очередь после истечения lease.

---

## Логи, мониторинг, отладка
- `docker compose logs -f llmcore llmworker llmmcp`
- SSE‑стрим: `GET /v1/jobs/{id}/stream`
- Статусы задач: `GET /v1/jobs/{id}`
- Telegram‑телеметрия: сервис `llmtelemetry` редактирует одно сообщение и каждые ~2 секунды показывает очередь, бенчмарки, устройства и нагрузку.
  - Рекомендуемый маршрут: через `telegram-mcp` (`TELEGRAM_USE_MCP=1`, `TELEGRAM_MCP_BASE_URL`, `TELEGRAM_MCP_BOT_ID`, `TELEGRAM_MCP_CHAT_ID`).
  - Default URL маршрута: `http://tgapi:8000`; при неявной конфигурации сохранён compat retry на `http://telegram-api:8000` (1 релиз).
  - Fallback: прямой Telegram (`TELEGRAM_MCP_FALLBACK_DIRECT=1`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID`).
  - Частоту можно менять через `TELEGRAM_UPDATE_INTERVAL`.

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

---

## Интеграция в другие сервисы
См. подробный гайд: `doc/integration_channel_mcp.md`
