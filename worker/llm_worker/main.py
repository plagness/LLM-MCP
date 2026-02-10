import json
import logging
import os
import platform
import socket
import time
import urllib.request
import urllib.error
import threading

import grpc
import importlib.util
import sys
from pathlib import Path


def _load_pb():
    pb_dir = Path(__file__).parent / "pb"
    pb_path = pb_dir / "llm_pb2.py"
    pb_grpc_path = pb_dir / "llm_pb2_grpc.py"
    spec = importlib.util.spec_from_file_location("llm_pb2", pb_path)
    if spec is None or spec.loader is None:
        raise RuntimeError("pb spec not found")
    module = importlib.util.module_from_spec(spec)
    sys.modules["llm_pb2"] = module
    spec.loader.exec_module(module)

    spec_grpc = importlib.util.spec_from_file_location("llm_pb2_grpc", pb_grpc_path)
    if spec_grpc is None or spec_grpc.loader is None:
        raise RuntimeError("pb grpc spec not found")
    module_grpc = importlib.util.module_from_spec(spec_grpc)
    sys.modules["llm_pb2_grpc"] = module_grpc
    spec_grpc.loader.exec_module(module_grpc)
    return module, module_grpc


logging.basicConfig(level=logging.INFO)
log = logging.getLogger("llm-worker")

llm_pb2, llm_pb2_grpc = _load_pb()
_bench_stub = None
_bench_worker_id = ""


def _build_stub(addr: str):
    channel = grpc.insecure_channel(addr)
    stub = llm_pb2_grpc.CoreStub(channel)
    return channel, stub


def _register_worker(stub) -> str:
    worker_id = os.getenv("WORKER_ID", "").strip()
    host = socket.gethostname()
    tags = {}
    if os.getenv("OLLAMA_BASE_URL", "").strip():
        tags["ollama"] = True
    req = llm_pb2.RegisterWorkerRequest(
        worker=llm_pb2.WorkerInfo(
            id=worker_id,
            name=os.getenv("WORKER_NAME", "worker"),
            platform=platform.system(),
            arch=platform.machine(),
            host=host,
            tags_json=json.dumps(tags, ensure_ascii=False),
        )
    )
    resp = stub.RegisterWorker(req)
    if not resp.worker_id:
        raise RuntimeError("register failed: missing worker_id")
    return resp.worker_id


def _claim_job(stub, worker_id: str, kinds: list[str], lease_seconds: int):
    req = llm_pb2.ClaimJobRequest(
        worker_id=worker_id,
        kinds=kinds,
        lease_seconds=lease_seconds,
    )
    resp = stub.ClaimJob(req)
    return resp.job if resp.job and resp.job.id else None


def _complete_job(stub, worker_id: str, job_id: str, result: dict, metrics: dict | None = None) -> None:
    req = llm_pb2.CompleteJobRequest(
        worker_id=worker_id,
        job_id=job_id,
        result_json=json.dumps(result, ensure_ascii=False),
        metrics_json=json.dumps(metrics or {}, ensure_ascii=False),
    )
    stub.CompleteJob(req)


def _fail_job(stub, worker_id: str, job_id: str, error: str, metrics: dict | None = None) -> None:
    req = llm_pb2.FailJobRequest(
        worker_id=worker_id,
        job_id=job_id,
        error=error,
        metrics_json=json.dumps(metrics or {}, ensure_ascii=False),
    )
    stub.FailJob(req)


def _parse_payload(raw: str) -> dict:
    if not raw:
        return {}
    try:
        return json.loads(raw)
    except Exception:
        return {}


def _post_json(url: str, payload: dict, headers: dict | None = None, timeout: int = 120, retries: int = 3):
    """HTTP POST с exponential backoff retry для transient ошибок."""
    data = json.dumps(payload).encode("utf-8")
    last_exc: Exception | None = None
    for attempt in range(retries):
        try:
            req = urllib.request.Request(
                url,
                data=data,
                headers={"Content-Type": "application/json", **(headers or {})},
                method="POST",
            )
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                body = resp.read().decode("utf-8")
                return resp.status, body
        except urllib.error.HTTPError as exc:
            # 4xx — не ретраим (ошибка клиента), кроме 429 (rate limit)
            if 400 <= exc.code < 500 and exc.code != 429:
                raise
            last_exc = exc
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            last_exc = exc
        if attempt < retries - 1:
            delay = min(2 ** attempt, 10)
            log.warning("_post_json retry %d/%d url=%s delay=%ds err=%s", attempt + 1, retries, url, delay, last_exc)
            time.sleep(delay)
    raise last_exc  # type: ignore[misc]


def _format_addr(addr: str) -> str:
    """Оборачивает IPv6-адрес в квадратные скобки для HTTP URL."""
    if ":" in addr and not addr.startswith("["):
        return f"[{addr}]"
    return addr


def _split_host_port(addr: str) -> tuple[str, str]:
    """Разделяет addr на (host, port). Возвращает (addr, '') если порт не указан."""
    # IPv6 с портом: [::1]:11435
    if addr.startswith("["):
        idx = addr.find("]:")
        if idx > 0:
            return addr[1:idx], addr[idx + 2:]
        return addr.strip("[]"), ""
    # IPv4 с портом: 192.168.1.10:11435 — ровно одно двоеточие + цифры
    parts = addr.rsplit(":", 1)
    if len(parts) == 2 and parts[1].isdigit() and ":" not in parts[0]:
        return parts[0], parts[1]
    return addr, ""


def _resolve_ollama_base(payload: dict) -> str:
    base = payload.get("ollama_base_url")
    if base:
        return str(base).rstrip("/")
    addr = payload.get("ollama_addr")
    if addr:
        addr = str(addr)
        if addr.startswith("http://") or addr.startswith("https://"):
            return addr.rstrip("/")
        # Проверяем, содержит ли addr порт (формат ip:port от multi-port discovery)
        host, port = _split_host_port(addr)
        if port:
            return f"http://{_format_addr(host)}:{port}"
        return f"http://{_format_addr(addr)}:11434"
    return os.getenv("OLLAMA_BASE_URL", "http://127.0.0.1:11434").rstrip("/")


def _mark_device_offline(device_id: str, reason: str) -> None:
    core_http = os.getenv("CORE_HTTP_URL", "http://127.0.0.1:8080").rstrip("/")
    url = f"{core_http}/v1/devices/offline"
    try:
        _post_json(url, {"device_id": device_id, "reason": reason}, timeout=5)
    except Exception as exc:
        log.warning("device offline report failed id=%s err=%s", device_id, exc)


def _should_mark_offline(exc: Exception) -> bool:
    if isinstance(exc, urllib.error.URLError):
        return True
    msg = str(exc).lower()
    for token in ("connection refused", "timed out", "timeout", "no route", "network is unreachable"):
        if token in msg:
            return True
    return False


def _ollama_generate(payload: dict):
    base = _resolve_ollama_base(payload)
    model = payload.get("model") or os.getenv("OLLAMA_MODEL", "llama3.2:3b")
    prompt = payload.get("prompt") or ""
    options = payload.get("options") or {}
    url = f"{base}/api/generate"
    headers = {}
    if payload.get("ollama_host"):
        headers["Host"] = str(payload.get("ollama_host"))
    req = {
        "model": model,
        "prompt": prompt,
        "options": options,
        "stream": False,
    }
    log.info("ollama.generate model=%s chars=%d", model, len(prompt))
    t0 = time.perf_counter()
    status, body = _post_json(url, req, headers=headers, timeout=120)
    ms = int((time.perf_counter() - t0) * 1000)
    data = json.loads(body) if body else {}
    log.info("ollama.generate done status=%s ms=%s", status, ms)
    return {"status": status, "ms": ms, "data": data}


def _ollama_embed(payload: dict):
    base = _resolve_ollama_base(payload)
    model = payload.get("model") or os.getenv("OLLAMA_EMBED_MODEL", "nomic-embed-text")
    text = payload.get("prompt") or ""
    url = f"{base}/api/embeddings"
    headers = {}
    if payload.get("ollama_host"):
        headers["Host"] = str(payload.get("ollama_host"))
    req = {"model": model, "prompt": text}
    log.info("ollama.embed model=%s chars=%d", model, len(text))
    t0 = time.perf_counter()
    status, body = _post_json(url, req, headers=headers, timeout=60)
    ms = int((time.perf_counter() - t0) * 1000)
    data = json.loads(body) if body else {}
    log.info("ollama.embed done status=%s ms=%s", status, ms)
    return {"status": status, "ms": ms, "data": data}


def _extract_usage(data: dict) -> dict:
    """Извлекает usage (токены) из ответа OpenAI/OpenRouter."""
    usage = data.get("usage") or {}
    return {
        "tokens_in": int(usage.get("prompt_tokens") or 0),
        "tokens_out": int(usage.get("completion_tokens") or 0),
        "total_tokens": int(usage.get("total_tokens") or 0),
    }


def _openai_chat(payload: dict):
    api_key = os.getenv("OPENAI_API_KEY", "").strip()
    if not api_key:
        raise RuntimeError("missing OPENAI_API_KEY")
    base = os.getenv("OPENAI_BASE_URL", "https://api.openai.com/v1/chat/completions").rstrip("/")
    model = payload.get("model") or os.getenv("OPENAI_MODEL", "gpt-4o-mini")
    messages = payload.get("messages") or []
    req = {
        "model": model,
        "messages": messages,
        "temperature": payload.get("temperature", 0.2),
        "max_tokens": payload.get("max_tokens", 256),
    }
    log.info("openai.chat model=%s messages=%d", model, len(messages))
    t0 = time.perf_counter()
    status, body = _post_json(
        base,
        req,
        headers={"Authorization": f"Bearer {api_key}"},
        timeout=120,
    )
    ms = int((time.perf_counter() - t0) * 1000)
    data = json.loads(body) if body else {}
    usage = _extract_usage(data)
    log.info("openai.chat done status=%s ms=%s tokens_in=%d tokens_out=%d", status, ms, usage["tokens_in"], usage["tokens_out"])
    return {"status": status, "ms": ms, "data": data, "usage": usage, "model": model, "provider": "openai"}


def _openrouter_chat(payload: dict):
    api_key = os.getenv("OPENROUTER_API_KEY", "").strip()
    if not api_key:
        raise RuntimeError("missing OPENROUTER_API_KEY")
    base = os.getenv("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1/chat/completions").rstrip("/")
    model = payload.get("model") or os.getenv("OPENROUTER_MODEL", "google/gemini-2.0-flash")
    messages = payload.get("messages") or []
    req = {
        "model": model,
        "messages": messages,
        "temperature": payload.get("temperature", 0.3),
        "max_tokens": payload.get("max_tokens", 256),
    }
    log.info("openrouter.chat model=%s messages=%d", model, len(messages))
    t0 = time.perf_counter()
    status, body = _post_json(
        base,
        req,
        headers={"Authorization": f"Bearer {api_key}"},
        timeout=120,
    )
    ms = int((time.perf_counter() - t0) * 1000)
    data = json.loads(body) if body else {}
    usage = _extract_usage(data)
    log.info("openrouter.chat done status=%s ms=%s tokens_in=%d tokens_out=%d", status, ms, usage["tokens_in"], usage["tokens_out"])
    return {"status": status, "ms": ms, "data": data, "usage": usage, "model": model, "provider": "openrouter"}


def _handle_job(kind: str, payload: dict) -> tuple[dict, dict]:
    if kind == "ollama.generate":
        resp = _ollama_generate(payload)
        return {"ok": True, "provider": "ollama", "data": resp["data"]}, {"ms": resp["ms"]}
    if kind == "ollama.embed":
        resp = _ollama_embed(payload)
        return {"ok": True, "provider": "ollama", "data": resp["data"]}, {"ms": resp["ms"]}
    if kind == "benchmark.ollama.generate":
        return _benchmark_ollama_generate(payload)
    if kind == "benchmark.ollama.embed":
        return _benchmark_ollama_embed(payload)
    if kind == "openai.chat":
        resp = _openai_chat(payload)
        usage = resp.get("usage", {})
        return {"ok": True, "provider": "openai", "data": resp["data"]}, {
            "ms": resp["ms"],
            "model": resp.get("model", ""),
            "provider": "openai",
            "tokens_in": usage.get("tokens_in", 0),
            "tokens_out": usage.get("tokens_out", 0),
        }
    if kind == "openrouter.chat":
        resp = _openrouter_chat(payload)
        usage = resp.get("usage", {})
        return {"ok": True, "provider": "openrouter", "data": resp["data"]}, {
            "ms": resp["ms"],
            "model": resp.get("model", ""),
            "provider": "openrouter",
            "tokens_in": usage.get("tokens_in", 0),
            "tokens_out": usage.get("tokens_out", 0),
        }

    return {"ok": True, "echo": payload}, {"ms": 0}


def _report_benchmark(stub, device_id: str, model_id: str, task_type: str, tokens_in: int, tokens_out: int, latency_ms: int, tps: float, meta: dict | None = None) -> None:
    if not stub or not device_id:
        log.warning("benchmark report skipped: missing stub/device_id")
        return
    req = llm_pb2.ReportBenchmarkRequest(
        benchmark=llm_pb2.Benchmark(
            device_id=device_id,
            model_id=model_id,
            task_type=task_type,
            tokens_in=tokens_in,
            tokens_out=tokens_out,
            latency_ms=latency_ms,
            tps=tps,
            meta_json=json.dumps(meta or {}, ensure_ascii=False),
        )
    )
    stub.ReportBenchmark(req)


def _bench_stats_from_ollama(data: dict, total_ms: int) -> tuple[int, int, int, float]:
    tokens_in = int(data.get("prompt_eval_count") or 0)
    tokens_out = int(data.get("eval_count") or 0)
    eval_ns = int(data.get("eval_duration") or 0)
    latency_ms = total_ms
    tps = 0.0
    if eval_ns > 0 and tokens_out > 0:
        tps = tokens_out / (eval_ns / 1_000_000_000)
    return tokens_in, tokens_out, latency_ms, tps


def _benchmark_ollama_generate(payload: dict) -> tuple[dict, dict]:
    resp = _ollama_generate(payload)
    data = resp["data"] or {}
    tokens_in, tokens_out, latency_ms, tps = _bench_stats_from_ollama(data, resp["ms"])
    model_id = payload.get("model") or os.getenv("OLLAMA_MODEL", "llama3.2:3b")
    device_id = payload.get("device_id") or _bench_worker_id
    meta = {"provider": "ollama", "raw": {k: data.get(k) for k in ["model", "total_duration", "load_duration", "prompt_eval_duration", "eval_duration"]}}
    _report_benchmark(_bench_stub, device_id, model_id, "generate", tokens_in, tokens_out, latency_ms, tps, meta)
    return {
        "ok": True,
        "provider": "ollama",
        "tokens_in": tokens_in,
        "tokens_out": tokens_out,
        "latency_ms": latency_ms,
        "tps": tps,
    }, {"ms": latency_ms}


def _benchmark_ollama_embed(payload: dict) -> tuple[dict, dict]:
    resp = _ollama_embed(payload)
    data = resp["data"] or {}
    tokens_in = int(len(payload.get("prompt", "")) / 4)
    tokens_out = 0
    latency_ms = resp["ms"]
    tps = 0.0
    model_id = payload.get("model") or os.getenv("OLLAMA_EMBED_MODEL", "nomic-embed-text")
    device_id = payload.get("device_id") or _bench_worker_id
    meta = {"provider": "ollama", "raw": {k: data.get(k) for k in ["model"]}}
    _report_benchmark(_bench_stub, device_id, model_id, "embed", tokens_in, tokens_out, latency_ms, tps, meta)
    return {
        "ok": True,
        "provider": "ollama",
        "tokens_in": tokens_in,
        "tokens_out": tokens_out,
        "latency_ms": latency_ms,
        "tps": tps,
    }, {"ms": latency_ms}


def _heartbeat_loop(stub, worker_id: str, job_id: str, extend_seconds: int, stop_event: threading.Event) -> None:
    interval = max(5, extend_seconds // 2)
    while not stop_event.wait(interval):
        try:
            req = llm_pb2.HeartbeatRequest(
                worker_id=worker_id,
                job_id=job_id,
                extend_seconds=extend_seconds,
            )
            stub.Heartbeat(req)
            log.info("job heartbeat id=%s extend=%s", job_id, extend_seconds)
        except Exception as exc:
            log.warning("heartbeat error id=%s err=%s", job_id, exc)


def main() -> None:
    grpc_addr = os.getenv("CORE_GRPC_ADDR", "127.0.0.1:9090")
    lease_seconds = int(os.getenv("WORKER_LEASE_SECONDS", "60"))
    kinds_env = os.getenv("WORKER_KINDS", "")
    kinds = [k.strip() for k in kinds_env.split(",") if k.strip()]

    log.info("worker starting; core_grpc=%s", grpc_addr)
    channel, stub = _build_stub(grpc_addr)

    worker_id = ""
    while not worker_id:
        try:
            worker_id = _register_worker(stub)
            log.info("worker registered id=%s", worker_id)
        except Exception as exc:
            log.warning("register failed: %s", exc)
            time.sleep(2)

    global _bench_stub, _bench_worker_id
    _bench_stub = stub
    _bench_worker_id = worker_id

    while True:
        job_id = None
        stop_event: threading.Event | None = None
        payload: dict = {}
        try:
            job = _claim_job(stub, worker_id, kinds, lease_seconds)
            if not job:
                time.sleep(1.5)
                continue

            job_id = job.id
            job_kind = job.kind
            payload = _parse_payload(job.payload_json)
            log.info("job claimed id=%s kind=%s", job_id, job_kind)

            stop_event = threading.Event()
            hb_thread = threading.Thread(
                target=_heartbeat_loop,
                args=(stub, worker_id, job_id, lease_seconds, stop_event),
                daemon=True,
            )
            hb_thread.start()

            result, metrics = _handle_job(job_kind, payload)
            _complete_job(stub, worker_id, job_id, result, metrics)
            log.info("job done id=%s", job_id)
            if stop_event:
                stop_event.set()
        except Exception as exc:
            if job_id:
                try:
                    _fail_job(stub, worker_id, job_id, str(exc))
                except Exception:
                    pass
            device_id = payload.get("device_id") if isinstance(payload, dict) else None
            if device_id and _should_mark_offline(exc):
                log.warning("mark device offline id=%s reason=%s", device_id, exc)
                _mark_device_offline(str(device_id), str(exc))
            if stop_event:
                stop_event.set()
            log.warning("worker loop error: %s", exc)
            time.sleep(2)


if __name__ == "__main__":
    main()
