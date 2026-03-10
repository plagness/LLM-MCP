-- Migration 05: Chat completions support + model rankings/stats
-- Enables smart model selection and per-model analytics

-- Allow chat completions to record costs without a job
ALTER TABLE llm_costs ALTER COLUMN job_id DROP NOT NULL;
ALTER TABLE llm_costs DROP CONSTRAINT IF EXISTS llm_costs_job_id_fkey;

-- Model catalog with category scores for smart routing
CREATE TABLE IF NOT EXISTS model_rankings (
  model_id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  display_name TEXT,
  category_scores JSONB DEFAULT '{}',
  context_k INT,
  price_in_1m NUMERIC,
  price_out_1m NUMERIC,
  modalities JSONB DEFAULT '["text"]',
  supports_streaming BOOLEAN DEFAULT TRUE,
  supports_tools BOOLEAN DEFAULT FALSE,
  supports_vision BOOLEAN DEFAULT FALSE,
  is_local BOOLEAN DEFAULT FALSE,
  updated_at TIMESTAMPTZ DEFAULT now()
);

-- Accumulative per-model statistics
CREATE TABLE IF NOT EXISTS model_stats (
  model_id TEXT PRIMARY KEY,
  total_requests INT DEFAULT 0,
  total_tokens_in BIGINT DEFAULT 0,
  total_tokens_out BIGINT DEFAULT 0,
  total_cost_usd NUMERIC DEFAULT 0,
  avg_duration_ms INT DEFAULT 0,
  error_count INT DEFAULT 0,
  last_used_at TIMESTAMPTZ,
  feedback_positive INT DEFAULT 0,
  feedback_negative INT DEFAULT 0,
  success_rate NUMERIC GENERATED ALWAYS AS (
    CASE WHEN total_requests > 0
    THEN ((total_requests - error_count)::numeric / total_requests * 100)
    ELSE 0 END
  ) STORED,
  avg_cost_per_request NUMERIC GENERATED ALWAYS AS (
    CASE WHEN total_requests > 0
    THEN total_cost_usd / total_requests
    ELSE 0 END
  ) STORED,
  updated_at TIMESTAMPTZ DEFAULT now()
);

-- Seed top models with category scores
-- Categories: programming, search, analysis, creative, science, multilingual
INSERT INTO model_rankings (model_id, provider, display_name, category_scores, context_k, price_in_1m, price_out_1m, supports_vision) VALUES
('google/gemini-2.5-flash-lite', 'openrouter', 'Gemini 2.5 Flash Lite',
 '{"search":90,"analysis":75,"creative":70,"programming":65,"science":70,"multilingual":80}',
 262, 0.075, 0.30, false),
('google/gemini-2.5-flash', 'openrouter', 'Gemini 2.5 Flash',
 '{"search":92,"analysis":82,"creative":78,"programming":78,"science":80,"multilingual":85}',
 1000, 0.15, 0.60, true),
('anthropic/claude-sonnet-4-6', 'openrouter', 'Claude Sonnet 4.6',
 '{"programming":95,"analysis":92,"science":88,"creative":85,"search":75,"multilingual":80}',
 200, 3.0, 15.0, true),
('anthropic/claude-opus-4-6', 'openrouter', 'Claude Opus 4.6',
 '{"programming":98,"analysis":97,"science":95,"creative":90,"search":78,"multilingual":85}',
 200, 15.0, 75.0, true),
('deepseek/deepseek-chat-v3', 'openrouter', 'DeepSeek V3',
 '{"programming":88,"analysis":80,"science":82,"creative":72,"search":70,"multilingual":75}',
 128, 0.27, 1.10, false),
('x-ai/grok-4.1', 'openrouter', 'Grok 4.1',
 '{"analysis":85,"programming":82,"science":83,"creative":80,"search":85,"multilingual":70}',
 131, 3.0, 15.0, false),
('meta-llama/llama-4-maverick', 'openrouter', 'Llama 4 Maverick',
 '{"programming":80,"analysis":78,"creative":82,"search":75,"science":76,"multilingual":85}',
 128, 0.20, 0.60, false),
('qwen/qwen3.5-9b', 'openrouter', 'Qwen 3.5 9B',
 '{"multilingual":90,"programming":75,"search":78,"analysis":72,"creative":70,"science":70}',
 262, 0.10, 0.15, false)
ON CONFLICT (model_id) DO UPDATE SET
  category_scores = EXCLUDED.category_scores,
  price_in_1m = EXCLUDED.price_in_1m,
  price_out_1m = EXCLUDED.price_out_1m,
  context_k = EXCLUDED.context_k,
  updated_at = now();

-- Init model_stats for seeded models
INSERT INTO model_stats (model_id)
SELECT model_id FROM model_rankings
ON CONFLICT DO NOTHING;
