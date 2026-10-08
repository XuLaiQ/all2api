"""Durable storage for generated images, videos and editable files."""

from __future__ import annotations

import base64
import binascii
import json
import mimetypes
import re
import time
import uuid
from collections.abc import Mapping
from pathlib import Path
from typing import Any

import httpx

from app.infrastructure.db import database, resolve_db_path
from app.infrastructure.http import build_client
from app.infrastructure.watermark import remove_doubao_watermark

MAX_ASSET_BYTES = 100 * 1024 * 1024
_KIND_VALUES = {"image", "video", "ppt", "psd", "archive", "other"}


def _safe_filename(value: object, fallback: str) -> str:
    name = Path(str(value or "")).name.replace("\x00", "").strip()
    name = re.sub(r"[^A-Za-z0-9._-]+", "-", name).strip(".-")
    return (name or fallback)[:160]


def _kind(value: str) -> str:
    normalized = str(value or "").strip().lower()
    return normalized if normalized in _KIND_VALUES else "other"


def _extension(mime_type: str, fallback: str) -> str:
    extension = mimetypes.guess_extension(str(mime_type or "").split(";", 1)[0].strip())
    return extension or fallback


class MediaAssetStore:
    """Store metadata in SQLite and bytes beside the configured database."""

    def __init__(self, db_path: str) -> None:
        self.db_path = db_path
        self.root = resolve_db_path(db_path).parent / "media-assets"
        self.root.mkdir(parents=True, exist_ok=True)

    def _record(
        self,
        *,
        actor: str,
        run_id: str | None,
        conversation_id: str | None,
        channel: str,
        model: str,
        kind: str,
        mime_type: str,
        filename: str,
        storage_path: str | None,
        source_url: str | None,
        size_bytes: int,
        metadata: Mapping[str, Any] | None,
        asset_id: str | None = None,
    ) -> dict[str, Any]:
        asset_id = asset_id or f"asset_{uuid.uuid4().hex}"
        now = int(time.time())
        relative_path = storage_path or None
        serialized_metadata = json.dumps(dict(metadata or {}), ensure_ascii=False)
        with database(self.db_path) as conn:
            conn.execute(
                """INSERT INTO media_assets
                (id, actor, run_id, conversation_id, channel, model, kind, mime_type,
                 filename, storage_path, source_url, size_bytes, metadata, created_at, updated_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                (
                    asset_id,
                    str(actor or "admin")[:128],
                    run_id,
                    conversation_id,
                    str(channel or "")[:64],
                    str(model or "")[:256],
                    _kind(kind),
                    str(mime_type or "application/octet-stream")[:128],
                    filename,
                    relative_path,
                    source_url,
                    int(size_bytes),
                    serialized_metadata,
                    now,
                    now,
                ),
            )
        return {
            "id": asset_id,
            "actor": str(actor or "admin"),
            "run_id": run_id,
            "conversation_id": conversation_id,
            "channel": channel,
            "model": model,
            "kind": _kind(kind),
            "mime_type": mime_type,
            "filename": filename,
            "storage_path": relative_path,
            "source_url": source_url,
            "size_bytes": int(size_bytes),
            "metadata": dict(metadata or {}),
            "created_at": now,
            "updated_at": now,
        }

    def add_bytes(
        self,
        data: bytes,
        *,
        actor: str,
        channel: str,
        model: str,
        kind: str,
        mime_type: str,
        filename: str,
        run_id: str | None = None,
        conversation_id: str | None = None,
        source_url: str | None = None,
        metadata: Mapping[str, Any] | None = None,
    ) -> dict[str, Any]:
        payload = bytes(data)
        if not payload:
            raise ValueError("media asset is empty")
        if len(payload) > MAX_ASSET_BYTES:
            raise ValueError("media asset exceeds the 100 MB limit")
        asset_id = f"asset_{uuid.uuid4().hex}"
        safe_name = _safe_filename(filename, f"{_kind(kind)}{_extension(mime_type, '.bin')}")
        folder = self.root / asset_id
        folder.mkdir(parents=True, exist_ok=False)
        path = folder / safe_name
        try:
            path.write_bytes(payload)
            relative_path = str(path.relative_to(self.root))
            record = self._record(
                actor=actor,
                run_id=run_id,
                conversation_id=conversation_id,
                channel=channel,
                model=model,
                kind=kind,
                mime_type=mime_type,
                filename=safe_name,
                storage_path=relative_path,
                source_url=source_url,
                size_bytes=len(payload),
                metadata=metadata,
                asset_id=asset_id,
            )
        except Exception:
            path.unlink(missing_ok=True)
            folder.rmdir()
            raise
        return record

    async def add_url(
        self,
        url: str,
        *,
        actor: str,
        channel: str,
        model: str,
        kind: str,
        filename: str,
        run_id: str | None = None,
        conversation_id: str | None = None,
        metadata: Mapping[str, Any] | None = None,
    ) -> dict[str, Any]:
        source_url = str(url or "").strip()
        if not source_url.startswith(("http://", "https://")):
            raise ValueError("media source URL is invalid")
        client = build_client(timeout=90, connect_timeout=10)
        try:
            response = await client.get(source_url)
            response.raise_for_status()
            data = bytes(response.content)
            mime_type = str(response.headers.get("content-type") or "").split(";", 1)[0].strip()
            mime_type = mime_type or mimetypes.guess_type(filename)[0] or "application/octet-stream"
            return self.add_bytes(
                data,
                actor=actor,
                channel=channel,
                model=model,
                kind=kind,
                mime_type=mime_type,
                filename=filename,
                run_id=run_id,
                conversation_id=conversation_id,
                source_url=source_url,
                metadata=metadata,
            )
        except (httpx.HTTPError, ValueError):
            # Preserve the result as a remote asset when a signed CDN URL
            # cannot be downloaded. The library can still open the source URL.
            return self._record(
                actor=actor,
                run_id=run_id,
                conversation_id=conversation_id,
                channel=channel,
                model=model,
                kind=kind,
                mime_type=mimetypes.guess_type(filename)[0] or "application/octet-stream",
                filename=_safe_filename(filename, _kind(kind)),
                storage_path=None,
                source_url=source_url,
                size_bytes=0,
                metadata={**dict(metadata or {}), "storage_status": "remote"},
            )
        finally:
            await client.aclose()

    async def add_generation_response(
        self,
        response: object,
        *,
        actor: str,
        channel: str,
        model: str,
        capability: str,
        run_id: str | None = None,
        conversation_id: str | None = None,
        metadata: Mapping[str, Any] | None = None,
    ) -> list[dict[str, Any]]:
        if not isinstance(response, Mapping):
            return []
        values = response.get("data")
        if not isinstance(values, list):
            return []
        assets: list[dict[str, Any]] = []
        kind = "video" if capability == "video" else "image"
        for index, value in enumerate(values, start=1):
            if not isinstance(value, Mapping):
                continue
            item_metadata = dict(metadata or {})
            if value.get("watermark_free") is True:
                item_metadata.update(
                    {
                        "watermark_free": True,
                        "source_variant": str(value.get("source_variant") or "original"),
                    }
                )
            b64_value = value.get("b64_json")
            if isinstance(b64_value, str) and b64_value.strip():
                try:
                    data = base64.b64decode(b64_value, validate=True)
                except (binascii.Error, ValueError):
                    data = b""
                if data:
                    if kind == "image" and channel == "doubao":
                        data = remove_doubao_watermark(data)
                        item_metadata.update(
                            {
                                "watermark_free": True,
                                "watermark_method": "opencv_telea_inpaint",
                            }
                        )
                    assets.append(
                        self.add_bytes(
                            data,
                            actor=actor,
                            channel=channel,
                            model=model,
                            kind=kind,
                            mime_type="video/mp4" if kind == "video" else "image/png",
                            filename=f"{kind}_{index}.{'mp4' if kind == 'video' else 'png'}",
                            run_id=run_id,
                            conversation_id=conversation_id,
                            metadata=item_metadata,
                        )
                    )
                    continue
            source_url = value.get("video_url") or value.get("url")
            if isinstance(source_url, str) and source_url:
                extension = ".mp4" if kind == "video" else ".png"
                if kind == "image" and channel == "doubao":
                    try:
                        remote = await self._download_url(source_url)
                    except (httpx.HTTPError, ValueError):
                        remote = b""
                    if remote:
                        repaired = remove_doubao_watermark(remote)
                        assets.append(
                            self.add_bytes(
                                repaired,
                                actor=actor,
                                channel=channel,
                                model=model,
                                kind=kind,
                                mime_type="image/png",
                                filename=f"{kind}_{index}.png",
                                run_id=run_id,
                                conversation_id=conversation_id,
                                metadata={
                                    **item_metadata,
                                    "watermark_free": True,
                                    "watermark_method": "opencv_telea_inpaint",
                                },
                            )
                        )
                        continue
                assets.append(
                    await self.add_url(
                        source_url,
                        actor=actor,
                        channel=channel,
                        model=model,
                        kind=kind,
                        filename=f"{kind}_{index}{extension}",
                        run_id=run_id,
                        conversation_id=conversation_id,
                        metadata=item_metadata,
                    )
                )
        return assets

    async def _download_url(self, url: str) -> bytes:
        client = build_client(timeout=90, connect_timeout=10)
        try:
            response = await client.get(url)
            response.raise_for_status()
            data = bytes(response.content)
            if len(data) > MAX_ASSET_BYTES:
                raise ValueError("media asset exceeds the 100 MB limit")
            return data
        finally:
            await client.aclose()

    def list_assets(
        self,
        *,
        actor: str,
        page: int,
        page_size: int,
        kind: str | None = None,
        channel: str | None = None,
        search: str | None = None,
    ) -> tuple[list[dict[str, Any]], int]:
        filters = ["actor = ?"]
        values: list[Any] = [actor]
        if kind:
            filters.append("kind = ?")
            values.append(_kind(kind))
        if channel:
            filters.append("channel = ?")
            values.append(channel)
        if search:
            filters.append("(filename LIKE ? OR model LIKE ? OR channel LIKE ?)")
            needle = f"%{search}%"
            values.extend((needle, needle, needle))
        where = " AND ".join(filters)
        offset = (page - 1) * page_size
        with database(self.db_path) as conn:
            count_row = conn.execute(
                f"SELECT COUNT(*) FROM media_assets WHERE {where}", values
            ).fetchone()
            total = int(
                count_row[0]
            )
            rows = conn.execute(
                f"""SELECT id, actor, run_id, conversation_id, channel, model, kind,
                mime_type, filename, storage_path, source_url, size_bytes, metadata,
                created_at, updated_at FROM media_assets WHERE {where}
                ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?""",
                [*values, page_size, offset],
            ).fetchall()
        return [self._row(row) for row in rows], total

    def get(self, asset_id: str, actor: str) -> dict[str, Any] | None:
        with database(self.db_path) as conn:
            row = conn.execute(
                "SELECT * FROM media_assets WHERE id = ? AND actor = ?",
                (asset_id, actor),
            ).fetchone()
        return self._row(row) if row is not None else None

    def delete(self, asset_id: str, actor: str) -> bool:
        record = self.get(asset_id, actor)
        if record is None:
            return False
        storage_path = record.get("storage_path")
        if storage_path:
            path = (self.root / str(storage_path)).resolve()
            if self.root.resolve() in path.parents and path.is_file():
                path.unlink()
                try:
                    path.parent.rmdir()
                except OSError:
                    pass
        with database(self.db_path) as conn:
            conn.execute("DELETE FROM media_assets WHERE id = ? AND actor = ?", (asset_id, actor))
        return True

    def content_path(self, record: Mapping[str, Any]) -> Path | None:
        storage_path = record.get("storage_path")
        if not storage_path:
            return None
        path = (self.root / str(storage_path)).resolve()
        if self.root.resolve() not in path.parents or not path.is_file():
            return None
        return path

    @staticmethod
    def _row(row: Any) -> dict[str, Any]:
        metadata: dict[str, Any] = {}
        try:
            parsed = json.loads(row["metadata"] or "{}")
            if isinstance(parsed, dict):
                metadata = parsed
        except (TypeError, ValueError, json.JSONDecodeError):
            pass
        return {
            "id": str(row["id"]),
            "actor": str(row["actor"]),
            "run_id": row["run_id"],
            "conversation_id": row["conversation_id"],
            "channel": str(row["channel"]),
            "model": str(row["model"] or ""),
            "kind": str(row["kind"]),
            "mime_type": str(row["mime_type"]),
            "filename": str(row["filename"]),
            "storage_path": row["storage_path"],
            "source_url": row["source_url"],
            "size_bytes": int(row["size_bytes"] or 0),
            "metadata": metadata,
            "created_at": int(row["created_at"]),
            "updated_at": int(row["updated_at"]),
        }


__all__ = ["MAX_ASSET_BYTES", "MediaAssetStore"]
