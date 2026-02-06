package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"llm-mcp/core/internal/discovery"
	"llm-mcp/core/internal/grpcserver"
	"llm-mcp/core/internal/pb"
)

type healthResp struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

type submitJobRequest struct {
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload"`
	Priority    int             `json:"priority"`
	Source      string          `json:"source"`
	MaxAttempts int             `json:"max_attempts"`
	DeadlineAt  string          `json:"deadline_at"`
}

type submitJobResponse struct {
	JobID string `json:"job_id"`
}

type workerInfo struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Platform string          `json:"platform"`
	Arch     string          `json:"arch"`
	Host     string          `json:"host"`
	Tags     json.RawMessage `json:"tags"`
}

type registerWorkerRequest struct {
	Worker workerInfo `json:"worker"`
}

type registerWorkerResponse struct {
	WorkerID string `json:"worker_id"`
}

type claimJobRequest struct {
	WorkerID     string   `json:"worker_id"`
	Kinds        []string `json:"kinds"`
	LeaseSeconds int      `json:"lease_seconds"`
}

type claimJobResponse struct {
	Job *job `json:"job,omitempty"`
}

type completeJobRequest struct {
	WorkerID string          `json:"worker_id"`
	JobID    string          `json:"job_id"`
	Result   json.RawMessage `json:"result"`
	Metrics  json.RawMessage `json:"metrics"`
}

type failJobRequest struct {
	WorkerID string          `json:"worker_id"`
	JobID    string          `json:"job_id"`
	Error    string          `json:"error"`
	Metrics  json.RawMessage `json:"metrics"`
}

type heartbeatRequest struct {
	WorkerID      string `json:"worker_id"`
	JobID         string `json:"job_id"`
	ExtendSeconds int    `json:"extend_seconds"`
}

type deviceOfflineRequest struct {
	DeviceID string `json:"device_id"`
	Reason   string `json:"reason"`
}

type llmRequest struct {
	Task        string          `json:"task"`
	Provider    string          `json:"provider"`
	Model       string          `json:"model"`
	Prompt      string          `json:"prompt"`
	Messages    []map[string]string `json:"messages"`
	Temperature *float64        `json:"temperature"`
	MaxTokens   *int            `json:"max_tokens"`
	Options     map[string]any  `json:"options"`
	Payload     json.RawMessage `json:"payload"`
	Priority    int             `json:"priority"`
	Source      string          `json:"source"`
	MaxAttempts int             `json:"max_attempts"`
	DeadlineAt  string          `json:"deadline_at"`
	Constraints struct {
		PreferLocal bool `json:"prefer_local"`
		ForceCloud  bool `json:"force_cloud"`
		MaxLatencyMs int `json:"max_latency_ms"`
	} `json:"constraints"`
}

type llmResponse struct {
	JobID    string `json:"job_id"`
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
}

type benchmarkRunRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	TaskType string `json:"task_type"`
	Prompt   string `json:"prompt"`
	Runs     int    `json:"runs"`
	Priority int    `json:"priority"`
	DeviceID string `json:"device_id"`
}

type benchmarkRow struct {
	DeviceID  string    `json:"device_id"`
	ModelID   string    `json:"model_id"`
	TaskType  string    `json:"task_type"`
	TokensIn  int       `json:"tokens_in"`
	TokensOut int       `json:"tokens_out"`
	LatencyMs int       `json:"latency_ms"`
	TPS       float64   `json:"tps"`
	CreatedAt time.Time `json:"created_at"`
}

type job struct {
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

type server struct {
	db      *pgxpool.Pool
	version string
	discovery *discovery.Runner
}

type deviceTarget struct {
	ID   string
	Addr string
	Host string
}

type deviceLimitSpec struct {
	RamGB        *float64 `json:"ram_gb"`
	VramGB       *float64 `json:"vram_gb"`
	MaxParamsB   *float64 `json:"max_params_b"`
	MaxSizeGB    *float64 `json:"max_size_gb"`
	MaxContextK  *int     `json:"max_context_k"`
	AllowModels  []string `json:"allow_models"`
	DenyModels   []string `json:"deny_models"`
}

func main() {
	addr := getenv("CORE_HTTP_ADDR", ":8080")
	grpcAddr := getenv("CORE_GRPC_ADDR", ":9090")
	version := getenv("CORE_VERSION", getenv("LLM_MCP_VERSION", "0.1.0"))
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		log.Fatal("DB_DSN is required")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("db connect error: %v", err)
	}
	defer pool.Close()
	if err := ensureSchema(ctx, pool); err != nil {
		log.Printf("schema ensure error: %v", err)
	}

	disc := discovery.New(pool)
	srv := &server{db: pool, version: version, discovery: disc}
	if err := srv.applyDeviceLimits(context.Background()); err != nil {
		log.Printf("device limits apply error: %v", err)
	}
	if interval := getenvInt("DEVICE_LIMITS_INTERVAL", 0); interval > 0 {
		go func() {
			ticker := time.NewTicker(time.Duration(interval) * time.Second)
			defer ticker.Stop()
			for {
				if err := srv.applyDeviceLimits(context.Background()); err != nil {
					log.Printf("device limits apply error: %v", err)
				}
				<-ticker.C
			}
		}()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/version", srv.handleHealth)
	mux.HandleFunc("/v1/jobs", srv.handleJobs)
	mux.HandleFunc("/v1/jobs/", srv.handleJobByID)
	mux.HandleFunc("/v1/workers/register", srv.handleWorkerRegister)
	mux.HandleFunc("/v1/workers/claim", srv.handleWorkerClaim)
	mux.HandleFunc("/v1/workers/complete", srv.handleWorkerComplete)
	mux.HandleFunc("/v1/workers/fail", srv.handleWorkerFail)
	mux.HandleFunc("/v1/workers/heartbeat", srv.handleWorkerHeartbeat)
	mux.HandleFunc("/v1/devices/offline", srv.handleDeviceOffline)
	mux.HandleFunc("/v1/discovery/run", srv.handleDiscoveryRun)
	mux.HandleFunc("/v1/discovery/last", srv.handleDiscoveryLast)
	mux.HandleFunc("/v1/llm/request", srv.handleLLMRequest)
	mux.HandleFunc("/v1/benchmarks/run", srv.handleBenchmarkRun)
	mux.HandleFunc("/v1/benchmarks", srv.handleBenchmarksList)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("core http listening on %s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http error: %v", err)
		}
	}()

	go func() {
		lis, err := net.Listen("tcp", grpcAddr)
		if err != nil {
			log.Fatalf("grpc listen error: %v", err)
		}
		grpcSrv := grpc.NewServer()
		pb.RegisterCoreServer(grpcSrv, grpcserver.New(pool))
		log.Printf("core grpc listening on %s", grpcAddr)
		if err := grpcSrv.Serve(lis); err != nil {
			log.Fatalf("grpc serve error: %v", err)
		}
	}()

	if interval := getenv("DISCOVERY_INTERVAL", "0"); interval != "0" {
		if secs, err := strconv.Atoi(interval); err == nil && secs > 0 {
			go func() {
				ticker := time.NewTicker(time.Duration(secs) * time.Second)
				defer ticker.Stop()
				for {
					_ = srv.discovery.Run(context.Background())
					<-ticker.C
				}
			}()
		}
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	writeJSON(w, http.StatusOK, healthResp{Status: "ok", Version: s.version})
}

func (s *server) handleJobs(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req submitJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.Kind) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind_required"})
		return
	}
	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage("{}")
	}
	if req.MaxAttempts == 0 {
		req.MaxAttempts = 3
	}

	var deadline *time.Time
	if strings.TrimSpace(req.DeadlineAt) != "" {
		t, err := time.Parse(time.RFC3339, req.DeadlineAt)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_deadline_at"})
			return
		}
		deadline = &t
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if strings.HasPrefix(req.Kind, "ollama.") || strings.HasPrefix(req.Kind, "benchmark.ollama.") {
		model, device := parsePayloadModelDevice(req.Payload)
		if model != "" && device != "" {
			if ok, reason := s.modelAllowed(ctx, device, model); !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model_not_allowed", "reason": reason})
				return
			}
		}
	}

	row := s.db.QueryRow(ctx, `
		INSERT INTO jobs (kind, payload, priority, source, max_attempts, deadline_at)
		VALUES ($1, $2::jsonb, $3, $4, $5, $6)
		RETURNING id
	`, req.Kind, req.Payload, req.Priority, req.Source, req.MaxAttempts, deadline)

	var jobID string
	if err := row.Scan(&jobID); err != nil {
		log.Printf("job enqueue error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_insert_failed"})
		return
	}
	log.Printf("job enqueued id=%s kind=%s priority=%d", jobID, req.Kind, req.Priority)
	writeJSON(w, http.StatusAccepted, submitJobResponse{JobID: jobID})
}

func (s *server) handleJobByID(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	path := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	if path == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if strings.HasSuffix(path, "/stream") {
		jobID := strings.TrimSuffix(path, "/stream")
		s.handleJobStream(w, r, jobID)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	jobID := path
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	jobRow, err := s.fetchJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_read_failed"})
		return
	}
	writeJSON(w, http.StatusOK, jobRow)
}

func (s *server) handleWorkerRegister(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req registerWorkerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	workerID := strings.TrimSpace(req.Worker.ID)
	if workerID == "" {
		workerID = "worker-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	if len(req.Worker.Tags) == 0 {
		req.Worker.Tags = json.RawMessage("{}")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	_, err := s.db.Exec(ctx, `
		INSERT INTO devices (id, name, platform, arch, host, tags, status, last_seen, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, 'online', now(), now())
		ON CONFLICT (id) DO UPDATE SET
		  name = excluded.name,
		  platform = excluded.platform,
		  arch = excluded.arch,
		  host = excluded.host,
		  tags = excluded.tags,
		  status = 'online',
		  last_seen = now(),
		  updated_at = now()
	`, workerID, req.Worker.Name, req.Worker.Platform, req.Worker.Arch, req.Worker.Host, req.Worker.Tags)
	if err != nil {
		log.Printf("worker register db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_upsert_failed"})
		return
	}
	log.Printf("worker registered id=%s name=%s platform=%s arch=%s", workerID, req.Worker.Name, req.Worker.Platform, req.Worker.Arch)
	writeJSON(w, http.StatusOK, registerWorkerResponse{WorkerID: workerID})
}

func (s *server) handleWorkerClaim(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req claimJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.WorkerID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id_required"})
		return
	}
	leaseSeconds := req.LeaseSeconds
	if leaseSeconds <= 0 {
		leaseSeconds = 60
	}
	deviceLimit := getenvInt("DEVICE_MAX_CONCURRENCY", 1)
	if deviceLimit < 1 {
		deviceLimit = 1
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	tx, err := s.db.Begin(ctx)
	if err != nil {
		log.Printf("worker claim begin error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_tx_failed"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	query := `
		SELECT id, kind, payload, status, attempts, max_attempts,
		       lease_until, deadline_at, result, error, priority, queued_at, updated_at
		FROM jobs j
		WHERE status IN ('queued','running')
		  AND (lease_until IS NULL OR lease_until < now())
	`
	args := []any{}
	argPos := 1
	if len(req.Kinds) > 0 {
		query += " AND j.kind = ANY($1)\n"
		args = append(args, req.Kinds)
		argPos++
	}
	query += fmt.Sprintf(`
		  AND (
		    j.payload->>'device_id' IS NULL OR j.payload->>'device_id' = ''
		    OR (
		      SELECT count(*)
		      FROM jobs r
		      WHERE r.status = 'running'
		        AND r.lease_until > now()
		        AND r.payload->>'device_id' = j.payload->>'device_id'
		    ) < $%d
		  )
		  AND (
		    j.payload->>'device_id' IS NULL OR j.payload->>'device_id' = ''
		    OR EXISTS (
		      SELECT 1 FROM devices d
		      WHERE d.id = j.payload->>'device_id'
		        AND d.status = 'online'
		    )
		  )
	`, argPos)
	args = append(args, deviceLimit)
	argPos++
	query += " ORDER BY priority DESC, queued_at FOR UPDATE SKIP LOCKED LIMIT 1"

	row := tx.QueryRow(ctx, query, args...)
	var j job
	var errText sql.NullString
	var result json.RawMessage
	if err := row.Scan(
		&j.ID,
		&j.Kind,
		&j.Payload,
		&j.Status,
		&j.Attempts,
		&j.MaxAttempts,
		&j.LeaseUntil,
		&j.DeadlineAt,
		&result,
		&errText,
		&j.Priority,
		&j.QueuedAt,
		&j.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			log.Printf("worker claim: empty queue worker=%s", req.WorkerID)
			writeJSON(w, http.StatusOK, claimJobResponse{Job: nil})
			return
		}
		log.Printf("worker claim scan error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_read_failed"})
		return
	}
	if errText.Valid {
		j.Error = errText.String
	}
	j.Result = result

	leaseUntil := time.Now().UTC().Add(time.Duration(leaseSeconds) * time.Second)
	_, err = tx.Exec(ctx, `
		UPDATE jobs
		SET status='running', attempts=attempts+1, lease_until=$1, updated_at=now()
		WHERE id=$2
	`, leaseUntil, j.ID)
	if err != nil {
		log.Printf("worker claim update error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_update_failed"})
		return
	}
	_, _ = tx.Exec(ctx, `
		INSERT INTO job_attempts (job_id, worker_id, status)
		VALUES ($1, $2, 'running')
	`, j.ID, req.WorkerID)

	if err := tx.Commit(ctx); err != nil {
		log.Printf("worker claim commit error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_commit_failed"})
		return
	}

	j.Attempts += 1
	j.Status = "running"
	j.LeaseUntil = &leaseUntil
	log.Printf("job claimed id=%s worker=%s kind=%s attempts=%d", j.ID, req.WorkerID, j.Kind, j.Attempts)
	writeJSON(w, http.StatusOK, claimJobResponse{Job: &j})
}

func (s *server) handleWorkerComplete(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req completeJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.WorkerID) == "" || strings.TrimSpace(req.JobID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id_job_id_required"})
		return
	}
	if len(req.Result) == 0 {
		req.Result = json.RawMessage("{}")
	}
	if len(req.Metrics) == 0 {
		req.Metrics = json.RawMessage("{}")
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := s.db.Exec(ctx, `
		UPDATE jobs
		SET status='done', result=$1::jsonb, lease_until=NULL, updated_at=now()
		WHERE id=$2
	`, req.Result, req.JobID)
	if err != nil {
		log.Printf("worker complete update error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_update_failed"})
		return
	}
	_, _ = s.db.Exec(ctx, `
		UPDATE job_attempts
		SET status='done', finished_at=now(), metrics=$1::jsonb
		WHERE id = (
		  SELECT id FROM job_attempts
		  WHERE job_id=$2 AND status='running'
		  ORDER BY started_at DESC
		  LIMIT 1
		)
	`, req.Metrics, req.JobID)
	log.Printf("job complete id=%s worker=%s", req.JobID, req.WorkerID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleWorkerFail(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req failJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.WorkerID) == "" || strings.TrimSpace(req.JobID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id_job_id_required"})
		return
	}
	if len(req.Metrics) == 0 {
		req.Metrics = json.RawMessage("{}")
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var attempts int
	var maxAttempts int
	row := s.db.QueryRow(ctx, `SELECT attempts, max_attempts FROM jobs WHERE id=$1`, req.JobID)
	if err := row.Scan(&attempts, &maxAttempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		log.Printf("worker fail read error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_read_failed"})
		return
	}
	status := "queued"
	if attempts >= maxAttempts {
		status = "error"
	}
	_, err := s.db.Exec(ctx, `
		UPDATE jobs
		SET status=$1, error=$2, lease_until=NULL, updated_at=now()
		WHERE id=$3
	`, status, req.Error, req.JobID)
	if err != nil {
		log.Printf("worker fail update error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_update_failed"})
		return
	}
	_, _ = s.db.Exec(ctx, `
		UPDATE job_attempts
		SET status='error', finished_at=now(), error=$1, metrics=$2::jsonb
		WHERE id = (
		  SELECT id FROM job_attempts
		  WHERE job_id=$3 AND status='running'
		  ORDER BY started_at DESC
		  LIMIT 1
		)
	`, req.Error, req.Metrics, req.JobID)
	log.Printf("job failed id=%s worker=%s status=%s error=%s", req.JobID, req.WorkerID, status, req.Error)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req heartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.WorkerID) == "" || strings.TrimSpace(req.JobID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id_job_id_required"})
		return
	}
	if req.ExtendSeconds <= 0 {
		req.ExtendSeconds = 30
	}

	leaseUntil := time.Now().UTC().Add(time.Duration(req.ExtendSeconds) * time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	_, err := s.db.Exec(ctx, `
		UPDATE jobs
		SET lease_until=$1, updated_at=now()
		WHERE id=$2 AND status='running'
	`, leaseUntil, req.JobID)
	if err != nil {
		log.Printf("worker heartbeat update error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_update_failed"})
		return
	}
	log.Printf("job heartbeat id=%s worker=%s extend=%ds", req.JobID, req.WorkerID, req.ExtendSeconds)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleDeviceOffline(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req deviceOfflineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.DeviceID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_id_required"})
		return
	}
	meta := map[string]any{
		"last_error":   req.Reason,
		"last_error_at": time.Now().UTC().Format(time.RFC3339),
	}
	metaJSON, _ := json.Marshal(meta)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	_, err := s.db.Exec(ctx, `
		UPDATE devices
		SET status='offline',
		    tags = COALESCE(tags, '{}'::jsonb) || $1::jsonb,
		    updated_at=now()
		WHERE id=$2
	`, metaJSON, req.DeviceID)
	if err != nil {
		log.Printf("device offline update error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_update_failed"})
		return
	}
	log.Printf("device marked offline id=%s reason=%s", req.DeviceID, req.Reason)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleJobStream(w http.ResponseWriter, r *http.Request, jobID string) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "stream_not_supported"})
		return
	}

	ctx := r.Context()
	var lastStatus string
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
			jobRow, err := s.fetchJob(ctx, jobID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
					writeSSE(w, "error", map[string]string{"error": "not_found"})
					flusher.Flush()
					return
				}
				writeSSE(w, "error", map[string]string{"error": "db_read_failed"})
				flusher.Flush()
				continue
			}
			if lastStatus == "" || jobRow.Status != lastStatus {
				writeSSE(w, "status", jobRow)
				flusher.Flush()
				lastStatus = jobRow.Status
			}
			if jobRow.Status == "done" || jobRow.Status == "error" {
				return
			}
		}
	}
}

func (s *server) handleDiscoveryRun(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.discovery.Run(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "discovery_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handleDiscoveryLast(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	last := s.discovery.LastRun()
	resp := map[string]string{"last_run": ""}
	if !last.IsZero() {
		resp["last_run"] = last.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleLLMRequest(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req llmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	provider, kind, payload, err := s.routeLLM(ctx, req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.MaxAttempts == 0 {
		req.MaxAttempts = 3
	}

	var deadline *time.Time
	if strings.TrimSpace(req.DeadlineAt) != "" {
		t, err := time.Parse(time.RFC3339, req.DeadlineAt)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_deadline_at"})
			return
		}
		deadline = &t
	}

	row := s.db.QueryRow(ctx, `
		INSERT INTO jobs (kind, payload, priority, source, max_attempts, deadline_at)
		VALUES ($1, $2::jsonb, $3, $4, $5, $6)
		RETURNING id
	`, kind, payload, req.Priority, req.Source, req.MaxAttempts, deadline)
	var jobID string
	if err := row.Scan(&jobID); err != nil {
		log.Printf("llm request enqueue error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_insert_failed"})
		return
	}
	log.Printf("llm route provider=%s kind=%s job=%s", provider, kind, jobID)
	writeJSON(w, http.StatusAccepted, llmResponse{JobID: jobID, Provider: provider, Kind: kind})
}

func (s *server) handleBenchmarkRun(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req benchmarkRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" {
		provider = "ollama"
	}
	task := strings.ToLower(strings.TrimSpace(req.TaskType))
	if task == "" {
		task = "generate"
	}
	if provider != "ollama" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider_not_supported"})
		return
	}
	kind := "benchmark.ollama." + task
	if req.Runs <= 0 {
		req.Runs = 1
	}
	if req.Priority == 0 {
		req.Priority = 1
	}
	payload := map[string]any{
		"model":  req.Model,
		"prompt": req.Prompt,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if strings.TrimSpace(req.DeviceID) != "" {
		if req.Model != "" {
			if ok, reason := s.modelAllowed(ctx, req.DeviceID, req.Model); !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model_not_allowed", "reason": reason})
				return
			}
		}
		addr, host, err := s.fetchDeviceAddr(ctx, req.DeviceID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_not_found"})
			return
		}
		if addr == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_no_ollama_addr"})
			return
		}
		payload["ollama_addr"] = addr
		if host != "" {
			payload["ollama_host"] = host
		}
		payload["device_id"] = req.DeviceID
	}
	payloadJSON, _ := json.Marshal(payload)

	jobIDs := make([]string, 0, req.Runs)
	for i := 0; i < req.Runs; i++ {
		row := s.db.QueryRow(ctx, `
			INSERT INTO jobs (kind, payload, priority, source, max_attempts)
			VALUES ($1, $2::jsonb, $3, $4, $5)
			RETURNING id
		`, kind, payloadJSON, req.Priority, "benchmark", 2)
		var jobID string
		if err := row.Scan(&jobID); err != nil {
			log.Printf("benchmark enqueue error: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_insert_failed"})
			return
		}
		jobIDs = append(jobIDs, jobID)
	}
	log.Printf("benchmark queued kind=%s runs=%d", kind, len(jobIDs))
	writeJSON(w, http.StatusAccepted, map[string]any{"job_ids": jobIDs, "kind": kind})
}

func (s *server) handleBenchmarksList(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	rows, err := s.db.Query(ctx, `
		SELECT device_id, model_id, task_type, tokens_in, tokens_out, latency_ms, tps, created_at
		FROM benchmarks
		ORDER BY created_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_read_failed"})
		return
	}
	defer rows.Close()
	list := []benchmarkRow{}
	for rows.Next() {
		var row benchmarkRow
		if err := rows.Scan(&row.DeviceID, &row.ModelID, &row.TaskType, &row.TokensIn, &row.TokensOut, &row.LatencyMs, &row.TPS, &row.CreatedAt); err != nil {
			continue
		}
		list = append(list, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *server) routeLLM(ctx context.Context, req llmRequest) (string, string, json.RawMessage, error) {
	task := strings.ToLower(strings.TrimSpace(req.Task))
	if task == "" {
		task = "chat"
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" {
		provider = "auto"
	}
	localAvailable := s.hasLocalOllama(ctx)

	if provider == "auto" {
		if task == "embed" {
			provider = "ollama"
		} else if req.Constraints.ForceCloud {
			if hasOpenrouter() {
				provider = "openrouter"
			} else if hasOpenai() {
				provider = "openai"
			} else if localAvailable {
				provider = "ollama"
			} else {
				provider = "ollama"
			}
		} else if req.Constraints.PreferLocal && localAvailable {
			provider = "ollama"
		} else if hasOpenrouter() {
			provider = "openrouter"
		} else if hasOpenai() {
			provider = "openai"
		} else if localAvailable {
			provider = "ollama"
		} else {
			provider = "ollama"
		}
	}

	if provider == "ollama" && req.Constraints.MaxLatencyMs > 0 && req.Model != "" {
		if ok := s.meetsLatency(ctx, req.Model, task, req.Constraints.MaxLatencyMs); !ok {
			if hasOpenrouter() {
				provider = "openrouter"
			} else if hasOpenai() {
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
			model, device := parsePayloadModelDevice(req.Payload)
			if model != "" && device != "" {
				if ok, reason := s.modelAllowed(ctx, device, model); !ok {
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
			prompt = messagesToPrompt(req.Messages)
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
			if target := s.selectOllamaDevice(ctx, req.Model, "generate"); target != nil {
				payload["ollama_addr"] = target.Addr
				if target.Host != "" {
					payload["ollama_host"] = target.Host
				}
				payload["device_id"] = target.ID
			} else if getenv("STRICT_MODEL_LIMITS", "0") == "1" {
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
			if target := s.selectOllamaDevice(ctx, req.Model, "embed"); target != nil {
				payload["ollama_addr"] = target.Addr
				if target.Host != "" {
					payload["ollama_host"] = target.Host
				}
				payload["device_id"] = target.ID
			} else if getenv("STRICT_MODEL_LIMITS", "0") == "1" {
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

func (s *server) selectOllamaDevice(ctx context.Context, model string, task string) *deviceTarget {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	taskType := task
	strict := getenv("STRICT_MODEL_LIMITS", "0") == "1"
	strictInt := 0
	if strict {
		strictInt = 1
	}
	row := s.db.QueryRow(ctx, `
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
	`, model, taskType, strictInt)
	var target deviceTarget
	var tps sql.NullFloat64
	var latency sql.NullInt64
	if err := row.Scan(&target.ID, &target.Addr, &target.Host, &tps, &latency); err != nil {
		return nil
	}
	log.Printf("route: device select model=%s task=%s device=%s addr=%s tps=%v latency=%v", model, taskType, target.ID, target.Addr, tps, latency)
	return &target
}

func (s *server) fetchDeviceAddr(ctx context.Context, id string) (string, string, error) {
	row := s.db.QueryRow(ctx, `
		SELECT tags->>'ollama_addr' AS addr, tags->>'ollama_host' AS host
		FROM devices
		WHERE id = $1
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

func (s *server) hasLocalOllama(ctx context.Context) bool {
	row := s.db.QueryRow(ctx, `
		SELECT 1
		FROM devices
		WHERE status='online' AND tags->>'ollama'='true'
		LIMIT 1
	`)
	var v int
	if err := row.Scan(&v); err != nil {
		return false
	}
	return true
}

func (s *server) meetsLatency(ctx context.Context, model string, task string, maxMs int) bool {
	taskType := task
	row := s.db.QueryRow(ctx, `
		SELECT latency_ms
		FROM benchmarks
		WHERE model_id=$1 AND task_type=$2
		ORDER BY created_at DESC
		LIMIT 1
	`, model, taskType)
	var ms int
	if err := row.Scan(&ms); err != nil {
		return false
	}
	return ms > 0 && ms <= maxMs
}

func messagesToPrompt(messages []map[string]string) string {
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

func hasOpenai() bool {
	return os.Getenv("OPENAI_API_KEY") != ""
}

func hasOpenrouter() bool {
	return os.Getenv("OPENROUTER_API_KEY") != ""
}

func ensureSchema(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `ALTER TABLE IF EXISTS benchmarks ADD COLUMN IF NOT EXISTS meta JSONB;`)
	return err
}

func (s *server) fetchJob(ctx context.Context, id string) (*job, error) {
	row := s.db.QueryRow(ctx, `
		SELECT id, kind, payload, status, attempts, max_attempts,
		       lease_until, deadline_at, result, error, priority, queued_at, updated_at
		FROM jobs
		WHERE id = $1
	`, id)
	var j job
	var errText sql.NullString
	var result json.RawMessage
	if err := row.Scan(
		&j.ID,
		&j.Kind,
		&j.Payload,
		&j.Status,
		&j.Attempts,
		&j.MaxAttempts,
		&j.LeaseUntil,
		&j.DeadlineAt,
		&result,
		&errText,
		&j.Priority,
		&j.QueuedAt,
		&j.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if errText.Valid {
		j.Error = errText.String
	}
	j.Result = result
	return &j, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeSSE(w http.ResponseWriter, event string, payload any) {
	data, _ := json.Marshal(payload)
	_, _ = w.Write([]byte("event: " + event + "\n"))
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func parsePayloadModelDevice(payload json.RawMessage) (string, string) {
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

func (s *server) applyDeviceLimits(ctx context.Context) error {
	specs, defaultSpec, err := loadDeviceLimitSpecs()
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
		rows, err := s.db.Query(ctx, `
			SELECT id FROM devices
			WHERE tags->>'ollama' = 'true'
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
		derived := deriveDeviceLimits(spec)
		allowJSON, _ := json.Marshal(derived.AllowModels)
		denyJSON, _ := json.Marshal(derived.DenyModels)
		_, err := s.db.Exec(ctx, `
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

func loadDeviceLimitSpecs() (map[string]deviceLimitSpec, *deviceLimitSpec, error) {
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

	var rawMap map[string]deviceLimitSpec
	if err := json.Unmarshal([]byte(raw), &rawMap); err != nil {
		return nil, nil, err
	}
	specs := map[string]deviceLimitSpec{}
	var defaultSpec *deviceLimitSpec
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

func deriveDeviceLimits(spec deviceLimitSpec) deviceLimitSpec {
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

func (s *server) modelAllowed(ctx context.Context, deviceID string, model string) (bool, string) {
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

	err := s.db.QueryRow(ctx, `
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

	strict := getenv("STRICT_MODEL_LIMITS", "0") == "1"
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
