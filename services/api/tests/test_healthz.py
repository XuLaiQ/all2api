from __future__ import annotations

from types import SimpleNamespace

from fastapi import FastAPI
from fastapi.testclient import TestClient

from app import main
from app.infrastructure import db


def test_healthz_detail_reports_configuration_without_probing_upstream(tmp_path, monkeypatch):
    db_path = tmp_path / "healthz.db"
    db.migrate(str(db_path))
    settings = SimpleNamespace(db_path=str(db_path))
    registry = {
        "ready": SimpleNamespace(
            slug="ready",
            models_configured=True,
            provision_configured=True,
        ),
        "pending": SimpleNamespace(
            slug="pending",
            models_configured=False,
            provision_configured=True,
        ),
    }
    monkeypatch.setattr(main, "settings", settings)
    monkeypatch.setattr(main, "get_registry", lambda _settings: registry)
    app = FastAPI()
    app.include_router(main.admin_router)

    with TestClient(app) as client:
        basic = client.get("/admin/api/healthz")
        detailed = client.get("/admin/api/healthz?detail=true")

    assert basic.json() == {"status": "ok", "service": "all2api-api", "database": "ok"}
    body = detailed.json()
    assert body["status"] == "degraded"
    assert body["checks"]["storage"] == {"status": "ready"}
    assert body["checks"]["channels"]["ready"]["platform_probe"] == "not_run"
    assert body["checks"]["channels"]["pending"]["status"] == "not_configured"
