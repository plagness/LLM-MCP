package routing

import (
	"encoding/json"
	"testing"
	"time"
)

// === EstimateTokens ===

func TestEstimateTokens_PromptOnly(t *testing.T) {
	// 100 символов → 25 токенов, но min 256
	prompt := "Hello world, this is a test prompt."
	tokens := EstimateTokens(prompt, nil)
	if tokens < 256 {
		t.Errorf("expected at least 256, got %d", tokens)
	}
}

func TestEstimateTokens_EmptyInput(t *testing.T) {
	tokens := EstimateTokens("", nil)
	if tokens != 256 {
		t.Errorf("expected 256 for empty input, got %d", tokens)
	}
}

func TestEstimateTokens_LargeInput(t *testing.T) {
	// 4000 символов → ~1000 токенов
	prompt := make([]byte, 4000)
	for i := range prompt {
		prompt[i] = 'a'
	}
	tokens := EstimateTokens(string(prompt), nil)
	if tokens != 1000 {
		t.Errorf("expected 1000, got %d", tokens)
	}
}

func TestEstimateTokens_WithMessages(t *testing.T) {
	msgs := []map[string]string{
		{"role": "user", "content": "Hello"},
		{"role": "assistant", "content": "Hi there"},
	}
	tokens := EstimateTokens("", msgs)
	// (0 + 5 + 8) / 4 = 3, min 256
	if tokens != 256 {
		t.Errorf("expected 256, got %d", tokens)
	}
}

func TestEstimateTokens_PromptAndMessages(t *testing.T) {
	prompt := make([]byte, 2000)
	for i := range prompt {
		prompt[i] = 'x'
	}
	msgs := []map[string]string{
		{"role": "user", "content": string(make([]byte, 2000))},
	}
	tokens := EstimateTokens(string(prompt), msgs)
	// (2000 + 2000) / 4 = 1000
	if tokens != 1000 {
		t.Errorf("expected 1000, got %d", tokens)
	}
}

// === MessagesToPrompt ===

func TestMessagesToPrompt_Basic(t *testing.T) {
	msgs := []map[string]string{
		{"role": "user", "content": "Hello"},
		{"role": "assistant", "content": "Hi"},
	}
	result := MessagesToPrompt(msgs)
	expected := "user: Hello\nassistant: Hi"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestMessagesToPrompt_Empty(t *testing.T) {
	result := MessagesToPrompt(nil)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestMessagesToPrompt_SkipEmpty(t *testing.T) {
	msgs := []map[string]string{
		{"role": "", "content": ""},
		{"role": "user", "content": "test"},
	}
	result := MessagesToPrompt(msgs)
	expected := "user: test"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

// === ParsePayloadModelDevice ===

func TestParsePayloadModelDevice_Both(t *testing.T) {
	payload := json.RawMessage(`{"model": "qwen3:1.7b", "device_id": "dev-1"}`)
	model, device := ParsePayloadModelDevice(payload)
	if model != "qwen3:1.7b" {
		t.Errorf("expected model=qwen3:1.7b, got %q", model)
	}
	if device != "dev-1" {
		t.Errorf("expected device=dev-1, got %q", device)
	}
}

func TestParsePayloadModelDevice_OnlyModel(t *testing.T) {
	payload := json.RawMessage(`{"model": "llama3"}`)
	model, device := ParsePayloadModelDevice(payload)
	if model != "llama3" {
		t.Errorf("expected model=llama3, got %q", model)
	}
	if device != "" {
		t.Errorf("expected empty device, got %q", device)
	}
}

func TestParsePayloadModelDevice_Empty(t *testing.T) {
	model, device := ParsePayloadModelDevice(nil)
	if model != "" || device != "" {
		t.Errorf("expected empty, got model=%q device=%q", model, device)
	}
}

func TestParsePayloadModelDevice_InvalidJSON(t *testing.T) {
	payload := json.RawMessage(`not json`)
	model, device := ParsePayloadModelDevice(payload)
	if model != "" || device != "" {
		t.Errorf("expected empty for invalid JSON, got model=%q device=%q", model, device)
	}
}

func TestParsePayloadModelDevice_TrimSpaces(t *testing.T) {
	payload := json.RawMessage(`{"model": " qwen3 ", "device_id": " dev-2 "}`)
	model, device := ParsePayloadModelDevice(payload)
	if model != "qwen3" {
		t.Errorf("expected trimmed model=qwen3, got %q", model)
	}
	if device != "dev-2" {
		t.Errorf("expected trimmed device=dev-2, got %q", device)
	}
}

// === Circuit Breaker ===

func TestCircuitBreaker_InitialState(t *testing.T) {
	rt := New(nil)
	if rt.IsDeviceDegraded("dev-1") {
		t.Error("new device should not be degraded")
	}
	if status := rt.GetCircuitStatus("dev-1"); status != "ok" {
		t.Errorf("expected status=ok, got %q", status)
	}
}

func TestCircuitBreaker_OneFailure(t *testing.T) {
	rt := New(nil)
	rt.RecordDeviceResult("dev-1", false)
	if rt.IsDeviceDegraded("dev-1") {
		t.Error("1 failure should not degrade")
	}
	if status := rt.GetCircuitStatus("dev-1"); status != "ok" {
		t.Errorf("expected status=ok after 1 failure, got %q", status)
	}
}

func TestCircuitBreaker_ThreeFailuresDegrades(t *testing.T) {
	rt := New(nil)
	rt.RecordDeviceResult("dev-1", false)
	rt.RecordDeviceResult("dev-1", false)
	rt.RecordDeviceResult("dev-1", false)
	if !rt.IsDeviceDegraded("dev-1") {
		t.Error("3 failures should degrade device")
	}
	if status := rt.GetCircuitStatus("dev-1"); status != "degraded" {
		t.Errorf("expected status=degraded, got %q", status)
	}
}

func TestCircuitBreaker_SuccessResets(t *testing.T) {
	rt := New(nil)
	rt.RecordDeviceResult("dev-1", false)
	rt.RecordDeviceResult("dev-1", false)
	rt.RecordDeviceResult("dev-1", true) // reset
	if rt.IsDeviceDegraded("dev-1") {
		t.Error("success should reset circuit breaker")
	}
}

func TestCircuitBreaker_ProbeAfter5Min(t *testing.T) {
	rt := New(nil)
	rt.RecordDeviceResult("dev-1", false)
	rt.RecordDeviceResult("dev-1", false)
	rt.RecordDeviceResult("dev-1", false)

	// Simulate 6 minutes passing
	rt.mu.Lock()
	rt.circuits["dev-1"].DegradedAt = time.Now().Add(-6 * time.Minute)
	rt.mu.Unlock()

	if rt.IsDeviceDegraded("dev-1") {
		t.Error("after 5+ min should allow probe (not degraded)")
	}
	if status := rt.GetCircuitStatus("dev-1"); status != "probe" {
		t.Errorf("expected status=probe, got %q", status)
	}
}

func TestCircuitBreaker_EmptyDeviceID(t *testing.T) {
	rt := New(nil)
	// Не должно паниковать
	rt.RecordDeviceResult("", false)
	if rt.IsDeviceDegraded("") {
		t.Error("empty device should not be degraded")
	}
}

func TestCircuitBreaker_MultipleDevices(t *testing.T) {
	rt := New(nil)
	// Деградируем dev-1
	for i := 0; i < 3; i++ {
		rt.RecordDeviceResult("dev-1", false)
	}
	// dev-2 не тронут
	if rt.IsDeviceDegraded("dev-2") {
		t.Error("dev-2 should not be degraded")
	}
	if !rt.IsDeviceDegraded("dev-1") {
		t.Error("dev-1 should be degraded")
	}
}

// === qualityTiers ===

func TestQualityTiers_AllQualitiesExist(t *testing.T) {
	expected := []string{"turbo", "economy", "standard", "premium", "ultra", "max"}
	for _, q := range expected {
		if _, ok := qualityTiers[q]; !ok {
			t.Errorf("qualityTiers missing key %q", q)
		}
	}
}

func TestCloudFallbackTiers_AllQualitiesExist(t *testing.T) {
	expected := []string{"turbo", "economy", "standard", "premium", "ultra", "max"}
	for _, q := range expected {
		if _, ok := cloudFallbackTiers[q]; !ok {
			t.Errorf("cloudFallbackTiers missing key %q", q)
		}
	}
}
