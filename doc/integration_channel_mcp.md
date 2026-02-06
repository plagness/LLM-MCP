# Интеграция: переход с прямого Ollama на llm-mcp

Цель: заменить прямые вызовы Ollama на единый слой llm‑mcp, сохранив функционал, но добавив очередь, ретраи, мониторинг, умный роутинг и облачный fallback.

---

## 1) Что меняется
### Было
- сервис напрямую дергал `/api/generate` или `/api/embeddings`.
- ошибки не централизованы.
- нет очереди и трекинга статуса.

### Станет
- сервис отправляет задачу в `llm-mcp`.
- получает `job_id`.
- отслеживает статус через `GET /v1/jobs/{id}` или SSE.
- получает результат и метрики.

---

## 2) Минимальная интеграция (HTTP)

### 2.1 Эмбеддинги
```python
import requests

payload = {
    "task": "embed",
    "provider": "auto",
    "prompt": text,
    "priority": 1,
}
resp = requests.post("http://llm-core:8080/v1/llm/request", json=payload, timeout=10)
job_id = resp.json()["job_id"]

# polling статуса
status = requests.get(f"http://llm-core:8080/v1/jobs/{job_id}").json()
if status.get("status") == "done":
    embedding = status["result"]["data"]["embedding"]
```

### 2.2 Генерация текста (локально или облако)
```python
payload = {
    "task": "chat",
    "provider": "auto",
    "prompt": prompt_text,
    "temperature": 0.3,
    "max_tokens": 256,
    "constraints": {"prefer_local": True}
}
resp = requests.post("http://llm-core:8080/v1/llm/request", json=payload, timeout=10)
job_id = resp.json()["job_id"]

status = requests.get(f"http://llm-core:8080/v1/jobs/{job_id}").json()
if status.get("status") == "done":
    content = status["result"]["data"].get("choices", [{}])[0].get("message", {}).get("content", "")
```

---

## 3) Универсальная функция submit_and_wait
Чтобы упростить код, можно сделать единую функцию:

```python
import time
import requests

CORE = "http://llm-core:8080"

def submit_and_wait(payload: dict, timeout_sec: int = 60):
    resp = requests.post(f"{CORE}/v1/llm/request", json=payload, timeout=10)
    job_id = resp.json()["job_id"]

    deadline = time.time() + timeout_sec
    while time.time() < deadline:
        status = requests.get(f"{CORE}/v1/jobs/{job_id}").json()
        if status.get("status") in ("done", "error"):
            return status
        time.sleep(1)

    raise TimeoutError("job timeout")
```

---

## 4) SSE для длинных задач
Если генерация может быть долгой:

```python
import requests

with requests.get(f"{CORE}/v1/jobs/{job_id}/stream", stream=True) as r:
    for line in r.iter_lines():
        if line:
            print(line.decode("utf-8"))
```

---

## 5) Вариант для channel‑mcp
### Локально было (условно)
```python
# old
resp = requests.post("http://ollama:11434/api/generate", json={"model":"...","prompt":prompt})
```

### Стало
```python
payload = {
  "task": "chat",
  "provider": "auto",
  "prompt": prompt,
  "temperature": 0.2,
  "max_tokens": 200,
  "constraints": {"prefer_local": True}
}
status = submit_and_wait(payload)
if status.get("status") == "done":
    content = status["result"]["data"]["choices"][0]["message"]["content"]
```

---

## 6) Ошибки и ретраи
- `jobs.status = error` означает, что попытки исчерпаны.
- `jobs.error` содержит причину ошибки.
- `max_attempts` задаётся в запросе.

---

## 7) Роутинг и ограничения
Можно управлять выбором провайдера:
```json
{
  "provider": "openai",
  "task": "chat",
  "prompt": "..."
}
```

Либо задать ограничение по задержке:
```json
{
  "task": "chat",
  "provider": "auto",
  "constraints": {"max_latency_ms": 2000}
}
```

---

## 8) Воркеры и масштабирование
- Можно запускать несколько воркеров для параллели.
- Можно разделять задачи через `WORKER_KINDS`.

---

## 9) MCP‑мост (если нужен)
Если в будущем понадобится использовать MCP‑инструменты:
- `POST http://llm-mcp:3333/submit`
- `GET http://llm-mcp:3333/jobs/{id}`
- `GET http://llm-mcp:3333/jobs/{id}/stream`

---

## 10) Результат
После миграции сервис получает:
- устойчивую очередь,
- централизованное логирование,
- автоматический выбор провайдера,
- возможность масштабирования.
