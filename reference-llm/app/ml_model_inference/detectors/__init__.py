# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Fitted-model detectors — a family of model-inference detectors.

A detector reuses the framework's bundled-artifact integrity primitive
(``framework.artifact.sha256_bytes``) but owns its own model spec — its transformers,
a revision-pinned manifest, a startup loader, and its own classification head. The
round-trip PAIRS detector is the first member: it constructs a MiniLM encoder plus an
NLI cross-encoder and serves a scaler+logistic-regression head over a 19-feature vector.
"""
