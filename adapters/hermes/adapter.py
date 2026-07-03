"""claude-peers platform plugin for hermes-agent (NousResearch/hermes-agent).

Bridges a running Hermes gateway onto the cp3 peer network: inbound peer
messages become agent turns on a per-peer session, and the agent's replies
route back to the sending peer automatically — a peer is just a chat.

All network logic stays in the cp3 binary: inbound rides the `cp3 subscribe`
sidecar (one JSON message per line), outbound shells `cp3 send`. Zero Python
dependencies beyond the Hermes plugin API.

Install: copy this directory to ~/.hermes/plugins/claude-peers/ (or
plugins/platforms/claude-peers/ in a source checkout). Requires the cp3
binary on PATH: https://github.com/WillyV3/cp3

Status: written to the documented plugin contract (modeled line-for-line on
the stdlib IRC plugin); not yet live-tested against a running gateway.
"""

import asyncio
import json
import logging
import os
import shutil
import socket
import time
from typing import Any, Dict, List, Optional

from gateway.platforms.base import (
    BasePlatformAdapter,
    SendResult,
    MessageEvent,
    MessageType,
)
from gateway.config import Platform

logger = logging.getLogger(__name__)


def _cp3_bin() -> str:
    return os.getenv("CP3_BIN", "cp3")


def _agent_name(extra: Dict[str, Any]) -> str:
    return (
        os.getenv("CLAUDE_PEERS_AGENT")
        or extra.get("agent", "")
        or "hermes"
    )


class ClaudePeersAdapter(BasePlatformAdapter):
    """Peer network adapter: cp3 subscribe sidecar in, cp3 send out."""

    def __init__(self, config, **kwargs):
        platform = Platform("claude-peers")
        super().__init__(config=config, platform=platform)
        extra = getattr(config, "extra", {}) or {}
        self.agent = _agent_name(extra)
        self._proc: Optional[asyncio.subprocess.Process] = None
        self._reader_task: Optional[asyncio.Task] = None

    # ── Inbound ─────────────────────────────────────────────────────────

    async def connect(self, *, is_reconnect: bool = False) -> bool:
        cmd = [
            _cp3_bin(), "subscribe",
            "--agent", self.agent,
            "--machine", socket.gethostname(),
            "--cwd", os.getcwd(),
        ]
        try:
            self._proc = await asyncio.create_subprocess_exec(
                *cmd,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.DEVNULL,
            )
        except OSError as e:
            logger.error("claude-peers: cannot spawn %s: %s", cmd[0], e)
            return False
        self._reader_task = asyncio.create_task(self._read_loop())
        logger.info("claude-peers: %s online (subscriber pid %s)", self.agent, self._proc.pid)
        return True

    async def _read_loop(self) -> None:
        assert self._proc is not None and self._proc.stdout is not None
        async for raw in self._proc.stdout:
            line = raw.decode("utf-8", "replace").strip()
            if not line:
                continue
            try:
                m = json.loads(line)
            except json.JSONDecodeError:
                continue
            sender = m.get("from") or "unknown-peer"
            text = m.get("content") or ""
            if not text:
                continue
            await self._dispatch(sender, text, m.get("id") or str(int(time.time() * 1000)))
        logger.warning("claude-peers: subscriber exited")

    async def _dispatch(self, sender: str, text: str, message_id: str) -> None:
        if not self._message_handler:
            return
        source = self.build_source(
            chat_id=sender,
            chat_name=sender,
            chat_type="dm",
            user_id=sender,
            user_name=sender,
        )
        event = MessageEvent(
            text=text,
            message_type=MessageType.TEXT,
            source=source,
            message_id=message_id,
            timestamp=__import__("datetime").datetime.now(),
        )
        await self.handle_message(event)

    # ── Outbound ────────────────────────────────────────────────────────

    async def send(
        self,
        chat_id: str,
        content: str,
        reply_to: Optional[str] = None,
        metadata: Optional[Dict[str, Any]] = None,
    ):
        rc, err = await _cp3_send(self.agent, chat_id, content)
        if rc != 0:
            return SendResult(success=False, error=err or f"cp3 send exit {rc}")
        return SendResult(success=True, message_id=str(int(time.time() * 1000)))

    async def send_typing(self, chat_id: str, metadata=None) -> None:
        pass  # peers have no typing indicator

    async def get_chat_info(self, chat_id: str) -> Dict[str, Any]:
        return {"name": chat_id, "type": "dm"}

    async def disconnect(self) -> None:
        if self._reader_task:
            self._reader_task.cancel()
        if self._proc and self._proc.returncode is None:
            self._proc.terminate()


async def _cp3_send(sender: str, to: str, content: str) -> tuple:
    proc = await asyncio.create_subprocess_exec(
        _cp3_bin(), "send", "--from", sender, "--to", to, content,
        stdout=asyncio.subprocess.DEVNULL,
        stderr=asyncio.subprocess.PIPE,
    )
    _, err = await proc.communicate()
    return proc.returncode, (err or b"").decode("utf-8", "replace").strip()


def check_requirements() -> bool:
    return shutil.which(_cp3_bin()) is not None


async def _standalone_send(
    pconfig,
    chat_id: str,
    message: str,
    *,
    thread_id: Optional[str] = None,
    media_files: Optional[List[str]] = None,
    force_document: bool = False,
) -> Dict[str, Any]:
    """Out-of-process delivery (cron / send_message tool without a live
    gateway adapter): messages queue durably for offline peers, so this is
    always safe."""
    extra = getattr(pconfig, "extra", {}) or {}
    rc, err = await _cp3_send(_agent_name(extra), chat_id, message)
    if rc != 0:
        return {"success": False, "error": err or f"cp3 send exit {rc}"}
    return {"success": True}


def register(ctx):
    """Plugin entry point: called by the Hermes plugin system."""
    ctx.register_platform(
        name="claude-peers",
        label="Claude Peers",
        adapter_factory=lambda cfg: ClaudePeersAdapter(cfg),
        check_fn=check_requirements,
        install_hint="Install cp3: https://github.com/WillyV3/cp3 (brew install WillyV3/tap/cp3)",
        standalone_sender_fn=_standalone_send,
        cron_deliver_env_var="CLAUDE_PEERS_HOME",
        max_message_length=60000,  # under cp3's 64KB event cap
        emoji="🤝",
    )
