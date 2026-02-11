package models

import (
	"encoding/json"
	"time"
)

type HealthResp struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

type SubmitJobRequest struct {
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload"`
	Priority    int             `json:"priority"`
	Source      string          `json:"source"`
	MaxAttempts int             `json:"max_attempts"`
	DeadlineAt  string          `json:"deadline_at"`
}

type SubmitJobResponse struct {
	JobID string `json:"job_id"`
}

type WorkerInfo struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Platform string          `json:"platform"`
	Arch     string          `json:"arch"`
	Host     string          `json:"host"`
	Tags     json.RawMessage `json:"tags"`
}

type RegisterWorkerRequest struct {
	Worker WorkerInfo `json:"worker"`
}

type RegisterWorkerResponse struct {
	WorkerID string `json:"worker_id"`
}

type ClaimJobRequest struct {
	WorkerID     string   `json:"worker_id"`
	Kinds        []string `json:"kinds"`
	LeaseSeconds int      `json:"lease_seconds"`
}

type ClaimJobResponse struct {
	Job *Job `json:"job,omitempty"`
}

type CompleteJobRequest struct {
	WorkerID string          `json:"worker_id"`
	JobID    string          `json:"job_id"`
	Result   json.RawMessage `json:"result"`
	Metrics  json.RawMessage `json:"metrics"`
}

type FailJobRequest struct {
	WorkerID string          `json:"worker_id"`
	JobID    string          `json:"job_id"`
	Error    string          `json:"error"`
	Metrics  json.RawMessage `json:"metrics"`
}

type HeartbeatRequest struct {
	WorkerID      string `json:"worker_id"`
	JobID         string `json:"job_id"`
	ExtendSeconds int    `json:"extend_seconds"`
}

type DeviceOfflineRequest struct {
	DeviceID string `json:"device_id"`
	Reason   string `json:"reason"`
}

type LLMRequest struct {
	Task        string              `json:"task"`
	Provider    string              `json:"provider"`
	Model       string              `json:"model"`
	Prompt      string              `json:"prompt"`
	Messages    []map[string]string `json:"messages"`
	Temperature *float64            `json:"temperature"`
	MaxTokens   *int                `json:"max_tokens"`
	Options     map[string]any      `json:"options"`
	Payload     json.RawMessage     `json:"payload"`
	Priority    int                 `json:"priority"`
	Source      string              `json:"source"`
	MaxAttempts int                 `json:"max_attempts"`
	DeadlineAt  string              `json:"deadline_at"`
	Constraints struct {
		PreferLocal  bool `json:"prefer_local"`
		ForceCloud   bool `json:"force_cloud"`
		MaxLatencyMs int  `json:"max_latency_ms"`
	} `json:"constraints"`
}

type LLMResponse struct {
	JobID    string `json:"job_id"`
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
}

type BenchmarkRunRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	TaskType string `json:"task_type"`
	Prompt   string `json:"prompt"`
	Runs     int    `json:"runs"`
	Priority int    `json:"priority"`
	DeviceID string `json:"device_id"`
}

type BenchmarkRow struct {
	DeviceID  string    `json:"device_id"`
	ModelID   string    `json:"model_id"`
	TaskType  string    `json:"task_type"`
	TokensIn  int       `json:"tokens_in"`
	TokensOut int       `json:"tokens_out"`
	LatencyMs int       `json:"latency_ms"`
	TPS       float64   `json:"tps"`
	CreatedAt time.Time `json:"created_at"`
}

type Job struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	LeaseUntil  *time.Time      `json:"lease_until,omitempty"`
	DeadlineAt  *time.Time      `json:"deadline_at,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
	Priority    int             `json:"priority"`
	QueuedAt    time.Time       `json:"queued_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type DeviceTarget struct {
	ID   string
	Addr string
	Host string
}

type DeviceLimitSpec struct {
	RamGB       *float64 `json:"ram_gb"`
	VramGB      *float64 `json:"vram_gb"`
	MaxParamsB  *float64 `json:"max_params_b"`
	MaxSizeGB   *float64 `json:"max_size_gb"`
	MaxContextK *int     `json:"max_context_k"`
	AllowModels []string `json:"allow_models"`
	DenyModels  []string `json:"deny_models"`
}

// RunningJob — информация о текущей job для dashboard
type RunningJob struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Model     string    `json:"model"`
	Provider  string    `json:"provider"`
	DeviceID  string    `json:"device_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DeviceInfo — информация об устройстве для dashboard
type DeviceInfo struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Status     string          `json:"status"`
	Platform   string          `json:"platform"`
	Arch       string          `json:"arch"`
	Host       string          `json:"host"`
	Models     int             `json:"models_count"`
	ModelNames json.RawMessage `json:"model_names"`
	Load       int             `json:"running_jobs"`
	Latency    *int            `json:"latency_ms"`
	LastSeen   *time.Time      `json:"last_seen"`
}

// ProviderCost — расходы по провайдеру
type ProviderCost struct {
	Provider  string  `json:"provider"`
	Cost      float64 `json:"cost_usd"`
	Jobs      int     `json:"jobs"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
}
