# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Startup loader for the round-trip detector.

Called once from the FastAPI lifespan when the detector is enabled. Resolves the bundle root,
loads + integrity-checks the artifact (classifier hash + the two pinned transformer
revisions), constructs the resident MiniLM encoder and NLI cross-encoder from the bundled
dirs, and returns the ``RoundTripDetector``. Any failure raises so startup crashes rather than
serving a half-loaded detector — the load gates startup, never the probe.
"""

from __future__ import annotations

from pathlib import Path

import structlog

from app.ml_model_inference.detectors.round_trip.artifact import load_artifact
from app.ml_model_inference.detectors.round_trip.detector import RoundTripDetector

logger = structlog.get_logger(__name__)

# The detector bundle's sub-path under the configured artifact root, doubling as the version.
ARTIFACT = "reference-llm/round-trip-detector/v1"

# Default artifact root when unset: the in-repo artifacts dir at the package root (gitignored;
# dev runs the generator to bake the bundle here). ``parents[2]`` walks
# round_trip/ -> detectors/ -> ml_model_inference/, then the "artifacts" dir under it.
_DEFAULT_ARTIFACT_ROOT = Path(__file__).resolve().parents[2] / "artifacts"


def load_detector(artifact_root: str = "") -> RoundTripDetector:
    """Load the round-trip detector bundle and construct its two resident transformers.

    ``artifact_root`` is the dir holding ``<root>/reference-llm/round-trip-detector/v1/``; empty
    resolves to the in-repo artifacts dir (dev). Raises on a missing/corrupt bundle, a
    classifier-hash mismatch, or a transformer-revision mismatch.
    """
    # sentence_transformers is imported lazily so a build without it (or a disabled detector)
    # never pays the import at module load — the lazy-import posture for an optional model.
    from sentence_transformers import CrossEncoder, SentenceTransformer

    root = Path(artifact_root) if artifact_root else _DEFAULT_ARTIFACT_ROOT
    if not root.exists():
        raise FileNotFoundError(f"detector artifact root {root} does not exist — is the round-trip bundle synced?")
    bundle_dir = root / ARTIFACT
    artifact = load_artifact(bundle_dir)
    logger.info(
        "loading round-trip detector bundle",
        bundle=str(bundle_dir),
        version=artifact.version,
        encoder=artifact.encoder_id,
        nli=artifact.nli_id,
    )
    # CPU explicitly: prod is CPU-only and forcing CPU on a
    # dev box keeps inference numerically identical to prod rather than diverging onto MPS.
    encoder = SentenceTransformer(str(artifact.model_dir), device="cpu")
    nli = CrossEncoder(str(artifact.nli_dir), device="cpu")
    return RoundTripDetector(
        encoder,
        nli,
        artifact.classifier,
        encoder_id=artifact.encoder_id,
        nli_id=artifact.nli_id,
    )
