# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""The round-trip detector bundle — ``manifest.json`` + ``classifier.json`` + two models.

A detector bundle for one version (``reference-llm/round-trip-detector/v1``) holds:

    classifier.json   the scaler+logreg+threshold head (placeholder in v1; see the file)
    manifest.json     { version, encoder_id, nli_id, encoder_revision, nli_revision,
                        classifier_sha256 } — pins BOTH transformer revisions, not only the
                        classifier
    model/            the MiniLM SentenceTransformer.save output (loaded from here, not HF)
    nli_model/        the nli-deberta-v3-small CrossEncoder.save output (loaded from here)

The manifest pins each transformer as a content hash of its WHOLE bundled model dir, so the
artifact pins the two transformer revisions, not only the classifier. Hashing only the
weights file would let a swapped tokenizer, vocab, config.json, or pooling/modules config slip
through — each changes the embeddings or the NLI inputs under the fixed scaler/logreg, the exact
drift the pin exists to catch. A frozen head over a MiniLM or NLI whose files silently change on
a restart would shift the 19-feature distribution invisibly to the feature-parity test — so the
loader recomputes each model dir's digest and asserts it equals the pinned revision at startup
(``assert_bundled_revisions``), failing loud rather than serving drift.

Reuses ``framework.artifact.sha256_bytes`` — the shared integrity primitive — to hash-pin its
bundled weights so a corrupt or swapped file fails the load.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path

from app.ml_model_inference.detectors.round_trip.classifier import RoundTripClassifier, classifier_from_manifest
from app.ml_model_inference.framework.artifact import MODEL_SUBDIR, sha256_bytes

# The detector's two bundled models. The encoder is MiniLM with its OWN resident instance
# (a detector owns its model topology); the NLI cross-encoder is the one new model dependency.
MANIFEST_FILENAME = "manifest.json"
CLASSIFIER_FILENAME = "classifier.json"
NLI_SUBDIR = "nli_model"
ENCODER_MODEL_ID = "all-MiniLM-L6-v2"
NLI_MODEL_ID = "cross-encoder/nli-deberta-v3-small"


@dataclass(frozen=True)
class RoundTripArtifact:
    """A loaded, integrity-checked detector bundle: the head + both models' pinned revisions.

    ``encoder_revision`` / ``nli_revision`` are the whole-dir content digests the manifest
    pinned and the loader has re-asserted against the bundled model dirs. ``model_dir`` /
    ``nli_dir`` are the local dirs the detector constructs the two transformers from (never
    HuggingFace).
    """

    version: str
    encoder_id: str
    nli_id: str
    encoder_revision: str
    nli_revision: str
    classifier: RoundTripClassifier
    model_dir: Path
    nli_dir: Path


def dir_sha256(model_dir: Path) -> str:
    """Deterministic digest of a saved transformer dir — its bundled revision pin.

    Covers EVERY file the runtime loads: weights, tokenizer, vocab, config.json, and
    pooling/modules config. Files are sorted by POSIX relative path (stable across filesystems),
    each hashed as ``relpath:sha256(bytes)`` (one file read at a time), and the per-file lines
    folded into one dir digest. So any swapped or edited file — not just the weights — changes
    the digest. Raises on an empty/absent dir: an incomplete bake must fail loud, never resolve
    to a constant hash that would pass the assertion.
    """
    if not model_dir.is_dir():
        raise FileNotFoundError(f"model dir {model_dir} does not exist — the bundled transformer is incomplete")
    files = sorted(path for path in model_dir.rglob("*") if path.is_file())
    if not files:
        raise FileNotFoundError(f"no files under {model_dir} — the bundled transformer is incomplete")
    lines = [f"{path.relative_to(model_dir).as_posix()}:{sha256_bytes(path.read_bytes())}" for path in files]
    return sha256_bytes("\n".join(lines).encode())


def assert_bundled_revisions(bundle_dir: Path, encoder_revision: str, nli_revision: str) -> None:
    """Recompute each bundled model dir's digest and assert it equals the pinned revision.

    The startup revision assertion: a swapped or silently-updated file in either
    model dir — weights, tokenizer, or config — changes the digest and fails here, before the
    detector serves a single pair under a fixed classifier. Testable without a model download —
    a fixture writes fake model dirs plus a manifest whose digests match (or do not).
    """
    actual_encoder = dir_sha256(bundle_dir / MODEL_SUBDIR)
    if actual_encoder != encoder_revision:
        raise ValueError(
            f"encoder dir digest {actual_encoder} != pinned revision {encoder_revision} "
            f"(bundle {bundle_dir}) — the bundled MiniLM changed under the fitted classifier"
        )
    actual_nli = dir_sha256(bundle_dir / NLI_SUBDIR)
    if actual_nli != nli_revision:
        raise ValueError(
            f"NLI dir digest {actual_nli} != pinned revision {nli_revision} "
            f"(bundle {bundle_dir}) — the bundled NLI changed under the fitted classifier"
        )


def load_artifact(bundle_dir: Path) -> RoundTripArtifact:
    """Load + integrity-check a detector bundle from ``bundle_dir`` (no model construction).

    Verifies the classifier bytes re-hash to the manifest ``classifier_sha256`` and that the
    bundled transformer weights match the pinned revisions. Raises on any mismatch so startup
    fails loudly rather than serving under a drifted model. The transformers themselves are
    constructed by the detector from ``model_dir``/``nli_dir`` after this check passes.
    """
    manifest_path = bundle_dir / MANIFEST_FILENAME
    if not manifest_path.exists():
        raise FileNotFoundError(f"detector manifest not found at {manifest_path}")
    manifest = json.loads(manifest_path.read_text())

    classifier_path = bundle_dir / CLASSIFIER_FILENAME
    if not classifier_path.exists():
        raise FileNotFoundError(f"detector classifier not found at {classifier_path}")
    classifier_bytes = classifier_path.read_bytes()
    expected_sha = manifest["classifier_sha256"]
    actual_sha = sha256_bytes(classifier_bytes)
    if actual_sha != expected_sha:
        raise ValueError(
            f"classifier sha256 mismatch: manifest {expected_sha} != file {actual_sha} "
            f"(bundle {bundle_dir}) — corrupt or swapped classifier artifact"
        )

    encoder_revision = str(manifest["encoder_revision"])
    nli_revision = str(manifest["nli_revision"])
    assert_bundled_revisions(bundle_dir, encoder_revision, nli_revision)

    classifier = classifier_from_manifest(json.loads(classifier_bytes))
    return RoundTripArtifact(
        version=str(manifest["version"]),
        encoder_id=str(manifest.get("encoder_id", ENCODER_MODEL_ID)),
        nli_id=str(manifest.get("nli_id", NLI_MODEL_ID)),
        encoder_revision=encoder_revision,
        nli_revision=nli_revision,
        classifier=classifier,
        model_dir=bundle_dir / MODEL_SUBDIR,
        nli_dir=bundle_dir / NLI_SUBDIR,
    )


def build_manifest(*, version: str, bundle_dir: Path) -> dict:
    """Assemble the manifest dict for a freshly-baked bundle (offline producer).

    Hashes the bundled classifier + both models' whole dirs so the pinned revisions ARE the
    bake-time digests the loader re-asserts. Called by ``generate.py`` after the models and
    classifier are written into ``bundle_dir``.
    """
    classifier_sha = sha256_bytes((bundle_dir / CLASSIFIER_FILENAME).read_bytes())
    return {
        "version": version,
        "encoder_id": ENCODER_MODEL_ID,
        "nli_id": NLI_MODEL_ID,
        "encoder_revision": dir_sha256(bundle_dir / MODEL_SUBDIR),
        "nli_revision": dir_sha256(bundle_dir / NLI_SUBDIR),
        "classifier_sha256": classifier_sha,
    }
