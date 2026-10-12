# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Task-agnostic machinery shared by the fitted-model detectors.

Holds the bundled-artifact integrity primitives a detector reuses: the ``model/``
sub-dir name and the ``sha256_bytes`` hash-pin. Nothing here knows about a specific
detector — a detector owns its own model spec (its transformers, a revision-pinned
manifest, a startup loader, and a classification head) and reuses these primitives.
Adding a detector never edits this package.
"""
