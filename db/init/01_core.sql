CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Устройства, найденные в сети
CREATE TABLE IF NOT EXISTS devices (
  id TEXT PRIMARY KEY,
  name TEXT,
  platform TEXT,
  arch TEXT,
  host TEXT,
  tags JSONB,
  status TEXT NOT NULL DEFAULT 'unknown',
  last_seen TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS devices_status_idx ON devices (status, last_seen);

-- Снимки метрик устройств
CREATE TABLE IF NOT EXISTS device_metrics (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  ts TIMESTAMPTZ NOT NULL DEFAULT now(),
  cpu_pct NUMERIC,
  mem_used_mb INT,
  mem_total_mb INT,
  gpu_name TEXT,
  vram_used_mb INT,
  vram_total_mb INT,
  tps NUMERIC,
  latency_ms INT,
  notes JSONB
);
CREATE INDEX IF NOT EXISTS device_metrics_device_idx ON device_metrics (device_id, ts DESC);

-- Каталог моделей
CREATE TABLE IF NOT EXISTS models (
  id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  family TEXT,
  kind TEXT NOT NULL DEFAULT 'chat',
  params_b NUMERIC,
  context_k INT,
  size_gb NUMERIC,
  quant TEXT,
  status TEXT NOT NULL DEFAULT 'active',
  meta JSONB,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS models_provider_idx ON models (provider, family);

-- Цены (за 1M токенов)
CREATE TABLE IF NOT EXISTS model_pricing (
  model_id TEXT PRIMARY KEY REFERENCES models(id) ON DELETE CASCADE,
  price_in_1m NUMERIC,
  price_out_1m NUMERIC,
  currency TEXT NOT NULL DEFAULT 'USD',
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Доступность моделей на устройствах
CREATE TABLE IF NOT EXISTS device_models (
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  model_id TEXT NOT NULL REFERENCES models(id) ON DELETE CASCADE,
  available BOOLEAN NOT NULL DEFAULT TRUE,
  max_context_k INT,
  meta JSONB,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (device_id, model_id)
);

-- Бенчмарки
CREATE TABLE IF NOT EXISTS benchmarks (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  model_id TEXT NOT NULL REFERENCES models(id) ON DELETE CASCADE,
  task_type TEXT NOT NULL,
  tokens_in INT,
  tokens_out INT,
  latency_ms INT,
  tps NUMERIC,
  meta JSONB,
  ok BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS benchmarks_device_idx ON benchmarks (device_id, created_at DESC);

-- Устойчивая очередь задач
CREATE TABLE IF NOT EXISTS jobs (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  kind TEXT NOT NULL,
  payload JSONB NOT NULL,
  priority INT NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'queued',
  source TEXT,
  attempts INT NOT NULL DEFAULT 0,
  max_attempts INT NOT NULL DEFAULT 3,
  lease_until TIMESTAMPTZ,
  deadline_at TIMESTAMPTZ,
  result JSONB,
  error TEXT,
  queued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS jobs_status_idx ON jobs (status, priority DESC, queued_at);
CREATE INDEX IF NOT EXISTS jobs_lease_idx ON jobs (lease_until);

-- Попытки выполнения задач
CREATE TABLE IF NOT EXISTS job_attempts (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  worker_id TEXT,
  started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ,
  status TEXT NOT NULL DEFAULT 'running',
  error TEXT,
  metrics JSONB
);
CREATE INDEX IF NOT EXISTS job_attempts_job_idx ON job_attempts (job_id, started_at DESC);

-- Ограничения по моделям для устройств (строгая отсечка)
CREATE TABLE IF NOT EXISTS device_limits (
  device_id TEXT PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
  ram_gb NUMERIC,
  vram_gb NUMERIC,
  max_params_b NUMERIC,
  max_size_gb NUMERIC,
  max_context_k INT,
  allow_models JSONB,
  deny_models JSONB,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
