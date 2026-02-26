"""llm — MCP сервер для LLM-MCP (Node.js proxy к llmcore).

LLM routing, job management, device discovery.
Проксирует HTTP endpoints llmmcp (порт 3333).
"""

import os
import logging
from typing import Optional

import httpx
from fastmcp import FastMCP

BACKEND_URL = os.environ.get("BACKEND_URL", "http://localhost:30333")

logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
log = logging.getLogger("llm")

mcp = FastMCP("llm")
_client: httpx.AsyncClient | None = None


async def client() -> httpx.AsyncClient:
    global _client
    if _client is None:
        _client = httpx.AsyncClient(base_url=BACKEND_URL, timeout=120.0)
    return _client


async def _get(path: str, params: dict | None = None) -> str:
    c = await client()
    resp = await c.get(path, params=params)
    resp.raise_for_status()
    return resp.text


async def _post(path: str, json: dict | None = None) -> str:
    c = await client()
    resp = await c.post(path, json=json or {})
    resp.raise_for_status()
    return resp.text


# === Dashboard ===

@mcp.tool()
async def llm_dashboard() -> str:
    """Полная панель LLM: устройства, модели, задачи, расходы, хосты."""
    return await _get("/dashboard")


# === Jobs ===

@mcp.tool()
async def llm_submit(model: str, prompt: str, system: Optional[str] = None,
                     temperature: float = 0.7, max_tokens: int = 2048,
                     device: Optional[str] = None) -> str:
    """Отправить LLM задачу на выполнение. Возвращает job_id."""
    data: dict = {
        "model": model,
        "prompt": prompt,
        "temperature": temperature,
        "max_tokens": max_tokens,
    }
    if system:
        data["system"] = system
    if device:
        data["device"] = device
    return await _post("/submit", data)


@mcp.tool()
async def llm_job_status(job_id: str) -> str:
    """Получить статус и результат LLM задачи."""
    return await _get(f"/jobs/{job_id}")


# === LLM Routing ===

@mcp.tool()
async def llm_request(model: str, prompt: str, system: Optional[str] = None,
                      temperature: float = 0.7, max_tokens: int = 2048) -> str:
    """Синхронный LLM запрос через роутинг (выбирает оптимальное устройство)."""
    data: dict = {
        "model": model,
        "prompt": prompt,
        "temperature": temperature,
        "max_tokens": max_tokens,
    }
    if system:
        data["system"] = system
    return await _post("/llm/request", data)


# === Costs ===

@mcp.tool()
async def llm_costs(period: str = "day") -> str:
    """Сводка расходов на LLM по провайдерам."""
    return await _get("/costs/summary", {"period": period})


# === Benchmarks ===

@mcp.tool()
async def llm_benchmarks() -> str:
    """Результаты бенчмарков моделей на устройствах."""
    return await _get("/benchmarks")


log.info("LLM FastMCP server ready")

if __name__ == "__main__":
    mcp.run()
