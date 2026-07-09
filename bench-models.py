#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "openai>=1.60",
#   "anthropic>=0.40",
#   "websockets>=13",
#   "httpx>=0.27",
#   "rich>=13.7",
# ]
# ///
"""
bench-models.py — benchmark tokens-per-second for GPT 5.4 (all reasoning levels,
normal + Fast priority tier, HTTP + raw WS paths) and MiniMax-M2.7-highspeed.

Self-contained: all credentials hardcoded. Run from anywhere with `uv run`.

HTTP path:  client → cli-proxy-plus (localhost:8318) → WS bridge → Codex backend
WS path:    client → ws://localhost:8318/v1/responses/ws → WS passthrough → backend
MiniMax:    client → api.minimax.io/anthropic (direct, no proxy)

Metric:
  tokens/sec = output_tokens / (completed_at - first_token_at)
  TTFT = first_token_at - request_sent_at
"""

from __future__ import annotations

import asyncio
import json
import statistics
import sys
import time
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import httpx
from datetime import datetime

# openai / anthropic / websockets are only needed for the GPT-5.4 and MiniMax
# paths. The GLM paths (zai-raw / ollama-raw / glm-proxy) use httpx only, so we
# import these lazily — a GLM-only run on a box without them still works.
try:
    import anthropic
except ImportError:
    anthropic = None
try:
    import openai
except ImportError:
    openai = None
try:
    import websockets
except ImportError:
    websockets = None

from rich.console import Console
from rich.live import Live
from rich.table import Table

# Console with record=True so we can dump the entire session to a text file.
# Works cleanly with Live — final rendering of each live frame is captured.
# Force width so Rich tables don't truncate columns on narrow terminals;
# saved run files will render identically regardless of terminal width at runtime.
import os as _os
_console_width = int(_os.environ.get("BENCH_CONSOLE_WIDTH", "200"))
console = Console(record=True, width=_console_width)


# ──────────────────────────────────────────────────────────────────────────
# Live progress display — in-place updating panel showing current in-flight
# tasks with live elapsed timers. Completed tasks scroll above via console.print.
# ──────────────────────────────────────────────────────────────────────────

class LiveProgress:
    """Single-line status bar for Rich Live. Intentionally compact so it doesn't
    eat terminal real estate or interfere with scrollback of completed entries."""

    def __init__(self, total: int) -> None:
        self.total = total
        self.done = 0
        self.running: dict[str, float] = {}
        self.started_at = time.monotonic()

    def __rich__(self) -> str:
        now = time.monotonic()
        elapsed = now - self.started_at
        n_run = len(self.running)
        if n_run == 0:
            tail = "[dim]idle[/dim]"
        else:
            oldest_name, oldest_start = min(self.running.items(), key=lambda kv: kv[1])
            oldest_age = now - oldest_start
            extra = f" +{n_run - 1} more" if n_run > 1 else ""
            tail = f"[cyan]{oldest_name.strip()}[/cyan] [dim]{oldest_age:.0f}s{extra}[/dim]"
        return (
            f"[bold]t+{elapsed:5.1f}s[/bold]  "
            f"[green]{self.done:>3d}[/green]/[bold]{self.total}[/bold] done  "
            f"[yellow]{n_run}[/yellow] running  "
            f"→  {tail}"
        )

    def start(self, key: str) -> None:
        self.running[key] = time.monotonic()

    def finish(self, key: str) -> None:
        self.running.pop(key, None)
        self.done += 1

# ══════════════════════════════════════════════════════════════════════════
# CONFIG — edit these to change endpoints, keys, prompt, or variants
# ══════════════════════════════════════════════════════════════════════════

# GPT 5.4 via cli-proxy-plus.
#   GPT_BASE_URL           → /v1 root used by openai SDK (bridge path)
#   GPT_PASSTHROUGH_URL    → /v1/passthrough/responses direct HTTP passthrough
#                             (bypasses WS bridge, still handled by proxy)
#   GPT_WS_URL             → ws://…/v1/responses  client-WS passthrough
GPT_BASE_URL         = _os.environ.get("GPT_BASE_URL", "http://localhost:8312/v1")
GPT_PASSTHROUGH_URL  = _os.environ.get("GPT_PASSTHROUGH_URL", "http://localhost:8312/v1/passthrough/responses")
# Chat-completions ingress (the real Cursor path): client → proxy
# /v1/chat/completions → ConvertOpenAIRequestToCodex → codex WS/HTTP upstream.
GPT_CHAT_URL         = _os.environ.get("GPT_CHAT_URL", GPT_BASE_URL.rstrip("/") + "/chat/completions")
GPT_API_KEY          = _os.environ.get("GPT_API_KEY", "droid")
GPT_MODEL            = _os.environ.get("GPT_MODEL", "gpt-5.5")

# MiniMax via direct Anthropic-compatible API (no proxy)
MINIMAX_BASE_URL = "https://api.minimax.io/anthropic"
MINIMAX_API_KEY  = "sk-cp-rM8GtOaMNgDITmsyXn8CUp172Ec_S4wrbzLCpIx2i0t3QLoep9K-4d_65Uf5kr0N1UR6gzhMm4WlC13FWlXiD9DugmSp_mB-QUTvVwNehDonfBHF_ItG4RY"
MINIMAX_MODEL    = "MiniMax-M2.7-highspeed"
MINIMAX_MAX_OUT  = 64000

# Direct Codex OAuth backend — bypasses cli-proxy-plus entirely.
# Reads access_token from the auth JSON Codex CLI writes.
CODEX_AUTH_FILE   = _os.environ.get("CODEX_AUTH_FILE", str(Path.home() / "Documents/GitHub/claude-tools/ccs/docker/config/cliproxy-auth/codex-fallinparadise@pm.me-pro.json"))
CODEX_DIRECT_URL  = "https://chatgpt.com/backend-api/codex/responses"
CODEX_ORIGINATOR  = "codex-tui"
CODEX_OPENAI_BETA = "responses_websockets=2026-02-06"

# Reasoning efforts for GPT 5.4 variants — suffix form: gpt-5.4(<effort>)
# Proxy strips the suffix and translates to reasoning.effort on the wire.
EFFORTS = ["none", "low", "medium", "high", "xhigh"]

# ── GLM providers (chat-completions): z.ai coding plan vs Ollama, raw + via cliproxy ──
# Added so you can compare provider inference speed at any reasoning effort:
#   ./bench-models.py --paths zai-raw,ollama-raw,glm-proxy --efforts max --no-minimax
# RAW = hit the provider directly (pure inference + your network position).
# PROXY = through cliproxy (the glm-proxy target). Run this ON the ARM VM for a
# clean comparison; localhost:8317 is the proxy's port there. Override the proxy
# URL with GLM_PROXY_URL if running elsewhere (e.g. https://clanker.ecorp.cc/...).
GLM_MAX_OUT = int(_os.environ.get("GLM_MAX_OUT", "1500"))
GLM_PROXY_URL = _os.environ.get("GLM_PROXY_URL", "http://localhost:8317/v1/chat/completions")
GLM_TARGETS: dict[str, dict[str, Any]] = {
    "zai-raw": {
        "url": "https://api.z.ai/api/coding/paas/v4/chat/completions",
        "key": "990a13700e8f4866b3e7c7d8e4167308.USp85J3PWOE5ETK1",
        "model": "GLM-5.2",
        "is_proxy": False,
    },
    "ollama-raw": {
        "url": "https://ollama.com/v1/chat/completions",
        "key": "3c797f3dc83f496a8a3f140a9ec04ac1.nGLzWS7zMBb_OF0mARePVzQZ",
        "model": "glm-5.2",
        "is_proxy": False,
    },
    "glm-proxy": {
        "url": GLM_PROXY_URL,
        "key": "cursor",
        "model": "glm-5.2-max",  # client alias → z.ai via cliproxy (reasoning_effort=max override applied by proxy)
        "is_proxy": True,
    },
}
GLM_PATHS = list(GLM_TARGETS.keys())

# Concurrency: how many GPT requests can be in-flight at once against the proxy.
# MiniMax runs independently (different endpoint) and is not gated by this.
GPT_CONCURRENCY = 12

# Hard per-request timeout. Applies to every single bench_* call.
# xhigh reasoning typically takes 90-180s; we use a generous 10 min (600s) margin.
# Without this, a silently-dead upstream WS can hang forever (no keepalive pings).
PER_TASK_TIMEOUT_S = 600.0

# Benchmark prompt: targets ~500-1000 output tokens of reasoning-heavy code
PROMPT = (
    "Write a complete Python implementation of Dijkstra's shortest path algorithm "
    "with priority queue, including comments explaining each step, a small test "
    "harness with 3 sample graphs, and brief complexity analysis. "
    "Target around 800 tokens of output."
)
SYSTEM_PROMPT = "You are a concise technical writer. Output Python code with comments."
CONTINUE_PROMPT = "Continue the implementation with more detail or a related variant."


def _default_input(user_text: str = PROMPT) -> list[dict[str, Any]]:
    return [{"role": "user", "content": [{"type": "input_text", "text": user_text}]}]


def _extend_input_with_turn(
    input_messages: list[dict[str, Any]],
    assistant_text: str,
    next_user_text: str = CONTINUE_PROMPT,
) -> list[dict[str, Any]]:
    """Append an assistant response + a new user turn to an input list,
    producing the input for the next turn. Mimics a real multi-turn convo
    so the bridge can detect shared prefix growth and delta-send."""
    return input_messages + [
        {"role": "assistant", "content": [{"type": "output_text", "text": assistant_text}]},
        {"role": "user", "content": [{"type": "input_text", "text": next_user_text}]},
    ]

# ══════════════════════════════════════════════════════════════════════════


@dataclass
class Result:
    label: str
    # path: "bridge-http" | "bridge-ws" | "proxy-http" | "direct-http" | "direct-ws" | "anthropic"
    path: str
    model: str
    effort: str
    fast: bool
    turn: int = 1
    ok: bool = False
    ttft_s: float = 0.0          # sent_at → first text delta (prefill+queue+reasoning+network)
    dur_s: float = 0.0           # first text delta → response.completed (decode/text window)
    e2e_s: float = 0.0           # sent_at → response.completed (full wall time = TTFT + dur)
    input_tokens: int = 0
    cached_tokens: int = 0
    output_tokens: int = 0       # total output (includes reasoning tokens per OpenAI usage schema)
    reasoning_tokens: int = 0
    # Metrics per NVIDIA GenAI-Perf canonical definitions:
    #   https://developer.nvidia.com/blog/llm-benchmarking-fundamental-concepts/
    tps: float = 0.0             # TPS = output_tokens / e2e_s  (primary throughput)
    tpot_ms: float = 0.0         # ITL = 1000*(e2e - TTFT) / (output - 1)  per-token decode latency
    # Reasoning-aware secondaries (not in NVIDIA spec; useful for thinking models):
    tps_reason: float = 0.0      # reasoning_tokens / ttft_s  (gen rate during silent thinking window)
    tps_text: float = 0.0        # text_tokens / dur_s  (gen rate during streaming tail)
    response_text: str = ""      # accumulated output text (used by multi-turn runner to build next turn's input)
    error: str = ""

    @property
    def cache_pct(self) -> float:
        return (self.cached_tokens / self.input_tokens * 100) if self.input_tokens else 0.0

    @property
    def text_tokens(self) -> int:
        return max(self.output_tokens - self.reasoning_tokens, 0)

    @property
    def tps_perceived(self) -> float:
        """User-facing throughput: visible text tokens per full wall-second.
        Unlike NVIDIA TPS (which treats reasoning tokens as output), this metric
        drops for reasoning models because the silent TTFT window counts against
        the denominator while hidden reasoning tokens don't count in the numerator.
        Matches the "which one streams faster to the eye" intuition."""
        return (self.text_tokens / self.e2e_s) if self.e2e_s > 0 else 0.0


def _finalize(r: Result, *, sent_at: float, first_token_at: float, last_event_at: float,
              in_tok: int, cached: int, out_tok: int, reas_tok: int) -> None:
    """Populate Result with canonical LLM benchmark metrics.

    Formulas follow NVIDIA GenAI-Perf (LLM Inference Benchmarking Fundamentals):
      TTFT = first_token_at - sent_at
      TPS  = output_tokens / (last_event_at - sent_at)        # throughput
      ITL  = (e2e - TTFT) / (output_tokens - 1)                # inter-token latency (s/tok)
    Reasoning-aware splits (model thinks silently during TTFT for thinking models):
      TPS_reason = reasoning_tokens / TTFT                     # tokens/s during thinking
      TPS_text   = text_tokens / dur                           # tokens/s during streaming tail
    """
    r.ttft_s = first_token_at - sent_at
    r.dur_s = last_event_at - first_token_at
    r.e2e_s = last_event_at - sent_at
    r.input_tokens = in_tok
    r.cached_tokens = cached
    r.output_tokens = out_tok
    r.reasoning_tokens = reas_tok
    r.tps = (out_tok / r.e2e_s) if r.e2e_s > 0 else 0.0
    r.tpot_ms = (1000.0 * (r.e2e_s - r.ttft_s) / (out_tok - 1)) if out_tok > 1 else 0.0
    text_tok = max(out_tok - reas_tok, 0)
    r.tps_text = (text_tok / r.dur_s) if r.dur_s > 0 else 0.0
    r.tps_reason = (reas_tok / r.ttft_s) if (reas_tok > 0 and r.ttft_s > 0) else 0.0
    r.ok = True


# ──────────────────────────────────────────────────────────────────────────
# Usage parser — extracts input/cached/output/reasoning tokens from a
# response.completed event's usage block. Works for OpenAI Responses API
# shape (both SDK events and raw SSE JSON).
# ──────────────────────────────────────────────────────────────────────────

def _parse_usage(usage_obj: Any) -> tuple[int, int, int, int]:
    """Return (input_tokens, cached_tokens, output_tokens, reasoning_tokens).
    Accepts either a dict or an openai SDK Pydantic object."""
    if usage_obj is None:
        return 0, 0, 0, 0

    def g(obj: Any, key: str, default: int = 0) -> Any:
        if isinstance(obj, dict):
            return obj.get(key, default)
        return getattr(obj, key, default)

    input_tok = g(usage_obj, "input_tokens") or 0
    output_tok = g(usage_obj, "output_tokens") or 0
    # cached_tokens is nested: usage.input_tokens_details.cached_tokens
    details_in = g(usage_obj, "input_tokens_details")
    cached = g(details_in, "cached_tokens") if details_in is not None else 0
    # reasoning_tokens: usage.output_tokens_details.reasoning_tokens
    details_out = g(usage_obj, "output_tokens_details")
    reasoning = g(details_out, "reasoning_tokens") if details_out is not None else 0
    return int(input_tok or 0), int(cached or 0), int(output_tok or 0), int(reasoning or 0)


def _parse_chat_usage(usage_obj: Any) -> tuple[int, int, int, int]:
    """Parse an OpenAI chat-completions usage block (the shape z.ai/ollama emit
    at stream end when stream_options.include_usage=true).

    Returns (input_tokens, cached_tokens, output_tokens, reasoning_tokens).
    Fields:
      prompt_tokens, completion_tokens (top-level)
      prompt_tokens_details.cached_tokens
      completion_tokens_details.reasoning_tokens
    Also accepts the Responses-API aliases (input_tokens/output_tokens +
    input_tokens_details/output_tokens_details) so the proxy path's usage
    chunk parses identically.
    """
    if usage_obj is None:
        return 0, 0, 0, 0

    def g(obj: Any, key: str, default: int = 0) -> Any:
        if isinstance(obj, dict):
            return obj.get(key, default)
        return getattr(obj, key, default)

    input_tok = g(usage_obj, "prompt_tokens") or g(usage_obj, "input_tokens") or 0
    output_tok = g(usage_obj, "completion_tokens") or g(usage_obj, "output_tokens") or 0
    # cached_tokens: prompt_tokens_details.cached_tokens (chat-completions) or
    # input_tokens_details.cached_tokens (Responses API).
    details_in = g(usage_obj, "prompt_tokens_details") or g(usage_obj, "input_tokens_details")
    cached = g(details_in, "cached_tokens") if details_in is not None else 0
    # reasoning_tokens: completion_tokens_details.reasoning_tokens (chat-completions)
    # or output_tokens_details.reasoning_tokens (Responses API).
    details_out = g(usage_obj, "completion_tokens_details") or g(usage_obj, "output_tokens_details")
    reasoning = g(details_out, "reasoning_tokens") if details_out is not None else 0
    return int(input_tok or 0), int(cached or 0), int(output_tok or 0), int(reasoning or 0)


# ──────────────────────────────────────────────────────────────────────────
# HTTP path — openai SDK pointed at cli-proxy-plus (WS bridge dispatch)
# ──────────────────────────────────────────────────────────────────────────

async def bench_http(effort: str, fast: bool, label: str, *, session_key: str | None = None, turn: int = 1,
                     input_messages: list[dict[str, Any]] | None = None) -> Result:
    r = Result(label=label, path="http-cli-ws", model=GPT_MODEL, effort=effort, fast=fast, turn=turn)
    try:
        extra_args: dict[str, Any] = {}
        msgs = input_messages if input_messages is not None else _default_input()
        if fast:
            extra_args["service_tier"] = "priority"
        # Use plain model id + explicit reasoning.effort (avoids the proxy's
        # per-variant auth state bug that 400s on unused suffix variants).
        if effort != "none":
            extra_args["reasoning"] = {"effort": effort}

        client = openai.AsyncOpenAI(api_key=GPT_API_KEY, base_url=GPT_BASE_URL)

        body: dict[str, Any] = {
            "model": GPT_MODEL,
            "input": msgs,
            "instructions": SYSTEM_PROMPT,
            "stream": True,
            "store": False,
            "prompt_cache_key": session_key or f"bench-{uuid.uuid4()}",
            **extra_args,
        }

        sent_at = time.monotonic()
        first_token_at = None
        last_event_at = None
        in_tok = cached = out_tok = reas_tok = 0
        resp_text_parts: list[str] = []

        stream = await client.responses.create(**body)
        async for event in stream:
            now = time.monotonic()
            etype = getattr(event, "type", "")
            if etype == "response.output_text.delta":
                if first_token_at is None:
                    first_token_at = now
                last_event_at = now
                delta = getattr(event, "delta", "") or ""
                if delta:
                    resp_text_parts.append(delta)
            if etype == "response.completed":
                last_event_at = now
                resp = getattr(event, "response", None)
                usage = getattr(resp, "usage", None) if resp else None
                in_tok, cached, out_tok, reas_tok = _parse_usage(usage)

        if first_token_at and last_event_at and out_tok:
            _finalize(r, sent_at=sent_at, first_token_at=first_token_at, last_event_at=last_event_at,
                      in_tok=in_tok, cached=cached, out_tok=out_tok, reas_tok=reas_tok)
            r.response_text = "".join(resp_text_parts)
        else:
            r.error = "no tokens streamed"
    except Exception as e:
        r.error = f"{type(e).__name__}: {e}"
    return r


# ──────────────────────────────────────────────────────────────────────────
# WS path — raw WebSocket to cli-proxy-plus /v1/responses/ws
# ──────────────────────────────────────────────────────────────────────────

async def bench_ws(effort: str, fast: bool, label: str, *, session_key: str | None = None, turn: int = 1,
                   input_messages: list[dict[str, Any]] | None = None) -> Result:
    r = Result(label=label, path="ws-cli-upstream", model=GPT_MODEL, effort=effort, fast=fast, turn=turn)
    try:
        # http://host:port/v1 → ws://host:port/v1/responses (WS upgrade on GET)
        ws_url = GPT_BASE_URL.replace("http://", "ws://").replace("https://", "wss://").rstrip("/") + "/responses"

        headers = [("Authorization", f"Bearer {GPT_API_KEY}")]

        extra_args: dict[str, Any] = {}
        msgs = input_messages if input_messages is not None else _default_input()
        if fast:
            extra_args["service_tier"] = "priority"
        if effort != "none":
            extra_args["reasoning"] = {"effort": effort}

        msg: dict[str, Any] = {
            "type": "response.create",
            "model": GPT_MODEL,
            "input": msgs,
            "instructions": SYSTEM_PROMPT,
            "store": False,
            "prompt_cache_key": session_key or f"bench-{uuid.uuid4()}",
            **extra_args,
        }

        sent_at = time.monotonic()
        first_token_at = None
        last_event_at = None
        in_tok = cached = out_tok = reas_tok = 0
        resp_text_parts: list[str] = []

        # Disable client-side keepalive pings — high/xhigh reasoning can silently
        # think for 2+ minutes before emitting text, which would trip the default
        # 20s ping timeout and kill the connection with 1011.
        async with websockets.connect(
            ws_url,
            additional_headers=headers,
            ping_interval=None,
            close_timeout=10,
            max_size=None,
        ) as ws:
            await ws.send(json.dumps(msg))
            async for raw in ws:
                now = time.monotonic()
                try:
                    event = json.loads(raw)
                except Exception:
                    continue
                etype = event.get("type", "")
                if etype == "response.output_text.delta":
                    if first_token_at is None:
                        first_token_at = now
                    last_event_at = now
                    d = event.get("delta", "")
                    if d:
                        resp_text_parts.append(d)
                if etype == "response.completed":
                    last_event_at = now
                    usage = (event.get("response") or {}).get("usage") or {}
                    in_tok, cached, out_tok, reas_tok = _parse_usage(usage)
                    break

        if first_token_at and last_event_at and out_tok:
            _finalize(r, sent_at=sent_at, first_token_at=first_token_at, last_event_at=last_event_at,
                      in_tok=in_tok, cached=cached, out_tok=out_tok, reas_tok=reas_tok)
            r.response_text = "".join(resp_text_parts)
        else:
            r.error = "no tokens streamed"
    except Exception as e:
        r.error = f"{type(e).__name__}: {e}"
    return r


# ──────────────────────────────────────────────────────────────────────────
# Chat-completions ingress — POST /v1/chat/completions (the real Cursor path).
# Proxy translates chat → Codex Responses via ConvertOpenAIRequestToCodex,
# then routes to the codex WS bridge / HTTP upstream. Response is
# chat.completion.chunk SSE (delta.content / delta.reasoning_content).
# ──────────────────────────────────────────────────────────────────────────

def _responses_input_to_chat_messages(msgs: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Flatten Responses-style input items ({role, content:[{type,text}]}) into
    chat-completions messages ({role, content:"text"}). Preserves turn order so
    the growing multi-turn history still shares a cacheable prefix."""
    out: list[dict[str, Any]] = [{"role": "system", "content": SYSTEM_PROMPT}]
    for m in msgs:
        role = m.get("role", "user")
        content = m.get("content")
        if isinstance(content, str):
            text = content
        else:
            parts = []
            for blk in content or []:
                t = blk.get("text")
                if t:
                    parts.append(t)
            text = "\n".join(parts)
        out.append({"role": role, "content": text})
    return out


async def bench_chat_completions(effort: str, fast: bool, label: str, *, session_key: str | None = None, turn: int = 1,
                                 input_messages: list[dict[str, Any]] | None = None) -> Result:
    r = Result(label=label, path="chat-cli-ws", model=GPT_MODEL, effort=effort, fast=fast, turn=turn)
    try:
        msgs = input_messages if input_messages is not None else _default_input()
        body: dict[str, Any] = {
            "model": GPT_MODEL,
            "messages": _responses_input_to_chat_messages(msgs),
            "stream": True,
            "store": False,
            "prompt_cache_key": session_key or f"bench-{uuid.uuid4()}",
            "stream_options": {"include_usage": True},
        }
        if effort != "none":
            body["reasoning_effort"] = effort
        if fast:
            body["service_tier"] = "priority"

        headers = {
            "Authorization": f"Bearer {GPT_API_KEY}",
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
        }

        sent_at = time.monotonic()
        first_token_at = None
        last_event_at = None
        in_tok = cached = out_tok = reas_tok = 0
        got_usage = False
        content_delta_count = reason_delta_count = 0
        resp_text_parts: list[str] = []

        async with httpx.AsyncClient(timeout=None) as client:
            async with client.stream("POST", GPT_CHAT_URL, headers=headers, json=body) as resp:
                resp.raise_for_status()
                async for line in resp.aiter_lines():
                    if not line or not line.startswith("data:"):
                        continue
                    data = line[5:].strip()
                    if not data or data == "[DONE]":
                        continue
                    try:
                        event = json.loads(data)
                    except Exception:
                        continue
                    now = time.monotonic()
                    usage = event.get("usage")
                    if usage:
                        in_tok, cached, out_tok, reas_tok = _parse_chat_usage(usage)
                        got_usage = True
                        last_event_at = now
                        continue
                    delta = (event.get("choices") or [{}])[0].get("delta") or {}
                    c = delta.get("content")
                    rc = delta.get("reasoning_content") or delta.get("reasoning")
                    if c or rc:
                        if first_token_at is None:
                            first_token_at = now
                        last_event_at = now
                        if c:
                            content_delta_count += 1
                            resp_text_parts.append(c)
                        if rc:
                            reason_delta_count += 1

        if not got_usage:
            out_tok = content_delta_count + reason_delta_count
            reas_tok = reason_delta_count

        if first_token_at and last_event_at and out_tok:
            _finalize(r, sent_at=sent_at, first_token_at=first_token_at, last_event_at=last_event_at,
                      in_tok=in_tok, cached=cached, out_tok=out_tok, reas_tok=reas_tok)
            r.response_text = "".join(resp_text_parts)
        else:
            r.error = "no tokens streamed"
    except Exception as e:
        r.error = f"{type(e).__name__}: {e}"
    return r


# ──────────────────────────────────────────────────────────────────────────
# Proxy HTTP passthrough — /v1/passthrough/responses. HTTP in, HTTP out.
# Proxy forwards to Codex backend's HTTP endpoint, no WS bridge.
# ──────────────────────────────────────────────────────────────────────────

async def bench_proxy_http(effort: str, fast: bool, label: str, *, session_key: str | None = None, turn: int = 1,
                           input_messages: list[dict[str, Any]] | None = None) -> Result:
    r = Result(label=label, path="http-cli-upstream", model=GPT_MODEL, effort=effort, fast=fast, turn=turn)
    try:
        msgs = input_messages if input_messages is not None else _default_input()
        body: dict[str, Any] = {
            "model": GPT_MODEL,
            "input": msgs,
            "instructions": SYSTEM_PROMPT,
            "stream": True,
            "store": False,
            "prompt_cache_key": session_key or f"bench-{uuid.uuid4()}",
        }
        if effort != "none":
            body["reasoning"] = {"effort": effort}
        if fast:
            body["service_tier"] = "priority"

        headers = {
            "Authorization": f"Bearer {GPT_API_KEY}",
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
        }

        sent_at = time.monotonic()
        first_token_at = None
        last_event_at = None
        in_tok = cached = out_tok = reas_tok = 0
        resp_text_parts: list[str] = []

        async with httpx.AsyncClient(timeout=None) as client:
            async with client.stream("POST", GPT_PASSTHROUGH_URL, headers=headers, json=body) as resp:
                resp.raise_for_status()
                async for line in resp.aiter_lines():
                    if not line or not line.startswith("data: "):
                        continue
                    data = line[6:].strip()
                    if not data or data == "[DONE]":
                        continue
                    try:
                        event = json.loads(data)
                    except Exception:
                        continue
                    now = time.monotonic()
                    etype = event.get("type", "")
                    if etype == "response.output_text.delta":
                        if first_token_at is None:
                            first_token_at = now
                        last_event_at = now
                        d = event.get("delta", "")
                        if d:
                            resp_text_parts.append(d)
                    if etype == "response.completed":
                        last_event_at = now
                        usage = (event.get("response") or {}).get("usage") or {}
                        in_tok, cached, out_tok, reas_tok = _parse_usage(usage)
                        break

        if first_token_at and last_event_at and out_tok:
            _finalize(r, sent_at=sent_at, first_token_at=first_token_at, last_event_at=last_event_at,
                      in_tok=in_tok, cached=cached, out_tok=out_tok, reas_tok=reas_tok)
            r.response_text = "".join(resp_text_parts)
        else:
            r.error = "no tokens streamed"
    except Exception as e:
        r.error = f"{type(e).__name__}: {e}"
    return r


# ──────────────────────────────────────────────────────────────────────────
# Direct Codex OAuth backend — bypasses cli-proxy-plus entirely.
# Uses raw httpx (HTTP) and websockets (WS) with OAuth Bearer token.
# ──────────────────────────────────────────────────────────────────────────

def _load_codex_token() -> str:
    d = json.loads(Path(CODEX_AUTH_FILE).read_text())
    tok = d.get("access_token")
    if not tok:
        raise RuntimeError(f"no access_token in {CODEX_AUTH_FILE}")
    return tok


def _codex_headers(token: str, session_id: str) -> dict[str, str]:
    return {
        "Authorization": f"Bearer {token}",
        "OpenAI-Beta": CODEX_OPENAI_BETA,
        "Originator": CODEX_ORIGINATOR,
        "session_id": session_id,
        "Content-Type": "application/json",
        "Accept": "text/event-stream",
    }


def _codex_body(effort: str, fast: bool, session_id: str,
                input_messages: list[dict[str, Any]] | None = None) -> dict[str, Any]:
    body: dict[str, Any] = {
        "model": GPT_MODEL,
        "input": input_messages if input_messages is not None else _default_input(),
        "instructions": SYSTEM_PROMPT,
        "stream": True,
        "store": False,
        "prompt_cache_key": session_id,
    }
    if effort != "none":
        body["reasoning"] = {"effort": effort}
    if fast:
        body["service_tier"] = "priority"
    return body


async def bench_direct_http(effort: str, fast: bool, label: str, *, session_key: str | None = None, turn: int = 1,
                            input_messages: list[dict[str, Any]] | None = None) -> Result:
    r = Result(label=label, path="http-direct-upstream", model=GPT_MODEL, effort=effort, fast=fast, turn=turn)
    try:
        token = _load_codex_token()
        session_id = session_key or f"bench-{uuid.uuid4()}"
        headers = _codex_headers(token, session_id)
        body = _codex_body(effort, fast, session_id, input_messages=input_messages)

        sent_at = time.monotonic()
        first_token_at = None
        last_event_at = None
        in_tok = cached = out_tok = reas_tok = 0
        resp_text_parts: list[str] = []

        async with httpx.AsyncClient(timeout=None) as client:
            async with client.stream("POST", CODEX_DIRECT_URL, headers=headers, json=body) as resp:
                resp.raise_for_status()
                async for line in resp.aiter_lines():
                    if not line or not line.startswith("data: "):
                        continue
                    data = line[6:].strip()
                    if not data or data == "[DONE]":
                        continue
                    try:
                        event = json.loads(data)
                    except Exception:
                        continue
                    now = time.monotonic()
                    etype = event.get("type", "")
                    if etype == "response.output_text.delta":
                        if first_token_at is None:
                            first_token_at = now
                        last_event_at = now
                        d = event.get("delta", "")
                        if d:
                            resp_text_parts.append(d)
                    if etype == "response.completed":
                        last_event_at = now
                        usage = (event.get("response") or {}).get("usage") or {}
                        in_tok, cached, out_tok, reas_tok = _parse_usage(usage)
                        break

        if first_token_at and last_event_at and out_tok:
            _finalize(r, sent_at=sent_at, first_token_at=first_token_at, last_event_at=last_event_at,
                      in_tok=in_tok, cached=cached, out_tok=out_tok, reas_tok=reas_tok)
            r.response_text = "".join(resp_text_parts)
        else:
            r.error = "no tokens streamed"
    except Exception as e:
        r.error = f"{type(e).__name__}: {e}"
    return r


async def bench_direct_ws(effort: str, fast: bool, label: str, *, session_key: str | None = None, turn: int = 1,
                          input_messages: list[dict[str, Any]] | None = None) -> Result:
    r = Result(label=label, path="ws-direct-upstream", model=GPT_MODEL, effort=effort, fast=fast, turn=turn)
    try:
        token = _load_codex_token()
        session_id = session_key or f"bench-{uuid.uuid4()}"
        ws_url = CODEX_DIRECT_URL.replace("https://", "wss://").replace("http://", "ws://")
        hdrs = _codex_headers(token, session_id)
        hdrs.pop("Content-Type", None)
        hdrs.pop("Accept", None)
        headers_list = list(hdrs.items())

        msg = {"type": "response.create", **_codex_body(effort, fast, session_id, input_messages=input_messages)}
        msg.pop("stream", None)

        sent_at = time.monotonic()
        first_token_at = None
        last_event_at = None
        in_tok = cached = out_tok = reas_tok = 0
        resp_text_parts: list[str] = []

        async with websockets.connect(
            ws_url,
            additional_headers=headers_list,
            ping_interval=None,
            close_timeout=10,
            max_size=None,
        ) as ws:
            await ws.send(json.dumps(msg))
            async for raw in ws:
                now = time.monotonic()
                try:
                    event = json.loads(raw)
                except Exception:
                    continue
                etype = event.get("type", "")
                if etype == "response.output_text.delta":
                    if first_token_at is None:
                        first_token_at = now
                    last_event_at = now
                    d = event.get("delta", "")
                    if d:
                        resp_text_parts.append(d)
                if etype == "response.completed":
                    last_event_at = now
                    usage = (event.get("response") or {}).get("usage") or {}
                    in_tok, cached, out_tok, reas_tok = _parse_usage(usage)
                    break

        if first_token_at and last_event_at and out_tok:
            _finalize(r, sent_at=sent_at, first_token_at=first_token_at, last_event_at=last_event_at,
                      in_tok=in_tok, cached=cached, out_tok=out_tok, reas_tok=reas_tok)
            r.response_text = "".join(resp_text_parts)
        else:
            r.error = "no tokens streamed"
    except Exception as e:
        r.error = f"{type(e).__name__}: {e}"
    return r


# ──────────────────────────────────────────────────────────────────────────
# MiniMax — direct Anthropic-compatible API (no proxy, unrelated taxonomy)
# ──────────────────────────────────────────────────────────────────────────

async def bench_minimax(label: str, *, turn: int = 1) -> Result:
    r = Result(label=label, path="anthropic-direct", model=MINIMAX_MODEL, effort="n/a", fast=False, turn=turn)
    try:
        client = anthropic.AsyncAnthropic(api_key=MINIMAX_API_KEY, base_url=MINIMAX_BASE_URL)

        sent_at = time.monotonic()
        first_token_at = None
        last_event_at = None
        in_tok = out_tok = 0
        cached = 0

        async with client.messages.stream(
            model=MINIMAX_MODEL,
            max_tokens=MINIMAX_MAX_OUT,
            system=SYSTEM_PROMPT,
            messages=[{"role": "user", "content": PROMPT}],
        ) as stream:
            async for event in stream:
                now = time.monotonic()
                etype = getattr(event, "type", "")
                if etype == "content_block_delta":
                    if first_token_at is None:
                        first_token_at = now
                    last_event_at = now
                if etype == "message_delta":
                    usage = getattr(event, "usage", None)
                    if usage:
                        out_tok = getattr(usage, "output_tokens", 0) or out_tok
            final = await stream.get_final_message()
            if final and final.usage:
                out_tok = final.usage.output_tokens or out_tok
                in_tok = getattr(final.usage, "input_tokens", 0) or 0
                cached = getattr(final.usage, "cache_read_input_tokens", 0) or 0

        if first_token_at and last_event_at and out_tok:
            _finalize(r, sent_at=sent_at, first_token_at=first_token_at, last_event_at=last_event_at,
                      in_tok=in_tok, cached=cached, out_tok=out_tok, reas_tok=0)
        else:
            r.error = "no tokens streamed"
    except Exception as e:
        r.error = f"{type(e).__name__}: {e}"
    return r


# ──────────────────────────────────────────────────────────────────────────
# GLM — OpenAI chat-completions SSE (z.ai / ollama raw, or via cliproxy).
# Reuses the same _finalize() metrics. Token counts come from the `usage`
# block the provider emits in a final empty-delta chunk when
# `stream_options.include_usage=true` is set (real output/reasoning/cached
# token counts as billed by the provider). If the provider doesn't send a
# usage chunk, we fall back to counting content + reasoning_content deltas
# (best-effort: counts chunks, not tokens — only marginally useful).
# TTFT is to the first delta of either content or reasoning_content kind.
# Sends reasoning_effort=max + thinking.type=enabled on RAW paths; on the
# PROXY path the effort override is applied server-side by the proxy's
# payload override (so we send the bare alias).
# ──────────────────────────────────────────────────────────────────────────

async def bench_glm(target_id: str, effort: str, label: str, *, session_key: str | None = None,
                    turn: int = 1, input_messages: list[dict[str, Any]] | None = None) -> Result:
    t = GLM_TARGETS[target_id]
    r = Result(label=label, path=target_id, model=t["model"], effort=effort, fast=False, turn=turn)
    try:
        body: dict[str, Any] = {
            "model": t["model"],
            "messages": [
                {"role": "system", "content": SYSTEM_PROMPT},
                {"role": "user", "content": PROMPT},
            ],
            "stream": True,
            "max_tokens": GLM_MAX_OUT,
            # Force the provider to emit a usage chunk at stream end so we can
            # read real output/reasoning/cached token counts instead of
            # counting SSE deltas (which is not a token count).
            "stream_options": {"include_usage": True},
        }
        # RAW providers need the effort/thinking on the wire. The proxy target
        # gets it from its own payload override keyed on the alias, so we don't
        # send it raw to avoid double-setting (harmless, but keeps intent clear).
        if not t["is_proxy"] and effort != "none":
            body["reasoning_effort"] = effort
            body["thinking"] = {"type": "enabled"}

        headers = {
            "Authorization": f"Bearer {t['key']}",
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
        }

        sent_at = time.monotonic()
        first_token_at = None
        last_event_at = None
        # Real billed counts from the provider's usage chunk (preferred).
        in_tok = cached = out_tok = reas_tok = 0
        got_usage = False
        # Delta-count fallback (used only if the provider sends no usage chunk).
        content_delta_count = 0
        reason_delta_count = 0
        resp_text_parts: list[str] = []

        async with httpx.AsyncClient(timeout=None) as client:
            async with client.stream("POST", t["url"], headers=headers, json=body) as resp:
                resp.raise_for_status()
                async for line in resp.aiter_lines():
                    if not line or not line.startswith("data:"):
                        continue
                    data = line[5:].strip()
                    if not data or data == "[DONE]":
                        continue
                    try:
                        event = json.loads(data)
                    except Exception:
                        continue
                    now = time.monotonic()
                    # Usage chunk: chat-completions sends `usage` on a chunk
                    # whose `choices` is empty (or absent) at stream end.
                    usage = event.get("usage")
                    if usage:
                        in_tok, cached, out_tok, reas_tok = _parse_chat_usage(usage)
                        got_usage = True
                        last_event_at = now
                        continue
                    delta = (event.get("choices") or [{}])[0].get("delta") or {}
                    c = delta.get("content")
                    # Reasoning field name differs by provider:
                    #   z.ai        → delta.reasoning_content  (OpenAI-compat standard)
                    #   ollama.com  → delta.reasoning          (non-standard)
                    # Read both so RAW paths capture reasoning deltas regardless
                    # of provider. The proxy path also emits `reasoning` (it
                    # rewrites reasoning_content → reasoning for ollama).
                    rc = delta.get("reasoning_content") or delta.get("reasoning")
                    if c or rc:
                        if first_token_at is None:
                            first_token_at = now
                        last_event_at = now
                        if c:
                            content_delta_count += 1
                            resp_text_parts.append(c)
                        if rc:
                            reason_delta_count += 1

        if not got_usage:
            # Provider didn't emit a usage chunk — fall back to counting deltas.
            # NOTE: this counts SSE chunks, not tokens; treat as a rough
            # lower bound only. cached_tokens stays 0 (no usage info available).
            out_tok = content_delta_count + reason_delta_count
            reas_tok = reason_delta_count
        # If we got a usage block, reas_tok/cached come from it (real values).
        # ollama.com's usage block omits completion_tokens_details.reasoning_tokens,
        # so reas_tok will be 0 even though reasoning deltas arrived — that's
        # honest (the provider didn't report it). TPS uses completion_tokens
        # from the usage block, which is the real total output.
        if first_token_at and last_event_at and out_tok:
            _finalize(r, sent_at=sent_at, first_token_at=first_token_at, last_event_at=last_event_at,
                      in_tok=in_tok, cached=cached, out_tok=out_tok, reas_tok=reas_tok)
            r.response_text = "".join(resp_text_parts)
        else:
            r.error = "no tokens streamed"
    except Exception as e:
        r.error = f"{type(e).__name__}: {e}"
    return r


# ──────────────────────────────────────────────────────────────────────────
# Main runner + reporting
# ──────────────────────────────────────────────────────────────────────────

# Paths under test. Each tuple: (path_id, short_tag, bench_fn)
#   path_id = value stored in Result.path (used for grouping/lookup)
#   short_tag = fixed-width label for live progress lines
GPT_PATHS: list[tuple[str, str, Any]] = [
    ("chat-cli-ws",          "CHAT→CLI→WS  ", None),  # bench_chat_completions — chat ingress
    ("http-cli-ws",          "HTTP→CLI→WS  ", None),  # bench_http — resolved at call time
    ("ws-cli-upstream",      "WS  →CLI→WS  ", None),  # bench_ws
    ("http-cli-upstream",    "HTTP→CLI→HTTP", None),  # bench_proxy_http (NEW passthrough)
    ("http-direct-upstream", "HTTP→     UP ", None),  # bench_direct_http
    ("ws-direct-upstream",   "WS  →     UP ", None),  # bench_direct_ws
]

# Path metadata & palette
PATH_META: dict[str, dict[str, str]] = {
    "chat-cli-ws":          {"short": "CHAT→CLI→WS",   "color": "bright_white",   "note": "chat-completions ingress → proxy → codex (real Cursor path)"},
    "http-cli-ws":          {"short": "HTTP→CLI→WS",   "color": "bright_cyan",    "note": "Droid's prod path (WS bridge)"},
    "ws-cli-upstream":      {"short": "WS →CLI→WS",    "color": "cyan",           "note": "client WS → proxy → upstream WS"},
    "http-cli-upstream":    {"short": "HTTP→CLI→HTTP", "color": "magenta",        "note": "NEW passthrough (no bridge)"},
    "http-direct-upstream": {"short": "HTTP→ UP",      "color": "yellow",         "note": "direct to chatgpt.com (no proxy)"},
    "ws-direct-upstream":   {"short": "WS → UP",       "color": "bright_yellow",  "note": "direct WS (no proxy)"},
    "anthropic-direct":     {"short": "MINIMAX",       "color": "green",          "note": "direct Anthropic-compatible API"},
    "zai-raw":              {"short": "ZAI raw",       "color": "bright_magenta", "note": "z.ai coding plan GLM-5.2 (direct)"},
    "ollama-raw":           {"short": "OLLAMA raw",    "color": "bright_blue",    "note": "ollama.com glm-5.2 (direct)"},
    "glm-proxy":            {"short": "GLM via proxy", "color": "bright_green",   "note": "cliproxy → z.ai (glm-5.2-max)"},
}

# Map Result.path → bench function (for resolving the callable at runtime
# since we can't reference the functions before they're defined).
BENCH_FNS: dict[str, Any] = {
    "chat-cli-ws":          bench_chat_completions,
    "http-cli-ws":          bench_http,
    "ws-cli-upstream":      bench_ws,
    "http-cli-upstream":    bench_proxy_http,
    "http-direct-upstream": bench_direct_http,
    "ws-direct-upstream":   bench_direct_ws,
}


def _fmt_path(path: str) -> str:
    meta = PATH_META.get(path, {"short": path, "color": "white"})
    return f"[{meta['color']}]{meta['short']:<14}[/{meta['color']}]"


def _parse_args() -> tuple[int, list[str], bool, str | None, list[str] | None]:
    """CLI parser. Returns (turns, efforts, skip_minimax, output_path, path_filter)."""
    import argparse
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--turns", type=int, default=1, help="sequential turns per (path × effort × tier) combo, sharing prompt_cache_key (default 1)")
    p.add_argument("--efforts", type=str, default=",".join(EFFORTS), help=f"comma-sep reasoning efforts (default: {','.join(EFFORTS)})")
    p.add_argument("--no-minimax", action="store_true", help="skip MiniMax benchmark")
    p.add_argument("--paths", type=str, default=None,
                   help="comma-sep path ids to test. GPT: http-cli-ws, ws-cli-upstream, http-cli-upstream, http-direct-upstream, ws-direct-upstream (default: all 5). "
                        "GLM: zai-raw, ollama-raw, glm-proxy (only run when explicitly selected). "
                        "e.g. --paths zai-raw,ollama-raw,glm-proxy --efforts max --no-minimax")
    p.add_argument("--output", type=str, default=None,
                   help="save full console output to this file (default: auto-named under ~/.factory/bin/bench-runs/)")
    p.add_argument("--no-output", action="store_true", help="disable run-file saving")
    args = p.parse_args()
    efforts = [e.strip() for e in args.efforts.split(",") if e.strip()]

    output: str | None = None
    if not args.no_output:
        if args.output:
            output = args.output
        else:
            ts = datetime.now().strftime("%Y-%m-%d_%H-%M-%S")
            tag = f"turns{args.turns}"
            out_dir = Path.home() / ".factory" / "bin" / "bench-runs"
            out_dir.mkdir(parents=True, exist_ok=True)
            output = str(out_dir / f"bench-{ts}-{tag}.txt")
    path_filter: list[str] | None = None
    if args.paths:
        path_filter = [p.strip() for p in args.paths.split(",") if p.strip()]
    return args.turns, efforts, args.no_minimax, output, path_filter


async def main() -> int:
    turns, efforts, skip_minimax, output_path, path_filter = _parse_args()

    gpt_sema = asyncio.Semaphore(GPT_CONCURRENCY)
    run_start = time.monotonic()

    # Live display — rebuilds every 4 Hz showing currently-running tasks.
    # Completed tasks are printed above the live region via console.print.
    progress = LiveProgress(total=0)  # total set after combos built

    # Shared accumulator: every bench call pushes its Result here as soon as it
    # finishes (ok or failed). On Ctrl-C, we still have all completed results.
    accumulator: list[Result] = []

    def _completion_line(prefix: str, display: str, body: str) -> None:
        t = time.monotonic() - run_start
        console.print(f"{prefix} t+{t:5.1f}s  [{progress.done:2d}/{progress.total}] {display:<28} {body}")

    async def run_combo(path_id: str, fn: Any, effort: str, fast: bool) -> list[Result]:
        """Run `turns` sequential benchmarks of one combo as a simulated conversation.

        Turn 1: input = [user: PROMPT]
        Turn 2: input = [user: PROMPT, assistant: T1_text, user: CONTINUE]
        Turn N: input = T(N-1)_input + assistant: T(N-1)_text + user: CONTINUE

        Sharing prompt_cache_key across turns + growing input lets the bridge
        detect shared prefix and delta-send only new items. Non-bridge paths
        re-send full input (0% cache) every turn — that contrast is the point.
        """
        session_key = f"b-{uuid.uuid4().hex[:16]}"
        tier_tag = "fast" if fast else "norm"
        results: list[Result] = []
        async with gpt_sema:
            display_head = f"{PATH_META[path_id]['short']:<14} {effort:>6} {tier_tag:<4}"
            # Seed input with the initial user prompt; extend each turn.
            turn_input: list[dict[str, Any]] = _default_input()
            for turn_idx in range(1, turns + 1):
                label = f"{GPT_MODEL}{' fast' if fast else ''}({effort})"
                display = f"{display_head} t{turn_idx}/{turns}"
                progress.start(display)
                try:
                    r = await asyncio.wait_for(
                        fn(effort, fast, label, session_key=session_key, turn=turn_idx,
                           input_messages=turn_input),
                        timeout=PER_TASK_TIMEOUT_S,
                    )
                except asyncio.TimeoutError:
                    r = Result(label=label, path=path_id, model=GPT_MODEL, effort=effort,
                               fast=fast, turn=turn_idx, ok=False,
                               error=f"timeout after {PER_TASK_TIMEOUT_S:.0f}s (upstream likely dead; no keepalive)")
                results.append(r)
                accumulator.append(r)  # for partial-result recovery on Ctrl-C
                progress.finish(display)
                # Build next turn's input from this response (only if we have one).
                if r.ok and r.response_text:
                    turn_input = _extend_input_with_turn(turn_input, r.response_text)
                elif turn_idx < turns:
                    # If this turn failed, we can't meaningfully continue the convo;
                    # remaining turns will repeat the last good input (or the initial
                    # prompt if turn 1 failed). Better than aborting the whole combo.
                    pass
                if r.ok:
                    cache_str = f"cache={r.cache_pct:4.0f}%" if r.input_tokens else "cache= n/a"
                    _completion_line(
                        "[green]✓[/green]",
                        display,
                        f"[cyan]{r.tps:6.1f}[/cyan] tok/s  ttft={r.ttft_s:5.2f}s  in={r.input_tokens:>5}  out={r.output_tokens:>4}  reas={r.reasoning_tokens:>4}  {cache_str}",
                    )
                else:
                    _completion_line("[red]✗[/red]", display, f"[red]{r.error[:100]}[/red]")
        return results

    async def run_minimax() -> list[Result]:
        results: list[Result] = []
        for turn_idx in range(1, turns + 1):
            display = f"{PATH_META['anthropic-direct']['short']:<14}        t{turn_idx}/{turns}"
            progress.start(display)
            try:
                r = await asyncio.wait_for(
                    bench_minimax(MINIMAX_MODEL, turn=turn_idx),
                    timeout=PER_TASK_TIMEOUT_S,
                )
            except asyncio.TimeoutError:
                r = Result(label=MINIMAX_MODEL, path="anthropic-direct", model=MINIMAX_MODEL,
                           effort="n/a", fast=False, turn=turn_idx, ok=False,
                           error=f"timeout after {PER_TASK_TIMEOUT_S:.0f}s")
            results.append(r)
            accumulator.append(r)
            progress.finish(display)
            if r.ok:
                _completion_line(
                    "[green]✓[/green]",
                    display,
                    f"[cyan]{r.tps:6.1f}[/cyan] tok/s  ttft={r.ttft_s:5.2f}s  out={r.output_tokens:>4}",
                )
            else:
                _completion_line("[red]✗[/red]", display, f"[red]{r.error[:100]}[/red]")
        return results

    async def run_glm_combo(target_id: str, effort: str) -> list[Result]:
        """Run `turns` sequential GLM benchmarks for one (target × effort)."""
        session_key = f"glm-{uuid.uuid4().hex[:12]}"
        results: list[Result] = []
        async with gpt_sema:
            display_head = f"{PATH_META[target_id]['short']:<14} {effort:>6}     "
            for turn_idx in range(1, turns + 1):
                label = f"{target_id}({effort})"
                display = f"{display_head} t{turn_idx}/{turns}"
                progress.start(display)
                try:
                    r = await asyncio.wait_for(
                        bench_glm(target_id, effort, label, session_key=session_key, turn=turn_idx),
                        timeout=PER_TASK_TIMEOUT_S,
                    )
                except asyncio.TimeoutError:
                    r = Result(label=label, path=target_id, model=GLM_TARGETS[target_id]["model"],
                               effort=effort, fast=False, turn=turn_idx, ok=False,
                               error=f"timeout after {PER_TASK_TIMEOUT_S:.0f}s")
                results.append(r)
                accumulator.append(r)
                progress.finish(display)
                if r.ok:
                    _completion_line(
                        "[green]✓[/green]", display,
                        f"[cyan]{r.tps:6.1f}[/cyan] tok/s  ttft={r.ttft_s:5.2f}s  out={r.output_tokens:>4}  reas={r.reasoning_tokens:>4}  text/s={r.tps_text:5.1f}",
                    )
                else:
                    _completion_line("[red]✗[/red]", display, f"[red]{r.error[:100]}[/red]")
        return results

    # Build combo list (optionally filtered to a subset of paths via --paths).
    combos: list = []
    for path_id, _, _ in GPT_PATHS:
        if path_filter is not None and path_id not in path_filter:
            continue
        fn = BENCH_FNS[path_id]
        for fast in (False, True):
            for effort in efforts:
                combos.append(run_combo(path_id, fn, effort, fast))
    # GLM targets (z.ai / ollama raw + via proxy). Only when selected via --paths.
    for target_id in GLM_PATHS:
        if path_filter is not None and target_id in path_filter:
            for effort in efforts:
                combos.append(run_glm_combo(target_id, effort))
    if not skip_minimax and path_filter is None:
        combos.append(run_minimax())

    total_count = len(combos) * turns
    progress.total = total_count
    console.print(
        f"[bold]Firing {len(combos)} combos × {turns} turn(s) = {total_count} benchmarks[/bold]\n"
        f"  GPT concurrency: {GPT_CONCURRENCY}   efforts: {efforts}   multi-turn: {turns}   "
        f"minimax: {'skip' if skip_minimax else 'included'}\n"
    )
    for path_id, meta in PATH_META.items():
        console.print(f"  [{meta['color']}]{meta['short']:<14}[/{meta['color']}] — {meta['note']}")
    console.print()

    run_start = time.monotonic()
    progress.started_at = run_start
    interrupted = False
    try:
        # Live wraps the in-flight status at the bottom. console.print() calls
        # inside the block print ABOVE the live region (permanent scrollback).
        with Live(progress, console=console, refresh_per_second=4, transient=False):
            await asyncio.gather(*combos)
    except (KeyboardInterrupt, asyncio.CancelledError):
        interrupted = True
        console.print("\n[yellow]⚠ interrupted — building report from results captured so far…[/yellow]")
    wall = time.monotonic() - run_start
    # Results arrived in accumulator in completion order; that's fine for analysis.
    all_results: list[Result] = list(accumulator)
    if interrupted:
        console.print(f"[yellow]Partial run: {len(all_results)}/{total_count} result(s) captured before interrupt.[/yellow]\n")

    console.print(f"\n[bold]Wall time: {wall:.1f}s[/bold]\n")

    # ─── Results table — grouped by path then effort then tier, sorted by tok/s within path ───
    ok = [r for r in all_results if r.ok]
    fail = [r for r in all_results if not r.ok]

    path_order = [p for p, _, _ in GPT_PATHS] + ["anthropic-direct"] + GLM_PATHS

    def sort_key(r: Result) -> tuple:
        path_rank = path_order.index(r.path) if r.path in path_order else 99
        effort_rank = efforts.index(r.effort) if r.effort in efforts else 99
        return (path_rank, 0 if not r.fast else 1, effort_rank, r.turn)

    tbl = Table(
        title="Full results  (TPS = output_tok / e2e_wall per NVIDIA GenAI-Perf)",
        caption="TTFT=time to first token · WALL=e2e latency · TPOT=inter-token latency (ms/tok) · TEXT/S=streaming-tail speed",
        show_lines=False, header_style="bold cyan", expand=False,
        pad_edge=False, padding=(0, 1),
    )
    tbl.add_column("PATH", style="white", no_wrap=True)
    tbl.add_column("TIER", no_wrap=True)
    tbl.add_column("EFFORT", style="yellow", no_wrap=True)
    tbl.add_column("T", justify="right", no_wrap=True)
    tbl.add_column("TTFT", justify="right", no_wrap=True)
    tbl.add_column("WALL", justify="right", no_wrap=True)
    tbl.add_column("IN", justify="right", no_wrap=True)
    tbl.add_column("CACHED%", justify="right", style="cyan", no_wrap=True)
    tbl.add_column("OUT", justify="right", no_wrap=True)
    tbl.add_column("REAS", justify="right", no_wrap=True)
    tbl.add_column("TPS", justify="right", style="bold green", no_wrap=True)
    tbl.add_column("PERCEIVED", justify="right", style="bold magenta", no_wrap=True)
    tbl.add_column("TPOT", justify="right", no_wrap=True)
    tbl.add_column("TEXT/S", justify="right", style="dim", no_wrap=True)
    tbl.add_column("STATUS", no_wrap=True)

    last_path = None
    for r in sorted(all_results, key=sort_key):
        if last_path is not None and r.path != last_path:
            tbl.add_section()
        last_path = r.path
        if r.ok:
            tbl.add_row(
                _fmt_path(r.path),
                "[bold]fast[/bold]" if r.fast else "norm",
                r.effort,
                str(r.turn),
                f"{r.ttft_s:5.2f}",
                f"{r.e2e_s:5.2f}",
                f"{r.input_tokens}",
                f"{r.cache_pct:4.1f}" if r.input_tokens else "-",
                f"{r.output_tokens}",
                f"{r.reasoning_tokens}",
                f"{r.tps:5.1f}",
                f"{r.tps_perceived:5.1f}",
                f"{r.tpot_ms:4.1f}",
                f"{r.tps_text:5.1f}",
                "[green]ok[/green]",
            )
        else:
            tbl.add_row(
                _fmt_path(r.path), "fast" if r.fast else "norm", r.effort, str(r.turn),
                "-", "-", "-", "-", "-", "-", "-", "-", "-", "-",
                f"[red]{r.error[:40]}[/red]",
            )
    console.print(tbl)

    if not ok:
        console.print("\n[red](no successful runs — nothing to analyze)[/red]")
        return 2

    # ─── Per-path × tier means (tok/s + cache%) ───
    cat = Table(
        title="Per-path × tier aggregates  (TPS per NVIDIA canonical formula: output/e2e)",
        caption="TTFT/WALL in seconds; TPOT = inter-token latency in ms; TEXT/S = streaming speed after TTFT",
        header_style="bold cyan",
        show_lines=False,
        pad_edge=False,
        padding=(0, 1),
    )
    cat.add_column("PATH", no_wrap=True)
    cat.add_column("TIER", no_wrap=True)
    cat.add_column("N", justify="right", no_wrap=True)
    cat.add_column("TPS", justify="right", style="bold green", no_wrap=True)
    cat.add_column("PERCEIVED", justify="right", style="bold magenta", no_wrap=True)
    cat.add_column("MED", justify="right", no_wrap=True)
    cat.add_column("TTFT", justify="right", no_wrap=True)
    cat.add_column("WALL", justify="right", no_wrap=True)
    cat.add_column("TPOT", justify="right", no_wrap=True)
    cat.add_column("TEXT/S", justify="right", style="dim", no_wrap=True)
    cat.add_column("CACHE%", justify="right", style="cyan", no_wrap=True)

    cats: dict[tuple[str, bool], list[Result]] = {}
    for r in ok:
        cats.setdefault((r.path, r.fast), []).append(r)
    for path in path_order:
        for fast in (False, True):
            rs = cats.get((path, fast), [])
            if not rs:
                continue
            tpses = [r.tps for r in rs]
            perceiveds = [r.tps_perceived for r in rs]
            ttfts = [r.ttft_s for r in rs]
            walls = [r.e2e_s for r in rs]
            tpots = [r.tpot_ms for r in rs if r.tpot_ms > 0]
            tps_texts = [r.tps_text for r in rs if r.tps_text > 0]
            cache_pcts = [r.cache_pct for r in rs if r.input_tokens]
            cat.add_row(
                _fmt_path(path),
                "[bold]fast[/bold]" if fast else "norm",
                str(len(rs)),
                f"{statistics.mean(tpses):5.1f}",
                f"{statistics.mean(perceiveds):5.1f}",
                f"{statistics.median(tpses):5.1f}",
                f"{statistics.mean(ttfts):6.2f}",
                f"{statistics.mean(walls):6.2f}",
                f"{statistics.mean(tpots):5.1f}" if tpots else "-",
                f"{statistics.mean(tps_texts):5.1f}" if tps_texts else "-",
                f"{statistics.mean(cache_pcts):4.1f}" if cache_pcts else "-",
            )
    console.print(cat)

    # ─── Pairwise deltas ───
    def mean_tps(path: str, fast: bool | None = None) -> float | None:
        vals = [r.tps for r in ok if r.path == path and (fast is None or r.fast == fast)]
        return statistics.mean(vals) if vals else None

    def delta_pct(base: float | None, cmp: float | None) -> str | None:
        if base is None or cmp is None or base == 0:
            return None
        d = (cmp - base) / base * 100
        return f"[{'green' if d > 0 else 'red'}]{d:+.1f}%[/{'green' if d > 0 else 'red'}]"

    comp = Table(title="Pairwise deltas (mean tok/s)", header_style="bold cyan")
    comp.add_column("COMPARISON")
    comp.add_column("NORMAL", justify="right")
    comp.add_column("FAST", justify="right")

    def add_row(label: str, base_path: str, cmp_path: str) -> None:
        n = delta_pct(mean_tps(base_path, False), mean_tps(cmp_path, False))
        f = delta_pct(mean_tps(base_path, True),  mean_tps(cmp_path, True))
        comp.add_row(label, n or "-", f or "-")

    add_row("http-cli-ws    vs  http-cli-upstream (bridge overhead on HTTP)",        "http-cli-ws",         "http-cli-upstream")
    add_row("http-cli-upstream vs http-direct-upstream (proxy HTTP overhead)",       "http-cli-upstream",   "http-direct-upstream")
    add_row("ws-cli-upstream  vs ws-direct-upstream  (proxy WS overhead)",           "ws-cli-upstream",     "ws-direct-upstream")
    add_row("http-cli-ws    vs  ws-cli-upstream (WS client-side via bridge)",        "http-cli-ws",         "ws-cli-upstream")
    add_row("http-direct-upstream vs ws-direct-upstream (direct HTTP vs direct WS)", "http-direct-upstream","ws-direct-upstream")
    console.print(comp)

    # ─── Fast-vs-normal uplift per path ───
    uplift = Table(title="Fast tier uplift vs normal (same path)", header_style="bold cyan")
    uplift.add_column("PATH")
    uplift.add_column("DELTA", justify="right", style="bold")
    for path in path_order:
        d = delta_pct(mean_tps(path, False), mean_tps(path, True))
        if d:
            uplift.add_row(_fmt_path(path), d)
    console.print(uplift)

    # ─── Multi-turn trajectory (only meaningful when --turns > 1) ───
    if turns > 1:
        traj = Table(title=f"Per-turn trajectory — TTFT / cache% across {turns} turns (same session_key)", header_style="bold cyan")
        traj.add_column("PATH")
        traj.add_column("TIER")
        traj.add_column("EFFORT", style="yellow")
        for t_idx in range(1, turns + 1):
            traj.add_column(f"T{t_idx} ttft", justify="right")
            traj.add_column(f"T{t_idx} cache%", justify="right", style="cyan")
            traj.add_column(f"T{t_idx} tok/s", justify="right", style="green")

        by_combo: dict[tuple[str, bool, str], list[Result]] = {}
        for r in all_results:
            by_combo.setdefault((r.path, r.fast, r.effort), []).append(r)

        for path in path_order:
            for fast in (False, True):
                for effort in efforts:
                    rs = sorted(by_combo.get((path, fast, effort), []), key=lambda x: x.turn)
                    if not rs:
                        continue
                    row = [_fmt_path(path), "fast" if fast else "norm", effort]
                    for r in rs:
                        if r.ok:
                            row.extend([
                                f"{r.ttft_s:5.2f}",
                                f"{r.cache_pct:4.1f}" if r.input_tokens else "-",
                                f"{r.tps:5.1f}",
                            ])
                        else:
                            row.extend(["-", "-", "-"])
                    # pad missing turns
                    while len(row) < 3 + turns * 3:
                        row.extend(["-", "-", "-"])
                    traj.add_row(*row)
        console.print(traj)

    # ─── MiniMax summary + fastest ───
    mm = [r for r in ok if r.path == "anthropic-direct"]
    if mm:
        console.print(f"\n[green]{MINIMAX_MODEL}:[/green] mean [bold green]{statistics.mean([r.tps for r in mm]):.1f}[/bold green] tok/s across {len(mm)} turn(s)")

    fastest = max(ok, key=lambda r: r.tps)
    tier = "fast" if fastest.fast else "normal"
    console.print(
        f"\n[bold]fastest:[/bold] {fastest.model} {tier}({fastest.effort}) via "
        f"{_fmt_path(fastest.path)}  →  [bold green]{fastest.tps:.1f}[/bold green] tok/s "
        f"(ttft={fastest.ttft_s:.2f}s, out={fastest.output_tokens}, reas={fastest.reasoning_tokens})"
    )

    # ─── Total token usage for the run ───
    total_in      = sum(r.input_tokens     for r in ok)
    total_cached  = sum(r.cached_tokens    for r in ok)
    total_out     = sum(r.output_tokens    for r in ok)
    total_reas    = sum(r.reasoning_tokens for r in ok)
    total_text    = total_out - total_reas
    billed_in     = total_in - total_cached

    tok = Table(
        title="Token usage (totals across all successful samples)",
        header_style="bold cyan", show_lines=False, pad_edge=False, padding=(0, 1),
    )
    tok.add_column("CATEGORY", no_wrap=True)
    tok.add_column("TOKENS", justify="right", style="bold green", no_wrap=True)
    tok.add_column("NOTE", style="dim")
    tok.add_row("input (raw)",      f"{total_in:>10,}", "sum of all input_tokens")
    tok.add_row("input (cached)",   f"{total_cached:>10,}", f"{(total_cached/total_in*100 if total_in else 0):.1f}% of raw input — free")
    tok.add_row("input (billed)",   f"{billed_in:>10,}", "raw − cached")
    tok.add_section()
    tok.add_row("output (text)",    f"{total_text:>10,}", "visible assistant text")
    tok.add_row("output (reasoning)", f"{total_reas:>10,}", f"{(total_reas/total_out*100 if total_out else 0):.1f}% of total output" if total_out else "")
    tok.add_row("output (total)",   f"{total_out:>10,}", "text + reasoning")
    tok.add_section()
    tok.add_row("[bold]total on wire[/bold]", f"[bold]{(total_in + total_out):>10,}[/bold]", "input + output")
    tok.add_row("[bold]total billed[/bold]",  f"[bold]{(billed_in + total_out):>10,}[/bold]", "(input − cached) + output")
    console.print()
    console.print(tok)

    # Per-path totals (just output and reasoning — most useful for cost attribution)
    path_totals: dict[str, dict[str, int]] = {}
    for r in ok:
        d = path_totals.setdefault(r.path, {"n": 0, "in": 0, "cached": 0, "out": 0, "reas": 0})
        d["n"] += 1
        d["in"] += r.input_tokens
        d["cached"] += r.cached_tokens
        d["out"] += r.output_tokens
        d["reas"] += r.reasoning_tokens

    pt = Table(
        title="Token usage by path",
        header_style="bold cyan", show_lines=False, pad_edge=False, padding=(0, 1),
    )
    pt.add_column("PATH", no_wrap=True)
    pt.add_column("N", justify="right", no_wrap=True)
    pt.add_column("IN raw", justify="right", no_wrap=True)
    pt.add_column("IN cached", justify="right", no_wrap=True, style="cyan")
    pt.add_column("IN billed", justify="right", no_wrap=True)
    pt.add_column("OUT text", justify="right", no_wrap=True)
    pt.add_column("OUT reas", justify="right", no_wrap=True, style="dim")
    pt.add_column("OUT total", justify="right", no_wrap=True, style="bold green")
    for p in path_order:
        d = path_totals.get(p)
        if not d:
            continue
        pt.add_row(
            _fmt_path(p),
            str(d["n"]),
            f"{d['in']:,}",
            f"{d['cached']:,}",
            f"{d['in'] - d['cached']:,}",
            f"{d['out'] - d['reas']:,}",
            f"{d['reas']:,}",
            f"{d['out']:,}",
        )
    console.print(pt)

    if fail:
        console.print(f"\n[red]{len(fail)} failure(s); see STATUS column above for details.[/red]")

    # Dump full recorded console output to the run file if requested.
    if output_path:
        try:
            Path(output_path).write_text(console.export_text(clear=False, styles=False))
            console.print(f"\n[dim]run saved → {output_path}[/dim]")
        except Exception as e:
            console.print(f"\n[yellow]WARN: failed to save run file: {e}[/yellow]")

    return 0 if not fail else 2


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
