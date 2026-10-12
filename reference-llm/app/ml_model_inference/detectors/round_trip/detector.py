# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""The round-trip detector runtime: two resident transformers + the scaler+logreg head.

Holds the MiniLM encoder and the NLI cross-encoder (both loaded once at startup from the
bundle, never HuggingFace) plus the loaded classifier, and scores a batch of guard-valid
P/A/Q triples into round-trip probabilities. Only the transformer scoring lives here — the
Go spine does the enumeration and the validity guard, because those depend on the authorship
stamp the spine already holds.

The detector is a recall-oriented router; E is the precision filter. The threshold comes from
the loaded classifier artifact, never runtime config.
"""

from __future__ import annotations

import structlog

from app.ml_model_inference.detectors.round_trip.classifier import RoundTripClassifier
from app.ml_model_inference.detectors.round_trip.features import (
    FEATURE_NAMES,
    NliLabelIndex,
    PairInput,
    extract_features,
    features_to_matrix,
    resolve_nli_label_index,
)

logger = structlog.get_logger(__name__)


def _nli_id2label(nli) -> dict[int, str] | None:
    """Pull ``id2label`` off a CrossEncoder (``nli.model.config.id2label``), tolerating doubles."""
    config = getattr(getattr(nli, "model", None), "config", None)
    return getattr(config, "id2label", None)


class RoundTripDetector:
    """Scores guard-valid pairs with the resident MiniLM + NLI + the fitted classifier."""

    def __init__(
        self,
        encoder,
        nli,
        classifier: RoundTripClassifier,
        *,
        encoder_id: str,
        nli_id: str,
        nli_label_index: NliLabelIndex | None = None,
    ) -> None:
        self._encoder = encoder
        self._nli = nli
        self._classifier = classifier
        self.encoder_id = encoder_id
        self.nli_id = nli_id
        # Resolve the NLI class indices BY LABEL NAME once (never by position — the reference
        # fit read idx 2 for "entailment", which is neutral). A test may inject the resolved
        # index directly to stay independent of a double's config shape.
        self._nli_label_index = nli_label_index or resolve_nli_label_index(_nli_id2label(nli))
        logger.info(
            "round-trip detector ready",
            encoder=encoder_id,
            nli=nli_id,
            threshold=classifier.threshold,
            placeholder_classifier=classifier.is_placeholder,
            nli_contradiction_idx=self._nli_label_index.contradiction,
            nli_entailment_idx=self._nli_label_index.entailment,
        )

    @property
    def threshold(self) -> float:
        """The operating point bound to the fitted classifier (in-artifact, not config)."""
        return self._classifier.threshold

    def score(self, pairs: list[PairInput]) -> list[dict]:
        """Score each guard-valid pair, returning its probability + the 19 features."""
        if not pairs:
            return []
        feature_dicts = extract_features(pairs, self._encoder, self._nli, self._nli_label_index)
        probabilities = self._classifier.score(features_to_matrix(feature_dicts))
        results: list[dict] = []
        for pair, features, probability in zip(pairs, feature_dicts, probabilities, strict=True):
            score = float(probability)
            results.append(
                {
                    "prompt_index": pair.prompt_index,
                    "round_trip_score": round(score, 4),
                    "is_round_trip": bool(score >= self.threshold),
                    "features": {name: round(features[name], 4) for name in FEATURE_NAMES},
                }
            )
        return results
