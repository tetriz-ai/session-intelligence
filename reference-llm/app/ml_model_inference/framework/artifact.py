# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Shared bundled-artifact primitives for the model-inference detectors.

A detector ships a self-contained bundle on disk — a ``model/`` sub-directory holding its
transformer (SentenceTransformer.save output) alongside the detector's own manifest and
fitted head. This module holds the two primitives every detector's loader reuses: the
``model/`` sub-dir name and the SHA-256 digest used to hash-pin the bundled bytes.
"""

from __future__ import annotations

import hashlib

# Sub-dir name for a bundle's transformer (SentenceTransformer.save output).
MODEL_SUBDIR = "model"


def sha256_bytes(data: bytes) -> str:
    """Hex SHA-256 of ``data`` — the artifact integrity primitive.

    A detector's loader hash-pins its bundled weights by comparing this digest against the
    one recorded in its manifest, so a corrupt or swapped file fails the load loudly.
    """
    return hashlib.sha256(data).hexdigest()
