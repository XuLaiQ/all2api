from __future__ import annotations

import time
from email.utils import parsedate_to_datetime

from app.config import get_settings
from app.infrastructure.db import database

SOFT_COOLDOWN_BASE = 600
SOFT_COOLDOWN_MAX = 7200
BREAKER_THRESHOLD = 3
BREAKER_BASE = 1800
BREAKER_MAX = 21600
MODEL_NOT_FOUND_COOLDOWN = 21600


def block_until(channel: str, model: str, now: int | None = None) -> int | None:
    now = now or int(time.time())
    with database(get_settings().db_path) as conn:
        rows = conn.execute(
            """SELECT cooldown_until, breaker_until FROM channel_runtime_state
            WHERE channel = ? AND model IN ('', ?)""",
            (channel, model),
        ).fetchall()
    deadlines = [
        int(value)
        for row in rows
        for value in (row["cooldown_until"], row["breaker_until"])
        if value is not None and int(value) > now
    ]
    return max(deadlines) if deadlines else None


def channel_state(channel: str, now: int | None = None) -> dict:
    now = now or int(time.time())
    with database(get_settings().db_path) as conn:
        row = conn.execute(
            """SELECT consecutive_failures, breaker_streak, cooldown_until,
            breaker_until, last_status, last_error_kind, updated_at
            FROM channel_runtime_state WHERE channel = ? AND model = ''""",
            (channel,),
        ).fetchone()
    if row is None:
        return {
            "state": "closed",
            "consecutive_failures": 0,
            "cooldown_until": None,
            "breaker_until": None,
            "retry_after": None,
        }
    cooldown = int(row["cooldown_until"]) if row["cooldown_until"] else None
    breaker = int(row["breaker_until"]) if row["breaker_until"] else None
    open_until = max(
        (value for value in (cooldown, breaker) if value and value > now),
        default=None,
    )
    state = "breaker_open" if breaker and breaker > now else (
        "cooldown" if cooldown and cooldown > now else "closed"
    )
    return {
        "state": state,
        "consecutive_failures": int(row["consecutive_failures"]),
        "cooldown_until": cooldown,
        "breaker_until": breaker,
        "last_status": row["last_status"],
        "last_error_kind": row["last_error_kind"],
        "updated_at": int(row["updated_at"]),
        "retry_after": max(0, open_until - now) if open_until else None,
    }


def runtime_states(channel: str, now: int | None = None) -> list[dict]:
    now = now or int(time.time())
    with database(get_settings().db_path) as conn:
        rows = conn.execute(
            """SELECT model, consecutive_failures, breaker_streak, soft_streak,
            cooldown_until, breaker_until, last_status, last_error_kind, updated_at
            FROM channel_runtime_state WHERE channel = ? ORDER BY model""",
            (channel,),
        ).fetchall()
    result = []
    for row in rows:
        cooldown = int(row["cooldown_until"]) if row["cooldown_until"] else None
        breaker = int(row["breaker_until"]) if row["breaker_until"] else None
        open_until = max(
            (value for value in (cooldown, breaker) if value and value > now),
            default=None,
        )
        state = "breaker_open" if breaker and breaker > now else (
            "cooldown" if cooldown and cooldown > now else "closed"
        )
        result.append(
            {
                "model": row["model"] or None,
                "state": state,
                "consecutive_failures": int(row["consecutive_failures"]),
                "breaker_streak": int(row["breaker_streak"]),
                "soft_streak": int(row["soft_streak"]),
                "cooldown_until": cooldown,
                "breaker_until": breaker,
                "last_status": row["last_status"],
                "last_error_kind": row["last_error_kind"],
                "updated_at": int(row["updated_at"]),
                "retry_after": max(0, open_until - now) if open_until else None,
            }
        )
    return result


def account_runtime_snapshot(row, now: int | None = None) -> dict:
    now = int(time.time()) if now is None else now
    updated_at = row["runtime_updated_at"]
    if updated_at is None:
        return {
            "state": "unobserved",
            "success_count": 0,
            "fail_count": 0,
            "consecutive_failures": 0,
            "cooldown_until": None,
            "breaker_until": None,
            "last_status": None,
            "last_error_kind": None,
            "updated_at": None,
            "retry_after": None,
        }
    cooldown = row["runtime_cooldown_until"]
    breaker = row["runtime_breaker_until"]
    cooldown = int(cooldown) if cooldown is not None else None
    breaker = int(breaker) if breaker is not None else None
    open_until = max(
        (value for value in (cooldown, breaker) if value is not None and value > now),
        default=None,
    )
    state = "breaker_open" if breaker and breaker > now else (
        "cooldown" if cooldown and cooldown > now else "closed"
    )
    return {
        "state": state,
        "success_count": int(row["runtime_success_count"]),
        "fail_count": int(row["runtime_fail_count"]),
        "consecutive_failures": int(row["runtime_consecutive_failures"]),
        "cooldown_until": cooldown,
        "breaker_until": breaker,
        "last_status": row["runtime_last_status"],
        "last_error_kind": row["runtime_last_error_kind"],
        "updated_at": int(updated_at),
        "retry_after": max(0, open_until - now) if open_until else None,
    }


def _record_account_failure(
    account_id: str,
    status: int,
    error_kind: str,
    *,
    rate_limited: bool,
    retry_after: int | None = None,
    now: int | None = None,
) -> None:
    now = int(time.time()) if now is None else now
    with database(get_settings().db_path) as conn:
        conn.execute("BEGIN IMMEDIATE")
        row = conn.execute(
            """SELECT consecutive_failures, breaker_streak, soft_streak,
            breaker_until FROM account_runtime_state WHERE account_id = ?""",
            (account_id,),
        ).fetchone()
        consecutive = int(row["consecutive_failures"]) if row else 0
        if not rate_limited:
            consecutive += 1
        breaker_streak = int(row["breaker_streak"]) if row else 0
        soft_streak = int(row["soft_streak"]) if row else 0
        breaker_until = int(row["breaker_until"]) if row and row["breaker_until"] else None
        cooldown_until = None
        if rate_limited:
            soft_streak += 1
            duration = retry_after or min(
                SOFT_COOLDOWN_BASE * (2 ** (soft_streak - 1)), SOFT_COOLDOWN_MAX
            )
            cooldown_until = now + duration
        elif consecutive >= BREAKER_THRESHOLD and not (
            breaker_until and breaker_until > now
        ):
            breaker_streak += 1
            duration = min(BREAKER_BASE * (2 ** (breaker_streak - 1)), BREAKER_MAX)
            breaker_until = now + duration

        conn.execute(
            """INSERT INTO account_runtime_state
            (account_id, fail_count, consecutive_failures, breaker_streak, soft_streak,
             cooldown_until, breaker_until, last_status, last_error_kind, updated_at)
            VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(account_id) DO UPDATE SET
                fail_count=account_runtime_state.fail_count + 1,
                consecutive_failures=excluded.consecutive_failures,
                breaker_streak=excluded.breaker_streak,
                soft_streak=excluded.soft_streak,
                cooldown_until=COALESCE(excluded.cooldown_until,
                    account_runtime_state.cooldown_until),
                breaker_until=excluded.breaker_until,
                last_status=excluded.last_status,
                last_error_kind=excluded.last_error_kind,
                updated_at=excluded.updated_at""",
            (
                account_id,
                consecutive,
                breaker_streak,
                soft_streak,
                cooldown_until,
                breaker_until,
                status,
                error_kind[:64],
                now,
            ),
        )


def record_account_rate_limit(
    account_id: str,
    status: int,
    retry_after: int | None = None,
    now: int | None = None,
) -> None:
    _record_account_failure(
        account_id,
        status,
        "rate_limited",
        rate_limited=True,
        retry_after=retry_after,
        now=now,
    )


def record_account_transient_failure(
    account_id: str,
    status: int,
    error_kind: str,
    now: int | None = None,
) -> None:
    _record_account_failure(
        account_id,
        status,
        error_kind,
        rate_limited=False,
        now=now,
    )


def record_account_success(account_id: str, now: int | None = None) -> None:
    now = int(time.time()) if now is None else now
    with database(get_settings().db_path) as conn:
        conn.execute("BEGIN IMMEDIATE")
        conn.execute(
            """INSERT INTO account_runtime_state
            (account_id, success_count, last_status, updated_at)
            VALUES (?, 1, 200, ?)
            ON CONFLICT(account_id) DO UPDATE SET
                success_count=account_runtime_state.success_count + 1,
                consecutive_failures=0, breaker_streak=0, soft_streak=0,
                cooldown_until=NULL, breaker_until=NULL, last_status=200,
                last_error_kind=NULL, updated_at=excluded.updated_at""",
            (account_id, now),
        )


def record_success(channel: str, model: str, now: int | None = None) -> None:
    now = now or int(time.time())
    with database(get_settings().db_path) as conn:
        conn.execute("BEGIN IMMEDIATE")
        for state_model in ("", model):
            conn.execute(
                """INSERT INTO channel_runtime_state(channel, model, updated_at)
                VALUES (?, ?, ?)
                ON CONFLICT(channel, model) DO UPDATE SET
                    consecutive_failures=0, breaker_streak=0, soft_streak=0,
                    cooldown_until=NULL, breaker_until=NULL, last_status=?,
                    last_error_kind=NULL, updated_at=excluded.updated_at""",
                (channel, state_model, now, 200),
            )


def record_transient_failure(
    channel: str,
    status: int,
    error_kind: str,
    now: int | None = None,
) -> dict:
    now = now or int(time.time())
    with database(get_settings().db_path) as conn:
        conn.execute("BEGIN IMMEDIATE")
        row = conn.execute(
            "SELECT consecutive_failures, breaker_streak, breaker_until "
            "FROM channel_runtime_state WHERE channel = ? AND model = ''",
            (channel,),
        ).fetchone()
        consecutive = (int(row["consecutive_failures"]) if row else 0) + 1
        breaker_streak = int(row["breaker_streak"]) if row else 0
        breaker_until = int(row["breaker_until"]) if row and row["breaker_until"] else None
        if consecutive >= BREAKER_THRESHOLD and not (breaker_until and breaker_until > now):
            breaker_streak += 1
            duration = min(BREAKER_BASE * (2 ** (breaker_streak - 1)), BREAKER_MAX)
            breaker_until = now + duration
        conn.execute(
            """INSERT INTO channel_runtime_state
            (channel, model, consecutive_failures, breaker_streak, breaker_until,
             last_status, last_error_kind, updated_at)
            VALUES (?, '', ?, ?, ?, ?, ?, ?)
            ON CONFLICT(channel, model) DO UPDATE SET
                consecutive_failures=excluded.consecutive_failures,
                breaker_streak=excluded.breaker_streak,
                breaker_until=excluded.breaker_until,
                last_status=excluded.last_status,
                last_error_kind=excluded.last_error_kind,
                updated_at=excluded.updated_at""",
            (channel, consecutive, breaker_streak, breaker_until, status, error_kind[:64], now),
        )
    return channel_state(channel, now)


def retry_after_seconds(value: str | None, now: int | None = None) -> int | None:
    if not value:
        return None
    now = now or int(time.time())
    try:
        return max(1, min(SOFT_COOLDOWN_MAX, int(float(value))))
    except ValueError:
        try:
            parsed = parsedate_to_datetime(value)
            return max(1, min(SOFT_COOLDOWN_MAX, int(parsed.timestamp()) - now))
        except (TypeError, ValueError, OverflowError):
            return None


def record_rate_limit(
    channel: str,
    model: str,
    status: int,
    retry_after: int | None = None,
    *,
    model_limited: bool = False,
    now: int | None = None,
) -> dict:
    now = now or int(time.time())
    state_model = model if model_limited else ""
    with database(get_settings().db_path) as conn:
        conn.execute("BEGIN IMMEDIATE")
        row = conn.execute(
            "SELECT soft_streak FROM channel_runtime_state WHERE channel = ? AND model = ?",
            (channel, state_model),
        ).fetchone()
        streak = (int(row["soft_streak"]) if row else 0) + 1
        duration = retry_after or min(SOFT_COOLDOWN_BASE * (2 ** (streak - 1)), SOFT_COOLDOWN_MAX)
        until = now + duration
        conn.execute(
            """INSERT INTO channel_runtime_state
            (channel, model, soft_streak, cooldown_until, last_status, last_error_kind, updated_at)
            VALUES (?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(channel, model) DO UPDATE SET
                soft_streak=excluded.soft_streak,
                cooldown_until=excluded.cooldown_until,
                last_status=excluded.last_status,
                last_error_kind=excluded.last_error_kind,
                updated_at=excluded.updated_at""",
            (
                channel,
                state_model,
                streak,
                until,
                status,
                "model_rate_limited" if model_limited else "rate_limited",
                now,
            ),
        )
    return {"cooldown_until": until, "retry_after": duration}


def record_model_not_found(channel: str, model: str, status: int, now: int | None = None) -> dict:
    now = now or int(time.time())
    until = now + MODEL_NOT_FOUND_COOLDOWN
    with database(get_settings().db_path) as conn:
        conn.execute("BEGIN IMMEDIATE")
        conn.execute(
            """INSERT INTO channel_runtime_state
            (channel, model, cooldown_until, last_status, last_error_kind, updated_at)
            VALUES (?, ?, ?, ?, 'model_not_found', ?)
            ON CONFLICT(channel, model) DO UPDATE SET
                cooldown_until=excluded.cooldown_until,
                last_status=excluded.last_status,
                last_error_kind=excluded.last_error_kind,
                updated_at=excluded.updated_at""",
            (channel, model, until, status, now),
        )
    return {"cooldown_until": until, "retry_after": MODEL_NOT_FOUND_COOLDOWN}
