from __future__ import annotations

from types import SimpleNamespace

from fastapi import FastAPI
from fastapi.testclient import TestClient

from app.infrastructure import db, security
from app.routers import admin


def _app(role: str) -> FastAPI:
    app = FastAPI()
    app.dependency_overrides[security.require_admin_request] = lambda: {
        "role": role,
        "username": role,
    }
    app.include_router(admin.router)
    return app


def test_settings_are_readable_and_admin_updates_are_persisted_and_audited(
    tmp_path, monkeypatch
):
    db_path = tmp_path / "settings.db"
    db.migrate(str(db_path))
    monkeypatch.setattr(
        admin,
        "get_settings",
        lambda: SimpleNamespace(
            db_path=str(db_path),
            log_retention_days=30,
            usage_retention_days=365,
        ),
    )

    with TestClient(_app("viewer")) as viewer:
        initial = viewer.get("/admin/api/settings")
        assert initial.status_code == 200
        assert initial.json()["data"]["values"] == {
            "log_retention_days": 30,
            "usage_retention_days": 365,
        }
        denied = viewer.post("/admin/api/settings", json={"log_retention_days": 14})
        assert denied.status_code == 403

    with TestClient(_app("admin")) as admin_client:
        updated = admin_client.post(
            "/admin/api/settings",
            json={"log_retention_days": 14, "usage_retention_days": 90},
        )
        assert updated.status_code == 200
        body = updated.json()["data"]
        assert body["values"] == {
            "log_retention_days": 14,
            "usage_retention_days": 90,
        }
        assert body["sources"] == {
            "log_retention_days": "database",
            "usage_retention_days": "database",
        }

    with TestClient(_app("viewer")) as viewer:
        persisted = viewer.get("/admin/api/settings")
    assert persisted.json()["data"]["values"]["log_retention_days"] == 14
    assert admin._retention_settings()[0]["usage_retention_days"] == 90

    with db.database(str(db_path)) as conn:
        audit = conn.execute(
            "SELECT actor, action, target, detail FROM audit_logs "
            "WHERE action = 'update_settings'"
        ).fetchone()
    assert tuple(audit) == (
        "admin",
        "update_settings",
        "retention",
        "changed=log_retention_days,usage_retention_days",
    )


def test_settings_reject_invalid_retention_order_and_unknown_fields(tmp_path, monkeypatch):
    db_path = tmp_path / "settings-validation.db"
    db.migrate(str(db_path))
    monkeypatch.setattr(
        admin,
        "get_settings",
        lambda: SimpleNamespace(
            db_path=str(db_path),
            log_retention_days=30,
            usage_retention_days=365,
        ),
    )

    with TestClient(_app("admin")) as client:
        reversed_values = client.post(
            "/admin/api/settings",
            json={"log_retention_days": 100, "usage_retention_days": 90},
        )
        assert reversed_values.status_code == 422
        assert "greater than or equal" in reversed_values.json()["detail"]

        unknown = client.post(
            "/admin/api/settings",
            json={"session_secret": "do-not-store"},
        )
        assert unknown.status_code == 422
