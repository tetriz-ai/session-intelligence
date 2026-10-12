# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""The scaler + logistic-regression classification head — the JSON 19-coef artifact.

This is the detector's classification head: a
``StandardScaler`` (per-feature mean + scale) followed by a class-balanced
``LogisticRegression`` (19 coefficients + intercept), plus the operating threshold. All of
it is serialized as one small JSON so the head loads with no pickle and no sklearn-at-serve.

The threshold lives INSIDE this artifact, never in runtime config: it is bound to the fitted
scaler+coefficients, so a re-fit or a version bump is the only thing that may move it (design
 operating point). ``coef``/``mean``/``scale`` are indexed in ``FEATURE_NAMES`` order.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from app.ml_model_inference.detectors.round_trip.features import FEATURE_NAMES


@dataclass(frozen=True)
class RoundTripClassifier:
    """A loaded scaler + logistic-regression head over the 19-feature vector.

    ``feature_names`` is carried so the loader can assert the artifact was serialized against
    the exact frozen feature order this build extracts — a re-ordered fit served under this
    order would silently mis-weight every coefficient.
    """

    feature_names: tuple[str, ...]
    mean: np.ndarray
    scale: np.ndarray
    coef: np.ndarray
    intercept: float
    threshold: float
    is_placeholder: bool

    def score(self, feature_matrix: np.ndarray) -> np.ndarray:
        """Return the round-trip probability for each row of an (n, 19) feature matrix."""
        if feature_matrix.shape[0] == 0:
            return np.zeros((0,), dtype="float64")
        standardized = (feature_matrix - self.mean) / self.scale
        logits = standardized @ self.coef + self.intercept
        return 1.0 / (1.0 + np.exp(-logits))


def classifier_from_manifest(payload: dict) -> RoundTripClassifier:
    """Build a ``RoundTripClassifier`` from the parsed ``classifier.json`` payload.

    Fails loudly on a wrong feature order or a length mismatch — a head whose vectors do not
    line up with ``FEATURE_NAMES`` would serve confident-garbage scores otherwise.
    """
    feature_names = tuple(payload["feature_names"])
    if feature_names != FEATURE_NAMES:
        raise ValueError(
            f"classifier feature_names {feature_names} != serving order {FEATURE_NAMES} "
            "— the head was fit against a different feature ordering"
        )
    mean = np.asarray(payload["scaler_mean"], dtype="float64")
    scale = np.asarray(payload["scaler_scale"], dtype="float64")
    coef = np.asarray(payload["coef"], dtype="float64")
    for name, vector in (("scaler_mean", mean), ("scaler_scale", scale), ("coef", coef)):
        if vector.shape != (len(FEATURE_NAMES),):
            raise ValueError(f"classifier {name} has shape {vector.shape}, expected ({len(FEATURE_NAMES)},)")
    if (scale == 0).any():
        raise ValueError("classifier scaler_scale has a zero entry — would divide by zero at serve")
    threshold = float(payload["threshold"])
    if not 0.0 <= threshold <= 1.0:
        raise ValueError(f"classifier threshold {threshold} outside [0, 1]")
    return RoundTripClassifier(
        feature_names=feature_names,
        mean=mean,
        scale=scale,
        coef=coef,
        intercept=float(payload["intercept"]),
        threshold=threshold,
        is_placeholder=bool(payload.get("placeholder", False)),
    )
