-- Миграция v3: LISTEN/NOTIFY для SSE (замена polling)

-- Триггерная функция: уведомляет об изменении статуса задачи
CREATE OR REPLACE FUNCTION notify_job_status_change() RETURNS trigger AS $$
BEGIN
  IF OLD.status IS DISTINCT FROM NEW.status THEN
    PERFORM pg_notify('job_update', NEW.id::text);
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Триггер на UPDATE jobs.status
DROP TRIGGER IF EXISTS trg_job_status_notify ON jobs;
CREATE TRIGGER trg_job_status_notify
  AFTER UPDATE ON jobs
  FOR EACH ROW
  EXECUTE FUNCTION notify_job_status_change();

COMMENT ON FUNCTION notify_job_status_change IS 'Отправляет pg_notify при смене статуса задачи — для SSE без polling';
