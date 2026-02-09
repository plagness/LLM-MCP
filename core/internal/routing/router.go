package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"llm-mcp/core/internal/config"
	"llm-mcp/core/internal/limits"
	"llm-mcp/core/internal/models"
)

// Router отвечает за выбор провайдера и устройства
type Router struct {
	DB *pgxpool.Pool
}

// New создаёт Router
func New(db *pgxpool.Pool) *Router {
	return &Router{DB: db}
}

// RouteLLM определяет provider/kind/payload по запросу
func (rt *Router) RouteLLM(ctx context.Context, req models.LLMRequest) (string, string, json.RawMessage, error) {
	task := strings.ToLower(strings.TrimSpace(req.Task))
	if task == "" {
		task = "chat"
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" {
		provider = "auto"
	}
	localAvailable := rt.HasLocalOllama(ctx)

	if provider == "auto" {
		if task == "embed" {
			provider = "ollama"
		} else if req.Constraints.ForceCloud {
			if config.HasOpenrouter() {
				provider = "openrouter"
			} else if config.HasOpenai() {
				provider = "openai"
			} else {
				provider = "ollama"
			}
		} else if req.Constraints.PreferLocal && localAvailable {
			provider = "ollama"
		} else if config.HasOpenrouter() {
			provider = "openrouter"
		} else if config.HasOpenai() {
			provider = "openai"
		} else {
			provider = "ollama"
		}
	}

	if provider == "ollama" && req.Constraints.MaxLatencyMs > 0 && req.Model != "" {
		if ok := rt.MeetsLatency(ctx, req.Model, task, req.Constraints.MaxLatencyMs); !ok {
			if config.HasOpenrouter() {
				provider = "openrouter"
			} else if config.HasOpenai() {
				provider = "openai"
			}
		}
	}

	kind := ""
	switch provider {
	case "ollama":
		if task == "embed" {
			kind = "ollama.embed"
		} else {
			kind = "ollama.generate"
		}
	case "openai":
		kind = "openai.chat"
	case "openrouter":
		kind = "openrouter.chat"
	default:
		return "", "", nil, errors.New("provider_not_supported")
	}

	if len(req.Payload) > 0 {
		if provider == "ollama" {
			model, device := ParsePayloadModelDevice(req.Payload)
			if model != "" && device != "" {
				if ok, reason := limits.ModelAllowed(ctx, rt.DB, device, model); !ok {
					return "", "", nil, errors.New("model_not_allowed:" + reason)
				}
			}
		}
		log.Printf("route: task=%s provider=%s kind=%s local=%t", task, provider, kind, localAvailable)
		return provider, kind, req.Payload, nil
	}

	payload := map[string]any{}
	switch kind {
	case "ollama.generate":
		prompt := req.Prompt
		if prompt == "" && len(req.Messages) > 0 {
			prompt = MessagesToPrompt(req.Messages)
		}
		if prompt == "" {
			return "", "", nil, errors.New("prompt_required")
		}
		if req.Model != "" {
			payload["model"] = req.Model
		}
		payload["prompt"] = prompt
		if req.Options != nil {
			payload["options"] = req.Options
		}
		if req.Model != "" {
			if target := rt.SelectOllamaDevice(ctx, req.Model, "generate"); target != nil {
				payload["ollama_addr"] = target.Addr
				if target.Host != "" {
					payload["ollama_host"] = target.Host
				}
				payload["device_id"] = target.ID
			} else if config.Getenv("STRICT_MODEL_LIMITS", "0") == "1" {
				return "", "", nil, errors.New("no_eligible_device")
			}
		}
	case "ollama.embed":
		prompt := req.Prompt
		if prompt == "" {
			return "", "", nil, errors.New("prompt_required")
		}
		if req.Model != "" {
			payload["model"] = req.Model
		}
		payload["prompt"] = prompt
		if req.Model != "" {
			if target := rt.SelectOllamaDevice(ctx, req.Model, "embed"); target != nil {
				payload["ollama_addr"] = target.Addr
				if target.Host != "" {
					payload["ollama_host"] = target.Host
				}
				payload["device_id"] = target.ID
			} else if config.Getenv("STRICT_MODEL_LIMITS", "0") == "1" {
				return "", "", nil, errors.New("no_eligible_device")
			}
		}
	case "openai.chat", "openrouter.chat":
		msgs := req.Messages
		if len(msgs) == 0 && req.Prompt != "" {
			msgs = []map[string]string{{"role": "user", "content": req.Prompt}}
		}
		if len(msgs) == 0 {
			return "", "", nil, errors.New("messages_required")
		}
		if req.Model != "" {
			payload["model"] = req.Model
		}
		payload["messages"] = msgs
		if req.Temperature != nil {
			payload["temperature"] = *req.Temperature
		}
		if req.MaxTokens != nil {
			payload["max_tokens"] = *req.MaxTokens
		}
	}

	payloadJSON, _ := json.Marshal(payload)
	log.Printf("route: task=%s provider=%s kind=%s local=%t", task, provider, kind, localAvailable)
	return provider, kind, payloadJSON, nil
}

// SelectOllamaDevice выбирает лучшее устройство для модели
func (rt *Router) SelectOllamaDevice(ctx context.Context, model string, task string) *models.DeviceTarget {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	strict := config.Getenv("STRICT_MODEL_LIMITS", "0") == "1"
	strictInt := 0
	if strict {
		strictInt = 1
	}
	row := rt.DB.QueryRow(ctx, `
		SELECT d.id,
		       d.tags->>'ollama_addr' AS addr,
		       d.tags->>'ollama_host' AS host,
		       (SELECT tps FROM benchmarks b
		        WHERE b.device_id = d.id AND b.model_id = $1 AND b.task_type = $2
		        ORDER BY created_at DESC LIMIT 1) AS tps,
		       (SELECT latency_ms FROM benchmarks b
		        WHERE b.device_id = d.id AND b.model_id = $1 AND b.task_type = $2
		        ORDER BY created_at DESC LIMIT 1) AS latency
		FROM devices d
		JOIN device_models dm ON dm.device_id = d.id AND dm.model_id = $1 AND dm.available = TRUE
		LEFT JOIN models m ON m.id = dm.model_id
		LEFT JOIN device_limits dl ON dl.device_id = d.id
		WHERE d.status = 'online'
		  AND d.tags->>'ollama' = 'true'
		  AND COALESCE(d.tags->>'ollama_addr','') <> ''
		  AND (dl.allow_models IS NULL OR dl.allow_models = '[]'::jsonb OR dl.allow_models ? $1)
		  AND (dl.deny_models IS NULL OR dl.deny_models = '[]'::jsonb OR NOT (dl.deny_models ? $1))
		  AND (
		    dl.max_params_b IS NULL
		    OR (m.params_b IS NOT NULL AND m.params_b <= dl.max_params_b)
		    OR ($3 = 0 AND m.params_b IS NULL)
		  )
		  AND (
		    dl.max_size_gb IS NULL
		    OR (m.size_gb IS NOT NULL AND m.size_gb <= dl.max_size_gb)
		    OR ($3 = 0 AND m.size_gb IS NULL)
		  )
		  AND (
		    dl.max_context_k IS NULL
		    OR (m.context_k IS NOT NULL AND m.context_k <= dl.max_context_k)
		    OR ($3 = 0 AND m.context_k IS NULL)
		  )
		ORDER BY tps DESC NULLS LAST, latency ASC NULLS LAST, d.last_seen DESC NULLS LAST
		LIMIT 1
	`, model, task, strictInt)
	var target models.DeviceTarget
	var tps sql.NullFloat64
	var latency sql.NullInt64
	if err := row.Scan(&target.ID, &target.Addr, &target.Host, &tps, &latency); err != nil {
		return nil
	}
	log.Printf("route: device select model=%s task=%s device=%s addr=%s tps=%v latency=%v", model, task, target.ID, target.Addr, tps, latency)
	return &target
}

// HasLocalOllama проверяет наличие online-устройства с ollama
func (rt *Router) HasLocalOllama(ctx context.Context) bool {
	row := rt.DB.QueryRow(ctx, `
		SELECT 1 FROM devices
		WHERE status='online' AND tags->>'ollama'='true'
		LIMIT 1
	`)
	var v int
	return row.Scan(&v) == nil
}

// MeetsLatency проверяет, укладывается ли модель в лимит задержки
func (rt *Router) MeetsLatency(ctx context.Context, model string, task string, maxMs int) bool {
	row := rt.DB.QueryRow(ctx, `
		SELECT latency_ms FROM benchmarks
		WHERE model_id=$1 AND task_type=$2
		ORDER BY created_at DESC LIMIT 1
	`, model, task)
	var ms int
	if err := row.Scan(&ms); err != nil {
		return false
	}
	return ms > 0 && ms <= maxMs
}

// FetchDeviceAddr возвращает ollama-адрес устройства
func (rt *Router) FetchDeviceAddr(ctx context.Context, id string) (string, string, error) {
	row := rt.DB.QueryRow(ctx, `
		SELECT tags->>'ollama_addr' AS addr, tags->>'ollama_host' AS host
		FROM devices WHERE id = $1
	`, id)
	var addr sql.NullString
	var host sql.NullString
	if err := row.Scan(&addr, &host); err != nil {
		return "", "", err
	}
	if !addr.Valid {
		return "", "", nil
	}
	if host.Valid {
		return addr.String, host.String, nil
	}
	return addr.String, "", nil
}

// MessagesToPrompt конвертирует messages в строку для ollama
func MessagesToPrompt(messages []map[string]string) string {
	lines := []string{}
	for _, msg := range messages {
		role := msg["role"]
		content := msg["content"]
		if role == "" && content == "" {
			continue
		}
		lines = append(lines, role+": "+content)
	}
	return strings.Join(lines, "\n")
}

// ParsePayloadModelDevice извлекает model и device_id из payload
func ParsePayloadModelDevice(payload json.RawMessage) (string, string) {
	if len(payload) == 0 {
		return "", ""
	}
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return "", ""
	}
	model, _ := data["model"].(string)
	device, _ := data["device_id"].(string)
	return strings.TrimSpace(model), strings.TrimSpace(device)
}
