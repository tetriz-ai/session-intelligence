# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""The round-trip PAIRS detector — a fitted-model detector.

Reuses the framework's resident-encoder + bundled-artifact primitives, adds the NLI
cross-encoder, and serves a scaler+logistic-regression head over a 19-feature vector. The Go
spine enumerates and guards the P/A/Q triples; this scores them.
"""

from __future__ import annotations

from app.ml_model_inference.detectors.round_trip.detector import RoundTripDetector
from app.ml_model_inference.detectors.round_trip.features import PairInput
from app.ml_model_inference.detectors.round_trip.loader import ARTIFACT, load_detector

__all__ = ["ARTIFACT", "PairInput", "RoundTripDetector", "load_detector"]
