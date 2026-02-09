package limits

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"llm-mcp/core/internal/config"
	"llm-mcp/core/internal/models"
)

// ApplyDeviceLimits загружает конфиг лимитов из env и применяет к БД
func ApplyDeviceLimits(ctx context.Context, db *pgxpool.Pool) error {
	specs, defaultSpec, err := LoadDeviceLimitSpecs()
	if err != nil {
		return err
	}
	if len(specs) == 0 && defaultSpec == nil {
		return nil
	}

	deviceIDs := make([]string, 0, len(specs))
	for id := range specs {
		deviceIDs = append(deviceIDs, id)
	}

	if defaultSpec != nil {
		rows, err := db.Query(ctx, `
			SELECT id FROM devices WHERE tags->>'ollama' = 'true'
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				continue
			}
			if _, ok := specs[id]; ok {
				continue
			}
			deviceIDs = append(deviceIDs, id)
		}
	}

	for _, id := range deviceIDs {
		spec, ok := specs[id]
		if !ok && defaultSpec != nil {
			spec = *defaultSpec
		}
		derived := DeriveDeviceLimits(spec)
		allowJSON, _ := json.Marshal(derived.AllowModels)
		denyJSON, _ := json.Marshal(derived.DenyModels)
		_, err := db.Exec(ctx, `
			INSERT INTO device_limits
			 (device_id, ram_gb, vram_gb, max_params_b, max_size_gb, max_context_k, allow_models, deny_models, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, now())
			ON CONFLICT (device_id) DO UPDATE SET
			  ram_gb = excluded.ram_gb,
			  vram_gb = excluded.vram_gb,
			  max_params_b = excluded.max_params_b,
			  max_size_gb = excluded.max_size_gb,
			  max_context_k = excluded.max_context_k,
			  allow_models = excluded.allow_models,
			  deny_models = excluded.deny_models,
			  updated_at = now()
		`, id, derived.RamGB, derived.VramGB, derived.MaxParamsB, derived.MaxSizeGB, derived.MaxContextK, allowJSON, denyJSON)
		if err != nil {
			return err
		}
	}
	return nil
}

// LoadDeviceLimitSpecs загружает спецификации лимитов из env
func LoadDeviceLimitSpecs() (map[string]models.DeviceLimitSpec, *models.DeviceLimitSpec, error) {
	raw := strings.TrimSpace(os.Getenv("DEVICE_LIMITS_JSON"))
	file := strings.TrimSpace(os.Getenv("DEVICE_LIMITS_FILE"))
	if raw == "" && file == "" {
		return nil, nil, nil
	}
	if raw == "" && file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, nil, err
		}
		raw = string(data)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil, nil
	}

	var rawMap map[string]models.DeviceLimitSpec
	if err := json.Unmarshal([]byte(raw), &rawMap); err != nil {
		return nil, nil, err
	}
	specs := map[string]models.DeviceLimitSpec{}
	var defaultSpec *models.DeviceLimitSpec
	for key, spec := range rawMap {
		k := strings.TrimSpace(key)
		if k == "" {
			continue
		}
		if k == "*" {
			tmp := spec
			defaultSpec = &tmp
			continue
		}
		specs[k] = spec
	}
	return specs, defaultSpec, nil
}

// DeriveDeviceLimits автоматически вычисляет лимиты из RAM/VRAM
func DeriveDeviceLimits(spec models.DeviceLimitSpec) models.DeviceLimitSpec {
	mem := 0.0
	if spec.VramGB != nil && *spec.VramGB > 0 {
		mem = *spec.VramGB
	} else if spec.RamGB != nil && *spec.RamGB > 0 {
		mem = *spec.RamGB
	}
	if spec.MaxParamsB == nil && mem > 0 {
		val := 0.0
		switch {
		case mem <= 8:
			val = 5
		case mem <= 16:
			val = 12
		default:
			val = math.Floor(mem*0.75*2) / 2
		}
		spec.MaxParamsB = &val
	}
	if spec.MaxSizeGB == nil && mem > 0 {
		val := math.Floor(mem*0.8*10) / 10
		spec.MaxSizeGB = &val
	}
	if spec.MaxContextK == nil && mem > 0 {
		val := 0
		switch {
		case mem <= 8:
			val = 4096
		case mem <= 16:
			val = 8192
		default:
			val = 16384
		}
		spec.MaxContextK = &val
	}
	return spec
}

// ModelAllowed проверяет, разрешена ли модель на устройстве
func ModelAllowed(ctx context.Context, db *pgxpool.Pool, deviceID string, model string) (bool, string) {
	deviceID = strings.TrimSpace(deviceID)
	model = strings.TrimSpace(model)
	if deviceID == "" || model == "" {
		return true, ""
	}
	var available sql.NullBool
	var paramsB sql.NullFloat64
	var sizeGB sql.NullFloat64
	var contextK sql.NullInt64
	var maxParamsB sql.NullFloat64
	var maxSizeGB sql.NullFloat64
	var maxContextK sql.NullInt64
	var allowRaw []byte
	var denyRaw []byte

	err := db.QueryRow(ctx, `
		SELECT dm.available,
		       m.params_b, m.size_gb, m.context_k,
		       dl.max_params_b, dl.max_size_gb, dl.max_context_k,
		       dl.allow_models, dl.deny_models
		FROM device_models dm
		LEFT JOIN models m ON m.id = dm.model_id
		LEFT JOIN device_limits dl ON dl.device_id = dm.device_id
		WHERE dm.device_id = $1 AND dm.model_id = $2
	`, deviceID, model).Scan(
		&available,
		&paramsB,
		&sizeGB,
		&contextK,
		&maxParamsB,
		&maxSizeGB,
		&maxContextK,
		&allowRaw,
		&denyRaw,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			return false, "model_not_on_device"
		}
		return false, "db_error"
	}
	if available.Valid && !available.Bool {
		return false, "model_not_available"
	}

	allowList := parseStringList(allowRaw)
	if len(allowList) > 0 && !stringListContains(allowList, model) {
		return false, "model_not_in_allowlist"
	}
	denyList := parseStringList(denyRaw)
	if len(denyList) > 0 && stringListContains(denyList, model) {
		return false, "model_denied"
	}

	strict := config.Getenv("STRICT_MODEL_LIMITS", "0") == "1"
	if maxParamsB.Valid {
		if paramsB.Valid {
			if paramsB.Float64 > maxParamsB.Float64 {
				return false, "model_params_too_large"
			}
		} else if strict {
			return false, "model_params_unknown"
		}
	}
	if maxSizeGB.Valid {
		if sizeGB.Valid {
			if sizeGB.Float64 > maxSizeGB.Float64 {
				return false, "model_size_too_large"
			}
		} else if strict {
			return false, "model_size_unknown"
		}
	}
	if maxContextK.Valid {
		if contextK.Valid {
			if contextK.Int64 > maxContextK.Int64 {
				return false, "model_context_too_large"
			}
		} else if strict {
			return false, "model_context_unknown"
		}
	}
	return true, ""
}

func parseStringList(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func stringListContains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}
