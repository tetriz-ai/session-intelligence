# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""ml_model_inference — fitted-model inference: framework + detectors.

Two layers:
  - ``framework/`` — task-agnostic machinery shared by every detector: the
    bundled-artifact integrity primitives (the ``model/`` sub-dir name and the
    SHA-256 hash-pin). Adding a detector never edits this layer.
  - ``detectors/`` — one self-contained package per fitted-model detector, which
    owns its own transformers, revision-pinned manifest, startup loader, and
    classification head. The round-trip PAIRS detector is the first member.

Kept as a single self-contained package so it can later be extracted into its own
service without a rewrite.

Python note: the conceptual name for this module is ``ml-model-inference``, but a
hyphenated name is not an importable Python package, so the on-disk package is
``ml_model_inference`` (PEP 8). Same module, valid import path.
"""
