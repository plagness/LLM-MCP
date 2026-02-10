# LLM-MCP Kubernetes Deployment

Kubernetes манифесты для развёртывания модуля llm-mcp в кластере K8s.

## Структура файлов

1. **namespace.yaml** - создание namespace `ns-llm`
2. **secret.yaml** - секреты для паролей и API ключей
3. **configmap.yaml** - конфигурация приложений
4. **llmdb-pvc.yaml** - PersistentVolumeClaim для PostgreSQL (10Gi)
5. **llmdb-statefulset.yaml** - StatefulSet для PostgreSQL 16
6. **llmdb-service.yaml** - ClusterIP (5432) + NodePort (30435)
7. **llmcore-deployment.yaml** - Deployment для Go-сервиса (HTTP + gRPC)
8. **llmcore-service.yaml** - ClusterIP (8080+9090) + NodePort (30080)
9. **llmworker-deployment.yaml** - Deployment для Python worker
10. **llmtelemetry-deployment.yaml** - Deployment для телеметрии
11. **llmmcp-deployment.yaml** - Deployment для Node.js MCP-сервера
12. **llmmcp-service.yaml** - ClusterIP (3333) + NodePort (30333)
13. **kustomization.yaml** - Kustomize манифест для управления ресурсами

## Порты

### Internal (ClusterIP)
- llmdb: 5432
- llmcore: 8080 (HTTP), 9090 (gRPC)
- llmmcp: 3333

### External (NodePort)
- llmdb-external: 30435
- llmcore-external: 30080
- llmmcp-external: 30333

## Предварительные требования

1. **Образы Docker** должны быть доступны в кластере:
   - llm-mcp-llmcore:latest
   - llm-mcp-llmworker:latest
   - llm-mcp-llmtelemetry:latest
   - llm-mcp-llmmcp:latest

2. **Ollama** должен быть запущен на хост-машине на порту 11434

3. **Tailscale** должен быть установлен на хост-машине:
   - `/usr/bin/tailscale`
   - `/var/run/tailscale/tailscaled.sock`

4. **Init scripts** для БД должны находиться в:
   - `/path/to/llm-mcp/db/init`

## Развёртывание

### Использование kubectl

```bash
# Применить все манифесты
kubectl apply -f /path/to/llm-mcp/k8s/

# Или применить в определённом порядке
kubectl apply -f namespace.yaml
kubectl apply -f secret.yaml
kubectl apply -f configmap.yaml
kubectl apply -f llmdb-pvc.yaml
kubectl apply -f llmdb-statefulset.yaml
kubectl apply -f llmdb-service.yaml
kubectl apply -f llmcore-deployment.yaml
kubectl apply -f llmcore-service.yaml
kubectl apply -f llmworker-deployment.yaml
kubectl apply -f llmtelemetry-deployment.yaml
kubectl apply -f llmmcp-deployment.yaml
kubectl apply -f llmmcp-service.yaml
```

### Использование Kustomize

```bash
# Применить через kustomize
kubectl apply -k /path/to/llm-mcp/k8s/

# Предварительный просмотр
kubectl kustomize /path/to/llm-mcp/k8s/
```

## Конфигурация секретов

Перед развёртыванием **ОБЯЗАТЕЛЬНО** отредактируйте `secret.yaml`:

```bash
kubectl edit secret llm-secrets -n ns-llm
```

Установите реальные значения для:
- `DB_PASSWORD` - пароль БД (по умолчанию: change_me)
- `OPENAI_API_KEY` - ключ OpenAI API
- `OPENROUTER_API_KEY` - ключ OpenRouter API
- `TELEGRAM_BOT_TOKEN` - токен Telegram бота

## Проверка состояния

```bash
# Проверить все ресурсы в namespace
kubectl get all -n ns-llm

# Проверить состояние подов
kubectl get pods -n ns-llm

# Логи конкретного сервиса
kubectl logs -f deployment/llmcore -n ns-llm
kubectl logs -f deployment/llmworker -n ns-llm
kubectl logs -f deployment/llmtelemetry -n ns-llm
kubectl logs -f deployment/llmmcp -n ns-llm
kubectl logs -f statefulset/llmdb -n ns-llm

# Проверить StatefulSet БД
kubectl get statefulset -n ns-llm
kubectl get pvc -n ns-llm
```

## Доступ к сервисам

### Внутри кластера
- llmdb: `llmdb.ns-llm.svc.cluster.local:5432`
- llmcore HTTP: `llmcore.ns-llm.svc.cluster.local:8080`
- llmcore gRPC: `llmcore.ns-llm.svc.cluster.local:9090`
- llmmcp: `llmmcp.ns-llm.svc.cluster.local:3333`

### Извне кластера (через NodePort)
- llmdb: `<node-ip>:30435`
- llmcore HTTP: `<node-ip>:30080`
- llmmcp: `<node-ip>:30333`

### Health checks
```bash
# llmcore
curl http://<node-ip>:30080/health

# llmmcp
curl http://<node-ip>:30333/health
```

## Особенности конфигурации

### llmworker и Ollama
Worker использует `hostNetwork: true` для доступа к Ollama на `localhost:11434`.

Альтернативный подход через `hostAliases` закомментирован в `llmworker-deployment.yaml`:
```yaml
hostAliases:
- ip: "192.168.1.100"  # IP хост-машины
  hostnames:
  - "host.docker.internal"
```

### llmcore и Tailscale
Core монтирует Tailscale binary и socket с хост-машины для сетевого взаимодействия:
- `/usr/bin/tailscale` → `/usr/bin/tailscale:ro`
- `/var/run/tailscale/tailscaled.sock` → `/var/run/tailscale/tailscaled.sock:ro`

### Зависимости и инициализация
Все сервисы используют `initContainers` для ожидания готовности зависимостей:
- llmcore ждёт llmdb
- llmworker, llmtelemetry, llmmcp ждут llmcore

## Масштабирование

```bash
# Увеличить количество реплик worker
kubectl scale deployment llmworker -n ns-llm --replicas=3

# Увеличить количество реплик MCP сервера
kubectl scale deployment llmmcp -n ns-llm --replicas=2
```

## Удаление

```bash
# Удалить все ресурсы
kubectl delete -k /path/to/llm-mcp/k8s/

# Или
kubectl delete namespace ns-llm

# ВНИМАНИЕ: PVC не удаляется автоматически
kubectl delete pvc llmdb-data -n ns-llm
```

## Troubleshooting

### Pod не запускается
```bash
kubectl describe pod <pod-name> -n ns-llm
kubectl logs <pod-name> -n ns-llm --previous
```

### Проблемы с БД
```bash
# Подключиться к БД
kubectl exec -it llmdb-0 -n ns-llm -- psql -U llm -d llm_mcp

# Проверить liveness probe
kubectl exec -it llmdb-0 -n ns-llm -- pg_isready -U llm
```

### Проблемы с Ollama
```bash
# Проверить доступность Ollama с worker pod
kubectl exec -it deployment/llmworker -n ns-llm -- curl http://localhost:11434/api/tags
```

### Проблемы с образами
```bash
# Если используется локальный registry
# Убедитесь, что imagePullPolicy: IfNotPresent или Never

# Проверить образы на ноде
docker images | grep llm-mcp
```

## Интеграция с другими модулями

Для интеграции с telegram-mcp (namespace ns-telegram):
- В `configmap.yaml` установлен `TGAPI_URL: http://tgapi.ns-telegram.svc.cluster.local:8000`
- Убедитесь, что ns-telegram развёрнут в том же кластере
