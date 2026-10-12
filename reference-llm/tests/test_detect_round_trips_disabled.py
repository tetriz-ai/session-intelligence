# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""The round-trip detector ships as an uncalibrated placeholder, so it is disabled by
default and the endpoint returns 503 until an operator bakes and enables a real bundle."""

import os

os.environ.setdefault("ENVIRONMENT", "development")


def test_detector_enabled_defaults_to_false():
    from app.config.config import get_config

    config = get_config()
    assert config.ml_model_inference.DETECTOR_ENABLED is False


def test_detect_round_trips_returns_503_when_detector_disabled():
    from fastapi.testclient import TestClient

    from app.main import app

    # The lifespan runs on context entry; with DETECTOR_ENABLED false the detector is
    # never loaded, so app.state.round_trip_detector stays None.
    with TestClient(app) as client:
        resp = client.post("/api/v1/detect-round-trips", json={"pairs": []})
    assert resp.status_code == 503
