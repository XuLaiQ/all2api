from __future__ import annotations

import base64
import hashlib
import hmac
import ipaddress
import json
import re
import secrets
import threading
import time
from dataclasses import dataclass
from functools import lru_cache
from typing import Annotated
from urllib.parse import urlsplit

from fastapi import Header, HTTPException, Request

from app.config import get_settings
from app.infrastructure.credentials import encrypt_secret
from app.infrastructure.db import database

SESSION_COOKIE = "a2a_session"
SESSION_VERSION = 1
LOGIN_WINDOW_SECONDS = 600
PASSWORD_HASH_ITERATIONS = 310_000
_PASSWORD_SALT = secrets.token_bytes(16)


@dataclass
class SessionRecord:
    username: str
    role: str
    session_version: int
    expires_at: int
    last_seen_at: int


_state_lock = threading.RLock()
_sessions: dict[str, SessionRecord] = {}
_session_versions: dict[str, int] = {}
_login_failures: dict[str, list[int]] = {}


def hash_api_key(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def _admin_password_digest(password: str) -> bytes:
    return hashlib.pbkdf2_hmac(
        "sha256", password.encode("utf-8"), _PASSWORD_SALT, PASSWORD_HASH_ITERATIONS
    )


@lru_cache(maxsize=1)
def _configured_admin_password_digest() -> bytes:
    password = get_settings().admin_password
    return _admin_password_digest(password.get_secret_value() if password else "")


def admin_login_configured() -> bool:
    settings = get_settings()
    password = settings.admin_password.get_secret_value() if settings.admin_password else ""
    secret = settings.session_secret.get_secret_value()
    return bool(
        settings.admin_username.strip()
        and (len(password) >= 12 or settings.allow_weak_admin_password)
        and len(secret) >= 32
        and secret != "dev-only-change-me"
    )


def verify_admin_credentials(username: str, password: str) -> bool:
    settings = get_settings()
    if not admin_login_configured():
        return False
    supplied_hash = _admin_password_digest(password)
    configured_hash = _configured_admin_password_digest()
    username_ok = secrets.compare_digest(
        username.encode(), settings.admin_username.strip().encode()
    )
    password_ok = secrets.compare_digest(supplied_hash, configured_hash)
    return username_ok and password_ok


def _b64encode(value: bytes) -> str:
    return base64.urlsafe_b64encode(value).rstrip(b"=").decode("ascii")


def _b64decode(value: str) -> bytes:
    return base64.urlsafe_b64decode(value + "=" * (-len(value) % 4))


def _session_cookie_signature(payload: str) -> str:
    secret = get_settings().session_secret.get_secret_value().encode("utf-8")
    return _b64encode(hmac.new(secret, payload.encode("ascii"), hashlib.sha256).digest())


def create_session(username: str, role: str = "admin") -> tuple[str, int]:
    settings = get_settings()
    now = int(time.time())
    expires_at = now + settings.session_days * 86400
    session_id = secrets.token_urlsafe(32)
    with _state_lock:
        session_version = _session_versions.get(username, SESSION_VERSION)
        _sessions[session_id] = SessionRecord(
            username=username,
            role=role,
            session_version=session_version,
            expires_at=expires_at,
            last_seen_at=now,
        )
        for sid, record in list(_sessions.items()):
            if record.expires_at <= now:
                _sessions.pop(sid, None)
    raw_payload = json.dumps(
        {
            "sid": session_id,
            "username": username,
            "role": role,
            "sv": session_version,
            "exp": expires_at,
        },
        separators=(",", ":"),
    ).encode("utf-8")
    payload = _b64encode(raw_payload)
    return f"{payload}.{_session_cookie_signature(payload)}", expires_at


def _read_session_cookie(cookie: str | None) -> tuple[str, SessionRecord] | None:
    if not cookie or len(cookie) > 2048:
        return None
    payload, separator, signature = cookie.partition(".")
    if (
        not separator
        or not re.fullmatch(r"[A-Za-z0-9_-]+", payload)
        or not re.fullmatch(r"[A-Za-z0-9_-]+", signature)
        or not secrets.compare_digest(signature, _session_cookie_signature(payload))
    ):
        return None
    try:
        data = json.loads(_b64decode(payload))
        session_id = str(data["sid"])
        username = str(data["username"])
        role = str(data["role"])
        version = int(data["sv"])
        expires_at = int(data["exp"])
    except (ValueError, TypeError, KeyError, json.JSONDecodeError):
        return None
    now = int(time.time())
    idle_limit = get_settings().session_idle_hours * 3600
    with _state_lock:
        record = _sessions.get(session_id)
        expected_version = _session_versions.get(username, SESSION_VERSION)
        if (
            record is None
            or expires_at <= now
            or now - record.last_seen_at > idle_limit
            or version != expected_version
            or record.username != username
            or record.role != role
            or record.session_version != version
            or record.expires_at != expires_at
        ):
            _sessions.pop(session_id, None)
            return None
        record.last_seen_at = now
        return session_id, record


def current_admin_session(cookie: str | None) -> tuple[str, SessionRecord] | None:
    return _read_session_cookie(cookie)


def revoke_session(cookie: str | None) -> None:
    session = _read_session_cookie(cookie)
    if session:
        with _state_lock:
            _sessions.pop(session[0], None)


def revoke_user_sessions(username: str) -> None:
    with _state_lock:
        _session_versions[username] = _session_versions.get(username, SESSION_VERSION) + 1
        for session_id, record in list(_sessions.items()):
            if record.username == username:
                _sessions.pop(session_id, None)


def begin_login_attempt(ip: str, username: str) -> int:
    now = int(time.time())
    keys = (f"ip:{ip}", f"user:{username.casefold()}")
    with _state_lock:
        attempts_by_key: dict[str, list[int]] = {}
        blocked_until = 0
        for key in keys:
            attempts = [
                at for at in _login_failures.get(key, [])
                if now - at < LOGIN_WINDOW_SECONDS
            ]
            attempts_by_key[key] = attempts
            if attempts:
                if len(attempts) >= get_settings().login_max_fails:
                    blocked_until = max(blocked_until, attempts[0] + LOGIN_WINDOW_SECONDS)
        wait_seconds = max(0, blocked_until - now)
        if wait_seconds:
            return wait_seconds
        for key, attempts in attempts_by_key.items():
            attempts.append(now)
            _login_failures[key] = attempts
        if len(_login_failures) > 4096:
            for key, attempts in list(_login_failures.items()):
                if not attempts or now - attempts[-1] >= LOGIN_WINDOW_SECONDS:
                    _login_failures.pop(key, None)
        return 0


def clear_login_failures(ip: str, username: str) -> None:
    with _state_lock:
        _login_failures.pop(f"ip:{ip}", None)
        _login_failures.pop(f"user:{username.casefold()}", None)


def require_same_origin(request: Request) -> None:
    origin = request.headers.get("origin", "")
    host = request.headers.get("host", "")
    parsed = urlsplit(origin)
    if (
        parsed.scheme not in {"http", "https"}
        or not parsed.netloc
        or not host
        or parsed.netloc.casefold() != host.casefold()
    ):
        raise HTTPException(status_code=403, detail="origin does not match request host")


def _trusted_proxy(peer: str) -> bool:
    settings = get_settings()
    if not settings.trust_proxy:
        return False
    try:
        address = ipaddress.ip_address(peer)
    except ValueError:
        return False
    for item in settings.trusted_proxies.split(","):
        try:
            if address in ipaddress.ip_network(item.strip(), strict=False):
                return True
        except ValueError:
            continue
    return False


def client_ip(request: Request) -> str:
    peer = str(request.client.host if request.client else "unknown")
    if not _trusted_proxy(peer):
        return peer
    forwarded = request.headers.get("x-forwarded-for", "")
    current = peer
    candidates = [part.strip() for part in forwarded.split(",") if part.strip()]
    for candidate in reversed(candidates):
        if not _trusted_proxy(current):
            break
        try:
            current = str(ipaddress.ip_address(candidate))
        except ValueError:
            break
    return current


def session_cookie_options(request: Request) -> dict:
    setting = get_settings().secure_cookie.lower()
    peer = str(request.client.host if request.client else "")
    forwarded_proto = (
        request.headers.get("x-forwarded-proto", "").split(",")[-1].strip().lower()
    )
    secure = setting == "true" or (
        setting == "auto"
        and (
            request.url.scheme == "https"
            or (_trusted_proxy(peer) and forwarded_proto == "https")
        )
    )
    return {
        "httponly": True,
        "samesite": "lax",
        "secure": secure,
        "path": "/admin/api",
    }


def record_auth_audit(action: str, username: str, ip: str) -> None:
    safe_username = "".join(char for char in username if char.isprintable()).strip()[:128]
    with database(get_settings().db_path) as conn:
        conn.execute(
            "INSERT INTO audit_logs(ts, actor, action, target, detail, ip) "
            "VALUES (?, ?, ?, ?, '', ?)",
            (int(time.time()), safe_username, action, safe_username, ip[:64]),
        )


def initialize_bootstrap_key() -> None:
    settings = get_settings()
    configured = settings.bootstrap_api_key.get_secret_value() if settings.bootstrap_api_key else ""
    if not configured:
        with database(settings.db_path) as conn:
            conn.execute("UPDATE api_keys SET enabled = 0 WHERE name = 'bootstrap'")
        return
    if not configured.startswith("sk-a2a-") or len(configured) < 40:
        raise RuntimeError("A2A_BOOTSTRAP_API_KEY must be a long sk-a2a-* key")
    now = int(time.time())
    with database(settings.db_path) as conn:
        conn.execute(
            """INSERT INTO api_keys(name, key_hash, key_encrypted, prefix, created_at, limit_rpm)
            VALUES ('bootstrap', ?, ?, ?, ?, ?)
            ON CONFLICT(name) WHERE name = 'bootstrap' DO UPDATE SET
                key_hash=excluded.key_hash, key_encrypted=excluded.key_encrypted,
                prefix=excluded.prefix, enabled=1,
                expires_at=NULL, channels='[]', models='["*"]',
                limit_rpm=excluded.limit_rpm""",
            (
                hash_api_key(configured),
                encrypt_secret(configured, settings.credential_master_key),
                configured[:14],
                now,
                settings.bootstrap_rpm,
            ),
        )


def issue_api_key() -> str:
    return "sk-a2a-" + secrets.token_urlsafe(32)


def require_api_key(
    authorization: str | None = Header(default=None),
    x_api_key: str | None = Header(default=None, alias="x-api-key"),
) -> dict:
    raw_key = ""
    if authorization:
        scheme, _, bearer_key = authorization.partition(" ")
        if scheme.lower() != "bearer" or not bearer_key.strip():
            raise HTTPException(status_code=401, detail="valid API key required")
        raw_key = bearer_key.strip()
        if x_api_key and not secrets.compare_digest(raw_key, x_api_key.strip()):
            raise HTTPException(status_code=401, detail="conflicting API key headers")
    elif x_api_key:
        raw_key = x_api_key.strip()
    if not raw_key:
        raise HTTPException(status_code=401, detail="valid API key required")
    settings = get_settings()
    with database(settings.db_path) as conn:
        conn.execute("BEGIN IMMEDIATE")
        row = conn.execute(
            "SELECT id, name, channels, models, expires_at, limit_rpm FROM api_keys "
            "WHERE key_hash = ? AND enabled = 1",
            (hash_api_key(raw_key),),
        ).fetchone()
        if row is None or (row["expires_at"] is not None and row["expires_at"] <= int(time.time())):
            raise HTTPException(status_code=401, detail="invalid or expired API key")
        if row["limit_rpm"]:
            now = int(time.time())
            conn.execute(
                "DELETE FROM api_key_rate_events WHERE key_id = ? AND ts < ?",
                (row["id"], now - 60),
            )
            recent_count = conn.execute(
                "SELECT COUNT(*) FROM api_key_rate_events WHERE key_id = ? AND ts >= ?",
                (row["id"], now - 60),
            ).fetchone()[0]
            if recent_count >= row["limit_rpm"]:
                raise HTTPException(
                    status_code=429,
                    detail="API key rate limit exceeded",
                    headers={"Retry-After": "60"},
                )
            conn.execute(
                "INSERT INTO api_key_rate_events(key_id, ts) VALUES (?, ?)",
                (row["id"], now),
            )
        conn.execute(
            "UPDATE api_keys SET last_used_at = ? WHERE id = ?",
            (int(time.time()), row["id"]),
        )
        return dict(row)


def require_admin_token(
    authorization: Annotated[str | None, Header()] = None,
) -> dict:
    configured = get_settings().admin_token.get_secret_value()
    scheme, _, supplied = (authorization or "").partition(" ")
    if not configured:
        raise HTTPException(status_code=503, detail="management token is not configured")
    if not configured.startswith("wbt_") or len(configured) < 36:
        raise HTTPException(status_code=503, detail="management token configuration is invalid")
    if scheme.lower() != "bearer" or not secrets.compare_digest(supplied.strip(), configured):
        raise HTTPException(status_code=401, detail="valid management token required")
    return {"role": "admin"}


def require_admin_request(
    request: Request,
    authorization: Annotated[str | None, Header()] = None,
) -> dict:
    if authorization is not None:
        return require_admin_token(authorization)
    cookie_value = request.cookies.get(SESSION_COOKIE)
    session = current_admin_session(cookie_value)
    if not session:
        raise HTTPException(status_code=401, detail="valid admin session required")
    record = session[1]
    return {"username": record.username, "role": record.role, "session_id": session[0]}
