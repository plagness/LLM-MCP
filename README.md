# LLM-MCP

[![Version](https://img.shields.io/badge/version-2026.02.11-blue.svg)](VERSION)
[![Runtime](https://img.shields.io/badge/runtime-go%20%2B%20python%20%2B%20node-green.svg)](compose.yml)
[![Queue](https://img.shields.io/badge/queue-postgres-orange.svg)](db/init)
[![Transport](https://img.shields.io/badge/transport-http%20%2B%20grpc-7a3cff.svg)](proto/llm.proto)

Единый роутер LLM-запросов и MCP-набор инструментов с устойчивой очередью,
автодискавери устройств и телеметрией в Telegram.

[![Quick Start](https://img.shields.io/badge/Quick%20Start-Open-1f6feb?style=for-the-badge)](#-быстрый-старт)
[![Architecture](https://img.shields.io/badge/Architecture-Open-1f6feb?style=for-the-badge)](#-архитектура)
[![Changelog](https://img.shields.io/badge/Changelog-Open-1f6feb?style=for-the-badge)](CHANGELOG.md)

## ✨ Возможности

### 🧠 Единый роутинг LLM
- Локальные задачи через Ollama (`ollama.generate`, `ollama.embed`).
- Облачные провайдеры: OpenAI, OpenRouter.
- Единый job lifecycle: submit -> claim -> heartbeat -> complete/fail.

### 🔍 Multi-Ollama Discovery
- Автодискавери устройств через Tailscale mesh.
- Multi-port probe (`OLLAMA_PORTS`) — несколько Ollama инстансов на одном хосте.
- Compose profiles: `ollama` (single), `ollama-multi` (3 инстанса).

### 🗂️ Устойчивая очередь
- Postgres-очередь с `FOR UPDATE SKIP LOCKED`.
- Lease-механика: задачи возвращаются в очередь после таймаута воркера.
- Состояние переживает перезапуск контейнеров.

### 📡 MCP + телеметрия
- `llmmcp` отдаёт MCP/HTTP bridge к core.
- `llmtelemetry` публикует статус/прогресс в Telegram.
- Поддержан route через `telegram-mcp` и direct fallback.

### ☸️ Kubernetes
- Полный набор K8s манифестов с Kustomize.
- Ollama Deployment с nodeSelector для привязки к нодам.
- Готов к развёртыванию на K3s кластере.

## 🧱 Архитектура

```text
┌──────────────────────┐      ┌──────────────────────┐
│ Clients / MCP Hosts  │─────▶│ llmmcp (Node.js)     │
│                      │      │ :3333                │
└──────────────────────┘      └──────────┬───────────┘
                                          │ gRPC/HTTP
                                  ┌───────▼───────────┐
                                  │ llmcore (Go API)  │
                                  │ :8080 / :9090     │
                                  └───────┬───────────┘
                                          │
                     ┌────────────────────▼────────────────────┐
                     │ llmdb (PostgreSQL)                      │
                     │ jobs, devices, models, telemetry state  │
                     └──────────────────────────────────────────┘
                               ▲                   ▲
                               │                   │
                        ┌──────┴──────┐      ┌─────┴─────────┐
                        │ llmworker   │      │ llmtelemetry  │
                        │ execution   │      │ tg updates    │
                        └─────────────┘      └───────────────┘
```

| Компонент | Порт | Назначение |
|---|---|---|
| `llmcore` | `8080` | HTTP API для job-операций |
| `llmcore` | `9090` | внутренний gRPC transport |
| `llmmcp` | `3333` | MCP/HTTP bridge |
| `llmdb` | `5435` | очередь и состояние |

## 🚀 Быстрый старт

```bash
cd llm-mcp
cp .env.example .env

docker compose -f compose.yml --env-file .env up -d --build
```

Проверка:

```bash
curl -fsS http://127.0.0.1:8080/health || true
curl -fsS http://127.0.0.1:3333/health || true
```

## 🔌 Минимальные API точки (MVP)

- `POST /v1/jobs` — постановка задачи.
- `GET /v1/jobs/{id}` — статус.
- `GET /v1/jobs/{id}/stream` — SSE статус/прогресс.
- `POST /v1/workers/register|claim|complete|fail|heartbeat` — протокол worker.
- `POST /v1/discovery/run` — ручной запуск discovery.

## 🔧 Ключевые переменные окружения

- `PORT_DB_LLM=5435`, `PORT_HTTP_LLMCORE=8080`, `PORT_GRPC_LLMCORE=9090`, `PORT_MCP_LLM=3333`.
- `OLLAMA_BASE_URL`, `OLLAMA_MODEL`, `OLLAMA_EMBED_MODEL`.
- `OPENAI_API_KEY`, `OPENROUTER_API_KEY`.
- `TELEGRAM_USE_MCP`, `TELEGRAM_MCP_BASE_URL`, `TELEGRAM_MCP_BOT_ID`, `TELEGRAM_MCP_CHAT_ID`.
- `TELEGRAM_MCP_FALLBACK_DIRECT=1` для отказоустойчивого маршрута телеметрии.
- Если `TELEGRAM_MCP_BASE_URL` не задан, используется `http://tgapi:8000`; на 1 релиз включён legacy retry к `http://telegram-api:8000`.

## 📚 Документация

- `doc/README.md` — полный справочник по core/worker/mcp/telemetry.
- `doc/integration_channel_mcp.md` — интеграция с `channel-mcp`.
- `proto/llm.proto` — внутренний gRPC контракт.

## 📁 Структура

```text
llm-mcp/
├── core/           # Go API/router/queue/discovery
├── worker/         # Python execution adapters
├── telemetry/      # Telegram telemetry sender
├── mcp/            # MCP adapter (Node.js)
├── planner/        # Фоновые процессы (sync, cleanup, benchmarks)
├── db/init/        # SQL init + миграции
├── k8s/            # Kubernetes манифесты
├── proto/          # gRPC contracts
├── scripts/        # Утилиты (sync моделей, probe)
├── config/         # Курированные конфиги
└── compose.yml
```

## 🧭 Public Git Standards

- Версия хранится в `VERSION` в формате `YYYY.MM.x`.
- Все изменения описываются в `CHANGELOG.md`.
- Секреты и токены не попадают в git; используется только `.env.example`.
- Перед merge обязательны `docker compose config` и минимальный smoke по health/API.
