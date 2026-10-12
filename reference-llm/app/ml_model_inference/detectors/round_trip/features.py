# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""The round-trip 19-feature extractor — the ONE shared function.

Both the future corpus fit and the serving path call ``extract_features`` so they compute
byte-identical vectors; a feature-parity test pins that. The classifier's coefficients bind
to ``FEATURE_NAMES`` (alphabetical, the reference ``sorted(f)`` order), so that order is
frozen. Ported verbatim from the reference fit:

    cos_P_resp, cos_Q_P, cos_Q_prior, cos_Q_resp, cos_Q_resp_minus_Q_P, ends_q, has_prior,
    jac_Q_P, jac_Q_prior, jac_Q_resp, len_P, len_Q, len_resp, log_len_Q, n_q_marks,
    nli_contradiction, nli_entailment, regex_hit, resp_empty

The truncation caps (``_HEAD_TAIL_CHARS`` / the NLI premise+hypothesis caps) live here so
the fit and the serve truncate identically — a cap that differed between them would shift
the 19-feature distribution under a fixed classifier without the parity test seeing it.
"""

from __future__ import annotations

import math
import re
from dataclasses import dataclass, field

import numpy as np

# The legacy opening-negation regex — ONE feature (regex_hit), never an enumeration gate
# (the ML scores every guard-valid pair; the Go spine does the enumeration + guard).
# Verbatim from the reference.
_NEG_RE = re.compile(
    r"^\s*(no\b|nope\b|nah\b|wait\b|wrong\b|actually\b|i said\b|i meant\b|"
    r"that'?s not\b|not (what|like that)\b|stop\b|hold on\b|instead\b|rather\b)",
    re.I,
)

# Alphabetical — the reference ``names = sorted(f)``. A fitted classifier's coefficients bind
# to this exact order, so it is frozen; a new feature is an append + a classifier re-fit.
FEATURE_NAMES: tuple[str, ...] = (
    "cos_P_resp",
    "cos_Q_P",
    "cos_Q_prior",
    "cos_Q_resp",
    "cos_Q_resp_minus_Q_P",
    "ends_q",
    "has_prior",
    "jac_Q_P",
    "jac_Q_prior",
    "jac_Q_resp",
    "len_P",
    "len_Q",
    "len_resp",
    "log_len_Q",
    "n_q_marks",
    "nli_contradiction",
    "nli_entailment",
    "regex_hit",
    "resp_empty",
)

# Encoder head/tail half-window: MiniLM truncates at 256 word-pieces, so a long response is
# silently reduced to its opening. Encode head AND tail and keep the MAX cosine, so a
# correction landing on the response's conclusion stays visible. (reference verbatim)
_HEAD_TAIL_CHARS = 900
# NLI premise = response head, hypothesis = Q head. Asymmetric caps from the reference.
_NLI_PREMISE_CHARS = 900
_NLI_HYPOTHESIS_CHARS = 400


@dataclass
class PairInput:
    """One guard-valid P/A/Q triple the Go spine enumerated and guarded.

    ``prior`` is the human turn before P (feeds has_prior / cos_Q_prior); "" when P is the
    session's first pair, where there is no earlier human turn.
    """

    prompt_index: int
    P: str
    agent_response: str
    Q: str
    prior: str = ""


@dataclass(frozen=True)
class NliLabelIndex:
    """The NLI class indices resolved BY LABEL NAME, not by position.

    nli-deberta-v3-small orders labels [contradiction, entailment, neutral] = 0/1/2, but the
    order is model config, not a constant — the reference fit read idx 2 for "entailment",
    which is actually *neutral*, losing the real entailment signal. Always resolve from the
    model's ``id2label`` (see ``resolve_nli_label_index``); this carries the result.
    """

    contradiction: int
    entailment: int


def resolve_nli_label_index(id2label: dict[int, str] | None) -> NliLabelIndex:
    """Resolve contradiction/entailment column indices by label name from ``id2label``.

    Falls back to the known [contradiction, entailment, neutral] order when the model exposes
    no ``id2label`` — the same fallback the reference uses.
    """
    resolved = id2label or {0: "contradiction", 1: "entailment", 2: "neutral"}
    label_to_idx = {str(name).lower(): int(idx) for idx, name in resolved.items()}
    return NliLabelIndex(
        contradiction=label_to_idx.get("contradiction", 0),
        entailment=label_to_idx.get("entailment", 1),
    )


def _tokens(text: str) -> set[str]:
    return set(re.findall(r"[a-z0-9]+", (text or "").lower()))


def _jaccard(a: str, b: str) -> float:
    tokens_a, tokens_b = _tokens(a), _tokens(b)
    union = tokens_a | tokens_b
    return len(tokens_a & tokens_b) / len(union) if union else 0.0


def _halves(text: str) -> list[str]:
    text = text or ""
    if len(text) <= _HEAD_TAIL_CHARS:
        return [text[:_HEAD_TAIL_CHARS]]
    return [text[:_HEAD_TAIL_CHARS], text[-_HEAD_TAIL_CHARS:]]


def _softmax_rows(logits: np.ndarray) -> np.ndarray:
    """Row-wise softmax, max-subtracted for numerical stability.

    CrossEncoder.predict returns raw logits for this 3-label head (default Identity
    activation). Skip the softmax ONLY if the rows are already a probability distribution (a
    future library version that softmaxed internally): every value in [0,1] AND each row sums
    to ~1 — sum≈1 alone is not enough, since raw logits can coincidentally sum near 1.
    """
    already_prob = bool(((logits >= 0.0) & (logits <= 1.0)).all() and np.allclose(logits.sum(axis=1), 1.0, atol=1e-3))
    if already_prob:
        return logits
    shifted = np.exp(logits - logits.max(axis=1, keepdims=True))
    return shifted / shifted.sum(axis=1, keepdims=True)


@dataclass
class _Encoded:
    """The batched encoder/NLI outputs, regrouped per pair, feeding the per-pair fold."""

    embed_p: np.ndarray
    embed_q: np.ndarray
    embed_prior: np.ndarray
    embed_response_halves: list[np.ndarray]
    nli_probs: np.ndarray = field(default_factory=lambda: np.zeros((0, 3)))


def _encode(pairs: list[PairInput], encoder, nli, nli_idx: NliLabelIndex) -> _Encoded:
    """Batched MiniLM + NLI over every pair, regrouped per pair for the fold.

    ``encoder`` exposes ``encode(list[str], normalize_embeddings=True) -> ndarray`` and
    ``nli`` exposes ``predict(list[tuple[str, str]]) -> ndarray`` — the SentenceTransformer /
    CrossEncoder surfaces, so a test can pass deterministic doubles without a model download.
    """
    embed_p = encoder.encode([(p.P or "")[:_HEAD_TAIL_CHARS] for p in pairs], normalize_embeddings=True)
    embed_q = encoder.encode([(p.Q or "")[:_HEAD_TAIL_CHARS] for p in pairs], normalize_embeddings=True)
    embed_prior = encoder.encode([(p.prior or "")[:_HEAD_TAIL_CHARS] for p in pairs], normalize_embeddings=True)

    halves = [_halves(p.agent_response) for p in pairs]
    flat = [half for pair_halves in halves for half in pair_halves]
    flat_embeddings = encoder.encode(flat, normalize_embeddings=True)
    response_halves: list[np.ndarray] = []
    offset = 0
    for pair_halves in halves:
        response_halves.append(np.asarray(flat_embeddings[offset : offset + len(pair_halves)]))
        offset += len(pair_halves)

    nli_inputs = [((p.agent_response or "")[:_NLI_PREMISE_CHARS], (p.Q or "")[:_NLI_HYPOTHESIS_CHARS]) for p in pairs]
    nli_logits = np.asarray(nli.predict(nli_inputs))
    nli_probs = _softmax_rows(nli_logits) if len(pairs) else np.zeros((0, 3))
    return _Encoded(
        embed_p=np.asarray(embed_p),
        embed_q=np.asarray(embed_q),
        embed_prior=np.asarray(embed_prior),
        embed_response_halves=response_halves,
        nli_probs=nli_probs,
    )


def extract_features(pairs: list[PairInput], encoder, nli, nli_idx: NliLabelIndex) -> list[dict[str, float]]:
    """Compute the 19-feature dict for each pair — the single fit-and-serve extractor.

    Returns one dict per pair, keys == ``FEATURE_NAMES``. ``encoder``/``nli`` are the
    resident MiniLM + NLI models (or deterministic test doubles); ``nli_idx`` is the
    label-name-resolved index (``resolve_nli_label_index``), never a positional guess.
    """
    if not pairs:
        return []
    enc = _encode(pairs, encoder, nli, nli_idx)
    out: list[dict[str, float]] = []
    for i, pair in enumerate(pairs):
        q = pair.Q or ""
        cos_q_resp = float(max(enc.embed_q[i] @ half for half in enc.embed_response_halves[i]))
        cos_q_p = float(enc.embed_q[i] @ enc.embed_p[i])
        cos_p_resp = float(max(enc.embed_p[i] @ half for half in enc.embed_response_halves[i]))
        has_prior = 1.0 if (pair.prior or "").strip() else 0.0
        out.append(
            {
                "cos_P_resp": cos_p_resp,
                "cos_Q_P": cos_q_p,
                "cos_Q_prior": float(enc.embed_q[i] @ enc.embed_prior[i]) if has_prior else 0.0,
                "cos_Q_resp": cos_q_resp,
                "cos_Q_resp_minus_Q_P": cos_q_resp - cos_q_p,
                "ends_q": 1.0 if q.strip().endswith("?") else 0.0,
                "has_prior": has_prior,
                "jac_Q_P": _jaccard(q, pair.P),
                "jac_Q_prior": _jaccard(q, pair.prior),
                "jac_Q_resp": _jaccard(q, pair.agent_response),
                "len_P": float(len(pair.P or "")),
                "len_Q": float(len(q)),
                "len_resp": float(len(pair.agent_response or "")),
                "log_len_Q": float(math.log1p(len(q))),
                "n_q_marks": float(q.count("?")),
                "nli_contradiction": float(enc.nli_probs[i][nli_idx.contradiction]),
                "nli_entailment": float(enc.nli_probs[i][nli_idx.entailment]),
                "regex_hit": 1.0 if _NEG_RE.match(q) else 0.0,
                "resp_empty": 1.0 if not (pair.agent_response or "").strip() else 0.0,
            }
        )
    return out


def features_to_matrix(feature_dicts: list[dict[str, float]]) -> np.ndarray:
    """Stack feature dicts into an (n, 19) matrix in ``FEATURE_NAMES`` column order."""
    if not feature_dicts:
        return np.zeros((0, len(FEATURE_NAMES)), dtype="float64")
    return np.array([[f[name] for name in FEATURE_NAMES] for f in feature_dicts], dtype="float64")
