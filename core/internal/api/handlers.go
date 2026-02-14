package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"llm-mcp/core/internal/config"
	"llm-mcp/core/internal/limits"
	"llm-mcp/core/internal/models"
	"llm-mcp/core/internal/routing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (s *Server) HandleHealth(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	WriteJSON(w, http.StatusOK, models.HealthResp{Status: "ok", Version: s.Version})
}

func (s *Server) HandleJobs(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req models.SubmitJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.Kind) == "" {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "kind_required"})
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
			WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_deadline_at"})
			return
		}
		deadline = &t
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if strings.HasPrefix(req.Kind, "ollama.") || strings.HasPrefix(req.Kind, "benchmark.ollama.") {
		model, device := routing.ParsePayloadModelDevice(req.Payload)
		if model != "" && device != "" {
			if ok, reason := limits.ModelAllowed(ctx, s.DB, device, model); !ok {
				WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "model_not_allowed", "reason": reason})
				return
			}
		}
	}

	row := s.DB.QueryRow(ctx, `
		INSERT INTO jobs (kind, payload, priority, source, max_attempts, deadline_at)
		VALUES ($1, $2::jsonb, $3, $4, $5, $6)
		RETURNING id
	`, req.Kind, req.Payload, req.Priority, req.Source, req.MaxAttempts, deadline)

	var jobID string
	if err := row.Scan(&jobID); err != nil {
		log.Printf("job enqueue error: %v", err)
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_insert_failed"})
		return
	}
	log.Printf("job enqueued id=%s kind=%s priority=%d", jobID, req.Kind, req.Priority)
	WriteJSON(w, http.StatusAccepted, models.SubmitJobResponse{JobID: jobID})
}

func (s *Server) HandleJobByID(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	path := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	if path == "" {
		WriteJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if strings.HasSuffix(path, "/stream") {
		jobID := strings.TrimSuffix(path, "/stream")
		s.handleJobStream(w, r, jobID)
		return
	}
	if r.Method != http.MethodGet {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	jobID := path
	if !uuidRe.MatchString(jobID) {
		WriteJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	jobRow, err := s.fetchJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			WriteJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_read_failed"})
		return
	}
	WriteJSON(w, http.StatusOK, jobRow)
}

func (s *Server) HandleWorkerRegister(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req models.RegisterWorkerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
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
	_, err := s.DB.Exec(ctx, `
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
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_upsert_failed"})
		return
	}
	log.Printf("worker registered id=%s name=%s platform=%s arch=%s", workerID, req.Worker.Name, req.Worker.Platform, req.Worker.Arch)
	WriteJSON(w, http.StatusOK, models.RegisterWorkerResponse{WorkerID: workerID})
}

func (s *Server) HandleWorkerClaim(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req models.ClaimJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.WorkerID) == "" {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id_required"})
		return
	}
	leaseSeconds := req.LeaseSeconds
	if leaseSeconds <= 0 {
		leaseSeconds = 60
	}
	deviceLimit := config.GetenvInt("DEVICE_MAX_CONCURRENCY", 1)
	if deviceLimit < 1 {
		deviceLimit = 1
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		log.Printf("worker claim begin error: %v", err)
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_tx_failed"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// CTE: предварительно считаем running jobs per device (вместо correlated subquery)
	args := []any{}
	argPos := 1

	cte := `WITH running_per_device AS (
		SELECT payload->>'device_id' AS device_id, count(*) AS cnt
		FROM jobs
		WHERE status = 'running' AND lease_until > now()
		GROUP BY payload->>'device_id'
	)`

	query := cte + `
		SELECT j.id, j.kind, j.payload, j.status, j.attempts, j.max_attempts,
		       j.lease_until, j.deadline_at, j.result, j.error, j.priority, j.queued_at, j.updated_at
		FROM jobs j
		LEFT JOIN running_per_device rpd ON rpd.device_id = j.payload->>'device_id'
		WHERE j.status IN ('queued','running')
		  AND (j.lease_until IS NULL OR j.lease_until < now())
	`
	if len(req.Kinds) > 0 {
		query += fmt.Sprintf(" AND j.kind = ANY($%d)\n", argPos)
		args = append(args, req.Kinds)
		argPos++
	}
	query += fmt.Sprintf(`
		  AND (
		    j.payload->>'device_id' IS NULL OR j.payload->>'device_id' = ''
		    OR COALESCE(rpd.cnt, 0) < $%d
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
	query += " ORDER BY j.priority DESC, j.queued_at FOR UPDATE SKIP LOCKED LIMIT 1"

	row := tx.QueryRow(ctx, query, args...)
	var j models.Job
	var errText sql.NullString
	var result json.RawMessage
	if err := row.Scan(
		&j.ID, &j.Kind, &j.Payload, &j.Status, &j.Attempts, &j.MaxAttempts,
		&j.LeaseUntil, &j.DeadlineAt, &result, &errText, &j.Priority, &j.QueuedAt, &j.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			log.Printf("worker claim: empty queue worker=%s", req.WorkerID)
			WriteJSON(w, http.StatusOK, models.ClaimJobResponse{Job: nil})
			return
		}
		log.Printf("worker claim scan error: %v", err)
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_read_failed"})
		return
	}
	if errText.Valid {
		j.Error = errText.String
	}
	j.Result = result

	leaseUntil := time.Now().UTC().Add(time.Duration(leaseSeconds) * time.Second)
	_, err = tx.Exec(ctx, `
		UPDATE jobs SET status='running', attempts=attempts+1, lease_until=$1, updated_at=now() WHERE id=$2
	`, leaseUntil, j.ID)
	if err != nil {
		log.Printf("worker claim update error: %v", err)
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_update_failed"})
		return
	}
	_, _ = tx.Exec(ctx, `INSERT INTO job_attempts (job_id, worker_id, status) VALUES ($1, $2, 'running')`, j.ID, req.WorkerID)

	if err := tx.Commit(ctx); err != nil {
		log.Printf("worker claim commit error: %v", err)
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_commit_failed"})
		return
	}

	j.Attempts += 1
	j.Status = "running"
	j.LeaseUntil = &leaseUntil
	log.Printf("job claimed id=%s worker=%s kind=%s attempts=%d", j.ID, req.WorkerID, j.Kind, j.Attempts)
	WriteJSON(w, http.StatusOK, models.ClaimJobResponse{Job: &j})
}

func (s *Server) HandleWorkerComplete(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req models.CompleteJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.WorkerID) == "" || strings.TrimSpace(req.JobID) == "" {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id_job_id_required"})
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
	_, err := s.DB.Exec(ctx, `
		UPDATE jobs SET status='done', result=$1::jsonb, lease_until=NULL, updated_at=now() WHERE id=$2
	`, req.Result, req.JobID)
	if err != nil {
		log.Printf("worker complete update error: %v", err)
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_update_failed"})
		return
	}
	_, _ = s.DB.Exec(ctx, `
		UPDATE job_attempts
		SET status='done', finished_at=now(), metrics=$1::jsonb
		WHERE id = (
		  SELECT id FROM job_attempts
		  WHERE job_id=$2 AND status='running'
		  ORDER BY started_at DESC LIMIT 1
		)
	`, req.Metrics, req.JobID)

	// Запись стоимости в llm_costs
	s.RecordCost(ctx, req.JobID, req.Metrics)

	// Circuit breaker: reset для устройства при успехе
	if deviceID := extractDeviceID(req.Result); deviceID != "" {
		s.Router.RecordDeviceResult(deviceID, true)
	}

	log.Printf("job complete id=%s worker=%s", req.JobID, req.WorkerID)
	WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) HandleWorkerFail(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req models.FailJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.WorkerID) == "" || strings.TrimSpace(req.JobID) == "" {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id_job_id_required"})
		return
	}
	if len(req.Metrics) == 0 {
		req.Metrics = json.RawMessage("{}")
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var attempts int
	var maxAttempts int
	row := s.DB.QueryRow(ctx, `SELECT attempts, max_attempts FROM jobs WHERE id=$1`, req.JobID)
	if err := row.Scan(&attempts, &maxAttempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			WriteJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		log.Printf("worker fail read error: %v", err)
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_read_failed"})
		return
	}
	status := "queued"
	if attempts >= maxAttempts {
		status = "error"
	}
	_, err := s.DB.Exec(ctx, `
		UPDATE jobs SET status=$1, error=$2, lease_until=NULL, updated_at=now() WHERE id=$3
	`, status, req.Error, req.JobID)
	if err != nil {
		log.Printf("worker fail update error: %v", err)
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_update_failed"})
		return
	}
	_, _ = s.DB.Exec(ctx, `
		UPDATE job_attempts
		SET status='error', finished_at=now(), error=$1, metrics=$2::jsonb
		WHERE id = (
		  SELECT id FROM job_attempts
		  WHERE job_id=$3 AND status='running'
		  ORDER BY started_at DESC LIMIT 1
		)
	`, req.Error, req.Metrics, req.JobID)
	// Circuit breaker: инкремент ошибок для устройства
	if deviceID := extractDeviceIDFromJob(ctx, s, req.JobID); deviceID != "" {
		s.Router.RecordDeviceResult(deviceID, false)
	}

	log.Printf("job failed id=%s worker=%s status=%s error=%s", req.JobID, req.WorkerID, status, req.Error)
	WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) HandleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req models.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.WorkerID) == "" || strings.TrimSpace(req.JobID) == "" {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id_job_id_required"})
		return
	}
	if req.ExtendSeconds <= 0 {
		req.ExtendSeconds = 30
	}

	leaseUntil := time.Now().UTC().Add(time.Duration(req.ExtendSeconds) * time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	_, err := s.DB.Exec(ctx, `
		UPDATE jobs SET lease_until=$1, updated_at=now() WHERE id=$2 AND status='running'
	`, leaseUntil, req.JobID)
	if err != nil {
		log.Printf("worker heartbeat update error: %v", err)
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_update_failed"})
		return
	}
	log.Printf("job heartbeat id=%s worker=%s extend=%ds", req.JobID, req.WorkerID, req.ExtendSeconds)
	WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) HandleDeviceOffline(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req models.DeviceOfflineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.DeviceID) == "" {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "device_id_required"})
		return
	}
	meta := map[string]any{
		"last_error":    req.Reason,
		"last_error_at": time.Now().UTC().Format(time.RFC3339),
	}
	metaJSON, _ := json.Marshal(meta)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	_, err := s.DB.Exec(ctx, `
		UPDATE devices SET status='offline', tags = COALESCE(tags, '{}'::jsonb) || $1::jsonb, updated_at=now() WHERE id=$2
	`, metaJSON, req.DeviceID)
	if err != nil {
		log.Printf("device offline update error: %v", err)
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_update_failed"})
		return
	}
	log.Printf("device marked offline id=%s reason=%s", req.DeviceID, req.Reason)
	WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleJobStream(w http.ResponseWriter, r *http.Request, jobID string) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodGet {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if !uuidRe.MatchString(jobID) {
		WriteJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "stream_not_supported"})
		return
	}

	ctx := r.Context()

	// Получаем dedicated connection для LISTEN
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		log.Printf("sse: acquire conn error: %v", err)
		WriteSSE(w, "error", map[string]string{"error": "db_conn_failed"})
		flusher.Flush()
		return
	}
	defer conn.Release()

	// Подписываемся на канал job_update
	_, err = conn.Exec(ctx, "LISTEN job_update")
	if err != nil {
		log.Printf("sse: LISTEN error: %v", err)
		// Fallback на polling при ошибке LISTEN
		s.handleJobStreamPoll(w, r, jobID, flusher)
		return
	}

	// Начальное чтение статуса
	var lastStatus string
	jobRow, err := s.fetchJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			WriteSSE(w, "error", map[string]string{"error": "not_found"})
			flusher.Flush()
			return
		}
		WriteSSE(w, "error", map[string]string{"error": "db_read_failed"})
		flusher.Flush()
		return
	}
	WriteSSE(w, "status", jobRow)
	flusher.Flush()
	lastStatus = jobRow.Status
	if lastStatus == "done" || lastStatus == "error" {
		return
	}

	// Ждём NOTIFY или fallback-таймаут 15 сек (на случай пропущенного notify)
	for {
		waitCtx, waitCancel := context.WithTimeout(ctx, 15*time.Second)
		notification, err := conn.Conn().WaitForNotification(waitCtx)
		waitCancel()

		if ctx.Err() != nil {
			return // клиент отключился
		}

		if err != nil {
			// Таймаут — делаем fallback poll
			jobRow, err = s.fetchJob(ctx, jobID)
			if err != nil {
				continue
			}
		} else if notification.Payload != jobID {
			// Уведомление для другой job — пропускаем
			continue
		} else {
			// Наша job обновилась — читаем из БД
			jobRow, err = s.fetchJob(ctx, jobID)
			if err != nil {
				continue
			}
		}

		if jobRow.Status != lastStatus {
			WriteSSE(w, "status", jobRow)
			flusher.Flush()
			lastStatus = jobRow.Status
		}
		if jobRow.Status == "done" || jobRow.Status == "error" {
			return
		}
	}
}

// handleJobStreamPoll — fallback polling (если LISTEN недоступен)
func (s *Server) handleJobStreamPoll(w http.ResponseWriter, r *http.Request, jobID string, flusher http.Flusher) {
	ctx := r.Context()
	var lastStatus string
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
			jobRow, err := s.fetchJob(ctx, jobID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
					WriteSSE(w, "error", map[string]string{"error": "not_found"})
					flusher.Flush()
					return
				}
				continue
			}
			if lastStatus == "" || jobRow.Status != lastStatus {
				WriteSSE(w, "status", jobRow)
				flusher.Flush()
				lastStatus = jobRow.Status
			}
			if jobRow.Status == "done" || jobRow.Status == "error" {
				return
			}
		}
	}
}

func (s *Server) HandleDiscoveryRun(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.Discovery.Run(ctx); err != nil {
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "discovery_failed"})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) HandleDiscoveryLast(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodGet {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	last := s.Discovery.LastRun()
	resp := map[string]string{"last_run": ""}
	if !last.IsZero() {
		resp["last_run"] = last.UTC().Format(time.RFC3339)
	}
	WriteJSON(w, http.StatusOK, resp)
}

// qualityTimeouts — таймаут в секундах по quality level
var qualityTimeouts = map[string]int{
	"turbo": 15, "economy": 30, "standard": 60,
	"premium": 90, "ultra": 120, "max": 180,
}

func (s *Server) HandleLLMRequest(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req models.LLMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	provider, kind, payload, err := s.Router.RouteLLM(ctx, req)
	if err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.MaxAttempts == 0 {
		req.MaxAttempts = 3
	}

	var deadline *time.Time
	if strings.TrimSpace(req.DeadlineAt) != "" {
		t, err := time.Parse(time.RFC3339, req.DeadlineAt)
		if err != nil {
			WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_deadline_at"})
			return
		}
		deadline = &t
	}
	// Авто-deadline по quality
	if deadline == nil {
		quality := strings.ToLower(strings.TrimSpace(req.Quality))
		if secs, ok := qualityTimeouts[quality]; ok {
			t := time.Now().UTC().Add(time.Duration(secs) * time.Second)
			deadline = &t
		}
	}

	row := s.DB.QueryRow(ctx, `
		INSERT INTO jobs (kind, payload, priority, source, max_attempts, deadline_at)
		VALUES ($1, $2::jsonb, $3, $4, $5, $6)
		RETURNING id
	`, kind, payload, req.Priority, req.Source, req.MaxAttempts, deadline)
	var jobID string
	if err := row.Scan(&jobID); err != nil {
		log.Printf("llm request enqueue error: %v", err)
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_insert_failed"})
		return
	}
	log.Printf("llm route provider=%s kind=%s job=%s quality=%s", provider, kind, jobID, req.Quality)
	WriteJSON(w, http.StatusAccepted, models.LLMResponse{JobID: jobID, Provider: provider, Kind: kind})
}

func (s *Server) HandleBenchmarkRun(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req models.BenchmarkRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
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
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "provider_not_supported"})
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
			if ok, reason := limits.ModelAllowed(ctx, s.DB, req.DeviceID, req.Model); !ok {
				WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "model_not_allowed", "reason": reason})
				return
			}
		}
		addr, host, err := s.Router.FetchDeviceAddr(ctx, req.DeviceID)
		if err != nil {
			WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "device_not_found"})
			return
		}
		if addr == "" {
			WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "device_no_ollama_addr"})
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
		row := s.DB.QueryRow(ctx, `
			INSERT INTO jobs (kind, payload, priority, source, max_attempts)
			VALUES ($1, $2::jsonb, $3, $4, $5)
			RETURNING id
		`, kind, payloadJSON, req.Priority, "benchmark", 2)
		var jobID string
		if err := row.Scan(&jobID); err != nil {
			log.Printf("benchmark enqueue error: %v", err)
			WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_insert_failed"})
			return
		}
		jobIDs = append(jobIDs, jobID)
	}
	log.Printf("benchmark queued kind=%s runs=%d", kind, len(jobIDs))
	WriteJSON(w, http.StatusAccepted, map[string]any{"job_ids": jobIDs, "kind": kind})
}

func (s *Server) HandleBenchmarksList(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodGet {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
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
	rows, err := s.DB.Query(ctx, `
		SELECT device_id, model_id, task_type, tokens_in, tokens_out, latency_ms, tps, created_at
		FROM benchmarks ORDER BY created_at DESC LIMIT $1
	`, limit)
	if err != nil {
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_read_failed"})
		return
	}
	defer rows.Close()
	list := []models.BenchmarkRow{}
	for rows.Next() {
		var row models.BenchmarkRow
		if err := rows.Scan(&row.DeviceID, &row.ModelID, &row.TaskType, &row.TokensIn, &row.TokensOut, &row.LatencyMs, &row.TPS, &row.CreatedAt); err != nil {
			continue
		}
		list = append(list, row)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) fetchJob(ctx context.Context, id string) (*models.Job, error) {
	row := s.DB.QueryRow(ctx, `
		SELECT id, kind, payload, status, attempts, max_attempts,
		       lease_until, deadline_at, result, error, priority, queued_at, updated_at
		FROM jobs WHERE id = $1
	`, id)
	var j models.Job
	var errText sql.NullString
	var result json.RawMessage
	if err := row.Scan(
		&j.ID, &j.Kind, &j.Payload, &j.Status, &j.Attempts, &j.MaxAttempts,
		&j.LeaseUntil, &j.DeadlineAt, &result, &errText, &j.Priority, &j.QueuedAt, &j.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if errText.Valid {
		j.Error = errText.String
	}
	j.Result = result
	return &j, nil
}

// RecordCost записывает стоимость job в llm_costs на основе metrics от worker
func (s *Server) RecordCost(ctx context.Context, jobID string, metricsRaw json.RawMessage) {
	if len(metricsRaw) == 0 {
		return
	}
	var m map[string]any
	if err := json.Unmarshal(metricsRaw, &m); err != nil {
		return
	}
	tokensIn, _ := ToInt(m["tokens_in"])
	tokensOut, _ := ToInt(m["tokens_out"])
	if tokensIn == 0 && tokensOut == 0 {
		return
	}
	provider, _ := m["provider"].(string)
	model, _ := m["model"].(string)
	if model == "" || provider == "" {
		return
	}
	var cost float64
	row := s.DB.QueryRow(ctx, `SELECT calculate_job_cost($1, $2, $3)`, model, tokensIn, tokensOut)
	if err := row.Scan(&cost); err != nil {
		log.Printf("cost calc error job=%s: %v", jobID, err)
		cost = 0
	}
	_, err := s.DB.Exec(ctx, `
		INSERT INTO llm_costs (id, job_id, model_id, provider, tokens_in, tokens_out, cost_usd)
		VALUES (gen_random_uuid(), $1::uuid, $2, $3, $4, $5, $6)
	`, jobID, model, provider, tokensIn, tokensOut, cost)
	if err != nil {
		log.Printf("cost insert error job=%s: %v", jobID, err)
	} else {
		log.Printf("cost recorded job=%s model=%s provider=%s tokens=%d/%d cost=%.6f", jobID, model, provider, tokensIn, tokensOut, cost)
	}
}

// HandleCostsSummary возвращает агрегацию расходов за период
func (s *Server) HandleCostsSummary(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodGet {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "day"
	}

	var interval string
	switch period {
	case "day":
		interval = "1 day"
	case "week":
		interval = "7 days"
	case "month":
		interval = "30 days"
	default:
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_period"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Проверяем существование таблицы llm_costs (может быть ещё не создана)
	var tableExists bool
	_ = s.DB.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='llm_costs')
	`).Scan(&tableExists)

	var totalCost float64
	var totalJobs int
	byProvider := []models.ProviderCost{}

	if tableExists {
		err := s.DB.QueryRow(ctx, `
			SELECT COALESCE(SUM(cost_usd), 0), COUNT(*) FROM llm_costs WHERE created_at >= now() - $1::interval
		`, interval).Scan(&totalCost, &totalJobs)
		if err != nil {
			WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_read_failed"})
			return
		}

		rows, err := s.DB.Query(ctx, `
			SELECT provider, COALESCE(SUM(cost_usd), 0) AS cost, COUNT(*) AS jobs,
			       COALESCE(SUM(tokens_in), 0) AS tokens_in, COALESCE(SUM(tokens_out), 0) AS tokens_out
			FROM llm_costs WHERE created_at >= now() - $1::interval
			GROUP BY provider ORDER BY cost DESC
		`, interval)
		if err != nil {
			WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_read_failed"})
			return
		}
		defer rows.Close()

		for rows.Next() {
			var pc models.ProviderCost
			if err := rows.Scan(&pc.Provider, &pc.Cost, &pc.Jobs, &pc.TokensIn, &pc.TokensOut); err != nil {
				continue
			}
			byProvider = append(byProvider, pc)
		}
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"period":      period,
		"total_cost":  totalCost,
		"total_jobs":  totalJobs,
		"by_provider": byProvider,
	})
}

// HandleDashboard возвращает полный snapshot для web UI
func (s *Server) HandleDashboard(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodGet {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	jobStats := map[string]int{}
	rows, err := s.DB.Query(ctx, `SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var status string
			var count int
			if rows.Scan(&status, &count) == nil {
				jobStats[status] = count
			}
		}
	}

	benchStats := map[string]int{}
	rows2, err := s.DB.Query(ctx, `SELECT status, COUNT(*) FROM jobs WHERE kind LIKE 'benchmark.%' GROUP BY status`)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var status string
			var count int
			if rows2.Scan(&status, &count) == nil {
				benchStats[status] = count
			}
		}
	}

	runningJobs := []models.RunningJob{}
	rows3, err := s.DB.Query(ctx, `
		SELECT id, kind, payload, updated_at FROM jobs WHERE status='running' ORDER BY updated_at DESC LIMIT 10
	`)
	if err == nil {
		defer rows3.Close()
		for rows3.Next() {
			var rj models.RunningJob
			var payload json.RawMessage
			if rows3.Scan(&rj.ID, &rj.Kind, &payload, &rj.UpdatedAt) == nil {
				var p map[string]any
				if json.Unmarshal(payload, &p) == nil {
					rj.Model, _ = p["model"].(string)
					rj.Provider, _ = p["provider"].(string)
					rj.DeviceID, _ = p["device_id"].(string)
				}
				runningJobs = append(runningJobs, rj)
			}
		}
	}

	devices := []models.DeviceInfo{}
	rows4, err := s.DB.Query(ctx, `
		SELECT d.id, d.name, d.status, d.platform, d.arch, d.host,
		       (SELECT COUNT(*) FROM device_models dm WHERE dm.device_id = d.id AND dm.available = TRUE) AS models,
		       (SELECT COALESCE(json_agg(dm.model_id), '[]'::json) FROM device_models dm WHERE dm.device_id = d.id AND dm.available = TRUE) AS model_names,
		       (SELECT COUNT(*) FROM jobs j WHERE j.status='running' AND j.payload->>'device_id' = d.id) AS load,
		       (d.tags->>'ollama_latency_ms')::int AS latency,
		       d.last_seen,
		       d.tags
		FROM devices d
		WHERE COALESCE(d.tags->>'ollama', 'false') = 'true'
		  AND d.id NOT LIKE 'worker-%'
		ORDER BY d.status DESC, d.name ASC LIMIT 50
	`)
	if err == nil {
		defer rows4.Close()
		for rows4.Next() {
			var di models.DeviceInfo
			if rows4.Scan(&di.ID, &di.Name, &di.Status, &di.Platform, &di.Arch, &di.Host, &di.Models, &di.ModelNames, &di.Load, &di.Latency, &di.LastSeen, &di.Tags) == nil {
				devices = append(devices, di)
			}
		}
	}

	// Подсчёт online workers
	var workersOnline int
	_ = s.DB.QueryRow(ctx, `
		SELECT COUNT(*) FROM devices
		WHERE id LIKE 'worker-%' AND status = 'online'
		  AND last_seen > now() - interval '10 minutes'
	`).Scan(&workersOnline)

	var costDay, costWeek, costMonth float64
	_ = s.DB.QueryRow(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM llm_costs WHERE created_at >= now() - interval '1 day'`).Scan(&costDay)
	_ = s.DB.QueryRow(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM llm_costs WHERE created_at >= now() - interval '7 days'`).Scan(&costWeek)
	_ = s.DB.QueryRow(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM llm_costs WHERE created_at >= now() - interval '30 days'`).Scan(&costMonth)

	var modelsCount int
	_ = s.DB.QueryRow(ctx, `SELECT COUNT(DISTINCT model_id) FROM device_models WHERE available = TRUE`).Scan(&modelsCount)

	// Загружаем device stats из v_device_stats view
	deviceStatsMap := map[string]*models.DeviceStats{}
	rows5, err := s.DB.Query(ctx, `
		SELECT device_id, total_jobs_7d, done_jobs_7d, success_rate, avg_latency_ms
		FROM v_device_stats
	`)
	if err == nil {
		defer rows5.Close()
		for rows5.Next() {
			var deviceID string
			var ds models.DeviceStats
			if rows5.Scan(&deviceID, &ds.TotalJobs7d, &ds.DoneJobs7d, &ds.SuccessRate, &ds.AvgLatencyMs) == nil {
				deviceStatsMap[deviceID] = &ds
			}
		}
	}

	// Добавляем stats и circuit status к каждому устройству
	for i := range devices {
		if st, ok := deviceStatsMap[devices[i].ID]; ok {
			devices[i].Stats = st
		}
		devices[i].Circuit = s.Router.GetCircuitStatus(devices[i].ID)
	}

	// Строим иерархию Host→Node
	maxConc := config.GetenvInt("DEVICE_MAX_CONCURRENCY", 1)
	hosts := buildHostHierarchy(devices, deviceStatsMap, s.Router, maxConc)

	// Генерируем issues для dashboard
	issues := buildDashboardIssues(hosts, jobStats, workersOnline)

	WriteJSON(w, http.StatusOK, map[string]any{
		"jobs":           jobStats,
		"benchmarks":     benchStats,
		"running_jobs":   runningJobs,
		"devices":        devices,
		"hosts":          hosts,
		"workers_online": workersOnline,
		"issues":         issues,
		"costs": map[string]float64{
			"today": costDay,
			"week":  costWeek,
			"month": costMonth,
		},
		"models_count": modelsCount,
		"updated_at":   time.Now().UTC().Format(time.RFC3339),
	})
}

// buildHostHierarchy группирует плоский список devices в иерархию Host→Node
func buildHostHierarchy(devices []models.DeviceInfo, statsMap map[string]*models.DeviceStats, router *routing.Router, maxConc int) []models.HostInfo {
	type parsedTags struct {
		PortDevice bool   `json:"port_device"`
		BaseDevice string `json:"base_device"`
		OllamaPort int    `json:"ollama_port"`
	}

	// Парсим tags и разделяем на base hosts и port devices
	hostMap := map[string]*models.HostInfo{}
	portDevices := []struct {
		device models.DeviceInfo
		tags   parsedTags
	}{}

	for _, d := range devices {
		var t parsedTags
		if len(d.Tags) > 0 {
			_ = json.Unmarshal(d.Tags, &t)
		}

		if t.PortDevice && t.BaseDevice != "" {
			portDevices = append(portDevices, struct {
				device models.DeviceInfo
				tags   parsedTags
			}{d, t})
			continue
		}

		// Это base host
		hostMap[d.ID] = &models.HostInfo{
			ID:       d.ID,
			Name:     d.Name,
			Platform: d.Platform,
			Status:   d.Status,
			LastSeen: d.LastSeen,
			Circuit:  "ok",
		}
	}

	// Привязываем port devices к base hosts
	for _, pd := range portDevices {
		host, ok := hostMap[pd.tags.BaseDevice]
		if !ok {
			// Base host не найден — создаём из port device
			host = &models.HostInfo{
				ID:            pd.tags.BaseDevice,
				Name:          pd.tags.BaseDevice,
				Platform:      pd.device.Platform,
				Status:        pd.device.Status,
				LastSeen:      pd.device.LastSeen,
				Orchestration: "docker",
				Circuit:       "ok",
			}
			hostMap[pd.tags.BaseDevice] = host
		}
		host.Orchestration = "docker"

		port := pd.tags.OllamaPort
		if port == 0 {
			port = 11434
		}
		node := models.NodeInfo{
			Port:       port,
			DeviceID:   pd.device.ID,
			Role:       inferNodeRole(pd.device.ModelNames),
			Models:     pd.device.Models,
			ModelNames: pd.device.ModelNames,
			Latency:    pd.device.Latency,
			Running:    pd.device.Load,
			Stats:      pd.device.Stats,
			Circuit:    pd.device.Circuit,
		}
		host.Nodes = append(host.Nodes, node)
		host.TotalModels += pd.device.Models
		host.TotalRunning += pd.device.Load
	}

	// Добавляем base host как ноду :11434 (он сам содержит модели на дефолтном порту)
	for _, host := range hostMap {
		for _, d := range devices {
			if d.ID == host.ID && d.Models > 0 {
				node := models.NodeInfo{
					Port:       11434,
					DeviceID:   d.ID,
					Role:       inferNodeRole(d.ModelNames),
					Models:     d.Models,
					ModelNames: d.ModelNames,
					Latency:    d.Latency,
					Running:    d.Load,
					Stats:      d.Stats,
					Circuit:    d.Circuit,
				}
				host.Nodes = append(host.Nodes, node)
				host.TotalModels += d.Models
				host.TotalRunning += d.Load
				break
			}
		}
		// Если нет port devices — native, иначе уже docker
		if host.Orchestration == "" {
			host.Orchestration = "native"
		}
	}

	// Агрегация circuit и slots
	for _, host := range hostMap {
		host.TotalSlots = len(host.Nodes) * maxConc

		worst := "ok"
		for _, n := range host.Nodes {
			if n.Circuit == "degraded" || (n.Circuit == "probe" && worst == "ok") {
				worst = n.Circuit
			}
		}
		host.Circuit = worst

		// Агрегация stats
		var totalJobs, doneJobs, latencySum, latencyCount int
		for _, n := range host.Nodes {
			if n.Stats != nil {
				totalJobs += n.Stats.TotalJobs7d
				doneJobs += n.Stats.DoneJobs7d
				if n.Stats.AvgLatencyMs > 0 {
					latencySum += n.Stats.AvgLatencyMs * n.Stats.TotalJobs7d
					latencyCount += n.Stats.TotalJobs7d
				}
			}
		}
		if totalJobs > 0 {
			sr := float64(0)
			if totalJobs > 0 {
				sr = float64(doneJobs) * 100.0 / float64(totalJobs)
			}
			avgLat := 0
			if latencyCount > 0 {
				avgLat = latencySum / latencyCount
			}
			host.Stats = &models.DeviceStats{
				TotalJobs7d:  totalJobs,
				DoneJobs7d:   doneJobs,
				SuccessRate:  sr,
				AvgLatencyMs: avgLat,
			}
		}
	}

	// Сортировка: online first, потом по имени
	result := make([]models.HostInfo, 0, len(hostMap))
	for _, h := range hostMap {
		result = append(result, *h)
	}
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			// online < offline, потом по имени
			si := 0
			if result[i].Status == "online" {
				si = 1
			}
			sj := 0
			if result[j].Status == "online" {
				sj = 1
			}
			if sj > si || (si == sj && result[j].Name < result[i].Name) {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	return result
}

// inferNodeRole определяет роль ноды (chat/embed/mixed) по именам моделей
func inferNodeRole(modelNames json.RawMessage) string {
	var names []string
	if len(modelNames) > 0 {
		_ = json.Unmarshal(modelNames, &names)
	}
	if len(names) == 0 {
		return "chat"
	}
	hasEmbed := false
	hasChat := false
	for _, n := range names {
		nl := strings.ToLower(n)
		if strings.Contains(nl, "embed") {
			hasEmbed = true
		} else {
			hasChat = true
		}
	}
	if hasEmbed && hasChat {
		return "mixed"
	}
	if hasEmbed {
		return "embed"
	}
	return "chat"
}

// buildDashboardIssues генерирует список проблем на человеческом языке
func buildDashboardIssues(hosts []models.HostInfo, jobStats map[string]int, workersOnline int) []string {
	var issues []string

	// Offline hosts
	for _, h := range hosts {
		if h.Status != "online" && h.LastSeen != nil {
			ago := time.Since(*h.LastSeen)
			var agoStr string
			if ago < time.Hour {
				agoStr = fmt.Sprintf("%dm ago", int(ago.Minutes()))
			} else {
				agoStr = fmt.Sprintf("%dh ago", int(ago.Hours()))
			}
			issues = append(issues, fmt.Sprintf("Host '%s' offline (last seen %s)", h.Name, agoStr))
		}
	}

	// Circuit breakers
	for _, h := range hosts {
		if h.Circuit == "degraded" {
			issues = append(issues, fmt.Sprintf("Host '%s' circuit degraded", h.Name))
		}
	}

	// Low success rate
	for _, h := range hosts {
		if h.Stats != nil && h.Stats.TotalJobs7d >= 5 && h.Stats.SuccessRate < 80 {
			issues = append(issues, fmt.Sprintf("Host '%s' low success rate: %.1f%%", h.Name, h.Stats.SuccessRate))
		}
	}

	// Stuck queue
	queued := jobStats["queued"]
	running := jobStats["running"]
	if queued > 0 && running == 0 && workersOnline > 0 {
		issues = append(issues, fmt.Sprintf("Queue stuck: %d jobs queued but no workers processing", queued))
	}

	// Large backlog
	if queued > 10 {
		issues = append(issues, fmt.Sprintf("Queue backlog: %d jobs waiting", queued))
	}

	return issues
}

// extractDeviceID извлекает device_id из result JSON
func extractDeviceID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	id, _ := m["device_id"].(string)
	return strings.TrimSpace(id)
}

// extractDeviceIDFromJob извлекает device_id из payload задачи
func extractDeviceIDFromJob(ctx context.Context, s *Server, jobID string) string {
	var payload json.RawMessage
	err := s.DB.QueryRow(ctx, `SELECT payload FROM jobs WHERE id = $1`, jobID).Scan(&payload)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	id, _ := m["device_id"].(string)
	return strings.TrimSpace(id)
}

// ═══ Debug Endpoints ═══

// HandleDebugHealth — глубокая диагностика всех компонентов
func (s *Server) HandleDebugHealth(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodGet {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	overallStatus := "ok"
	setWorst := func(s string) {
		if s == "error" || (s == "warning" && overallStatus != "error") {
			overallStatus = s
		}
	}

	// 1. Database check
	dbCheck := map[string]any{"status": "ok"}
	dbStart := time.Now()
	var one int
	if err := s.DB.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		dbCheck["status"] = "error"
		dbCheck["error"] = err.Error()
		setWorst("error")
	} else {
		dbCheck["latency_ms"] = time.Since(dbStart).Milliseconds()
		stat := s.DB.Stat()
		dbCheck["connections"] = map[string]any{
			"active": stat.AcquiredConns(),
			"max":    stat.MaxConns(),
			"idle":   stat.IdleConns(),
		}
	}

	// 2. Queue check
	queueCheck := map[string]any{"status": "ok"}
	var queued, queueRunning, stuck int
	_ = s.DB.QueryRow(ctx, `SELECT COUNT(*) FROM jobs WHERE status = 'queued'`).Scan(&queued)
	_ = s.DB.QueryRow(ctx, `SELECT COUNT(*) FROM jobs WHERE status = 'running'`).Scan(&queueRunning)
	_ = s.DB.QueryRow(ctx, `SELECT COUNT(*) FROM jobs WHERE status = 'running' AND lease_until < now()`).Scan(&stuck)
	queueCheck["queued"] = queued
	queueCheck["running"] = queueRunning
	queueCheck["stuck"] = stuck
	var oldestSec *float64
	var oldest sql.NullFloat64
	_ = s.DB.QueryRow(ctx, `SELECT EXTRACT(epoch FROM (now() - MIN(queued_at))) FROM jobs WHERE status = 'queued'`).Scan(&oldest)
	if oldest.Valid {
		v := oldest.Float64
		oldestSec = &v
		queueCheck["oldest_queued_sec"] = int(v)
	}
	if stuck > 0 {
		queueCheck["status"] = "warning"
		setWorst("warning")
	}

	// 3. Hosts check
	hostsCheck := map[string]any{"status": "ok"}
	var totalHosts, onlineHosts, offlineHosts int
	_ = s.DB.QueryRow(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE status = 'online'),
		       COUNT(*) FILTER (WHERE status != 'online')
		FROM devices
		WHERE COALESCE(tags->>'ollama', 'false') = 'true'
		  AND id NOT LIKE 'worker-%'
		  AND COALESCE(tags->>'port_device', 'false') != 'true'
	`).Scan(&totalHosts, &onlineHosts, &offlineHosts)
	hostsCheck["total"] = totalHosts
	hostsCheck["online"] = onlineHosts
	hostsCheck["offline"] = offlineHosts
	if offlineHosts > 0 {
		hostsCheck["status"] = "warning"
		setWorst("warning")
	}
	if onlineHosts == 0 && totalHosts > 0 {
		hostsCheck["status"] = "error"
		setWorst("error")
	}

	// 4. Workers check
	workersCheck := map[string]any{"status": "ok"}
	var workersOnline int
	_ = s.DB.QueryRow(ctx, `
		SELECT COUNT(*) FROM devices
		WHERE id LIKE 'worker-%' AND status = 'online'
		  AND last_seen > now() - interval '10 minutes'
	`).Scan(&workersOnline)
	maxConc := config.GetenvInt("DEVICE_MAX_CONCURRENCY", 1)
	workersCheck["online"] = workersOnline
	workersCheck["capacity"] = workersOnline * maxConc
	if workersOnline == 0 {
		workersCheck["status"] = "warning"
		setWorst("warning")
	}

	// Issues
	var issues []string
	// Offline hosts
	rows, err := s.DB.Query(ctx, `
		SELECT name, last_seen FROM devices
		WHERE COALESCE(tags->>'ollama', 'false') = 'true'
		  AND id NOT LIKE 'worker-%'
		  AND COALESCE(tags->>'port_device', 'false') != 'true'
		  AND status != 'online'
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			var ls *time.Time
			if rows.Scan(&name, &ls) == nil {
				ago := "unknown"
				if ls != nil {
					d := time.Since(*ls)
					if d < time.Hour {
						ago = fmt.Sprintf("%dm ago", int(d.Minutes()))
					} else {
						ago = fmt.Sprintf("%dh ago", int(d.Hours()))
					}
				}
				issues = append(issues, fmt.Sprintf("Host '%s' offline (last seen %s)", name, ago))
			}
		}
	}
	if stuck > 0 {
		issues = append(issues, fmt.Sprintf("%d jobs with expired lease", stuck))
	}
	if queued > 0 && queueRunning == 0 && workersOnline > 0 {
		issues = append(issues, fmt.Sprintf("Queue stuck: %d jobs queued but no workers processing", queued))
	}
	if queued > 10 {
		issues = append(issues, fmt.Sprintf("Queue backlog: %d jobs waiting", queued))
	}
	_ = oldestSec // используется в queueCheck

	WriteJSON(w, http.StatusOK, map[string]any{
		"status":  overallStatus,
		"version": s.Version,
		"checks": map[string]any{
			"database": dbCheck,
			"queue":    queueCheck,
			"hosts":    hostsCheck,
			"workers":  workersCheck,
		},
		"issues": issues,
	})
}

// HandleDebugActions — каталог всех API endpoints
func (s *Server) HandleDebugActions(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodGet {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}

	type endpoint struct {
		Method      string `json:"method"`
		Path        string `json:"path"`
		Description string `json:"description"`
		Example     string `json:"example"`
	}

	endpoints := []endpoint{
		{"GET", "/health", "Базовая проверка здоровья сервиса", "curl http://localhost:8080/health"},
		{"GET", "/version", "Версия сервиса", "curl http://localhost:8080/version"},
		{"GET", "/v1/dashboard", "Полный snapshot: jobs, hosts, costs, running jobs, issues", "curl http://localhost:8080/v1/dashboard | jq ."},
		{"POST", "/v1/jobs", "Создать задачу в очереди", "curl -X POST http://localhost:8080/v1/jobs -d '{\"kind\":\"ollama.generate\",\"payload\":{\"model\":\"qwen3:1.7b\",\"prompt\":\"Hello\"}}'"},
		{"GET", "/v1/jobs/{id}", "Статус задачи (payload, result, attempts)", "curl http://localhost:8080/v1/jobs/abc123"},
		{"GET", "/v1/jobs/{id}/stream", "SSE-поток обновлений задачи в реальном времени", "curl -N http://localhost:8080/v1/jobs/abc123/stream"},
		{"POST", "/v1/llm/request", "Smart routing: отправить LLM-запрос с автовыбором модели по quality", "curl -X POST http://localhost:8080/v1/llm/request -d '{\"task\":\"chat\",\"quality\":\"standard\",\"prompt\":\"Hello\"}'"},
		{"POST", "/v1/workers/register", "Регистрация worker-а (обычно автоматически)", "curl -X POST http://localhost:8080/v1/workers/register -d '{\"worker\":{\"id\":\"w1\",\"name\":\"test\"}}'"},
		{"POST", "/v1/workers/claim", "Worker забирает задачу из очереди", "curl -X POST http://localhost:8080/v1/workers/claim -d '{\"worker_id\":\"w1\",\"kinds\":[\"ollama.generate\"]}'"},
		{"POST", "/v1/workers/complete", "Worker завершает задачу с результатом", "curl -X POST http://localhost:8080/v1/workers/complete -d '{\"worker_id\":\"w1\",\"job_id\":\"abc123\",\"result\":{}}'"},
		{"POST", "/v1/workers/fail", "Worker помечает задачу как ошибку", "curl -X POST http://localhost:8080/v1/workers/fail -d '{\"worker_id\":\"w1\",\"job_id\":\"abc123\",\"error\":\"timeout\"}'"},
		{"POST", "/v1/workers/heartbeat", "Worker продлевает lease задачи", "curl -X POST http://localhost:8080/v1/workers/heartbeat -d '{\"worker_id\":\"w1\",\"job_id\":\"abc123\",\"extend_seconds\":30}'"},
		{"POST", "/v1/devices/offline", "Пометить устройство как offline", "curl -X POST http://localhost:8080/v1/devices/offline -d '{\"device_id\":\"my-host\",\"reason\":\"unreachable\"}'"},
		{"POST", "/v1/discovery/run", "Принудительный запуск discovery всех Ollama-устройств", "curl -X POST http://localhost:8080/v1/discovery/run"},
		{"GET", "/v1/discovery/last", "Время последнего запуска discovery", "curl http://localhost:8080/v1/discovery/last"},
		{"POST", "/v1/benchmarks/run", "Запустить синтетический бенчмарк на модели", "curl -X POST http://localhost:8080/v1/benchmarks/run -d '{\"model\":\"qwen3:1.7b\",\"task_type\":\"generate\",\"runs\":3}'"},
		{"GET", "/v1/benchmarks", "Последние бенчмарки (по умолчанию limit=20)", "curl 'http://localhost:8080/v1/benchmarks?limit=10'"},
		{"GET", "/v1/costs/summary", "Сводка расходов за период по провайдерам", "curl 'http://localhost:8080/v1/costs/summary?period=week'"},
		{"GET", "/v1/debug/health", "Глубокая диагностика: БД, очередь, устройства, workers, issues", "curl http://localhost:8080/v1/debug/health | jq .issues"},
		{"GET", "/v1/debug/actions", "Этот эндпоинт: каталог всех API actions", "curl http://localhost:8080/v1/debug/actions | jq '.endpoints[].path'"},
		{"GET", "/v1/debug/capacity", "Мощности кластера: слоты, загрузка, утилизация по хостам", "curl http://localhost:8080/v1/debug/capacity | jq ."},
		{"POST", "/v1/debug/test", "Smoke test: проверяет БД, Ollama, job pipeline", "curl -X POST http://localhost:8080/v1/debug/test | jq .results"},
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"endpoints": endpoints,
		"total":     len(endpoints),
	})
}

// HandleDebugCapacity — мощности кластера: слоты, загрузка, утилизация
func (s *Server) HandleDebugCapacity(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodGet {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	maxConc := config.GetenvInt("DEVICE_MAX_CONCURRENCY", 1)

	// Загружаем все Ollama devices (не workers)
	devices := []models.DeviceInfo{}
	rows, err := s.DB.Query(ctx, `
		SELECT d.id, d.name, d.status, d.platform, d.arch, d.host,
		       (SELECT COUNT(*) FROM device_models dm WHERE dm.device_id = d.id AND dm.available = TRUE) AS models,
		       (SELECT COALESCE(json_agg(dm.model_id), '[]'::json) FROM device_models dm WHERE dm.device_id = d.id AND dm.available = TRUE) AS model_names,
		       (SELECT COUNT(*) FROM jobs j WHERE j.status='running' AND j.payload->>'device_id' = d.id) AS load,
		       (d.tags->>'ollama_latency_ms')::int AS latency,
		       d.last_seen,
		       d.tags
		FROM devices d
		WHERE COALESCE(d.tags->>'ollama', 'false') = 'true'
		  AND d.id NOT LIKE 'worker-%'
		ORDER BY d.status DESC, d.name ASC LIMIT 50
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var di models.DeviceInfo
			if rows.Scan(&di.ID, &di.Name, &di.Status, &di.Platform, &di.Arch, &di.Host, &di.Models, &di.ModelNames, &di.Load, &di.Latency, &di.LastSeen, &di.Tags) == nil {
				devices = append(devices, di)
			}
		}
	}

	// Загружаем stats
	deviceStatsMap := map[string]*models.DeviceStats{}
	rows2, err := s.DB.Query(ctx, `SELECT device_id, total_jobs_7d, done_jobs_7d, success_rate, avg_latency_ms FROM v_device_stats`)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var deviceID string
			var ds models.DeviceStats
			if rows2.Scan(&deviceID, &ds.TotalJobs7d, &ds.DoneJobs7d, &ds.SuccessRate, &ds.AvgLatencyMs) == nil {
				deviceStatsMap[deviceID] = &ds
			}
		}
	}
	for i := range devices {
		if st, ok := deviceStatsMap[devices[i].ID]; ok {
			devices[i].Stats = st
		}
		devices[i].Circuit = s.Router.GetCircuitStatus(devices[i].ID)
	}

	hosts := buildHostHierarchy(devices, deviceStatsMap, s.Router, maxConc)

	// Подсчёт workers
	var workersOnline int
	_ = s.DB.QueryRow(ctx, `
		SELECT COUNT(*) FROM devices
		WHERE id LIKE 'worker-%' AND status = 'online'
		  AND last_seen > now() - interval '10 minutes'
	`).Scan(&workersOnline)

	// Агрегация
	var totalSlots, usedSlots int
	type hostCap struct {
		Name           string  `json:"name"`
		Status         string  `json:"status"`
		Slots          int     `json:"slots"`
		Used           int     `json:"used"`
		Free           int     `json:"free"`
		UtilizationPct float64 `json:"utilization_pct"`
		Models         int     `json:"models"`
		Circuit        string  `json:"circuit"`
	}
	hostCaps := []hostCap{}
	for _, h := range hosts {
		slots := h.TotalSlots
		used := h.TotalRunning
		free := slots - used
		if free < 0 {
			free = 0
		}
		pct := float64(0)
		if slots > 0 {
			pct = float64(used) * 100.0 / float64(slots)
		}
		totalSlots += slots
		usedSlots += used
		hostCaps = append(hostCaps, hostCap{
			Name:           h.Name,
			Status:         h.Status,
			Slots:          slots,
			Used:           used,
			Free:           free,
			UtilizationPct: pct,
			Models:         h.TotalModels,
			Circuit:        h.Circuit,
		})
	}

	freeSlots := totalSlots - usedSlots
	if freeSlots < 0 {
		freeSlots = 0
	}
	pct := float64(0)
	if totalSlots > 0 {
		pct = float64(usedSlots) * 100.0 / float64(totalSlots)
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"total_slots":     totalSlots,
		"used_slots":      usedSlots,
		"free_slots":      freeSlots,
		"utilization_pct": pct,
		"hosts":           hostCaps,
		"workers": map[string]any{
			"online":         workersOnline,
			"total_capacity": workersOnline * maxConc,
		},
	})
}

// HandleDebugTest — smoke test: проверяет БД, Ollama, job pipeline
func (s *Server) HandleDebugTest(w http.ResponseWriter, r *http.Request) {
	log.Printf("http %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	start := time.Now()
	overallStatus := "pass"
	type testResult struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Ms     int64  `json:"ms"`
		Detail string `json:"detail"`
	}
	results := []testResult{}
	var issues []string

	// 1. db_ping
	t0 := time.Now()
	var one int
	if err := s.DB.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		results = append(results, testResult{"db_ping", "fail", time.Since(t0).Milliseconds(), "Error: " + err.Error()})
		overallStatus = "fail"
		issues = append(issues, "Database unreachable")
	} else {
		results = append(results, testResult{"db_ping", "pass", time.Since(t0).Milliseconds(), "PostgreSQL connected"})
	}

	// 2. db_read
	t0 = time.Now()
	var devCount, modelCount int
	_ = s.DB.QueryRow(ctx, `SELECT COUNT(*) FROM devices WHERE COALESCE(tags->>'ollama','false') = 'true'`).Scan(&devCount)
	_ = s.DB.QueryRow(ctx, `SELECT COUNT(*) FROM device_models WHERE available = TRUE`).Scan(&modelCount)
	results = append(results, testResult{"db_read", "pass", time.Since(t0).Milliseconds(), fmt.Sprintf("Read %d devices, %d models", devCount, modelCount)})

	// 3. ollama_ping — ping каждого online base host
	t0 = time.Now()
	type ollamaHost struct {
		ID   string
		Name string
		Addr string
	}
	var ollamaHosts []ollamaHost
	rows, err := s.DB.Query(ctx, `
		SELECT id, name, tags->>'ollama_addr'
		FROM devices
		WHERE COALESCE(tags->>'ollama', 'false') = 'true'
		  AND id NOT LIKE 'worker-%'
		  AND COALESCE(tags->>'port_device', 'false') != 'true'
		  AND status = 'online'
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var h ollamaHost
			if rows.Scan(&h.ID, &h.Name, &h.Addr) == nil && h.Addr != "" {
				ollamaHosts = append(ollamaHosts, h)
			}
		}
	}
	reachable := 0
	for _, h := range ollamaHosts {
		// Quick HTTP check
		addr := h.Addr
		if !strings.Contains(addr, "://") {
			addr = "http://" + addr
		}
		client := http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(addr + "/api/tags")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				reachable++
			} else {
				issues = append(issues, fmt.Sprintf("Host '%s' returned status %d", h.Name, resp.StatusCode))
			}
		} else {
			issues = append(issues, fmt.Sprintf("Host '%s' unreachable (timeout 2s)", h.Name))
		}
	}
	ollamaDetail := fmt.Sprintf("%d/%d hosts reachable", reachable, len(ollamaHosts))
	ollamaStatus := "pass"
	if reachable < len(ollamaHosts) {
		ollamaStatus = "warn"
		if overallStatus == "pass" {
			overallStatus = "warn"
		}
	}
	results = append(results, testResult{"ollama_ping", ollamaStatus, time.Since(t0).Milliseconds(), ollamaDetail})

	// 4. job_create — создаём тестовый benchmark job и отменяем
	t0 = time.Now()
	var testJobID string
	err = s.DB.QueryRow(ctx, `
		INSERT INTO jobs (kind, payload, priority, source, max_attempts)
		VALUES ('debug.test', '{"smoke_test": true}'::jsonb, -1, 'debug', 1)
		RETURNING id
	`).Scan(&testJobID)
	if err != nil {
		results = append(results, testResult{"job_create", "fail", time.Since(t0).Milliseconds(), "Error: " + err.Error()})
		overallStatus = "fail"
		issues = append(issues, "Cannot create jobs in database")
	} else {
		// Сразу помечаем как done чтобы не засорять очередь
		_, _ = s.DB.Exec(ctx, `UPDATE jobs SET status = 'done', result = '{"test": true}' WHERE id = $1`, testJobID)
		results = append(results, testResult{"job_create", "pass", time.Since(t0).Milliseconds(), fmt.Sprintf("Job %s created and cleaned", testJobID[:8])})
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"status":      overallStatus,
		"duration_ms": time.Since(start).Milliseconds(),
		"results":     results,
		"issues":      issues,
	})
}
