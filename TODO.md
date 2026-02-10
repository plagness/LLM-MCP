# TODO — llm-mcp Multi-Ollama + Distributed Compute

## Статус: In Progress

llm-mcp поддерживает 3 режима деплоя:
1. **Standalone** — 1 контейнер + нативный Ollama
2. **Compose** — несколько Ollama контейнеров (profiles)
3. **K3s cluster** — Ollama pods на remote agent nodes

## Выполнено

### Multi-port Discovery
- [x] `OLLAMA_PORTS` env var для probe нескольких портов (default: 11434)
- [x] `probeOllamaPort()` — probe конкретного порта
- [x] `ensurePortDevice()` — создание device entry для non-default портов
- [x] `ollama_addr` хранит `ip:port` формат
- [x] Subnet scan с multi-port поддержкой
- [x] Файл: `core/internal/discovery/discovery.go`

### Worker addr:port
- [x] `_split_host_port()` — разделение addr на host и port
- [x] `_resolve_ollama_base()` — корректная обработка `ip:port` формата
- [x] Backward compat: addr без порта → default 11434
- [x] Файл: `worker/llm_worker/main.py`

### Compose Ollama profiles
- [x] Profile `ollama` — 1 инстанс (:11434)
- [x] Profile `ollama-multi` — 3 инстанса (:11434, :11435, :11436)
- [x] Изолированные volumes для каждого инстанса
- [x] `OLLAMA_PORTS` env в llmcore
- [x] Файл: `compose.yml`

### K8s манифесты Ollama
- [x] `ollama-deployment.yaml` — Deployment с nodeSelector, health probes
- [x] `ollama-service.yaml` — NodePort 31434
- [x] Kustomization: опционально (закомментировано)
- [x] Файлы: `k8s/ollama-deployment.yaml`, `k8s/ollama-service.yaml`

### Device limits для удалённых нод
- [x] Per-device RAM limits в DEVICE_LIMITS_JSON
- [x] DISCOVERY_INTERVAL для периодического сканирования
- [x] Файлы: `.env`, `.env.example`

### Remote node setup
- [x] SSH доступ к удалённым нодам через Tailscale
- [x] Ollama установлен на ноды (CPU-only)
- [x] `OLLAMA_HOST=0.0.0.0` для remote access
- [x] Discovery видит удалённые ноды с моделями

## В ожидании

### K3s cluster
- [ ] K3s server на master node (cgroup_memory ребут)
- [ ] K3s agents на удалённых нодах через Tailscale
- [ ] Ollama pods на agent nodes
- [ ] Миграция: systemd Ollama → K8s pod

### Улучшения
- [ ] Per-port device limits (наследование от parent device)
- [ ] Auto-pull моделей при старте Ollama контейнера (entrypoint script)
- [ ] Grafana dashboard: devices, models, queue depth
- [ ] Benchmarks для remote devices (автоматические при первом запросе)
- [ ] Pull дополнительных моделей на ноды с достаточным RAM
