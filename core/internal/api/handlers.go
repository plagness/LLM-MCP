package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"llm-mcp/core/internal/config"
	"llm-mcp/core/internal/limits"
	"llm-mcp/core/internal/models"
	"llm-mcp/core/internal/routing"
)

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
	log.Printf("llm route provider=%s kind=%s job=%s", provider, kind, jobID)
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

	var totalCost float64
	var totalJobs int
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

	byProvider := []models.ProviderCost{}
	for rows.Next() {
		var pc models.ProviderCost
		if err := rows.Scan(&pc.Provider, &pc.Cost, &pc.Jobs, &pc.TokensIn, &pc.TokensOut); err != nil {
			continue
		}
		byProvider = append(byProvider, pc)
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
		SELECT d.id, d.name, d.status,
		       (SELECT COUNT(*) FROM device_models dm WHERE dm.device_id = d.id AND dm.available = TRUE) AS models,
		       (SELECT COUNT(*) FROM jobs j WHERE j.status='running' AND j.payload->>'device_id' = d.id) AS load,
		       (d.tags->>'ollama_latency_ms')::int AS latency
		FROM devices d
		WHERE COALESCE(d.tags->>'ollama', 'false') = 'true'
		ORDER BY d.status DESC, d.name ASC LIMIT 20
	`)
	if err == nil {
		defer rows4.Close()
		for rows4.Next() {
			var di models.DeviceInfo
			if rows4.Scan(&di.ID, &di.Name, &di.Status, &di.Models, &di.Load, &di.Latency) == nil {
				devices = append(devices, di)
			}
		}
	}

	var costDay, costWeek, costMonth float64
	_ = s.DB.QueryRow(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM llm_costs WHERE created_at >= now() - interval '1 day'`).Scan(&costDay)
	_ = s.DB.QueryRow(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM llm_costs WHERE created_at >= now() - interval '7 days'`).Scan(&costWeek)
	_ = s.DB.QueryRow(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM llm_costs WHERE created_at >= now() - interval '30 days'`).Scan(&costMonth)

	var modelsCount int
	_ = s.DB.QueryRow(ctx, `SELECT COUNT(*) FROM models`).Scan(&modelsCount)

	WriteJSON(w, http.StatusOK, map[string]any{
		"jobs":         jobStats,
		"benchmarks":   benchStats,
		"running_jobs": runningJobs,
		"devices":      devices,
		"costs": map[string]float64{
			"today": costDay,
			"week":  costWeek,
			"month": costMonth,
		},
		"models_count": modelsCount,
		"updated_at":   time.Now().UTC().Format(time.RFC3339),
	})
}
