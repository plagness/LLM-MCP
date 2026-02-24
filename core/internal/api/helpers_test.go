package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llm-mcp/core/internal/models"
)

// === WriteJSON ===

func TestWriteJSON_StatusOK(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("expected status=ok, got %q", resp["status"])
	}
}

func TestWriteJSON_StatusNotFound(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// === WriteError (NeuronSwarm Error Contract) ===

func TestWriteError_Contract(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, http.StatusBadRequest, "invalid_json", "Невалидный JSON в теле запроса")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	var resp models.ErrorResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Error != "invalid_json" {
		t.Errorf("expected error=invalid_json, got %q", resp.Error)
	}
	if resp.Message != "Невалидный JSON в теле запроса" {
		t.Errorf("unexpected message: %q", resp.Message)
	}
}

func TestWriteError_ServerError(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, http.StatusInternalServerError, "db_insert_failed", "Ошибка записи в БД")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
	var resp models.ErrorResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Error != "db_insert_failed" {
		t.Errorf("expected error=db_insert_failed, got %q", resp.Error)
	}
}

// === WriteSSE ===

func TestWriteSSE_Format(t *testing.T) {
	w := httptest.NewRecorder()
	WriteSSE(w, "status", map[string]string{"id": "job-1"})
	body := w.Body.String()
	if body == "" {
		t.Fatal("expected non-empty SSE output")
	}
	// Проверяем формат SSE
	expected := "event: status\ndata: "
	if len(body) < len(expected) || body[:len(expected)] != expected {
		t.Errorf("expected SSE format starting with %q, got %q", expected, body)
	}
}

// === ToInt ===

func TestToInt_Float64(t *testing.T) {
	v, ok := ToInt(float64(42))
	if !ok || v != 42 {
		t.Errorf("expected 42/true, got %d/%v", v, ok)
	}
}

func TestToInt_Int(t *testing.T) {
	v, ok := ToInt(42)
	if !ok || v != 42 {
		t.Errorf("expected 42/true, got %d/%v", v, ok)
	}
}

func TestToInt_JsonNumber(t *testing.T) {
	v, ok := ToInt(json.Number("123"))
	if !ok || v != 123 {
		t.Errorf("expected 123/true, got %d/%v", v, ok)
	}
}

func TestToInt_String(t *testing.T) {
	_, ok := ToInt("42")
	if ok {
		t.Error("expected false for string input")
	}
}

func TestToInt_Nil(t *testing.T) {
	_, ok := ToInt(nil)
	if ok {
		t.Error("expected false for nil input")
	}
}
