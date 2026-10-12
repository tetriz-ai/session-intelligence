# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Exercise shipped config defaults and the overrides used by standalone deployments."""

import os

import pytest

from app.config.config import Config, Environment


@pytest.fixture(autouse=True)
def isolated_environment(monkeypatch):
    # Keep tests independent of real provider keys, config mounts, and AWS settings.
    monkeypatch.setattr(os, "environ", {"ENVIRONMENT": "development"})


def test_shipped_development_config_loads():
    config = Config.from_yaml_and_env()

    assert config.ENVIRONMENT == Environment.DEVELOPMENT
    assert config.server.HOST == "0.0.0.0"
    assert config.server.PORT == 8000
    assert config.server.WORKERS == 1
    assert config.server.LOG_LEVEL == "info"
    assert config.llm.REQUEST_TIMEOUT == 180
    assert config.llm.GEMINI_API_KEY == ""
    assert config.features.ANALYZE_PROMPT_VALIDATION is True
    assert config.features.GEMINI_MODEL_FALLBACK_ENABLED is True
    assert config.ml_model_inference.DETECTOR_ENABLED is False
    assert config.langfuse.ENABLED is False


def test_environment_overrides_shipped_config(monkeypatch):
    monkeypatch.setenv("SERVER__PORT", "9000")
    monkeypatch.setenv("LLM__GEMINI_API_KEY", "test-provider-key")
    monkeypatch.setenv("FEATURES__GEMINI_MODEL_FALLBACK_ENABLED", "false")

    config = Config.from_yaml_and_env()

    assert config.server.PORT == 9000
    assert config.server.WORKERS == 1
    assert config.llm.GEMINI_API_KEY == "test-provider-key"
    assert config.features.GEMINI_MODEL_FALLBACK_ENABLED is False


def test_env_overrides_apply_to_groups_absent_from_yaml(tmp_path, monkeypatch):
    # The shipped prod image loads params.yml only (no tetriz-py-prod.yml), and params.yml has
    # no features/ml_model_inference sections. An operator must still be able to toggle those
    # groups via env vars — the override must not be silently dropped for an absent group.
    (tmp_path / "tetriz-py-params.yml").write_text(
        "server: {HOST: '0.0.0.0', PORT: 8000, WORKERS: 1, LOG_LEVEL: info}\n"
        "llm: {REQUEST_TIMEOUT: 180, GEMINI_API_KEY: ''}\n"
        "langfuse: {ENABLED: false, PUBLIC_KEY: '', SECRET_KEY: '', HOST: ''}\n"
    )
    monkeypatch.setenv("ENVIRONMENT", "prod")
    monkeypatch.setenv("CONFIG_DIR", str(tmp_path))
    monkeypatch.setenv("ML_MODEL_INFERENCE__DETECTOR_ENABLED", "true")
    monkeypatch.setenv("FEATURES__GEMINI_MODEL_FALLBACK_ENABLED", "true")

    config = Config.from_yaml_and_env()

    assert config.ENVIRONMENT == Environment.PROD
    assert config.ml_model_inference.DETECTOR_ENABLED is True
    assert config.features.GEMINI_MODEL_FALLBACK_ENABLED is True


@pytest.mark.parametrize("environment", ["development", "deploy", "prod"])
def test_config_dir_loads_tetriz_py_files_with_environment_precedence(tmp_path, monkeypatch, environment):
    (tmp_path / "tetriz-py-params.yml").write_text(
        "server: {HOST: '127.0.0.1', PORT: 8100, WORKERS: 1, LOG_LEVEL: warning}\n"
        "llm: {REQUEST_TIMEOUT: 90, GEMINI_API_KEY: ''}\n"
        "langfuse: {ENABLED: false, PUBLIC_KEY: '', SECRET_KEY: '', HOST: ''}\n"
    )
    (tmp_path / f"tetriz-py-{environment}.yml").write_text("server: {PORT: 8200, WORKERS: 2}\n")
    monkeypatch.setenv("ENVIRONMENT", environment)
    monkeypatch.setenv("CONFIG_DIR", str(tmp_path))
    monkeypatch.setenv("SERVER__PORT", "8300")

    config = Config.from_yaml_and_env()

    assert config.ENVIRONMENT == Environment(environment)
    assert config.server.HOST == "127.0.0.1"
    assert config.server.PORT == 8300
    assert config.server.WORKERS == 2
    assert config.server.LOG_LEVEL == "warning"
    assert config.llm.REQUEST_TIMEOUT == 90
