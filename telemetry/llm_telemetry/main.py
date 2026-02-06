import json
import logging
import os
import time
from datetime import datetime, timezone

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


def _parse_json(value):
    if value is None:
        return {}
    if isinstance(value, dict):
        return value
    if isinstance(value, (bytes, bytearray)):
        try:
            return json.loads(value.decode("utf-8"))
        except Exception:
            return {}
    if isinstance(value, str):
        try:
            return json.loads(value)
        except Exception:
            return {}
    return {}


def _format_duration(seconds: float) -> str:
    if seconds < 0:
        seconds = 0
    minutes, sec = divmod(int(seconds), 60)
    hours, minutes = divmod(minutes, 60)
    if hours > 0:
        return f"{hours}h{minutes:02d}m"
    if minutes > 0:
        return f"{minutes}m{sec:02d}s"
    return f"{sec}s"


def _format_ts(dt):
    if not dt:
        return "-"
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    local = dt.astimezone()
    return local.strftime("%H:%M:%S")


def _trim(text: str, limit: int) -> str:
    if len(text) <= limit:
        return text
    return text[: max(0, limit - 3)] + "..."


def _bar(value: int, total: int, width: int = 12) -> str:
    if total <= 0:
        total = 1
    value = max(0, min(value, total))
    filled = int(round((value / total) * width))
    filled = max(0, min(filled, width))
    return "[" + ("#" * filled) + ("." * (width - filled)) + "]"


def _fetch_snapshot(conn, running_limit: int, device_limit: int) -> dict:
    snapshot = {
        "jobs": {},
        "benchmarks": {},
        "running_jobs": [],
        "devices": [],
        "device_models": {},
        "device_load": {},
    }
    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute("SELECT status, COUNT(*) AS count FROM jobs GROUP BY status")
        snapshot["jobs"] = {row["status"]: row["count"] for row in cur.fetchall()}

        cur.execute(
            "SELECT status, COUNT(*) AS count FROM jobs WHERE kind = 'benchmark' GROUP BY status"
        )
        snapshot["benchmarks"] = {row["status"]: row["count"] for row in cur.fetchall()}

        cur.execute(
            """
            SELECT id, kind, payload, updated_at, queued_at, attempts, max_attempts
            FROM jobs
            WHERE status = 'running'
            ORDER BY updated_at DESC
            LIMIT %s
            """,
            (running_limit,),
        )
        snapshot["running_jobs"] = cur.fetchall()

        cur.execute(
            """
            SELECT payload->>'device_id' AS device_id, COUNT(*) AS count
            FROM jobs
            WHERE status = 'running' AND payload ? 'device_id'
            GROUP BY 1
            """
        )
        snapshot["device_load"] = {row["device_id"]: row["count"] for row in cur.fetchall() if row["device_id"]}

        cur.execute(
            """
            SELECT device_id, COUNT(*) AS count
            FROM device_models
            WHERE available = TRUE
            GROUP BY device_id
            """
        )
        snapshot["device_models"] = {row["device_id"]: row["count"] for row in cur.fetchall() if row["device_id"]}

        cur.execute(
            """
            SELECT id, name, status, last_seen, tags
            FROM devices
            WHERE COALESCE(tags->>'ollama', 'false') = 'true'
              AND (tags ? 'ips' OR tags ? 'dns' OR tags ? 'ip')
            ORDER BY status DESC, name ASC
            """
        )
        rows = cur.fetchall()
        devices = []
        for row in rows:
            tags = _parse_json(row.get("tags"))
            devices.append(
                {
                    "id": row.get("id"),
                    "name": row.get("name") or row.get("id"),
                    "status": row.get("status"),
                    "last_seen": row.get("last_seen"),
                    "tags": tags,
                }
            )
        if device_limit > 0:
            devices = devices[:device_limit]
        snapshot["devices"] = devices
    return snapshot


def _format_snapshot(snapshot: dict, spinner: str, max_len: int) -> str:
    now = datetime.now().astimezone()
    lines: list[str] = []
    lines.append(f"{spinner} LLM-MCP  {now.strftime('%H:%M:%S')}")

    jobs = snapshot.get("jobs", {})
    queued = jobs.get("queued", 0)
    running = jobs.get("running", 0)
    done = jobs.get("done", 0)
    failed = jobs.get("failed", 0)
    total_jobs = queued + running + done + failed
    lines.append(f"⚙ Q {_bar(done, total_jobs, 10)} q{queued} r{running} d{done} f{failed}")

    bench = snapshot.get("benchmarks", {})
    if bench:
        bq = bench.get("queued", 0)
        br = bench.get("running", 0)
        bd = bench.get("done", 0)
        bf = bench.get("failed", 0)
        total_b = bq + br + bd + bf
        lines.append(f"🏁 B {_bar(bd, total_b, 10)} q{bq} r{br} d{bd} f{bf}")

    devices = snapshot.get("devices", [])
    device_models = snapshot.get("device_models", {})
    device_load = snapshot.get("device_load", {})

    if devices:
        lines.append("🤖 FLEET")
        count = 0
        for dev in devices:
            tags = dev.get("tags", {})
            models_count = device_models.get(dev["id"]) or len(tags.get("models", []) or [])
            if models_count <= 0:
                continue
            latency = tags.get("ollama_latency_ms")
            load = device_load.get(dev["id"], 0)
            status = dev.get("status") or "unknown"
            last_seen = _format_ts(dev.get("last_seen")) if status != "online" else ""
            latency_str = f"{latency}ms" if latency is not None else "--"
            name = dev["name"]
            if len(name) > 12:
                name = name[:9] + "..."
            badge = "🟢" if status == "online" else ("🔴" if status == "offline" else "🟡")
            line = f"{badge} {name:<12} 📦{models_count:<2} 🔥{load:<2}"
            if latency is not None:
                line += f" ⏱{latency_str}"
            if last_seen:
                line += f" last={last_seen}"
            lines.append(_trim(line, 120))
            count += 1
            if count >= 8:
                break
        lines.append("—" * 22)

    running_jobs = snapshot.get("running_jobs", [])
    if running_jobs:
        lines.append("▶ Running")
        device_names = {d["id"]: d["name"] for d in devices if d.get("id")}
        now_ts = datetime.now(timezone.utc)
        for idx, row in enumerate(running_jobs[:6], start=1):
            payload = _parse_json(row.get("payload"))
            model = payload.get("model") or payload.get("model_id") or "-"
            provider = payload.get("provider") or ""
            task_type = payload.get("task_type") or ""
            device_id = payload.get("device_id") or ""
            device_name = device_names.get(device_id, device_id or "cloud")
            updated_at = row.get("updated_at") or row.get("queued_at")
            age = ""
            if updated_at:
                if updated_at.tzinfo is None:
                    updated_at = updated_at.replace(tzinfo=timezone.utc)
                age = _format_duration((now_ts - updated_at).total_seconds())
            kind = row.get("kind") or ""
            if "benchmark" in kind:
                tag = "🏁"
            elif "embed" in kind:
                tag = "🧩"
            elif "generate" in kind or "chat" in kind:
                tag = "💬"
            else:
                tag = "⚡"
            if len(device_name) > 12:
                device_name = device_name[:9] + "..."
            short_model = model if len(model) <= 14 else model[:11] + "..."
            line = f"{tag} {short_model} @ {device_name} {age}"
            lines.append(_trim(line, 160))
    else:
        lines.append("▶ Running: —")

    text = "\n".join(lines)
    return _trim(text, max_len)


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

    update_interval = _env_float("TELEGRAM_UPDATE_INTERVAL", 2.0)
    running_limit = _env_int("TELEMETRY_RUNNING_LIMIT", 6)
    device_limit = _env_int("TELEMETRY_DEVICE_LIMIT", 20)
    max_len = _env_int("TELEMETRY_MAX_LEN", 3800)
    spinner = ["|", "/", "-", "\\"]
    spin_idx = 0

    mcp_client, direct_client = tg_cfg.build_clients()
    if tg_cfg.use_mcp and mcp_client is None and not (tg_cfg.fallback_direct and direct_client is not None):
        log.error("TELEGRAM_USE_MCP=1 but MCP client is unavailable and no direct fallback configured")
        return

    conn = None
    last_text = ""
    last_send = 0.0

    while True:
        if conn is None or conn.closed:
            try:
                conn = psycopg.connect(dsn, connect_timeout=5)
                conn.autocommit = True
                log.info("db connected")
            except Exception as exc:
                log.warning("db connect failed err=%s", exc)
                time.sleep(2)
                continue

        try:
            snapshot = _fetch_snapshot(conn, running_limit, device_limit)
            spin = spinner[spin_idx % len(spinner)]
            spin_idx += 1
            text = _format_snapshot(snapshot, spin, max_len)
            now = time.time()
            should_send = False
            if text != last_text:
                should_send = True
            elif now - last_send >= update_interval:
                should_send = True
            if should_send:
                sent = False
                route = "none"

                if mcp_client is not None:
                    try:
                        sent = mcp_client.send_or_edit(text)
                        route = "mcp"
                    except Exception as exc:
                        log.warning("telegram mcp send failed err=%s", exc)
                        sent = False

                if not sent and tg_cfg.fallback_direct and direct_client is not None:
                    sent = direct_client.send_or_edit(text)
                    route = "direct-fallback"

                if sent:
                    last_text = text
                    last_send = now
                    log.info(
                        "tick route=%s queued=%s running=%s devices=%s",
                        route,
                        snapshot.get("jobs", {}).get("queued", 0),
                        snapshot.get("jobs", {}).get("running", 0),
                        len(snapshot.get("devices", [])),
                    )
                else:
                    log.warning("telegram send skipped (all routes failed)")
        except Exception as exc:
            log.warning("telemetry loop error err=%s", exc)
            time.sleep(1)
        time.sleep(update_interval)


if __name__ == "__main__":
    main()
