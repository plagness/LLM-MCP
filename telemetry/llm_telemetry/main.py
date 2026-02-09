"""LLM Telemetry — alert-only мониторинг.

Вместо полного ASCII-дашборда каждые 2 сек, теперь:
- Проверяет состояние каждые CHECK_INTERVAL секунд (по умолчанию 30)
- Отправляет Telegram-уведомления только при проблемах:
  - Устройство перешло в offline
  - Job failed > N раз (ALERT_FAIL_THRESHOLD)
  - Очередь застряла (queued > 0, running = 0)
  - Устройство вернулось online (recovery)
- Визуализация перенесена на web UI (/llm страница в telegram-mcp)
"""

import logging
import os
import time
from datetime import datetime

import psycopg
from psycopg.rows import dict_row

from .telegram_gateway import TelegramGateway


logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
)
log = logging.getLogger("llm-telemetry")


def _env_int(name: str, default: int) -> int:
    raw = os.getenv(name, "").strip()
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError:
        return default


def _env_float(name: str, default: float) -> float:
    raw = os.getenv(name, "").strip()
    if not raw:
        return default
    try:
        return float(raw)
    except ValueError:
        return default


def _fetch_alerts(conn, fail_threshold: int) -> dict:
    """Собирает данные для алертов из БД."""
    alerts = {
        "devices_offline": [],
        "devices_online": [],
        "queue_stuck": False,
        "failed_jobs": [],
        "queued": 0,
        "running": 0,
    }
    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute("SELECT status, COUNT(*) AS count FROM jobs GROUP BY status")
        jobs = {row["status"]: row["count"] for row in cur.fetchall()}
        alerts["queued"] = jobs.get("queued", 0)
        alerts["running"] = jobs.get("running", 0)

        if alerts["queued"] > 0 and alerts["running"] == 0:
            alerts["queue_stuck"] = True

        cur.execute("""
            SELECT id, name, status, last_seen
            FROM devices
            WHERE COALESCE(tags->>'ollama', 'false') = 'true'
            ORDER BY name ASC
        """)
        for row in cur.fetchall():
            dev = {
                "id": row.get("id"),
                "name": row.get("name") or row.get("id"),
                "status": row.get("status"),
            }
            if dev["status"] == "offline":
                alerts["devices_offline"].append(dev)
            elif dev["status"] == "online":
                alerts["devices_online"].append(dev)

        cur.execute("""
            SELECT id, kind, error, attempts, max_attempts
            FROM jobs
            WHERE status = 'error'
              AND updated_at > now() - interval '1 hour'
              AND attempts >= %s
            ORDER BY updated_at DESC
            LIMIT 5
        """, (fail_threshold,))
        alerts["failed_jobs"] = cur.fetchall()

    return alerts


def _format_alert(alerts: dict, prev_offline: set[str]) -> str | None:
    """Формирует текст алерта. Возвращает None если алертов нет."""
    lines: list[str] = []

    current_offline = {d["id"] for d in alerts["devices_offline"]}
    new_offline = current_offline - prev_offline
    for dev in alerts["devices_offline"]:
        if dev["id"] in new_offline:
            lines.append(f"OFFLINE: {dev['name']}")

    recovered = prev_offline - current_offline
    online_map = {d["id"]: d for d in alerts["devices_online"]}
    for dev_id in recovered:
        name = online_map.get(dev_id, {}).get("name", dev_id)
        lines.append(f"ONLINE: {name}")

    if alerts["queue_stuck"]:
        lines.append(f"Queue stuck: {alerts['queued']} queued, 0 running")

    for job in alerts["failed_jobs"]:
        error = (job.get("error") or "unknown")[:80]
        lines.append(f"Job failed: {job['kind']} ({job['attempts']}/{job['max_attempts']}) - {error}")

    if not lines:
        return None

    now = datetime.now().astimezone()
    header = f"LLM Alert  {now.strftime('%H:%M:%S')}"
    return header + "\n" + "\n".join(lines)


def main() -> None:
    tg_cfg = TelegramGateway.from_env()
    ok, error = tg_cfg.validate()
    if not ok:
        log.error(error)
        return

    dsn = os.getenv("TELEMETRY_DB_DSN", os.getenv("DB_DSN", "")).strip()
    if not dsn:
        log.error("DB_DSN (or TELEMETRY_DB_DSN) is required")
        return

    check_interval = _env_float("TELEMETRY_CHECK_INTERVAL", 30.0)
    fail_threshold = _env_int("ALERT_FAIL_THRESHOLD", 3)

    mcp_client, direct_client = tg_cfg.build_clients()
    if tg_cfg.use_mcp and mcp_client is None and not (tg_cfg.fallback_direct and direct_client is not None):
        log.error("TELEGRAM_USE_MCP=1 but MCP client is unavailable and no direct fallback configured")
        return

    conn = None
    prev_offline: set[str] = set()
    seen_failed: set[str] = set()
    first_run = True

    log.info("alert-only mode started (interval=%.0fs, fail_threshold=%d)", check_interval, fail_threshold)

    while True:
        if conn is None or conn.closed:
            try:
                conn = psycopg.connect(dsn, connect_timeout=5)
                conn.autocommit = True
                log.info("db connected")
            except Exception as exc:
                log.warning("db connect failed err=%s", exc)
                time.sleep(5)
                continue

        try:
            alerts = _fetch_alerts(conn, fail_threshold)
            current_offline = {d["id"] for d in alerts["devices_offline"]}

            # Фильтруем уже отправленные failed jobs
            new_failed = [j for j in alerts["failed_jobs"] if j["id"] not in seen_failed]
            alerts["failed_jobs"] = new_failed

            if first_run:
                prev_offline = current_offline
                seen_failed = {j["id"] for j in alerts["failed_jobs"]}
                first_run = False
                log.info("baseline: devices_offline=%d queued=%d running=%d",
                         len(current_offline), alerts["queued"], alerts["running"])
                time.sleep(check_interval)
                continue

            text = _format_alert(alerts, prev_offline)
            prev_offline = current_offline
            # Запоминаем отправленные job id
            for j in new_failed:
                seen_failed.add(j["id"])
            # Чистим seen_failed от старых
            if len(seen_failed) > 100:
                seen_failed = {j["id"] for j in alerts["failed_jobs"]}

            if text:
                sent = False
                if mcp_client is not None:
                    try:
                        sent = mcp_client.send_or_edit(text)
                    except Exception as exc:
                        log.warning("telegram mcp send failed err=%s", exc)
                        sent = False

                if not sent and tg_cfg.fallback_direct and direct_client is not None:
                    sent = direct_client.send_or_edit(text)

                if sent:
                    log.info("alert sent: %s", text.replace('\n', ' | '))
                else:
                    log.warning("alert send failed (all routes)")
            else:
                log.info("tick: no alerts (offline=%d queued=%d running=%d)",
                         len(current_offline), alerts["queued"], alerts["running"])

        except Exception as exc:
            log.warning("telemetry loop error err=%s", exc)
            time.sleep(5)

        time.sleep(check_interval)


if __name__ == "__main__":
    main()
