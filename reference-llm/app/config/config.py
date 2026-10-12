# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""
Configuration via Pydantic BaseSettings + YAML files.

Load order (later overrides earlier):
  1. {service}-params.yml            (shared defaults)
  2. {service}-{environment}.yml     (environment-specific overrides)
  3. Environment variables           (highest priority for explicit values)

The Gemini provider key is configured from a single LLM__GEMINI_API_KEY env var in
every environment — bring your own key and the service runs.
"""

from __future__ import annotations

import os
from enum import StrEnum
from pathlib import Path
from typing import Any

import structlog
import yaml
from pydantic import BaseModel
from pydantic_settings import BaseSettings, SettingsConfigDict

# Note: this module runs during bootstrap *before* setup_logging() configures
# structlog. Early log calls here flow through structlog's default config
# (plain stderr); once setup_logging() runs, subsequent module-load cycles
# are unaffected because these calls happen once at startup.
logger = structlog.get_logger(__name__)

CONFIG_TYPE = "yml"
DEFAULT_VALUES_DIRECTORY = "values"
SERVICE_NAME = "tetriz-py"

_CONFIG_FILE_NAME_FORMAT = "{service}-{env}"
_PARAMS_FILE_NAME_FORMAT = "{service}-params"


class Environment(StrEnum):
    DEVELOPMENT = "development"
    DEPLOY = "deploy"
    PROD = "prod"


class ServerConfig(BaseModel):
    HOST: str
    PORT: int
    # Defaulted so a bare environment (one with only the shared params.yml, e.g. production
    # with no env-specific file) still boots; development.yml overrides both.
    WORKERS: int = 1
    LOG_LEVEL: str = "info"


class LangfuseConfig(BaseModel):
    ENABLED: bool
    PUBLIC_KEY: str
    SECRET_KEY: str
    HOST: str
    # Safe default: raw transcripts / user content are NEVER exported to Langfuse traces
    # (only a length marker is). Flip to true (LANGFUSE__SEND_RAW_TRANSCRIPTS) to send raw
    # content for debugging — the session-analysis producers stay redacted regardless.
    SEND_RAW_TRANSCRIPTS: bool = False


class LLMConfig(BaseModel):
    REQUEST_TIMEOUT: int
    # The single Gemini provider key, from LLM__GEMINI_API_KEY in every environment.
    # Defaulted to empty so the service still boots in an environment with no key (e.g.
    # production with no env-specific file and the env var unset); LLM calls then fail until
    # it is set, and main.py logs that at startup.
    GEMINI_API_KEY: str = ""

    # Primary / fallback models for the Gemini model fallback; see gemini_fallback.py for
    # the mechanism. Use explicit versioned ids (the version is what selects each model's
    # config style) — a floating alias that resolves to a 2.x model would be mis-detected.
    GEMINI_PRIMARY_MODEL: str = "gemini-2.5-flash"
    GEMINI_FALLBACK_MODEL: str = "gemini-3.5-flash"
    # Below this many seconds of budget left after a failed primary, the fallback attempt
    # is skipped (it can't finish within the deadline). See gemini_fallback.py.
    GEMINI_FALLBACK_MIN_BUDGET_SECONDS: float = 10.0

    # TTL for an explicit Gemini system-prompt cache (app/llm/prompt_cache.py), engaged per
    # call by the `use_system_prompt_cache` request flag. 24h on the explicit path:
    # long enough that a stable authored system prompt stays cached across a day of cron
    # ticks, short enough that an edit's superseded key expires rather than lingering.
    SYSTEM_PROMPT_CACHE_TTL_SECONDS: int = 86_400


class MLModelInferenceConfig(BaseModel):
    """Config for the round-trip PAIRS detector's bundled model.

    ARTIFACT_ROOT is the directory holding the baked detector bundle. In deploy this is the
    local image path the bundle is **baked into at build**: the versioned bundle is copied into
    the image at build time and the running pod loads it from that local path at startup —
    nothing is fetched at runtime. Empty → use the in-repo artifacts dir (dev/local).
    """

    ARTIFACT_ROOT: str = ""
    # DETECTOR_ENABLED gates the round-trip PAIRS detector (MiniLM + an NLI cross-encoder + a
    # scaler/logreg head) at startup. It ships false: the detector's bundle bakes into the image
    # separately and its fitted classifier is corpus-gated, so the machinery loads only where an
    # operator arms it. A true flip on an image without the bundle fails the pod loud (the loader
    # raises), never silently serving garbage.
    DETECTOR_ENABLED: bool = False


class FeaturesConfig(BaseModel):
    ANALYZE_PROMPT_VALIDATION: bool = False
    # Enables the 2.5→3.5 Gemini model fallback (see gemini_fallback.py). Per-pod kill
    # switch: FEATURES__GEMINI_MODEL_FALLBACK_ENABLED, off → single-model, no redeploy.
    GEMINI_MODEL_FALLBACK_ENABLED: bool = False


class Config(BaseSettings):
    model_config = SettingsConfigDict(
        env_prefix="",
        case_sensitive=True,
        env_nested_delimiter="__",
    )

    ENVIRONMENT: Environment
    server: ServerConfig
    llm: LLMConfig
    langfuse: LangfuseConfig
    features: FeaturesConfig = FeaturesConfig()
    ml_model_inference: MLModelInferenceConfig = MLModelInferenceConfig()

    @classmethod
    def from_yaml_and_env(cls, config_dir: str | None = None) -> Config:
        merged: dict[str, Any] = {}
        resolved_dir = config_dir or get_config_dir()

        params_path = get_service_params_path(resolved_dir)
        if params_path.exists():
            with open(params_path) as f:
                data = yaml.safe_load(f) or {}
                _deep_merge(merged, data)

        env = os.environ.get("ENVIRONMENT")
        if not env:
            logger.warning(
                "ENVIRONMENT env var is not set or empty; loading shared params only (no env-specific overrides)",
            )
        else:
            env_path = get_service_config_path(env, resolved_dir)
            if env_path.exists():
                with open(env_path) as f:
                    data = yaml.safe_load(f) or {}
                    _deep_merge(merged, data)

        # Env vars override YAML values. For top-level keys (ENVIRONMENT)
        # and nested keys (SERVER__PORT -> server.PORT).
        _apply_env_overrides(merged)

        return cls(**merged)


_config: Config | None = None


def get_config() -> Config:
    global _config
    if _config is None:
        _config = Config.from_yaml_and_env()
    return _config


# The nested config groups a GROUP__FIELD env var may target. Seeded on demand so an override
# lands even when the group has no section in the loaded YAML — e.g. a params-only prod boot,
# where features/ml_model_inference have no YAML and an operator still needs
# ML_MODEL_INFERENCE__DETECTOR_ENABLED / FEATURES__GEMINI_MODEL_FALLBACK_ENABLED to take effect.
_NESTED_GROUPS = ("server", "llm", "langfuse", "features", "ml_model_inference")


def _apply_env_overrides(merged: dict[str, Any]) -> None:
    """Override YAML values with env vars. Handles flat (ENVIRONMENT)
    and nested (SERVER__PORT -> server.PORT) keys using __ as delimiter."""
    for env_key, env_val in os.environ.items():
        parts = env_key.split("__")
        if len(parts) == 1:
            if env_key in merged:
                merged[env_key] = env_val
        elif len(parts) == 2:
            group = parts[0].lower()
            field = parts[1]
            if group in _NESTED_GROUPS:
                section = merged.setdefault(group, {})
                if isinstance(section, dict):
                    section[field] = env_val


def _deep_merge(base: dict[str, Any], override: dict[str, Any]) -> None:
    for key, value in override.items():
        if key in base and isinstance(base[key], dict) and isinstance(value, dict):
            _deep_merge(base[key], value)
        else:
            base[key] = value


def get_config_dir() -> str:
    config_dir = os.environ.get("CONFIG_DIR")
    if config_dir:
        return config_dir
    return str(Path(__file__).parent / DEFAULT_VALUES_DIRECTORY)


def _get_params_file_name() -> str:
    return _PARAMS_FILE_NAME_FORMAT.format(service=SERVICE_NAME)


def _get_config_file_name(env: str) -> str:
    return _CONFIG_FILE_NAME_FORMAT.format(service=SERVICE_NAME, env=env)


def get_service_params_path(config_dir: str) -> Path:
    return Path(config_dir) / f"{_get_params_file_name()}.{CONFIG_TYPE}"


def get_service_config_path(env: str, config_dir: str) -> Path:
    return Path(config_dir) / f"{_get_config_file_name(env)}.{CONFIG_TYPE}"
