package api

import (
	"encoding/json"
	"net/http"

	"llm-mcp/core/internal/models"
)

// WriteJSON отправляет JSON-ответ
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// WriteError отправляет ошибку в едином формате (NeuronSwarm Error Contract)
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, models.ErrorResp{Error: code, Message: message})
}

// WriteSSE отправляет SSE event
func WriteSSE(w http.ResponseWriter, event string, payload any) {
	data, _ := json.Marshal(payload)
	_, _ = w.Write([]byte("event: " + event + "\n"))
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
}

// ToInt конвертирует json-числа в int
func ToInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}
