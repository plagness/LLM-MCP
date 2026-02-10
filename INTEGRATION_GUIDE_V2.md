# Руководство по интеграции Planner в Core

## Шаг 1: Добавить импорты в core/cmd/core/main.go

```go
import (
	// ... существующие импорты ...

	// Добавить:
	"llm-mcp/planner/internal/cleanup"
	"llm-mcp/planner/internal/sync"
)
```

## Шаг 2: Запустить фоновые процессы planner

В функции `main()`, после создания `pool`, добавить:

```go
// После строки: pool, err := pgxpool.New(ctx, dsn)

// Запуск planner фоновых процессов
// Автосинхронизация моделей OpenRouter
sync.StartOpenRouterSync(pool)

// Автоочистка старых jobs
cleanup.StartJobCleanup(pool)
```

## Шаг 3: Добавить проверку цены перед бенчмарком

В функции `handleBenchmarkRun`, перед постановкой задачи добавить:

```go
// После строки: kind := "benchmark.ollama." + task

// Проверка цены модели (защита от дорогих бенчмарков)
if provider == "ollama" && req.Model != "" {
	allowed, reason, err := sync.CheckBenchmarkAllowed(ctx, s.db, req.Model, task)
	if err != nil {
		log.Printf("benchmark price check error: %v", err)
	} else if !allowed {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "benchmark_blocked", "reason": reason})
		return
	}
}
```

## Шаг 4: Применить миграцию БД

Выполнить миграцию:

```bash
cd /path/to/llm-mcp
docker compose exec llmdb psql -U llm -d llm_mcp -f /docker-entrypoint-initdb.d/02_v2_improvements.sql
```

Или скопировать `db/migrations/02_v2_improvements.sql` в `db/init/` и пересоздать контейнер.

## Шаг 5: Обновить .env

Добавить новые переменные в `.env`:

```bash
# Planner - автосинхронизация OpenRouter
OPENROUTER_SYNC_INTERVAL=24
OPENROUTER_TOP_N=100
OPENROUTER_MAX_PRICE_PER_1M=100.0
OPENROUTER_SYNC_ON_STARTUP=1

# Planner - очистка jobs
JOB_CLEANUP_INTERVAL=6
JOB_CLEANUP_RETENTION_DAYS=7

# Защита от дорогих бенчмарков
BENCHMARK_MAX_PRICE_PER_1M=10.0
```

## Шаг 6: Пересобрать и запустить

```bash
docker compose down
docker compose build llmcore
docker compose up -d
```

## Проверка работы

1. **Проверить логи синхронизации:**
```bash
docker compose logs -f llmcore | grep "planner/sync"
```

Должно быть:
```
planner/sync: running initial OpenRouter sync
planner/sync: fetched 500 models from OpenRouter
planner/sync: filtered to 150 models (price <= 100.00)
planner/sync: selected top 100 models
planner/sync: synced 100/100 models
planner/sync: OpenRouter sync started (interval=24h)
```

2. **Проверить очистку jobs:**
```bash
docker compose logs -f llmcore | grep "planner/cleanup"
```

3. **Проверить offline handling:**
```bash
docker compose logs -f llmcore | grep "offline"
```

## Откат (если что-то пошло не так)

```bash
# Удалить новые таблицы
docker compose exec llmdb psql -U llm -d llm_mcp -c "DROP TABLE IF EXISTS llm_costs CASCADE;"

# Вернуться на предыдущую версию
git checkout <previous-commit>
docker compose down && docker compose build && docker compose up -d
```

## Тестирование

### Тест 1: Синхронизация моделей

```bash
# Проверить количество моделей в БД
docker compose exec llmdb psql -U llm -d llm_mcp -c "SELECT provider, COUNT(*) FROM models GROUP BY provider;"
```

Должно быть примерно:
```
 provider   | count
------------+-------
 ollama     |    15
 openrouter |   100
```

### Тест 2: Защита от дорогих бенчмарков

```bash
# Попытка бенчмарка дорогой модели (должна заблокироваться)
curl -X POST http://127.0.0.1:8080/v1/benchmarks/run \
  -H 'Content-Type: application/json' \
  -d '{
    "provider": "ollama",
    "model": "openai/gpt-4",
    "task_type": "generate",
    "prompt": "test",
    "runs": 1
  }'
```

Ожидаемый ответ:
```json
{"error":"benchmark_blocked","reason":"model too expensive for benchmark..."}
```

### Тест 3: Offline handling

```bash
# Остановить одно устройство и запустить discovery
curl -X POST http://127.0.0.1:8080/v1/discovery/run
```

Проверить логи:
```bash
docker compose logs llmcore | grep "offline"
```

## Дальнейшие улучшения (опционально)

1. **Добавить Prometheus metrics** для мониторинга planner
2. **Реализовать Web UI** для просмотра статистики синхронизации
3. **Добавить rate limiting** для защиты API
4. **Вынести planner в отдельный контейнер** (в будущем)
