package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"llm-mcp/core/internal/config"
	"llm-mcp/core/internal/limits"
	"llm-mcp/core/internal/models"
)

// DeviceCircuit — состояние circuit breaker для устройства
type DeviceCircuit struct {
	Failures   int
	DegradedAt time.Time
}

// Router отвечает за выбор провайдера и устройства
type Router struct {
	DB       *pgxpool.Pool
	mu       sync.RWMutex
	circuits map[string]*DeviceCircuit // device_id → circuit state
}

// New создаёт Router
func New(db *pgxpool.Pool) *Router {
	return &Router{DB: db, circuits: make(map[string]*DeviceCircuit)}
}

// RecordDeviceResult обновляет circuit breaker при завершении/провале задачи
func (rt *Router) RecordDeviceResult(deviceID string, success bool) {
	if deviceID == "" {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	c := rt.circuits[deviceID]
	if success {
		delete(rt.circuits, deviceID) // reset
		return
	}
	if c == nil {
		c = &DeviceCircuit{}
		rt.circuits[deviceID] = c
	}
	c.Failures++
	if c.Failures >= 3 {
		c.DegradedAt = time.Now()
		log.Printf("circuit: device %s degraded (failures=%d)", deviceID, c.Failures)
	}
}

// IsDeviceDegraded проверяет, деградировано ли устройство
func (rt *Router) IsDeviceDegraded(deviceID string) bool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	c := rt.circuits[deviceID]
	if c == nil || c.Failures < 3 {
		return false
	}
	// Через 5 минут — пропускаем 1 задачу (probe)
	if time.Since(c.DegradedAt) > 5*time.Minute {
		return false
	}
	return true
}

// GetCircuitStatus возвращает статус circuit breaker для dashboard
func (rt *Router) GetCircuitStatus(deviceID string) string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	c := rt.circuits[deviceID]
	if c == nil || c.Failures < 3 {
		return "ok"
	}
	if time.Since(c.DegradedAt) > 5*time.Minute {
		return "probe"
	}
	return "degraded"
}

// qualityTiers — маппинг quality + context range → список tier'ов для поиска
var qualityTiers = map[string][3][]string{
	//           [0] ≤4K              [1] 4K-32K                [2] >32K
	"turbo":    {{"tiny"},            {"tiny", "small"},        {}},
	"economy":  {{"tiny", "small"},   {"small", "medium"},      {}},
	"standard": {{"small", "medium"}, {"medium", "large"},      {}},
	"premium":  {{"medium", "large"}, {"large", "xl"},          {}},
	"ultra":    {{"large", "xl"},     {"xl"},                   {}},
	"max":      {{},                  {},                       {}},
}

// cloudFallbackTiers — cloud tier'ы для fallback
var cloudFallbackTiers = map[string][]string{
	"turbo":    {"cloud_economy"},
	"economy":  {"cloud_economy"},
	"standard": {"cloud_economy"},
	"premium":  {"cloud_economy", "cloud_premium"},
	"ultra":    {"cloud_premium", "cloud_economy"},
	"max":      {"cloud_premium", "cloud_economy"},
}

// EstimateTokens оценивает количество токенов по длине текста
func EstimateTokens(prompt string, messages []map[string]string) int {
	total := len(prompt)
	for _, m := range messages {
		total += len(m["content"])
	}
	tokens := total / 4
	if tokens < 256 {
		tokens = 256
	}
	return tokens
}

// RouteLLM определяет provider/kind/payload по запросу
func (rt *Router) RouteLLM(ctx context.Context, req models.LLMRequest) (string, string, json.RawMessage, error) {
	// Smart routing: если модель не указана, но есть quality
	if strings.TrimSpace(req.Model) == "" && strings.TrimSpace(req.Quality) != "" {
		return rt.routeSmartLLM(ctx, req)
	}

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

// routeSmartLLM — автоматический выбор модели по quality
func (rt *Router) routeSmartLLM(ctx context.Context, req models.LLMRequest) (string, string, json.RawMessage, error) {
	quality := strings.ToLower(strings.TrimSpace(req.Quality))
	task := strings.ToLower(strings.TrimSpace(req.Task))
	if task == "" {
		task = "chat"
	}

	if _, ok := qualityTiers[quality]; !ok {
		return "", "", nil, fmt.Errorf("invalid_quality: %s (use turbo|economy|standard|premium|ultra|max)", quality)
	}

	// Оценка контекста
	estimatedTokens := EstimateTokens(req.Prompt, req.Messages)
	contextBucket := 0 // 0=≤4K, 1=4K-32K, 2=>32K
	if estimatedTokens > 32000 {
		contextBucket = 2
	} else if estimatedTokens > 4000 {
		contextBucket = 1
	}

	minContextK := estimatedTokens / 1000
	if minContextK < 1 {
		minContextK = 1
	}

	preferThinking := task == "reason"

	// Попытка найти local-модель
	localTiers := qualityTiers[quality][contextBucket]
	var sel *models.ModelSelection

	if len(localTiers) > 0 {
		sel = rt.findLocalModel(ctx, localTiers, minContextK, preferThinking)
	}

	// Fallback → cloud
	if sel == nil {
		cloudTiers := cloudFallbackTiers[quality]
		if len(cloudTiers) > 0 {
			sel = rt.findCloudModel(ctx, cloudTiers, minContextK, preferThinking)
		}
	}

	if sel == nil {
		return "", "", nil, errors.New("no_model_available: quality=" + quality)
	}

	// Определение kind
	kind := ""
	switch sel.Provider {
	case "ollama":
		if task == "embed" {
			kind = "ollama.embed"
		} else {
			kind = "ollama.generate"
		}
	case "openrouter":
		kind = "openrouter.chat"
	case "openai":
		kind = "openai.chat"
	}

	// Формирование payload
	payload := map[string]any{}
	switch kind {
	case "ollama.generate", "ollama.embed":
		prompt := req.Prompt
		if prompt == "" && len(req.Messages) > 0 {
			prompt = MessagesToPrompt(req.Messages)
		}
		if prompt == "" {
			return "", "", nil, errors.New("prompt_required")
		}
		payload["model"] = sel.Model
		payload["prompt"] = prompt
		if req.Options != nil {
			payload["options"] = req.Options
		}
		if sel.Addr != "" {
			payload["ollama_addr"] = sel.Addr
		}
		if sel.Host != "" {
			payload["ollama_host"] = sel.Host
		}
		if sel.DeviceID != "" {
			payload["device_id"] = sel.DeviceID
		}
	case "openai.chat", "openrouter.chat":
		msgs := req.Messages
		if len(msgs) == 0 && req.Prompt != "" {
			msgs = []map[string]string{{"role": "user", "content": req.Prompt}}
		}
		if len(msgs) == 0 {
			return "", "", nil, errors.New("messages_required")
		}
		payload["model"] = sel.Model
		payload["messages"] = msgs
		if req.Temperature != nil {
			payload["temperature"] = *req.Temperature
		}
		if req.MaxTokens != nil {
			payload["max_tokens"] = *req.MaxTokens
		}
	}

	// Мета-данные для worker: tier, pricing, thinking
	payload["_tier"] = sel.Tier
	priceIn, priceOut := rt.lookupPricing(ctx, sel.Model)
	payload["_price_in_1m"] = priceIn
	payload["_price_out_1m"] = priceOut

	// Пробрасывание thinking
	if req.Thinking != nil {
		payload["thinking"] = *req.Thinking
	} else if sel.Thinking {
		payload["thinking"] = true
	}

	payloadJSON, _ := json.Marshal(payload)
	log.Printf("smart_route: quality=%s task=%s model=%s provider=%s tier=%s device=%s tokens~%d reason=%s",
		quality, task, sel.Model, sel.Provider, sel.Tier, sel.DeviceID, estimatedTokens, sel.Reason)
	return sel.Provider, kind, payloadJSON, nil
}

// findLocalModel ищет ollama-модель по tier'ам с учётом контекста и thinking
func (rt *Router) findLocalModel(ctx context.Context, tiers []string, minContextK int, preferThinking bool) *models.ModelSelection {
	row := rt.DB.QueryRow(ctx, `
		SELECT m.id, m.tier, COALESCE(m.thinking, FALSE),
		       d.id, d.tags->>'ollama_addr', d.tags->>'ollama_host'
		FROM models m
		JOIN device_models dm ON dm.model_id = m.id AND dm.available = TRUE
		JOIN devices d ON d.id = dm.device_id AND d.status = 'online'
		     AND d.tags->>'ollama' = 'true'
		     AND COALESCE(d.tags->>'ollama_addr', '') <> ''
		WHERE m.tier = ANY($1)
		  AND m.provider = 'ollama'
		  AND COALESCE(m.status, 'active') = 'active'
		  AND COALESCE(m.context_k, 4) >= $2
		  AND m.kind != 'embed'
		ORDER BY
		  CASE WHEN $3 AND COALESCE(m.thinking, FALSE) THEN 0 ELSE 1 END,
		  array_position($1::text[], m.tier),
		  (SELECT count(*) FROM jobs WHERE status IN ('running','queued') AND payload->>'device_id' = d.id) ASC,
		  m.params_b DESC NULLS LAST,
		  d.last_seen DESC NULLS LAST
		LIMIT 1
	`, tiers, minContextK, preferThinking)

	var sel models.ModelSelection
	var tier string
	var thinking bool
	var addr, host sql.NullString
	if err := row.Scan(&sel.Model, &tier, &thinking, &sel.DeviceID, &addr, &host); err != nil {
		return nil
	}
	sel.Provider = "ollama"
	sel.Tier = tier
	sel.Thinking = thinking
	if addr.Valid {
		sel.Addr = addr.String
	}
	if host.Valid {
		sel.Host = host.String
	}

	// Circuit breaker: пропускаем degraded устройства
	if rt.IsDeviceDegraded(sel.DeviceID) {
		sel.Reason = "device_degraded"
		log.Printf("smart_route: skipping degraded device=%s", sel.DeviceID)
		return nil
	}
	sel.Reason = "local_match"
	return &sel
}

// findCloudModel ищет cloud-модель по tier'ам
func (rt *Router) findCloudModel(ctx context.Context, tiers []string, minContextK int, preferThinking bool) *models.ModelSelection {
	row := rt.DB.QueryRow(ctx, `
		SELECT m.id, m.tier, COALESCE(m.thinking, FALSE), m.provider
		FROM models m
		WHERE m.tier = ANY($1)
		  AND m.provider IN ('openrouter', 'openai')
		  AND COALESCE(m.status, 'active') = 'active'
		  AND COALESCE(m.context_k, 128) >= $2
		  AND m.kind != 'embed'
		ORDER BY
		  CASE WHEN $3 AND COALESCE(m.thinking, FALSE) THEN 0 ELSE 1 END,
		  array_position($1::text[], m.tier),
		  m.context_k DESC NULLS LAST
		LIMIT 1
	`, tiers, minContextK, preferThinking)

	var sel models.ModelSelection
	if err := row.Scan(&sel.Model, &sel.Tier, &sel.Thinking, &sel.Provider); err != nil {
		return nil
	}

	// Проверяем наличие API ключа
	switch sel.Provider {
	case "openrouter":
		if !config.HasOpenrouter() {
			return nil
		}
	case "openai":
		if !config.HasOpenai() {
			return nil
		}
	}
	sel.Reason = "cloud_fallback"
	return &sel
}

// lookupPricing возвращает цены за 1M токенов для модели
func (rt *Router) lookupPricing(ctx context.Context, modelID string) (float64, float64) {
	var priceIn, priceOut float64
	err := rt.DB.QueryRow(ctx, `
		SELECT COALESCE(price_in_1m, 0), COALESCE(price_out_1m, 0)
		FROM model_pricing WHERE model_id = $1
	`, modelID).Scan(&priceIn, &priceOut)
	if err != nil {
		return 0, 0
	}
	return priceIn, priceOut
}
