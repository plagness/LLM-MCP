from __future__ import annotations

import asyncio
import json
import logging
import time
from dataclasses import dataclass
from urllib import error as url_error
from urllib import parse as url_parse
from urllib import request as url_request

try:
    from telegram_api_client import TelegramAPI
except Exception:  # pragma: no cover - optional in some environments
    TelegramAPI = None  # type: ignore[assignment]


log = logging.getLogger("llm-telemetry")


def _env_bool(name: str, default: bool) -> bool:
    raw = __import__("os").getenv(name, "").strip().lower()
    if not raw:
        return default
    return raw in {"1", "true", "yes", "y", "on"}


def _env_int(name: str) -> int | None:
    raw = __import__("os").getenv(name, "").strip()
    if not raw:
        return None
    try:
        return int(raw)
    except ValueError:
        return None


def _escape_html(text: str) -> str:
    return (
        text.replace("&", "&amp;")
        .replace("<", "&lt;")
        .replace(">", "&gt;")
    )


class DirectTelegramClient:
    def __init__(self, token: str, chat_id: str):
        self.token = token
        self.chat_id = chat_id
        self.message_id: int | None = None

    def _call(self, method: str, payload: dict) -> dict | None:
        url = f"https://api.telegram.org/bot{self.token}/{method}"
        data = url_parse.urlencode(payload).encode("utf-8")
        req = url_request.Request(url, data=data, method="POST")
        body = None
        try:
            with url_request.urlopen(req, timeout=10) as resp:
                body = resp.read().decode("utf-8")
        except url_error.HTTPError as exc:
            body = exc.read().decode("utf-8") if exc.fp else ""
        except Exception as exc:
            log.warning("telegram direct call failed method=%s err=%s", method, exc)
            return None

        try:
            data = json.loads(body) if body else {}
        except Exception:
            log.warning("telegram direct bad response method=%s body=%s", method, body)
            return None
        if not data.get("ok"):
            params = data.get("parameters") or {}
            retry_after = params.get("retry_after")
            if retry_after:
                log.warning("telegram direct rate limit retry_after=%s", retry_after)
                time.sleep(float(retry_after))
                return None
            desc = data.get("description")
            if desc and "message is not modified" in desc.lower():
                return data
            log.warning("telegram direct error method=%s resp=%s", method, data)
            return None
        return data

    def send_or_edit(self, text: str) -> bool:
        safe = _escape_html(text)
        safe = f"<pre>{safe}</pre>"
        payload = {
            "chat_id": self.chat_id,
            "text": safe,
            "parse_mode": "HTML",
            "disable_web_page_preview": True,
        }
        if self.message_id is None:
            resp = self._call("sendMessage", payload)
            if resp and resp.get("result"):
                self.message_id = resp["result"].get("message_id")
                return True
            return False
        payload["message_id"] = self.message_id
        return self._call("editMessageText", payload) is not None


class MCPTelegramClient:
    def __init__(self, base_url: str, chat_id: str, bot_id: int | None):
        if TelegramAPI is None:
            raise RuntimeError("telegram_api_client is not available")
        self.api = TelegramAPI(base_url)
        self._loop = asyncio.new_event_loop()
        self.chat_id = chat_id
        self.bot_id = bot_id
        self.message_id: int | None = None  # internal message ID from telegram-mcp

    async def _send_or_edit_async(self, text: str) -> bool:
        safe = _escape_html(text)
        safe = f"<pre>{safe}</pre>"
        if self.message_id is None:
            if self.bot_id is None:
                msg = await self.api.send_message(
                    chat_id=self.chat_id,
                    text=safe,
                    parse_mode="HTML",
                    disable_web_page_preview=True,
                )
            else:
                try:
                    msg = await self.api.send_message(
                        chat_id=self.chat_id,
                        bot_id=self.bot_id,
                        text=safe,
                        parse_mode="HTML",
                        disable_web_page_preview=True,
                    )
                except TypeError:
                    msg = await self.api.send_message(
                        chat_id=self.chat_id,
                        text=safe,
                        parse_mode="HTML",
                        disable_web_page_preview=True,
                    )
            internal_id = msg.get("id")
            if internal_id is not None:
                self.message_id = int(internal_id)
                return True
            return False

        if self.bot_id is None:
            await self.api.edit_message(
                self.message_id,
                text=safe,
                parse_mode="HTML",
            )
        else:
            try:
                await self.api.edit_message(
                    self.message_id,
                    bot_id=self.bot_id,
                    text=safe,
                    parse_mode="HTML",
                )
            except TypeError:
                await self.api.edit_message(
                    self.message_id,
                    text=safe,
                    parse_mode="HTML",
                )
        return True

    def send_or_edit(self, text: str) -> bool:
        return self._loop.run_until_complete(self._send_or_edit_async(text))


@dataclass
class TelegramGateway:
    use_mcp: bool
    chat_id: str
    mcp_base_url: str
    mcp_bot_id: int | None
    fallback_direct: bool
    direct_token: str

    @classmethod
    def from_env(cls) -> TelegramGateway:
        import os

        use_mcp = _env_bool("TELEGRAM_USE_MCP", False)
        chat_id = (
            os.getenv("TELEGRAM_MCP_CHAT_ID", "").strip()
            or os.getenv("TELEGRAM_CHAT_ID", "").strip()
            or os.getenv("REPORT_CHAT_ID", "").strip()
        )
        return cls(
            use_mcp=use_mcp,
            chat_id=chat_id,
            mcp_base_url=os.getenv("TELEGRAM_MCP_BASE_URL", "http://telegram-api:8000").strip(),
            mcp_bot_id=_env_int("TELEGRAM_MCP_BOT_ID"),
            fallback_direct=_env_bool("TELEGRAM_MCP_FALLBACK_DIRECT", True),
            direct_token=os.getenv("TELEGRAM_BOT_TOKEN", "").strip(),
        )

    def validate(self) -> tuple[bool, str]:
        if not self.chat_id:
            return False, "TELEGRAM_CHAT_ID or TELEGRAM_MCP_CHAT_ID is required"
        if self.use_mcp:
            return True, ""
        if not self.direct_token:
            return False, "TELEGRAM_BOT_TOKEN is required when TELEGRAM_USE_MCP=0"
        return True, ""

    def build_clients(self) -> tuple[MCPTelegramClient | None, DirectTelegramClient | None]:
        mcp_client: MCPTelegramClient | None = None
        direct_client: DirectTelegramClient | None = None

        if self.use_mcp:
            try:
                mcp_client = MCPTelegramClient(self.mcp_base_url, self.chat_id, self.mcp_bot_id)
            except Exception as exc:
                log.warning("telegram mcp client init failed err=%s", exc)

        if self.direct_token:
            direct_client = DirectTelegramClient(self.direct_token, self.chat_id)

        return mcp_client, direct_client
