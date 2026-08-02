#!/usr/bin/env python3
"""列出当前环境的 Codex / Claude Code / pi / OpenCode 会话(session)。

支持四种 AI 编码工具的会话：
  - Codex:
      - 首选读取 ~/.codex/state_*.sqlite 的 threads 表
      - 没有 state_*.sqlite 时，回退读取 ~/.codex/sessions/**/rollout-*.jsonl
      - CODEX_HOME 可覆盖 ~/.codex；CODEX_SQLITE_HOME 可单独覆盖 SQLite 目录
  - Claude Code:
      - 读取 ~/.claude/projects/<cwd>/*.jsonl
      - CLAUDE_HOME 可覆盖 ~/.claude
  - pi:
      - 读取 ~/.pi/agent/sessions/<cwd>/*.jsonl
      - PI_CODING_AGENT_DIR 可覆盖 ~/.pi/agent
  - OpenCode:
      - 首选读取 Linux 下 ~/.local/share/opencode/opencode*.db
      - 没有数据库时，回退读取 ~/.local/share/opencode/storage/session/info/*.json
      - OPENCODE_DATA_HOME 可覆盖 OpenCode 数据目录
      - OPENCODE_DB 可直接指定数据库文件

用法示例：
  python3 scripts/codex_sessions.py                  # 最近 20 条（所有工具）
  python3 scripts/codex_sessions.py -n 50            # 最近 50 条
  python3 scripts/codex_sessions.py --all            # 全部
  python3 scripts/codex_sessions.py --tool claude    # 只看 Claude Code
  python3 scripts/codex_sessions.py --tool codex     # 只看 Codex
  python3 scripts/codex_sessions.py --tool pi        # 只看 pi
  python3 scripts/codex_sessions.py --tool opencode  # 只看 OpenCode
  python3 scripts/codex_sessions.py --cwd apid       # 按工作目录过滤
  python3 scripts/codex_sessions.py --since 2026-07-01 # 只看某时间之后更新的会话
  python3 scripts/codex_sessions.py --json           # JSON 输出
  python3 scripts/codex_sessions.py --sort created   # 按创建时间排序
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sqlite3
import sys
from dataclasses import asdict, dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path

try:
    from rich.console import Console
    from rich.table import Table
    from rich.text import Text
    from rich import box
    from rich.markup import escape
    HAS_RICH = True
except ImportError:
    HAS_RICH = False


# ── 路径探测 ──────────────────────────────────────────────────────────────

def find_codex_home() -> Path:
    """返回 Codex 配置目录，等价于 Rust 端的 find_codex_home()。"""
    env = os.environ.get("CODEX_HOME", "").strip()
    if env:
        p = Path(env).expanduser()
        if not p.is_dir():
            sys.exit(f"CODEX_HOME 指向 {p}，但该路径不存在或不是目录")
        return p.resolve()
    return Path.home() / ".codex"


def find_claude_home() -> Path:
    """返回 Claude Code 配置目录。"""
    env = os.environ.get("CLAUDE_HOME", "").strip()
    if env:
        return Path(env).expanduser().resolve()
    return Path.home() / ".claude"


def find_pi_home() -> Path:
    """返回 pi coding-agent 配置目录。"""
    env = os.environ.get("PI_CODING_AGENT_DIR", "").strip()
    if env:
        return Path(env).expanduser().resolve()
    return Path.home() / ".pi" / "agent"


def find_opencode_data_home() -> Path:
    """按 OpenCode 的 xdg-basedir 规则返回数据目录。"""
    env = os.environ.get("OPENCODE_DATA_HOME", "").strip()
    if env:
        return Path(env).expanduser().resolve()
    if sys.platform == "darwin":
        return Path.home() / "Library" / "Application Support" / "opencode"
    if os.name == "nt":
        return Path(os.environ.get("APPDATA", str(Path.home() / "AppData" / "Roaming"))) / "opencode"
    return Path(os.environ.get("XDG_DATA_HOME", str(Path.home() / ".local" / "share"))) / "opencode"


def find_opencode_db() -> Path | None:
    """返回 OpenCode 当前数据库；OPENCODE_DB 可指定绝对/相对路径。"""
    data_home = find_opencode_data_home()
    configured = os.environ.get("OPENCODE_DB", "").strip()
    if configured:
        path = Path(configured).expanduser()
        if not path.is_absolute():
            path = data_home / path
        return path if path.is_file() else None

    candidates = sorted(
        data_home.glob("opencode*.db"),
        key=lambda p: p.stat().st_mtime_ns if p.exists() else 0,
        reverse=True,
    )
    return candidates[0] if candidates else None


def find_sqlite_home(codex_home: Path) -> Path:
    """SQLite 数据库目录，可被 CODEX_SQLITE_HOME 单独覆盖。"""
    env = os.environ.get("CODEX_SQLITE_HOME", "").strip()
    if env:
        return Path(env).expanduser().resolve()
    return codex_home


def find_state_db(sqlite_home: Path) -> Path | None:
    """查找 state_*.sqlite，取版本号最大的那个。"""
    candidates = sorted(
        sqlite_home.glob("state_*.sqlite"),
        key=lambda p: _extract_version(p.name),
        reverse=True,
    )
    return candidates[0] if candidates else None


def _extract_version(filename: str) -> int:
    """从 state_5.sqlite 提取 5；无法解析返回 0。"""
    stem = Path(filename).stem
    parts = stem.rsplit("_", 1)
    if len(parts) == 2 and parts[1].isdigit():
        return int(parts[1])
    return 0


# ── 数据模型 ──────────────────────────────────────────────────────────────

@dataclass
class Session:
    id: str
    title: str
    created_at: str
    updated_at: str
    source: str
    model_provider: str
    cwd: str
    model: str
    reasoning_effort: str
    tokens_used: int
    archived: bool
    cli_version: str
    rollout_path: str
    cache_hit_rate: float | None = None
    tool: str = "codex"  # "codex" | "claude"
    input_tokens: int = 0
    output_tokens: int = 0
    cache_read_tokens: int = 0
    cache_write_tokens: int = 0


def _int_value(value: object) -> int:
    try:
        return max(0, int(value or 0))
    except (TypeError, ValueError):
        return 0


def _usage_fields(usage: object) -> tuple[int, int, int, int, int]:
    """返回 input, output, cache_read, cache_write, total。"""
    if not isinstance(usage, dict):
        return 0, 0, 0, 0, 0
    input_tokens = _int_value(usage.get("input_tokens", usage.get("input")))
    output_tokens = _int_value(usage.get("output_tokens", usage.get("output")))
    cache_read = _int_value(usage.get("cached_input_tokens", usage.get("cacheRead")))
    cache_write = _int_value(usage.get("cache_write_input_tokens", usage.get("cacheWrite")))
    cache = usage.get("cache")
    if isinstance(cache, dict):
        cache_read = max(cache_read, _int_value(cache.get("read")))
        cache_write = max(cache_write, _int_value(cache.get("write")))
    total = _int_value(usage.get("total_tokens", usage.get("totalTokens")))
    if not total:
        total = input_tokens + output_tokens + cache_read + cache_write
    return input_tokens, output_tokens, cache_read, cache_write, total


def _set_token_fields(
    session: Session,
    input_tokens: int,
    output_tokens: int,
    cache_read: int,
    cache_write: int,
    total: int | None = None,
) -> None:
    session.input_tokens = max(0, input_tokens)
    session.output_tokens = max(0, output_tokens)
    session.cache_read_tokens = max(0, cache_read)
    session.cache_write_tokens = max(0, cache_write)
    session.tokens_used = max(
        0,
        total if total is not None and total > 0
        else session.input_tokens
        + session.output_tokens
        + session.cache_read_tokens
        + session.cache_write_tokens,
    )
    denominator = session.input_tokens + session.cache_read_tokens + session.cache_write_tokens
    if denominator > 0:
        session.cache_hit_rate = max(0.0, min(1.0, session.cache_read_tokens / denominator))


def _set_codex_token_fields(
    session: Session,
    fields: tuple[int, int, int, int, int],
) -> None:
    """设置 Codex usage。

    Codex 的 ``input_tokens`` 是该请求的总输入量，``cached_input_tokens``
    是总输入中的缓存命中子集，不能把两者相加作为命中率分母。
    """
    input_tokens, output_tokens, cache_read, cache_write, total = fields
    _set_token_fields(
        session,
        input_tokens,
        output_tokens,
        cache_read,
        cache_write,
        total,
    )
    if session.input_tokens > 0:
        session.cache_hit_rate = max(
            0.0,
            min(1.0, session.cache_read_tokens / session.input_tokens),
        )


def _cache_rate_denominator(session: Session) -> int:
    """返回单个会话缓存命中率的分母，按各工具的 usage 口径计算。"""
    if session.tool == "codex":
        # Codex input_tokens 已包含 cached_input_tokens。
        return session.input_tokens
    # Claude/pi/OpenCode 的 input 通常表示未命中输入，缓存字段需另计。
    return session.input_tokens + session.cache_read_tokens + session.cache_write_tokens


# ── Codex: 状态数据库读取 ─────────────────────────────────────────────────

def load_from_state_db(db_path: Path) -> list[Session]:
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    conn.row_factory = sqlite3.Row
    try:
        rows = conn.execute(
            """
            SELECT id, title, created_at, updated_at, source, model_provider,
                   cwd, model, reasoning_effort, tokens_used, archived,
                   cli_version, rollout_path
              FROM threads
            """
        ).fetchall()
    finally:
        conn.close()

    sessions = []
    for r in rows:
        sessions.append(Session(
            id=r["id"],
            title=r["title"] or "(无标题)",
            created_at=_ts_to_local(r["created_at"]),
            updated_at=_ts_to_local(r["updated_at"]),
            source=r["source"] or "",
            model_provider=r["model_provider"] or "",
            cwd=r["cwd"] or "",
            model=r["model"] or "",
            reasoning_effort=r["reasoning_effort"] or "",
            tokens_used=r["tokens_used"] or 0,
            archived=bool(r["archived"]),
            cli_version=r["cli_version"] or "",
            rollout_path=r["rollout_path"] or "",
            tool="codex",
        ))
    return sessions


# ── Codex: JSONL 文件回退读取 ─────────────────────────────────────────────

def load_from_jsonl_files(codex_home: Path) -> list[Session]:
    sessions_dir = codex_home / "sessions"
    if not sessions_dir.is_dir():
        return []

    sessions = []
    for path in sorted(sessions_dir.rglob("rollout-*.jsonl")):
        try:
            with open(path, "r", errors="replace") as f:
                first_line = f.readline()
            entry = json.loads(first_line)
        except (json.JSONDecodeError, OSError):
            continue
        if entry.get("type") != "session_meta":
            continue
        meta = entry.get("payload") or {}
        ts_str = meta.get("timestamp", "")
        created = _parse_iso(ts_str)
        sessions.append(Session(
            id=meta.get("id", ""),
            title=meta.get("cwd", "").split("/")[-1] or "(无标题)",
            created_at=created,
            updated_at=created,
            source=meta.get("source", ""),
            model_provider=meta.get("model_provider", "") or "",
            cwd=meta.get("cwd", ""),
            model="",
            reasoning_effort="",
            tokens_used=0,
            archived=False,
            cli_version=meta.get("cli_version", ""),
            rollout_path=str(path),
            tool="codex",
        ))
    return sessions


# ── OpenCode: SQLite / legacy JSON storage 读取 ────────────────────────────

def _opencode_model_name(value: object) -> tuple[str, str]:
    if isinstance(value, str):
        raw = value.strip()
        if raw.startswith("{"):
            try:
                value = json.loads(raw)
            except json.JSONDecodeError:
                return raw, ""
        else:
            return raw, ""
    if not isinstance(value, dict):
        return "", ""
    model_id = str(value.get("id") or value.get("modelID") or "")
    provider = str(value.get("providerID") or value.get("provider") or "")
    if provider and model_id:
        return f"{provider}/{model_id}", provider
    return model_id or provider, provider


def _load_opencode_db(db_path: Path) -> list[Session]:
    try:
        conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
        conn.row_factory = sqlite3.Row
    except sqlite3.Error:
        return []

    try:
        columns = {
            row["name"]
            for row in conn.execute("PRAGMA table_info(session)").fetchall()
        }
        required = {"id", "directory", "title", "time_created", "time_updated"}
        if not required.issubset(columns):
            return []

        selected = [
            "id", "directory", "title", "time_created", "time_updated",
            "time_archived", "version", "agent", "model",
        ]
        token_columns = [
            "cost", "tokens_input", "tokens_output", "tokens_reasoning",
            "tokens_cache_read", "tokens_cache_write",
        ]
        selected.extend(column for column in token_columns if column in columns)
        rows = conn.execute(f"SELECT {', '.join(selected)} FROM session").fetchall()
    except sqlite3.Error:
        return []
    finally:
        conn.close()

    sessions: list[Session] = []
    for row in rows:
        model, provider = _opencode_model_name(row["model"])
        input_tokens = _int_value(row["tokens_input"]) if "tokens_input" in columns else 0
        output_tokens = _int_value(row["tokens_output"]) if "tokens_output" in columns else 0
        cache_read = _int_value(row["tokens_cache_read"]) if "tokens_cache_read" in columns else 0
        cache_write = _int_value(row["tokens_cache_write"]) if "tokens_cache_write" in columns else 0
        reasoning = _int_value(row["tokens_reasoning"]) if "tokens_reasoning" in columns else 0
        total = input_tokens + output_tokens + reasoning + cache_read + cache_write
        session = Session(
            id=str(row["id"] or ""),
            title=str(row["title"] or "(无标题)"),
            created_at=_ms_to_local(row["time_created"]),
            updated_at=_ms_to_local(row["time_updated"]),
            source="",
            model_provider=provider,
            cwd=str(row["directory"] or ""),
            model=model,
            reasoning_effort="",
            tokens_used=total,
            archived=bool(row["time_archived"]),
            cli_version=str(row["version"] or ""),
            rollout_path=str(db_path),
            tool="opencode",
            input_tokens=input_tokens,
            output_tokens=output_tokens + reasoning,
            cache_read_tokens=cache_read,
            cache_write_tokens=cache_write,
        )
        if total > 0:
            _set_token_fields(session, input_tokens, output_tokens + reasoning, cache_read, cache_write, total)
        sessions.append(session)
    return sessions


def _load_opencode_legacy(data_home: Path) -> list[Session]:
    """兼容数据库迁移前的 storage/session/info/*.json 格式。"""
    info_dir = data_home / "storage" / "session" / "info"
    if not info_dir.is_dir():
        return []
    sessions: list[Session] = []
    for path in info_dir.glob("*.json"):
        try:
            info = json.loads(path.read_text(errors="replace"))
        except (json.JSONDecodeError, OSError):
            continue
        if not isinstance(info, dict) or not info.get("id"):
            continue
        session_id = str(info["id"])
        model, provider = _opencode_model_name(info.get("model"))
        tokens = info.get("tokens") or {}
        cache = tokens.get("cache") if isinstance(tokens, dict) else {}
        input_tokens = _int_value(tokens.get("input")) if isinstance(tokens, dict) else 0
        output_tokens = _int_value(tokens.get("output")) if isinstance(tokens, dict) else 0
        reasoning = _int_value(tokens.get("reasoning")) if isinstance(tokens, dict) else 0
        cache_read = _int_value(cache.get("read")) if isinstance(cache, dict) else 0
        cache_write = _int_value(cache.get("write")) if isinstance(cache, dict) else 0
        created = _nested_time(info, "created")
        updated = _nested_time(info, "updated") or created
        session = Session(
            id=session_id,
            title=str(info.get("title") or "(无标题)"),
            created_at=_ms_to_local(created),
            updated_at=_ms_to_local(updated),
            source="",
            model_provider=provider,
            cwd=str((info.get("path") or {}).get("cwd") or info.get("directory") or ""),
            model=model,
            reasoning_effort="",
            tokens_used=input_tokens + output_tokens + reasoning + cache_read + cache_write,
            archived=False,
            cli_version=str(info.get("version") or ""),
            rollout_path=str(path),
            tool="opencode",
        )
        _set_token_fields(session, input_tokens, output_tokens + reasoning, cache_read, cache_write)
        sessions.append(session)
    return sessions


def _nested_time(data: dict[str, object], key: str) -> int:
    value = data.get("time")
    if isinstance(value, dict):
        value = value.get(key)
    return _int_value(value)


def load_opencode_sessions(data_home: Path) -> list[Session]:
    db_path = find_opencode_db()
    if db_path:
        sessions = _load_opencode_db(db_path)
        if sessions:
            return sessions
    return _load_opencode_legacy(data_home)


# ── pi: JSONL 会话读取 ────────────────────────────────────────────────────

def _message_text(content: object) -> str:
    if isinstance(content, str):
        return content
    if not isinstance(content, list):
        return ""
    return " ".join(
        str(block.get("text") or "")
        for block in content
        if isinstance(block, dict) and block.get("type") == "text"
    ).strip()


def _pi_entry_time(entry: dict[str, object]) -> str:
    timestamp = entry.get("timestamp")
    if isinstance(timestamp, (int, float)):
        return _ms_to_local(timestamp)
    return _parse_iso(str(timestamp or ""))


def load_pi_sessions(pi_home: Path) -> list[Session]:
    sessions_dir = pi_home / "sessions"
    if not sessions_dir.is_dir():
        return []

    sessions: list[Session] = []
    for path in sorted(sessions_dir.rglob("*.jsonl")):
        session = _scan_pi_session(path)
        if session:
            sessions.append(session)
    return sessions


def _scan_pi_session(path: Path) -> Session | None:
    session_id = path.stem.rsplit("_", 1)[-1]
    title = ""
    cwd = ""
    created_at = ""
    updated_at = ""
    model = ""
    provider = ""
    session_name = ""

    try:
        with open(path, "r", errors="replace") as f:
            for line in f:
                try:
                    entry = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if not isinstance(entry, dict):
                    continue
                entry_type = entry.get("type")
                entry_time = _pi_entry_time(entry)
                if entry_type == "session":
                    session_id = str(entry.get("id") or session_id)
                    cwd = str(entry.get("cwd") or cwd)
                    created_at = entry_time or created_at
                if entry_time:
                    updated_at = entry_time
                if entry_type == "session_info":
                    session_name = str(entry.get("name") or "").strip()
                elif entry_type == "model_change":
                    provider = str(entry.get("provider") or provider)
                    model = str(entry.get("modelId") or model)
                elif entry_type == "message":
                    message = entry.get("message") or {}
                    if not isinstance(message, dict):
                        continue
                    role = message.get("role")
                    if role == "user" and not title:
                        text = _message_text(message.get("content"))
                        if text and not text.startswith("<"):
                            title = text
                    if role == "assistant":
                        model = str(message.get("responseModel") or message.get("model") or model)
                        provider = str(message.get("provider") or provider)
    except OSError:
        return None

    if not created_at:
        created_at = _parse_iso(path.name[:24].replace("_", "T") + "Z")
    if not updated_at:
        try:
            updated_at = _ts_to_local(path.stat().st_mtime)
        except OSError:
            updated_at = created_at

    return Session(
        id=session_id,
        title=session_name or title or "(无标题)",
        created_at=created_at,
        updated_at=updated_at,
        source="",
        model_provider=provider,
        cwd=cwd,
        model=f"{provider}/{model}" if provider and model and "/" not in model else model,
        reasoning_effort="",
        tokens_used=0,
        archived=False,
        cli_version="",
        rollout_path=str(path),
        tool="pi",
    )


# ── Claude Code: 会话读取 ─────────────────────────────────────────────────

def _load_claude_session_names(claude_home: Path) -> dict[str, str]:
    """从 ~/.claude/sessions/*.json 读取会话名称（仅活跃会话有）。"""
    names: dict[str, str] = {}
    sessions_dir = claude_home / "sessions"
    if not sessions_dir.is_dir():
        return names
    for path in sessions_dir.glob("*.json"):
        try:
            with open(path, "r") as f:
                data = json.load(f)
            sid = data.get("sessionId", "")
            name = data.get("name", "")
            if sid and name:
                names[sid] = name
        except (json.JSONDecodeError, OSError):
            continue
    return names


def load_claude_sessions(claude_home: Path) -> list[Session]:
    """加载 Claude Code 会话（快速扫描首尾，token 数据延迟计算）。"""
    projects_dir = claude_home / "projects"
    if not projects_dir.is_dir():
        return []

    session_names = _load_claude_session_names(claude_home)

    sessions = []
    for path in sorted(projects_dir.rglob("*.jsonl")):
        session = _scan_claude_session_fast(path, session_names)
        if session:
            sessions.append(session)
    return sessions


def _scan_claude_session_fast(path: Path, session_names: dict[str, str]) -> Session | None:
    """快速扫描：仅读首 64KB + 尾 8KB，提取元数据。token 数据留给 enrich 阶段。"""
    session_id = path.stem

    cwd = ""
    version = ""
    created_ts = ""
    updated_ts = ""
    title = ""
    model = ""
    first_user_found = False

    try:
        with open(path, "rb") as f:
            first_chunk = f.read(65536)
            f.seek(0, 2)
            size = f.tell()
            tail_size = min(8192, size)
            f.seek(-tail_size, 2)
            last_chunk = f.read(tail_size)
    except OSError:
        return None

    # 解析首块：元数据 + 标题
    for line in first_chunk.decode("utf-8", errors="replace").split("\n"):
        if not line.strip():
            continue
        try:
            entry = json.loads(line)
        except json.JSONDecodeError:
            continue
        ts = entry.get("timestamp", "")
        if ts and not created_ts:
            created_ts = ts
        cwd = entry.get("cwd") or cwd
        version = entry.get("version") or version

        if entry.get("type") == "assistant":
            msg = entry.get("message") or {}
            m = msg.get("model", "")
            if m:
                model = m

        if entry.get("type") == "user" and not first_user_found:
            msg = entry.get("message") or {}
            content = msg.get("content", "")
            if isinstance(content, str) and content and not content.startswith("<"):
                title = content
                first_user_found = True
            elif isinstance(content, list):
                for block in content:
                    if isinstance(block, dict) and block.get("type") == "text":
                        text = block.get("text", "")
                        if text and not text.startswith("<"):
                            title = text
                            first_user_found = True
                            break

    # 解析尾块：最后时间戳 + 模型
    last_lines = [l for l in last_chunk.decode("utf-8", errors="replace").split("\n") if l.strip()]
    for line in reversed(last_lines):
        try:
            entry = json.loads(line)
        except json.JSONDecodeError:
            continue
        ts = entry.get("timestamp", "")
        if ts:
            updated_ts = ts
            if entry.get("type") == "assistant":
                msg = entry.get("message") or {}
                m = msg.get("model", "")
                if m:
                    model = m
            break

    # 优先用 sessions/*.json 中的名称
    if not title and session_id in session_names:
        title = session_names[session_id]
    if not title:
        title = "(无标题)"

    return Session(
        id=session_id,
        title=title,
        created_at=_parse_iso(created_ts),
        updated_at=_parse_iso(updated_ts),
        source="",
        model_provider="",
        cwd=cwd,
        model=model,
        reasoning_effort="",
        tokens_used=0,
        archived=False,
        cli_version=version,
        rollout_path=str(path),
        tool="claude",
    )


# ── 时间工具 ──────────────────────────────────────────────────────────────

def _local_tz() -> timezone:
    return datetime.now().astimezone().tzinfo  # type: ignore[return-value]


def _ts_to_local(ts: int | float | None) -> str:
    if ts is None:
        return ""
    dt = datetime.fromtimestamp(int(ts), tz=_local_tz())
    return dt.strftime("%Y-%m-%d %H:%M")


def _parse_iso(s: str) -> str:
    if not s:
        return ""
    try:
        dt = datetime.fromisoformat(s.replace("Z", "+00:00"))
        return dt.astimezone(_local_tz()).strftime("%Y-%m-%d %H:%M")
    except ValueError:
        return s[:16]


def _ms_to_local(ts: int | float | None) -> str:
    if ts is None:
        return ""
    value = float(ts)
    if value > 100_000_000_000:
        value /= 1000
    return _ts_to_local(value)


# ── 过滤与排序 ────────────────────────────────────────────────────────────

def apply_filters(
    sessions: list[Session],
    *,
    archived: bool,
    cwd_filter: str | None,
    source_filter: str | None,
    since: datetime | None = None,
) -> list[Session]:
    result = sessions
    result = [s for s in result if s.archived == archived]
    if cwd_filter:
        needle = cwd_filter.lower()
        result = [s for s in result if needle in s.cwd.lower()]
    if source_filter:
        needle = source_filter.lower()
        result = [s for s in result if needle in s.source.lower()]
    if since:
        result = [s for s in result if _session_datetime(s.updated_at) >= since]
    return result


def _session_datetime(value: str) -> datetime:
    try:
        return datetime.strptime(value, "%Y-%m-%d %H:%M").replace(tzinfo=_local_tz())
    except ValueError:
        return datetime.min.replace(tzinfo=_local_tz())


def _parse_since(value: str) -> datetime:
    raw = value.strip()
    if not raw:
        raise ValueError("时间不能为空")
    match = re.fullmatch(r"(?i)(\d+(?:\.\d+)?)([dhm])", raw)
    if match:
        amount = float(match.group(1))
        unit = match.group(2).lower()
        seconds = amount * {"d": 86400, "h": 3600, "m": 60}[unit]
        return datetime.now().astimezone() - timedelta(seconds=seconds)
    if raw.lower() == "now":
        return datetime.now().astimezone()
    try:
        if re.fullmatch(r"\d{4}-\d{2}-\d{2}", raw):
            return datetime.strptime(raw, "%Y-%m-%d").replace(tzinfo=_local_tz())
        parsed = datetime.fromisoformat(raw.replace("Z", "+00:00"))
        return (
            parsed.replace(tzinfo=_local_tz())
            if parsed.tzinfo is None
            else parsed.astimezone(_local_tz())
        )
    except ValueError as exc:
        raise ValueError(
            f"无法解析 --since={value!r}，请使用 YYYY-MM-DD、ISO 时间，或 7d/12h/30m"
        ) from exc


def sort_sessions(sessions: list[Session], sort_key: str) -> list[Session]:
    if sort_key == "created":
        return sorted(sessions, key=lambda s: s.created_at, reverse=True)
    return sorted(sessions, key=lambda s: s.updated_at, reverse=True)


# ── Codex: 缓存命中率提取 ─────────────────────────────────────────────────

def _extract_codex_usage(rollout_path: str) -> tuple[int, int, int, int, int] | None:
    """从 rollout JSONL 末尾的 token_count 事件提取累计 token 明细。"""
    if not rollout_path or not os.path.isfile(rollout_path):
        return None
    try:
        result = subprocess.run(
            ["rg", '"token_count"', rollout_path],
            capture_output=True, text=True, timeout=5,
        )
        lines = result.stdout.strip().splitlines()
        if not lines:
            return None
        entry = json.loads(lines[-1])
        usage = (entry.get("payload") or {}).get("info") or {}
        usage = usage.get("total_token_usage") or {}
        fields = _usage_fields(usage)
        if fields[-1] <= 0:
            return None
        return fields
    except (json.JSONDecodeError, subprocess.TimeoutExpired, FileNotFoundError, OSError):
        return None


def _extract_cache_hit_rate(rollout_path: str) -> float | None:
    usage = _extract_codex_usage(rollout_path)
    if not usage:
        return None
    input_tokens, _, cache_read, cache_write, _ = usage
    # Codex 的 input_tokens 已经包含缓存输入，cached_input_tokens 是其子集。
    return cache_read / input_tokens if input_tokens else None


# ── Claude Code: token 统计 ───────────────────────────────────────────────

def _compute_claude_tokens(session: Session) -> None:
    """完整读取 Claude Code 会话文件，计算 token 用量和缓存命中率。"""
    path = session.rollout_path
    if not path or not os.path.isfile(path):
        return

    total_input = 0
    total_output = 0
    total_cache_read = 0
    total_cache_creation = 0

    try:
        with open(path, "r", errors="replace") as f:
            for line in f:
                try:
                    entry = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if entry.get("type") != "assistant":
                    continue
                msg = entry.get("message") or {}
                usage = msg.get("usage") or {}
                total_input += usage.get("input_tokens", 0) or 0
                total_output += usage.get("output_tokens", 0) or 0
                total_cache_read += usage.get("cache_read_input_tokens", 0) or 0
                total_cache_creation += usage.get("cache_creation_input_tokens", 0) or 0
    except OSError:
        return

    _set_token_fields(
        session,
        total_input,
        total_output,
        total_cache_read,
        total_cache_creation,
    )


def _compute_pi_tokens(session: Session) -> None:
    """读取 pi JSONL 中 assistant/tool/summary 的 usage 累计值。"""
    input_tokens = output_tokens = cache_read = cache_write = total = 0
    try:
        with open(session.rollout_path, "r", errors="replace") as f:
            for line in f:
                try:
                    entry = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if not isinstance(entry, dict):
                    continue
                usage: object = None
                if entry.get("type") == "message":
                    message = entry.get("message") or {}
                    if isinstance(message, dict) and message.get("role") in ("assistant", "toolResult"):
                        usage = message.get("usage")
                elif entry.get("type") in ("branch_summary", "compaction"):
                    usage = entry.get("usage")
                fields = _usage_fields(usage)
                input_tokens += fields[0]
                output_tokens += fields[1]
                cache_read += fields[2]
                cache_write += fields[3]
                total += fields[4]
    except OSError:
        return
    _set_token_fields(session, input_tokens, output_tokens, cache_read, cache_write, total)


# ── 统一 enrich ───────────────────────────────────────────────────────────

def enrich_sessions(sessions: list[Session]) -> None:
    """对展示的会话补充 token / 缓存数据。"""
    for s in sessions:
        if s.tool == "codex":
            if s.cache_hit_rate is None:
                usage = _extract_codex_usage(s.rollout_path)
                if usage:
                    _set_codex_token_fields(s, usage)
                else:
                    s.cache_hit_rate = _extract_cache_hit_rate(s.rollout_path)
        elif s.tool == "claude":
            _compute_claude_tokens(s)
        elif s.tool == "pi":
            _compute_pi_tokens(s)


def enrich_and_filter_sessions(
    sessions: list[Session],
    *,
    limit: int,
    show_all: bool,
) -> list[Session]:
    """补全 token 后剔除 0 token 会话，再应用展示条数限制。"""
    if not show_all and limit <= 0:
        return []
    if show_all:
        enrich_sessions(sessions)
        return [s for s in sessions if s.tokens_used > 0]

    result: list[Session] = []
    for session in sessions:
        enrich_sessions([session])
        if session.tokens_used <= 0:
            continue
        result.append(session)
        if len(result) >= max(0, limit):
            break
    return result


# ── 格式化工具 ────────────────────────────────────────────────────────────

def _fmt_tokens(n: int) -> str:
    if n >= 1_000_000:
        return f"{n / 1_000_000:.1f}M"
    if n >= 1_000:
        return f"{n / 1_000:.0f}K"
    return str(n)


def _fmt_cache(rate: float | None) -> str:
    if rate is None:
        return "-"
    return f"{rate * 100:.1f}%"


def _cache_color(rate: float | None) -> str:
    if rate is None:
        return "dim"
    if rate >= 0.90:
        return "green"
    if rate >= 0.70:
        return "yellow"
    return "red"


def _clean(s: str) -> str:
    """去掉换行符，压缩空白。"""
    return " ".join(s.split())


def _tool_label(tool: str) -> str:
    if tool == "claude":
        return "CC"
    if tool == "pi":
        return "PI"
    if tool == "opencode":
        return "OC"
    return "CX"


def _tool_color(tool: str) -> str:
    return {
        "codex": "magenta",
        "claude": "yellow",
        "pi": "green",
        "opencode": "blue",
    }.get(tool, "white")


def _token_style(n: int) -> str:
    """Tokens 统一使用青色，只用亮度区分消耗量级。"""
    if n >= 1_000_000:
        return "bold bright_cyan"
    if n >= 100_000:
        return "cyan"
    return "dim cyan"


# ── Rich 表格输出 ─────────────────────────────────────────────────────────

def print_table_rich(sessions: list[Session]) -> None:
    if not sessions:
        print("没有找到匹配的会话。")
        return

    # 保证模型列不因当前终端较窄而被 Rich 自动替换成省略号；
    # 标题列有明确 max_width，终端本身仍可按需要横向滚动/换行。
    console = Console(width=max(160, Console().width))
    table = Table(
        show_header=True,
        header_style="bold cyan",
        show_lines=False,
        expand=False,
        box=box.SIMPLE,
        padding=(0, 0),
        collapse_padding=True,
    )
    table.add_column("时间", style="dim", no_wrap=True, width=16)
    table.add_column("T", no_wrap=True, width=3)
    table.add_column("ID", style="cyan", no_wrap=True, width=10)
    table.add_column("模型", overflow="fold", min_width=20)
    table.add_column("工作目录", no_wrap=True, width=18)
    table.add_column("Tokens", justify="right", no_wrap=True, width=7)
    table.add_column("缓存", justify="right", no_wrap=True, width=7)
    table.add_column("标题", overflow="ellipsis", no_wrap=True, max_width=48)

    for s in sessions:
        cwd_short = s.cwd.replace(str(Path.home()), "~") if s.cwd else ""
        cache_str = _fmt_cache(s.cache_hit_rate)
        cache_style = _cache_color(s.cache_hit_rate)

        table.add_row(
            escape(s.updated_at),
            Text(_tool_label(s.tool), style=_tool_color(s.tool)),
            escape(s.id[:8]),
            escape(_clean(s.model)),
            escape(cwd_short),
            Text(_fmt_tokens(s.tokens_used), style=_token_style(s.tokens_used)),
            Text(cache_str, style=cache_style),
            escape(_clean(s.title)),
        )

    console.print(table)
    console.print(f"[dim]共 {len(sessions)} 条会话[/dim]")


# ── 纯文本输出（rich 不可用时回退） ───────────────────────────────────────

def print_table_plain(sessions: list[Session]) -> None:
    if not sessions:
        print("没有找到匹配的会话。")
        return

    import shutil
    import unicodedata

    def dw(s: str) -> int:
        return sum(2 if unicodedata.east_asian_width(c) in ("W", "F") else 1 for c in s)

    def trunc(s: str, w: int) -> str:
        if dw(s) <= w:
            return s
        if w <= 1:
            return "…"
        n = 0
        out = []
        for c in s:
            cw = 2 if unicodedata.east_asian_width(c) in ("W", "F") else 1
            if n + cw > w - 1:
                break
            out.append(c)
            n += cw
        return "".join(out) + "…"

    def pad(s: str, w: int) -> str:
        return s + " " * max(0, w - dw(s))

    W_TIME, W_TOOL, W_ID, W_CWD, W_TOK, W_CACHE = 16, 2, 8, 14, 7, 7
    W_MODEL = max(4, max((dw(_clean(s.model)) for s in sessions), default=4))
    SEP = 2
    prefix = W_TIME + W_TOOL + W_ID + W_MODEL + W_CWD + W_TOK + W_CACHE + SEP * 7
    term_w = shutil.get_terminal_size((80, 24)).columns
    W_TITLE = max(1, term_w - prefix - 2)

    header = (
        f"{pad('时间', W_TIME)}  {pad('T', W_TOOL)}  {pad('ID', W_ID)}  {pad('模型', W_MODEL)}  "
        f"{pad('工作目录', W_CWD)}  {pad('Tokens', W_TOK)}  {pad('缓存', W_CACHE)}  标题"
    )
    print(header)
    print("-" * min(dw(header), term_w))

    for s in sessions:
        cwd_short = s.cwd.replace(str(Path.home()), "~") if s.cwd else ""
        line = (
            f"{pad(trunc(s.updated_at, W_TIME), W_TIME)}  "
            f"{pad(_tool_label(s.tool), W_TOOL)}  "
            f"{pad(s.id[:8], W_ID)}  "
            f"{pad(_clean(s.model), W_MODEL)}  "
            f"{pad(trunc(cwd_short, W_CWD), W_CWD)}  "
            f"{pad(_fmt_tokens(s.tokens_used), W_TOK)}  "
            f"{pad(_fmt_cache(s.cache_hit_rate), W_CACHE)}  "
            f"{trunc(_clean(s.title), W_TITLE)}"
        )
        print(line)

    print(f"\n共 {len(sessions)} 条会话")


def print_table(sessions: list[Session]) -> None:
    if HAS_RICH:
        print_table_rich(sessions)
    else:
        print_table_plain(sessions)


def print_token_summary(sessions: list[Session]) -> None:
    input_tokens = sum(s.input_tokens for s in sessions)
    output_tokens = sum(s.output_tokens for s in sessions)
    cache_read = sum(s.cache_read_tokens for s in sessions)
    cache_write = sum(s.cache_write_tokens for s in sessions)
    total = sum(s.tokens_used for s in sessions)
    # 不同工具对 input_tokens 的定义不同，不能把所有会话字段先混加。
    denominator = sum(_cache_rate_denominator(s) for s in sessions)
    cache_rate = cache_read / denominator if denominator else None
    print(
        "Token 总计（当前列表）："
        f"总计 {_fmt_tokens(total)} ({total:,})，"
        f"输入 {_fmt_tokens(input_tokens)} ({input_tokens:,})，"
        f"输出 {_fmt_tokens(output_tokens)} ({output_tokens:,})，"
        f"缓存命中 {_fmt_cache(cache_rate)}"
    )


def print_json(sessions: list[Session]) -> None:
    data = [asdict(s) for s in sessions]
    print(json.dumps(data, ensure_ascii=False, indent=2))


# ── 主入口 ────────────────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(
        description="列出当前环境的 Codex / Claude Code / pi / OpenCode 会话",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument("-n", "--limit", type=int, default=20, help="显示条数（默认 20）")
    parser.add_argument("--all", action="store_true", help="显示全部（忽略 --limit）")
    parser.add_argument(
        "--tool",
        "--agent",
        choices=["codex", "claude", "pi", "opencode", "all"],
        default="all",
        help="筛选工具（默认 all；也可用于按 agent 类型过滤）",
    )
    parser.add_argument("--archived", action="store_true", help="只看已归档会话（仅 Codex）")
    parser.add_argument("--cwd", metavar="PATH", help="按工作目录过滤（子串匹配）")
    parser.add_argument("--source", metavar="SRC", help="按来源过滤，如 vscode / cli（仅 Codex）")
    parser.add_argument(
        "--since",
        metavar="TIME",
        help="只显示该时间之后更新的会话：YYYY-MM-DD、ISO 时间，或 7d/12h/30m",
    )
    parser.add_argument("--sort", choices=["created", "updated"], default="updated", help="排序键（默认 updated）")
    parser.add_argument("--json", action="store_true", help="输出 JSON")
    args = parser.parse_args()

    try:
        since = _parse_since(args.since) if args.since else None
    except ValueError as exc:
        parser.error(str(exc))

    sessions: list[Session] = []
    if args.tool in ("codex", "all"):
        codex_home = find_codex_home()
        sqlite_home = find_sqlite_home(codex_home)
        state_db = find_state_db(sqlite_home)
        if state_db:
            sessions.extend(load_from_state_db(state_db))
        else:
            sessions.extend(load_from_jsonl_files(codex_home))

    if args.tool in ("claude", "all"):
        claude_home = find_claude_home()
        sessions.extend(load_claude_sessions(claude_home))

    if args.tool in ("pi", "all"):
        pi_home = find_pi_home()
        sessions.extend(load_pi_sessions(pi_home))

    if args.tool in ("opencode", "all"):
        opencode_home = find_opencode_data_home()
        sessions.extend(load_opencode_sessions(opencode_home))

    sessions = apply_filters(
        sessions,
        archived=args.archived,
        cwd_filter=args.cwd,
        source_filter=args.source,
        since=since,
    )
    sessions = sort_sessions(sessions, args.sort)

    sessions = enrich_and_filter_sessions(
        sessions,
        limit=args.limit,
        show_all=args.all,
    )

    if args.json:
        print_json(sessions)
    else:
        # 数据源信息
        sources = []
        if args.tool in ("codex", "all"):
            codex_home = find_codex_home()
            sqlite_home = find_sqlite_home(codex_home)
            state_db = find_state_db(sqlite_home)
            if state_db:
                sources.append(f"Codex: {state_db.name}")
            elif (codex_home / "sessions").is_dir():
                sources.append("Codex: sessions/ (JSONL)")
        if args.tool in ("claude", "all"):
            claude_home = find_claude_home()
            if (claude_home / "projects").is_dir():
                sources.append(f"Claude: {claude_home}/projects/")
        if args.tool in ("pi", "all"):
            pi_home = find_pi_home()
            if (pi_home / "sessions").is_dir():
                sources.append(f"pi: {pi_home}/sessions/")
        if args.tool in ("opencode", "all"):
            opencode_db = find_opencode_db()
            if opencode_db:
                sources.append(f"OpenCode: {opencode_db}")
            elif (find_opencode_data_home() / "storage").is_dir():
                sources.append(f"OpenCode: {find_opencode_data_home()}/storage/")
        print(f"数据源: {', '.join(sources)}")
        print()
        print_table(sessions)
        print_token_summary(sessions)


if __name__ == "__main__":
    main()
