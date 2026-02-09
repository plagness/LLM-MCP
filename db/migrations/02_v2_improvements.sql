-- Миграция v2: улучшения производительности и новые фичи

-- 1. Добавление per-device concurrency в device_limits
ALTER TABLE device_limits ADD COLUMN IF NOT EXISTS max_concurrency INT;

-- 2. Индексы для оптимизации queries
CREATE INDEX IF NOT EXISTS jobs_device_id_idx ON jobs ((payload->>'device_id'));
CREATE INDEX IF NOT EXISTS jobs_model_id_idx ON jobs ((payload->>'model_id'));
CREATE INDEX IF NOT EXISTS jobs_updated_status_idx ON jobs (updated_at DESC, status);

-- 3. Таблица для отслеживания cost (потраченных денег)
CREATE TABLE IF NOT EXISTS llm_costs (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  job_id UUID REFERENCES jobs(id) ON DELETE CASCADE,
  model_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  tokens_in INT NOT NULL DEFAULT 0,
  tokens_out INT NOT NULL DEFAULT 0,
  cost_usd NUMERIC NOT NULL DEFAULT 0,
  currency TEXT NOT NULL DEFAULT 'USD',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS llm_costs_job_idx ON llm_costs (job_id);
CREATE INDEX IF NOT EXISTS llm_costs_model_idx ON llm_costs (model_id, created_at DESC);
CREATE INDEX IF NOT EXISTS llm_costs_created_idx ON llm_costs (created_at DESC);

-- 4. Добавление метаданных в benchmarks (если ещё нет)
-- Уже есть поле meta, но убедимся что оно JSONB
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'benchmarks' AND column_name = 'meta'
  ) THEN
    ALTER TABLE benchmarks ADD COLUMN meta JSONB;
  END IF;
END $$;

-- 5. View для статистики по costs
CREATE OR REPLACE VIEW v_cost_stats AS
SELECT
  DATE(created_at) AS date,
  model_id,
  provider,
  COUNT(*) AS requests,
  SUM(tokens_in) AS total_tokens_in,
  SUM(tokens_out) AS total_tokens_out,
  SUM(cost_usd) AS total_cost_usd
FROM llm_costs
GROUP BY DATE(created_at), model_id, provider
ORDER BY date DESC, total_cost_usd DESC;

-- 6. Функция для автоматического расчёта cost
CREATE OR REPLACE FUNCTION calculate_job_cost(
  p_model_id TEXT,
  p_tokens_in INT,
  p_tokens_out INT
) RETURNS NUMERIC AS $$
DECLARE
  v_price_in NUMERIC;
  v_price_out NUMERIC;
  v_cost NUMERIC;
BEGIN
  SELECT price_in_1m, price_out_1m
  INTO v_price_in, v_price_out
  FROM model_pricing
  WHERE model_id = p_model_id;

  IF v_price_in IS NULL AND v_price_out IS NULL THEN
    RETURN 0;
  END IF;

  v_cost := COALESCE(v_price_in / 1000000.0 * p_tokens_in, 0) +
            COALESCE(v_price_out / 1000000.0 * p_tokens_out, 0);

  RETURN v_cost;
END;
$$ LANGUAGE plpgsql;

-- 7. Комментарии для документации
COMMENT ON TABLE llm_costs IS 'Отслеживание стоимости запросов к платным LLM провайдерам';
COMMENT ON COLUMN device_limits.max_concurrency IS 'Максимальное количество параллельных задач на устройстве (NULL = нет лимита)';
COMMENT ON FUNCTION calculate_job_cost IS 'Рассчитывает стоимость задачи на основе токенов и цен из model_pricing';
