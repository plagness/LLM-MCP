package grpcserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"time"

	"llm-mcp/core/internal/pb"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Server struct {
	pb.UnimplementedCoreServer
	DB *pgxpool.Pool
}

func New(db *pgxpool.Pool) *Server {
	return &Server{DB: db}
}

func (s *Server) SubmitJob(ctx context.Context, req *pb.SubmitJobRequest) (*pb.SubmitJobResponse, error) {
	payload := sanitizeJSON(req.PayloadJson)
	maxAttempts := int(req.MaxAttempts)
	if maxAttempts == 0 {
		maxAttempts = 3
	}

	var deadline *time.Time
	if req.DeadlineAt != "" {
		t, err := time.Parse(time.RFC3339, req.DeadlineAt)
		if err != nil {
			return nil, err
		}
		deadline = &t
	}

	row := s.DB.QueryRow(ctx, `
		INSERT INTO jobs (kind, payload, priority, source, max_attempts, deadline_at)
		VALUES ($1, $2::jsonb, $3, $4, $5, $6)
		RETURNING id
	`, req.Kind, payload, int(req.Priority), req.Source, maxAttempts, deadline)

	var id string
	if err := row.Scan(&id); err != nil {
		log.Printf("grpc submit error: %v", err)
		return nil, err
	}
	log.Printf("grpc submit job id=%s kind=%s", id, req.Kind)
	return &pb.SubmitJobResponse{JobId: id}, nil
}

func (s *Server) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.GetJobResponse, error) {
	job, err := fetchJob(ctx, s.DB, req.JobId)
	if err != nil {
		return nil, err
	}
	return &pb.GetJobResponse{Job: job}, nil
}

func (s *Server) StreamJob(req *pb.StreamJobRequest, stream pb.Core_StreamJobServer) error {
	ctx := stream.Context()
	var lastStatus string
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
			job, err := fetchJob(ctx, s.DB, req.JobId)
			if err != nil {
				return err
			}
			if lastStatus == "" || job.Status != lastStatus {
				payload, _ := json.Marshal(job)
				event := &pb.JobEvent{
					JobId:    job.Id,
					Type:     "status",
					Message:  job.Status,
					Ts:       time.Now().UTC().Format(time.RFC3339),
					DataJson: string(payload),
				}
				if err := stream.Send(event); err != nil {
					return err
				}
				lastStatus = job.Status
			}
			if job.Status == "done" || job.Status == "error" {
				return nil
			}
		}
	}
}

func (s *Server) RegisterWorker(ctx context.Context, req *pb.RegisterWorkerRequest) (*pb.RegisterWorkerResponse, error) {
	worker := req.Worker
	workerID := worker.Id
	if workerID == "" {
		workerID = "worker-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	tags := sanitizeJSON(worker.TagsJson)
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
	`, workerID, worker.Name, worker.Platform, worker.Arch, worker.Host, tags)
	if err != nil {
		log.Printf("grpc register error: %v", err)
		return nil, err
	}
	log.Printf("grpc worker registered id=%s name=%s", workerID, worker.Name)
	return &pb.RegisterWorkerResponse{WorkerId: workerID}, nil
}

func (s *Server) ClaimJob(ctx context.Context, req *pb.ClaimJobRequest) (*pb.ClaimJobResponse, error) {
	leaseSeconds := int(req.LeaseSeconds)
	if leaseSeconds <= 0 {
		leaseSeconds = 60
	}

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	query := `
		SELECT id, kind, payload, status, attempts, max_attempts,
		       lease_until, deadline_at, result, error, priority, queued_at, updated_at
		FROM jobs
		WHERE status IN ('queued','running')
		  AND (lease_until IS NULL OR lease_until < now())
	`
	args := []any{}
	if len(req.Kinds) > 0 {
		query += " AND kind = ANY($1)\n"
		args = append(args, req.Kinds)
	}
	query += " ORDER BY priority DESC, queued_at FOR UPDATE SKIP LOCKED LIMIT 1"

	row := tx.QueryRow(ctx, query, args...)
	var j jobRow
	if err := row.Scan(
		&j.ID,
		&j.Kind,
		&j.Payload,
		&j.Status,
		&j.Attempts,
		&j.MaxAttempts,
		&j.LeaseUntil,
		&j.DeadlineAt,
		&j.Result,
		&j.Error,
		&j.Priority,
		&j.QueuedAt,
		&j.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			return &pb.ClaimJobResponse{}, nil
		}
		return nil, err
	}

	leaseUntil := time.Now().UTC().Add(time.Duration(leaseSeconds) * time.Second)
	_, err = tx.Exec(ctx, `
		UPDATE jobs
		SET status='running', attempts=attempts+1, lease_until=$1, updated_at=now()
		WHERE id=$2
	`, leaseUntil, j.ID)
	if err != nil {
		return nil, err
	}
	_, _ = tx.Exec(ctx, `
		INSERT INTO job_attempts (job_id, worker_id, status)
		VALUES ($1, $2, 'running')
	`, j.ID, req.WorkerId)
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	j.Attempts += 1
	j.Status = "running"
	j.LeaseUntil = &leaseUntil
	job := rowToPB(j)
	log.Printf("grpc job claimed id=%s worker=%s kind=%s", job.Id, req.WorkerId, job.Kind)
	return &pb.ClaimJobResponse{Job: job}, nil
}

func (s *Server) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	extend := int(req.ExtendSeconds)
	if extend <= 0 {
		extend = 30
	}
	leaseUntil := time.Now().UTC().Add(time.Duration(extend) * time.Second)
	_, err := s.DB.Exec(ctx, `
		UPDATE jobs
		SET lease_until=$1, updated_at=now()
		WHERE id=$2 AND status='running'
	`, leaseUntil, req.JobId)
	if err != nil {
		return nil, err
	}
	return &pb.HeartbeatResponse{Ok: true}, nil
}

func (s *Server) CompleteJob(ctx context.Context, req *pb.CompleteJobRequest) (*pb.CompleteJobResponse, error) {
	result := sanitizeJSON(req.ResultJson)
	metrics := sanitizeJSON(req.MetricsJson)
	_, err := s.DB.Exec(ctx, `
		UPDATE jobs
		SET status='done', result=$1::jsonb, lease_until=NULL, updated_at=now()
		WHERE id=$2
	`, result, req.JobId)
	if err != nil {
		return nil, err
	}
	_, _ = s.DB.Exec(ctx, `
		UPDATE job_attempts
		SET status='done', finished_at=now(), metrics=$1::jsonb
		WHERE id = (
		  SELECT id FROM job_attempts
		  WHERE job_id=$2 AND status='running'
		  ORDER BY started_at DESC
		  LIMIT 1
		)
	`, metrics, req.JobId)
	log.Printf("grpc job complete id=%s worker=%s", req.JobId, req.WorkerId)
	return &pb.CompleteJobResponse{Ok: true}, nil
}

func (s *Server) FailJob(ctx context.Context, req *pb.FailJobRequest) (*pb.FailJobResponse, error) {
	metrics := sanitizeJSON(req.MetricsJson)
	var attempts int
	var maxAttempts int
	row := s.DB.QueryRow(ctx, `SELECT attempts, max_attempts FROM jobs WHERE id=$1`, req.JobId)
	if err := row.Scan(&attempts, &maxAttempts); err != nil {
		return nil, err
	}
	status := "queued"
	if attempts >= maxAttempts {
		status = "error"
	}
	_, err := s.DB.Exec(ctx, `
		UPDATE jobs
		SET status=$1, error=$2, lease_until=NULL, updated_at=now()
		WHERE id=$3
	`, status, req.Error, req.JobId)
	if err != nil {
		return nil, err
	}
	_, _ = s.DB.Exec(ctx, `
		UPDATE job_attempts
		SET status='error', finished_at=now(), error=$1, metrics=$2::jsonb
		WHERE id = (
		  SELECT id FROM job_attempts
		  WHERE job_id=$3 AND status='running'
		  ORDER BY started_at DESC
		  LIMIT 1
		)
	`, req.Error, metrics, req.JobId)
	log.Printf("grpc job failed id=%s worker=%s status=%s error=%s", req.JobId, req.WorkerId, status, req.Error)
	return &pb.FailJobResponse{Ok: true}, nil
}

func (s *Server) ReportMetrics(ctx context.Context, req *pb.ReportMetricsRequest) (*pb.ReportMetricsResponse, error) {
	if req.Worker != nil {
		worker := req.Worker
		_, _ = s.DB.Exec(ctx, `
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
		`, worker.Id, worker.Name, worker.Platform, worker.Arch, worker.Host, sanitizeJSON(worker.TagsJson))
	}
	if req.MetricsJson != "" && req.Worker != nil {
		_, _ = s.DB.Exec(ctx, `
			INSERT INTO device_metrics (device_id, notes)
			VALUES ($1, $2::jsonb)
		`, req.Worker.Id, sanitizeJSON(req.MetricsJson))
	}
	return &pb.ReportMetricsResponse{Ok: true}, nil
}

func (s *Server) ReportBenchmark(ctx context.Context, req *pb.ReportBenchmarkRequest) (*pb.ReportBenchmarkResponse, error) {
	if req.Benchmark == nil {
		return &pb.ReportBenchmarkResponse{Ok: true}, nil
	}
	b := req.Benchmark
	meta := sanitizeJSON(b.MetaJson)
	if _, err := s.DB.Exec(ctx, `
		INSERT INTO models (id, provider, kind, updated_at)
		VALUES ($1, COALESCE(($2::jsonb)->>'provider','unknown'), 'bench', now())
		ON CONFLICT (id) DO UPDATE SET
		  provider = excluded.provider,
		  updated_at = now()
	`, b.ModelId, meta); err != nil {
		log.Printf("grpc benchmark model upsert error: %v", err)
	}
	_, err := s.DB.Exec(ctx, `
		INSERT INTO benchmarks (device_id, model_id, task_type, tokens_in, tokens_out, latency_ms, tps, meta, ok)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, TRUE)
	`, b.DeviceId, b.ModelId, b.TaskType, b.TokensIn, b.TokensOut, b.LatencyMs, b.Tps, meta)
	if err != nil {
		log.Printf("grpc benchmark insert error: %v", err)
		return nil, err
	}
	log.Printf("grpc benchmark saved device=%s model=%s task=%s tps=%.2f", b.DeviceId, b.ModelId, b.TaskType, b.Tps)
	return &pb.ReportBenchmarkResponse{Ok: true}, nil
}

type jobRow struct {
	ID          string
	Kind        string
	Payload     []byte
	Status      string
	Attempts    int
	MaxAttempts int
	LeaseUntil  *time.Time
	DeadlineAt  *time.Time
	Result      []byte
	Error       sql.NullString
	Priority    int
	QueuedAt    time.Time
	UpdatedAt   time.Time
}

func fetchJob(ctx context.Context, db *pgxpool.Pool, id string) (*pb.Job, error) {
	row := db.QueryRow(ctx, `
		SELECT id, kind, payload, status, attempts, max_attempts,
		       lease_until, deadline_at, result, error, priority, queued_at, updated_at
		FROM jobs
		WHERE id = $1
	`, id)
	var j jobRow
	if err := row.Scan(
		&j.ID,
		&j.Kind,
		&j.Payload,
		&j.Status,
		&j.Attempts,
		&j.MaxAttempts,
		&j.LeaseUntil,
		&j.DeadlineAt,
		&j.Result,
		&j.Error,
		&j.Priority,
		&j.QueuedAt,
		&j.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return rowToPB(j), nil
}

func rowToPB(j jobRow) *pb.Job {
	job := &pb.Job{
		Id:          j.ID,
		Kind:        j.Kind,
		PayloadJson: string(j.Payload),
		Status:      j.Status,
		Attempts:    int32(j.Attempts),
		MaxAttempts: int32(j.MaxAttempts),
		Priority:    int32(j.Priority),
		QueuedAt:    j.QueuedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   j.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if j.LeaseUntil != nil {
		job.LeaseUntil = j.LeaseUntil.UTC().Format(time.RFC3339)
	}
	if j.DeadlineAt != nil {
		job.DeadlineAt = j.DeadlineAt.UTC().Format(time.RFC3339)
	}
	if len(j.Result) > 0 {
		job.ResultJson = string(j.Result)
	}
	if j.Error.Valid {
		job.Error = j.Error.String
	}
	return job
}

func sanitizeJSON(raw string) string {
	if raw == "" {
		return "{}"
	}
	var tmp any
	if err := json.Unmarshal([]byte(raw), &tmp); err != nil {
		return "{}"
	}
	return raw
}
