package limits

import (
	"testing"

	"llm-mcp/core/internal/models"
)

// === DeriveDeviceLimits ===

func ptr64(v float64) *float64 { return &v }
func ptrInt(v int) *int        { return &v }

func TestDeriveDeviceLimits_FromRAM_Small(t *testing.T) {
	spec := models.DeviceLimitSpec{RamGB: ptr64(4)}
	result := DeriveDeviceLimits(spec)

	if result.MaxParamsB == nil || *result.MaxParamsB != 5 {
		t.Errorf("expected MaxParamsB=5 for 4GB RAM, got %v", result.MaxParamsB)
	}
	if result.MaxContextK == nil || *result.MaxContextK != 4096 {
		t.Errorf("expected MaxContextK=4096 for 4GB RAM, got %v", result.MaxContextK)
	}
	if result.MaxSizeGB == nil {
		t.Fatal("expected MaxSizeGB to be set")
	}
	// 4 * 0.8 = 3.2
	if *result.MaxSizeGB != 3.2 {
		t.Errorf("expected MaxSizeGB=3.2, got %v", *result.MaxSizeGB)
	}
}

func TestDeriveDeviceLimits_FromRAM_Medium(t *testing.T) {
	spec := models.DeviceLimitSpec{RamGB: ptr64(16)}
	result := DeriveDeviceLimits(spec)

	if result.MaxParamsB == nil || *result.MaxParamsB != 12 {
		t.Errorf("expected MaxParamsB=12 for 16GB RAM, got %v", result.MaxParamsB)
	}
	if result.MaxContextK == nil || *result.MaxContextK != 8192 {
		t.Errorf("expected MaxContextK=8192 for 16GB RAM, got %v", result.MaxContextK)
	}
}

func TestDeriveDeviceLimits_FromVRAM(t *testing.T) {
	spec := models.DeviceLimitSpec{VramGB: ptr64(8), RamGB: ptr64(32)}
	result := DeriveDeviceLimits(spec)

	// VRAM имеет приоритет: 8GB → MaxParamsB=5
	if result.MaxParamsB == nil || *result.MaxParamsB != 5 {
		t.Errorf("expected MaxParamsB=5 from VRAM, got %v", result.MaxParamsB)
	}
}

func TestDeriveDeviceLimits_LargeRAM(t *testing.T) {
	spec := models.DeviceLimitSpec{RamGB: ptr64(64)}
	result := DeriveDeviceLimits(spec)

	// 64 * 0.75 * 2 / 2 = 48
	if result.MaxParamsB == nil || *result.MaxParamsB != 48 {
		t.Errorf("expected MaxParamsB=48 for 64GB RAM, got %v", result.MaxParamsB)
	}
	if result.MaxContextK == nil || *result.MaxContextK != 16384 {
		t.Errorf("expected MaxContextK=16384 for 64GB RAM, got %v", result.MaxContextK)
	}
}

func TestDeriveDeviceLimits_NoMemory(t *testing.T) {
	spec := models.DeviceLimitSpec{}
	result := DeriveDeviceLimits(spec)

	if result.MaxParamsB != nil {
		t.Errorf("expected nil MaxParamsB without memory, got %v", result.MaxParamsB)
	}
	if result.MaxSizeGB != nil {
		t.Errorf("expected nil MaxSizeGB without memory, got %v", result.MaxSizeGB)
	}
	if result.MaxContextK != nil {
		t.Errorf("expected nil MaxContextK without memory, got %v", result.MaxContextK)
	}
}

func TestDeriveDeviceLimits_PresetLimits(t *testing.T) {
	// Если лимиты уже заданы, не перезаписываем
	spec := models.DeviceLimitSpec{
		RamGB:       ptr64(8),
		MaxParamsB:  ptr64(99),
		MaxSizeGB:   ptr64(99),
		MaxContextK: ptrInt(99999),
	}
	result := DeriveDeviceLimits(spec)

	if *result.MaxParamsB != 99 {
		t.Errorf("expected preset MaxParamsB=99, got %v", *result.MaxParamsB)
	}
	if *result.MaxSizeGB != 99 {
		t.Errorf("expected preset MaxSizeGB=99, got %v", *result.MaxSizeGB)
	}
	if *result.MaxContextK != 99999 {
		t.Errorf("expected preset MaxContextK=99999, got %v", *result.MaxContextK)
	}
}

// === parseStringList ===

func TestParseStringList_Valid(t *testing.T) {
	raw := []byte(`["model-a", "model-b"]`)
	result := parseStringList(raw)
	if len(result) != 2 {
		t.Fatalf("expected 2 items, got %d", len(result))
	}
	if result[0] != "model-a" || result[1] != "model-b" {
		t.Errorf("unexpected values: %v", result)
	}
}

func TestParseStringList_Empty(t *testing.T) {
	result := parseStringList(nil)
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
}

func TestParseStringList_Invalid(t *testing.T) {
	result := parseStringList([]byte("not json"))
	if result != nil {
		t.Errorf("expected nil for invalid JSON, got %v", result)
	}
}

func TestParseStringList_TrimsBlanks(t *testing.T) {
	raw := []byte(`["  model-a  ", "", " "]`)
	result := parseStringList(raw)
	if len(result) != 1 {
		t.Fatalf("expected 1 item after trimming, got %d: %v", len(result), result)
	}
	if result[0] != "model-a" {
		t.Errorf("expected model-a, got %q", result[0])
	}
}

// === stringListContains ===

func TestStringListContains_Found(t *testing.T) {
	if !stringListContains([]string{"a", "b", "c"}, "b") {
		t.Error("expected to find 'b'")
	}
}

func TestStringListContains_NotFound(t *testing.T) {
	if stringListContains([]string{"a", "b", "c"}, "d") {
		t.Error("did not expect to find 'd'")
	}
}

func TestStringListContains_Empty(t *testing.T) {
	if stringListContains(nil, "a") {
		t.Error("expected false for nil list")
	}
}
