#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import sys
import time
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

import yaml


def _load_db_driver() -> tuple[str, Any]:
    try:
        import psycopg  # type: ignore

        return "psycopg", psycopg
    except Exception:
        try:
            import psycopg2  # type: ignore

            return "psycopg2", psycopg2
        except Exception as exc:
            raise RuntimeError(
                "postgres driver is missing; install `psycopg[binary]` or `psycopg2-binary`"
            ) from exc


def _connect_db(driver: str, module: Any, dsn: str) -> Any:
    if driver == "psycopg":
        return module.connect(dsn)
    conn = module.connect(dsn)
    conn.autocommit = False
    return conn


def _ts_now() -> str:
    return datetime.now(tz=UTC).isoformat()


def _post_json(url: str, payload: dict[str, Any], timeout_sec: float) -> dict[str, Any]:
    req = Request(
        url,
        headers={"Content-Type": "application/json", "Accept": "application/json"},
        data=json.dumps(payload).encode("utf-8"),
        method="POST",
    )
    try:
        with urlopen(req, timeout=timeout_sec) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except HTTPError as exc:
        body = exc.read().decode("utf-8", errors="ignore")
        raise RuntimeError(f"POST {url} failed: status={exc.code} body={body[:300]}") from exc
    except URLError as exc:
        raise RuntimeError(f"POST {url} failed: {exc}") from exc


def _get_json(url: str, timeout_sec: float) -> dict[str, Any]:
    req = Request(url, headers={"Accept": "application/json"}, method="GET")
    try:
        with urlopen(req, timeout=timeout_sec) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except HTTPError as exc:
        body = exc.read().decode("utf-8", errors="ignore")
        raise RuntimeError(f"GET {url} failed: status={exc.code} body={body[:300]}") from exc
    except URLError as exc:
        raise RuntimeError(f"GET {url} failed: {exc}") from exc


def _extract_output_text(result: dict[str, Any]) -> str:
    data = result.get("data")
    if not isinstance(data, dict):
        return ""
    choices = data.get("choices")
    if isinstance(choices, list) and choices:
        first = choices[0] if isinstance(choices[0], dict) else {}
        msg = first.get("message") if isinstance(first, dict) else {}
        if isinstance(msg, dict):
            content = msg.get("content")
            if isinstance(content, str):
                return content
    content = data.get("text")
    if isinstance(content, str):
        return content
    return ""


def _extract_usage(result: dict[str, Any], prompt: str) -> tuple[int, int]:
    data = result.get("data")
    if not isinstance(data, dict):
        return max(1, len(prompt) // 4), 0

    usage = data.get("usage")
    if isinstance(usage, dict):
        prompt_tokens = usage.get("prompt_tokens") or usage.get("input_tokens")
        completion_tokens = usage.get("completion_tokens") or usage.get("output_tokens")
        try:
            if prompt_tokens is not None and completion_tokens is not None:
                return int(prompt_tokens), int(completion_tokens)
        except Exception:
            pass

    text = _extract_output_text(result)
    return max(1, len(prompt) // 4), max(0, len(text) // 4)


def _percentile(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    if len(values) == 1:
        return float(values[0])
    ordered = sorted(values)
    idx = (len(ordered) - 1) * p
    low = int(idx)
    high = min(low + 1, len(ordered) - 1)
    frac = idx - low
    return ordered[low] * (1 - frac) + ordered[high] * frac


@dataclass
class ProbeRun:
    model_id: str
    run_no: int
    job_id: str
    status: str
    latency_ms: int
    tokens_in: int
    tokens_out: int
    tps: float
    error: str


def _load_models(path: Path) -> list[str]:
    raw = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    out: list[str] = []
    for item in raw.get("models", []):
        if isinstance(item, dict):
            model_id = str(item.get("id") or "").strip()
            if model_id:
                out.append(model_id)
    out = list(dict.fromkeys(out))
    if not out:
        raise RuntimeError("no models in curated config")
    return out


def _wait_job(base_url: str, job_id: str, timeout_sec: float, poll_sec: float) -> dict[str, Any]:
    deadline = time.time() + timeout_sec
    last: dict[str, Any] = {}
    while time.time() < deadline:
        last = _get_json(f"{base_url}/v1/jobs/{job_id}", timeout_sec=max(10.0, poll_sec + 5))
        status = str(last.get("status") or "")
        if status in {"done", "error"}:
            return last
        time.sleep(poll_sec)
    raise TimeoutError(f"job timeout: {job_id}")


def _ensure_probe_device(cur: Any) -> None:
    cur.execute(
        """
        INSERT INTO devices (id, name, platform, arch, host, tags, status, last_seen, updated_at)
        VALUES (%s, %s, %s, %s, %s, %s::jsonb, %s, now(), now())
        ON CONFLICT (id) DO UPDATE SET
          name=excluded.name,
          platform=excluded.platform,
          arch=excluded.arch,
          host=excluded.host,
          tags=excluded.tags,
          status='online',
          last_seen=now(),
          updated_at=now()
        """,
        (
            "cloud-openrouter",
            "Cloud OpenRouter",
            "cloud",
            "openrouter",
            "openrouter",
            json.dumps({"cloud": True, "provider": "openrouter", "synthetic_probe": True}),
            "online",
        ),
    )


def _insert_benchmark(cur: Any, run: ProbeRun, prompt: str, payload_result: dict[str, Any]) -> None:
    meta = {
        "synthetic": True,
        "provider": "openrouter",
        "probe_prompt_len": len(prompt),
        "probe_timestamp": _ts_now(),
        "job_id": run.job_id,
        "status": run.status,
        "error": run.error,
        "raw_result": payload_result,
    }
    cur.execute(
        """
        INSERT INTO benchmarks (device_id, model_id, task_type, tokens_in, tokens_out, latency_ms, tps, meta, ok)
        VALUES (%s, %s, %s, %s, %s, %s, %s, %s::jsonb, %s)
        """,
        (
            "cloud-openrouter",
            run.model_id,
            "chat.synthetic",
            run.tokens_in,
            run.tokens_out,
            run.latency_ms,
            run.tps,
            json.dumps(meta, ensure_ascii=False),
            run.status == "done",
        ),
    )


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Run synthetic benchmark probes for curated OpenRouter models.")
    parser.add_argument("--config", default="config/curated_openrouter_models.yaml")
    parser.add_argument("--base-url", default=os.getenv("LLM_CORE_URL", "http://127.0.0.1:8080").rstrip("/"))
    parser.add_argument(
        "--db-dsn",
        default=(
            os.getenv("LLM_DB_DSN")
            or os.getenv("DB_DSN")
            or "postgres://llm:change_me@127.0.0.1:5435/llm_mcp?sslmode=disable"
        ),
    )
    parser.add_argument("--runs-per-model", type=int, default=3)
    parser.add_argument("--max-tokens", type=int, default=64)
    parser.add_argument("--poll-sec", type=float, default=1.2)
    parser.add_argument("--job-timeout-sec", type=float, default=180.0)
    parser.add_argument("--http-timeout-sec", type=float, default=20.0)
    parser.add_argument("--artifacts-dir", default="artifacts/catalog")
    parser.add_argument("--dry-run", action="store_true")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    config_path = Path(args.config).resolve()
    if not config_path.exists():
        print(f"config not found: {config_path}", file=sys.stderr)
        return 2

    if args.runs_per_model <= 0:
        print("--runs-per-model must be > 0", file=sys.stderr)
        return 2

    model_ids = _load_models(config_path)
    prompt = (
        "Return compact JSON with keys summary, confidence, and one factual check. "
        "Keep answer under 80 tokens."
    )

    runs: list[ProbeRun] = []
    run_details: list[dict[str, Any]] = []

    for model_id in model_ids:
        for run_no in range(1, args.runs_per_model + 1):
            started = time.perf_counter()
            error_text = ""
            tokens_in = 0
            tokens_out = 0
            status = "error"
            job_id = ""
            result_payload: dict[str, Any] = {}
            try:
                body = {
                    "task": "chat",
                    "provider": "openrouter",
                    "model": model_id,
                    "messages": [{"role": "user", "content": prompt}],
                    "temperature": 0.1,
                    "max_tokens": args.max_tokens,
                    "source": "probe_openrouter_models",
                    "constraints": {
                        "prefer_local": False,
                        "force_cloud": True,
                        "max_latency_ms": int(args.job_timeout_sec * 1000),
                    },
                }
                enq = _post_json(f"{args.base_url}/v1/llm/request", body, timeout_sec=args.http_timeout_sec)
                job_id = str(enq.get("job_id") or "").strip()
                if not job_id:
                    raise RuntimeError("job_id missing from /v1/llm/request")

                job = _wait_job(args.base_url, job_id, timeout_sec=args.job_timeout_sec, poll_sec=args.poll_sec)
                status = str(job.get("status") or "error")
                result_payload = job.get("result") if isinstance(job.get("result"), dict) else {}

                if status != "done":
                    error_text = str(job.get("error") or "job_failed")
                else:
                    tokens_in, tokens_out = _extract_usage(result_payload, prompt)
            except Exception as exc:
                error_text = str(exc)
                status = "error"

            latency_ms = int((time.perf_counter() - started) * 1000)
            tps = 0.0
            if latency_ms > 0 and tokens_out > 0:
                tps = tokens_out / (latency_ms / 1000.0)

            run = ProbeRun(
                model_id=model_id,
                run_no=run_no,
                job_id=job_id,
                status=status,
                latency_ms=latency_ms,
                tokens_in=tokens_in,
                tokens_out=tokens_out,
                tps=tps,
                error=error_text,
            )
            runs.append(run)
            run_details.append(
                {
                    "model_id": model_id,
                    "run_no": run_no,
                    "job_id": job_id,
                    "status": status,
                    "latency_ms": latency_ms,
                    "tokens_in": tokens_in,
                    "tokens_out": tokens_out,
                    "tps": tps,
                    "error": error_text,
                }
            )

            if args.dry_run:
                continue

            driver_name, driver_module = _load_db_driver()
            conn = _connect_db(driver_name, driver_module, args.db_dsn)
            try:
                with conn.cursor() as cur:
                    _ensure_probe_device(cur)
                    _insert_benchmark(cur, run, prompt=prompt, payload_result=result_payload)
                conn.commit()
            finally:
                conn.close()

            print(
                json.dumps(
                    {
                        "model": model_id,
                        "run_no": run_no,
                        "status": status,
                        "latency_ms": latency_ms,
                        "tokens_out": tokens_out,
                        "error": error_text,
                    },
                    ensure_ascii=False,
                )
            )

    grouped: dict[str, list[ProbeRun]] = {}
    for row in runs:
        grouped.setdefault(row.model_id, []).append(row)

    summary_models: list[dict[str, Any]] = []
    has_failure = False
    for model_id in model_ids:
        rows = grouped.get(model_id, [])
        done_rows = [r for r in rows if r.status == "done"]
        latencies = [float(r.latency_ms) for r in done_rows]
        tps_values = [float(r.tps) for r in done_rows if r.tps > 0]
        summary = {
            "model_id": model_id,
            "runs": len(rows),
            "done_runs": len(done_rows),
            "error_runs": len(rows) - len(done_rows),
            "latency_ms_p50": round(_percentile(latencies, 0.5), 3) if latencies else 0.0,
            "latency_ms_p95": round(_percentile(latencies, 0.95), 3) if latencies else 0.0,
            "tps_p50": round(_percentile(tps_values, 0.5), 5) if tps_values else 0.0,
            "tps_p95": round(_percentile(tps_values, 0.95), 5) if tps_values else 0.0,
        }
        if len(done_rows) < args.runs_per_model:
            has_failure = True
        summary_models.append(summary)

    snapshot = {
        "probed_at": _ts_now(),
        "base_url": args.base_url,
        "config": str(config_path),
        "runs_per_model": args.runs_per_model,
        "dry_run": bool(args.dry_run),
        "models": summary_models,
        "runs": run_details,
    }
    ts = datetime.now(tz=UTC).strftime("%Y%m%d_%H%M%S")
    out_path = Path(args.artifacts_dir) / f"openrouter_probe_{ts}.json"
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(snapshot, ensure_ascii=False, indent=2), encoding="utf-8")

    print(json.dumps({"ok": not has_failure, "snapshot": str(out_path), "models": summary_models}, ensure_ascii=False))
    return 1 if has_failure else 0


if __name__ == "__main__":
    raise SystemExit(main())
