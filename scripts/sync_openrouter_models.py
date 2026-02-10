#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import sys
from dataclasses import dataclass
from datetime import UTC, datetime
from decimal import Decimal, InvalidOperation
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


def _parse_decimal(raw: Any) -> Decimal | None:
    if raw is None:
        return None
    text = str(raw).strip()
    if not text:
        return None
    try:
        return Decimal(text)
    except InvalidOperation:
        return None


def _to_per_million(raw: Any) -> Decimal | None:
    value = _parse_decimal(raw)
    if value is None:
        return None
    return value * Decimal(1_000_000)


def _null_if_empty(text: str | None) -> str | None:
    if text is None:
        return None
    cleaned = text.strip()
    return cleaned if cleaned else None


@dataclass
class CuratedConfig:
    version: str
    source: str
    model_ids: list[str]


def load_curated_config(path: Path) -> CuratedConfig:
    raw = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    model_ids: list[str] = []
    for item in raw.get("models", []):
        if not isinstance(item, dict):
            continue
        model_id = str(item.get("id") or "").strip()
        if model_id:
            model_ids.append(model_id)
    dedup = list(dict.fromkeys(model_ids))
    if not dedup:
        raise RuntimeError("curated model list is empty")
    return CuratedConfig(
        version=str(raw.get("version") or ""),
        source=str(raw.get("source") or "openrouter"),
        model_ids=dedup,
    )


def fetch_openrouter_models(api_url: str, api_key: str, timeout_sec: float) -> dict[str, dict[str, Any]]:
    req = Request(
        api_url,
        headers={
            "Authorization": f"Bearer {api_key}",
            "Accept": "application/json",
        },
        method="GET",
    )
    try:
        with urlopen(req, timeout=timeout_sec) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
    except HTTPError as exc:
        raise RuntimeError(f"openrouter http error: status={exc.code}") from exc
    except URLError as exc:
        raise RuntimeError(f"openrouter url error: {exc}") from exc

    items = payload.get("data")
    if not isinstance(items, list):
        raise RuntimeError("openrouter response missing `data` list")

    out: dict[str, dict[str, Any]] = {}
    for row in items:
        if not isinstance(row, dict):
            continue
        model_id = str(row.get("id") or "").strip()
        if model_id:
            out[model_id] = row
    return out


def build_model_meta(row: dict[str, Any], source_url: str) -> dict[str, Any]:
    meta = {
        "source": "openrouter",
        "source_url": source_url,
        "fetched_at": _ts_now(),
        "openrouter_id": row.get("id"),
        "name": row.get("name"),
        "description": row.get("description"),
        "architecture": row.get("architecture"),
        "top_provider": row.get("top_provider"),
        "supported_parameters": row.get("supported_parameters"),
        "pricing_raw": row.get("pricing"),
        "context_length": row.get("context_length"),
        "params_b_unknown": True,
        "size_gb_unknown": True,
    }
    return meta


def upsert_models_and_pricing(
    conn: Any,
    curated_ids: list[str],
    source_rows: dict[str, dict[str, Any]],
    dry_run: bool = False,
) -> list[dict[str, Any]]:
    synced: list[dict[str, Any]] = []
    sql_models = """
        INSERT INTO models (id, provider, family, kind, params_b, context_k, size_gb, quant, meta, updated_at)
        VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s::jsonb, now())
        ON CONFLICT (id) DO UPDATE SET
          provider=excluded.provider,
          family=excluded.family,
          kind=excluded.kind,
          params_b=excluded.params_b,
          context_k=excluded.context_k,
          size_gb=excluded.size_gb,
          quant=excluded.quant,
          meta=excluded.meta,
          updated_at=now()
    """
    sql_pricing = """
        INSERT INTO model_pricing (model_id, price_in_1m, price_out_1m, currency, updated_at)
        VALUES (%s, %s, %s, %s, now())
        ON CONFLICT (model_id) DO UPDATE SET
          price_in_1m=excluded.price_in_1m,
          price_out_1m=excluded.price_out_1m,
          currency=excluded.currency,
          updated_at=now()
    """

    with conn.cursor() as cur:
        for model_id in curated_ids:
            row = source_rows[model_id]
            family = model_id.split("/", 1)[0].strip() if "/" in model_id else "openrouter"
            context_length = row.get("context_length")
            context_k = None
            if isinstance(context_length, int) and context_length > 0:
                context_k = context_length // 1024

            pricing_raw = row.get("pricing") if isinstance(row.get("pricing"), dict) else {}
            price_in_1m = _to_per_million(pricing_raw.get("prompt"))
            price_out_1m = _to_per_million(pricing_raw.get("completion"))
            meta = build_model_meta(row, source_url="https://openrouter.ai/api/v1/models")

            synced.append(
                {
                    "model_id": model_id,
                    "provider": "openrouter",
                    "family": family,
                    "kind": "chat",
                    "context_k": context_k,
                    "price_in_1m": str(price_in_1m) if price_in_1m is not None else None,
                    "price_out_1m": str(price_out_1m) if price_out_1m is not None else None,
                }
            )

            if dry_run:
                continue

            cur.execute(
                sql_models,
                (
                    model_id,
                    "openrouter",
                    _null_if_empty(family),
                    "chat",
                    None,
                    context_k,
                    None,
                    None,
                    json.dumps(meta, ensure_ascii=False),
                ),
            )
            cur.execute(
                sql_pricing,
                (
                    model_id,
                    price_in_1m,
                    price_out_1m,
                    "USD",
                ),
            )
    return synced


def write_snapshot(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Sync curated OpenRouter models into llm-mcp catalog.")
    parser.add_argument(
        "--config",
        default="config/curated_openrouter_models.yaml",
        help="Path to curated models YAML.",
    )
    parser.add_argument(
        "--api-url",
        default="https://openrouter.ai/api/v1/models",
        help="OpenRouter models endpoint.",
    )
    parser.add_argument(
        "--db-dsn",
        default=(
            os.getenv("LLM_DB_DSN")
            or os.getenv("DB_DSN")
            or "postgres://llm:change_me@127.0.0.1:5435/llm_mcp?sslmode=disable"
        ),
        help="Postgres DSN for llm-mcp DB.",
    )
    parser.add_argument(
        "--artifacts-dir",
        default="artifacts/catalog",
        help="Directory for JSON snapshots.",
    )
    parser.add_argument("--timeout-sec", type=float, default=30.0)
    parser.add_argument("--dry-run", action="store_true")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    api_key = os.getenv("OPENROUTER_API_KEY", "").strip()
    if not api_key:
        print("OPENROUTER_API_KEY is required", file=sys.stderr)
        return 2

    config_path = Path(args.config).resolve()
    if not config_path.exists():
        print(f"config not found: {config_path}", file=sys.stderr)
        return 2

    curated = load_curated_config(config_path)
    rows = fetch_openrouter_models(args.api_url, api_key, timeout_sec=args.timeout_sec)

    missing = [mid for mid in curated.model_ids if mid not in rows]
    if missing:
        print(f"missing curated models in OpenRouter catalog: {missing}", file=sys.stderr)
        return 3

    driver_name, driver_module = _load_db_driver()
    conn = _connect_db(driver_name, driver_module, args.db_dsn)
    try:
        synced = upsert_models_and_pricing(conn, curated.model_ids, rows, dry_run=args.dry_run)
        if args.dry_run:
            conn.rollback()
        else:
            conn.commit()
    finally:
        conn.close()

    ts = datetime.now(tz=UTC).strftime("%Y%m%d_%H%M%S")
    snapshot = {
        "synced_at": _ts_now(),
        "catalog_source": args.api_url,
        "curated_config": str(config_path),
        "curated_version": curated.version,
        "driver": driver_name,
        "dry_run": bool(args.dry_run),
        "models": synced,
    }
    snapshot_path = Path(args.artifacts_dir) / f"openrouter_sync_{ts}.json"
    write_snapshot(snapshot_path, snapshot)

    print(json.dumps({"ok": True, "synced": len(synced), "snapshot": str(snapshot_path)}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
