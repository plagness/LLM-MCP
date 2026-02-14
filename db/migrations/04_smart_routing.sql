-- Миграция 04: Smart Routing
-- Добавляет tier, thinking, context_k для автоматического роутинга по quality

-- 1. Колонки tier и thinking
ALTER TABLE models ADD COLUMN IF NOT EXISTS tier TEXT;
ALTER TABLE models ADD COLUMN IF NOT EXISTS thinking BOOLEAN DEFAULT FALSE;

-- 2. Заполнение tier по params_b для ollama-моделей
UPDATE models SET tier = 'tiny'   WHERE provider='ollama' AND params_b <= 1.2 AND tier IS NULL;
UPDATE models SET tier = 'small'  WHERE provider='ollama' AND params_b > 1.2 AND params_b <= 2.0 AND tier IS NULL;
UPDATE models SET tier = 'medium' WHERE provider='ollama' AND params_b > 2.0 AND params_b <= 4.5 AND tier IS NULL;
UPDATE models SET tier = 'large'  WHERE provider='ollama' AND params_b > 4.5 AND params_b <= 10.0 AND tier IS NULL;
UPDATE models SET tier = 'xl'     WHERE provider='ollama' AND params_b > 10.0 AND tier IS NULL;
UPDATE models SET tier = 'embed'  WHERE (kind = 'embed' OR id LIKE '%embed%') AND tier IS NULL;

-- 3. Заполнение context_k (тысячи токенов, из документации моделей)
UPDATE models SET context_k = 2   WHERE id IN ('tinyllama:latest','tinyllama:1.1b-chat') AND context_k IS NULL;
UPDATE models SET context_k = 4   WHERE id IN ('yi:6b-chat','stablelm2:1.6b') AND context_k IS NULL;
UPDATE models SET context_k = 8   WHERE id IN ('gemma3:1b','gemma2:2b','llama3:8b',
    'deepseek-r1:1.5b','deepcoder:1.5b','deepscaler:1.5b','nomic-embed-text:latest') AND context_k IS NULL;
UPDATE models SET context_k = 32  WHERE id IN ('qwen3:0.6b','qwen3:1.7b',
    'qwen2.5:1.5b-instruct','qwen2.5:3b','qwen2.5:3b-instruct','qwen2.5:7b-instruct',
    'qwen2.5-coder:1.5b-instruct','exaone-deep:2.4b','granite4:1b',
    'lfm2.5-thinking:latest','falcon3:1b','smollm2:1.7b') AND context_k IS NULL;
UPDATE models SET context_k = 16  WHERE id = 'phi4-reasoning:14b' AND context_k IS NULL;
UPDATE models SET context_k = 128 WHERE id IN ('llama3.2:1b','llama3.2:3b',
    'phi3.5:3.8b','phi3.5:3.8b-mini-instruct-q4_0','phi3:mini',
    'qwen2.5vl:3b','qwen2.5vl:latest','qwen3-vl:latest') AND context_k IS NULL;
-- Fallback для оставшихся ollama-моделей
UPDATE models SET context_k = 4 WHERE context_k IS NULL AND provider = 'ollama';

-- 4. Thinking-модели
UPDATE models SET thinking = TRUE WHERE id IN (
    'qwen3:0.6b','qwen3:1.7b',
    'deepseek-r1:1.5b',
    'phi4-reasoning:14b',
    'lfm2.5-thinking:latest',
    'deepscaler:1.5b',
    'exaone-deep:2.4b'
);

-- 5. Фикс kind для embed-моделей
UPDATE models SET kind = 'embed', tier = 'embed'
    WHERE (id LIKE '%nomic-embed%' OR id LIKE '%embed-text%') AND kind != 'embed';

-- 6. Cloud-модели + pricing
INSERT INTO models (id, provider, family, kind, context_k, tier, thinking, meta) VALUES
  ('google/gemini-2.5-flash',      'openrouter', 'google',    'chat', 1000, 'cloud_economy', FALSE, '{}'),
  ('x-ai/grok-4.1-fast',           'openrouter', 'x-ai',     'chat', 131,  'cloud_economy', FALSE, '{}'),
  ('anthropic/claude-sonnet-4-5',   'openrouter', 'anthropic', 'chat', 200,  'cloud_premium', TRUE,  '{}')
ON CONFLICT (id) DO UPDATE SET
  context_k = EXCLUDED.context_k,
  tier = EXCLUDED.tier,
  thinking = EXCLUDED.thinking,
  updated_at = now();

INSERT INTO model_pricing (model_id, price_in_1m, price_out_1m) VALUES
  ('google/gemini-2.5-flash',    0.15,  0.60),
  ('x-ai/grok-4.1-fast',        5.00, 25.00),
  ('anthropic/claude-sonnet-4-5', 3.00, 15.00)
ON CONFLICT (model_id) DO UPDATE SET
  price_in_1m = EXCLUDED.price_in_1m,
  price_out_1m = EXCLUDED.price_out_1m,
  updated_at = now();

-- 7. Индексы
CREATE INDEX IF NOT EXISTS models_tier_idx ON models (tier, provider);
CREATE INDEX IF NOT EXISTS models_thinking_idx ON models (thinking) WHERE thinking = TRUE;

-- 8. View для статистики устройств (рейтинг + uptime)
CREATE OR REPLACE VIEW v_device_stats AS
SELECT
    d.id AS device_id,
    d.name,
    d.status,
    d.last_seen,
    COALESCE(s.total_jobs, 0) AS total_jobs_7d,
    COALESCE(s.done_jobs, 0) AS done_jobs_7d,
    CASE WHEN COALESCE(s.total_jobs, 0) > 0
         THEN ROUND(s.done_jobs::numeric / s.total_jobs * 100, 1)
         ELSE 0 END AS success_rate,
    COALESCE(s.avg_ms, 0)::int AS avg_latency_ms
FROM devices d
LEFT JOIN LATERAL (
    SELECT
        COUNT(*) AS total_jobs,
        COUNT(*) FILTER (WHERE ja.status = 'done') AS done_jobs,
        AVG((ja.metrics->>'ms')::numeric) FILTER (WHERE ja.metrics->>'ms' IS NOT NULL) AS avg_ms
    FROM job_attempts ja
    JOIN jobs j ON j.id = ja.job_id
    WHERE j.payload->>'device_id' = d.id
      AND ja.started_at >= now() - interval '7 days'
) s ON TRUE
WHERE d.tags->>'ollama' = 'true';
