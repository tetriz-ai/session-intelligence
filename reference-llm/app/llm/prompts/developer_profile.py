# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Developer profile formatter for the analyze_prompt task.

Reads the developer_profile key injected by Go's InferDeveloperProfile and
produces a system prompt addendum with persona-specific and tool-mode-specific
scoring adjustments. Returns None when no meaningful profile is present so the
caller falls through to the base rubric unchanged.
"""

from __future__ import annotations

from typing import Any

_MIN_CONFIDENCE = 0.35

# Per-persona scoring guidance keyed by persona string from Go.
_PERSONA_GUIDANCE: dict[str, dict[str, Any]] = {
    "ML_ENGINEER": {
        "label": "ML Engineer",
        "description": (
            "This developer primarily works on model training, data pipelines, and experiments "
            "(Python + ML framework signals detected)."
        ),
        "amplify": [
            "missing-constraints: for this persona, hyperparameter / device / seed / framework-version "
            "constraints are typically load-bearing AND unrecoverable (the AI cannot infer them) — apply "
            "the base per-prompt recoverability test and expect it to land high when they are absent.",
            "missing-state-delta: STRICT — ML bugs without loss curves, error traces, or metric "
            "comparisons are genuinely hard to diagnose. Flag any bug report missing this.",
        ],
        "suppress": [
            "unspecified-output: be lenient for exploration sessions — ML engineers often iterate "
            "interactively and the output format is implicit (notebook cell, variable inspection).",
            "missing-external-environment: flag ONLY for runtime hardware context that cannot be "
            "inferred from project files — specifically compute device (CPU/GPU/TPU), CUDA version, "
            "or distributed training topology when the task genuinely requires it. Framework version "
            "and library versions are readable from project files and should NOT be flagged.",
        ],
    },
    "DEVOPS_INFRA": {
        "label": "DevOps / Infra Engineer",
        "description": (
            "This developer primarily works on infrastructure, CI/CD, and deployment "
            "(YAML/HCL/Dockerfile signals detected)."
        ),
        "amplify": [
            "missing-external-environment: target environment, cloud provider, K8s version, and region "
            "are typically unrecoverable for infra tasks (the AI cannot infer them) — expect the base "
            "recoverability test to land these high when absent.",
            "missing-constraints: idempotency, rollback strategy, and state-backend are load-bearing, "
            "often-irreversible infra constraints — expect high under the base test when missing.",
            "unspecified-output: output format determines pipeline behavior (raw YAML vs annotated, "
            "terraform apply vs plan) — weight it heavily; expect high when the format is unspecified.",
        ],
        "suppress": [],
    },
    "SOFTWARE_ENGINEER": {
        "label": "Software Engineer",
        "description": (
            "This developer primarily works on application code "
            "(Go/TypeScript/Rust signals detected). Base rubric applies."
        ),
        "amplify": [],
        "suppress": [],
    },
}

# Per-tool-mode scoring guidance keyed by tool_mode string from Go.
_TOOL_MODE_GUIDANCE: dict[str, dict[str, Any]] = {
    "agentic": {
        "label": "Agentic IDE (Claude Code / Kiro / Antigravity)",
        "guidance": [
            "missing-constraints on SCOPE is high severity ONLY when the scope gap is load-bearing "
            "AND unrecoverable (an irreversible or side-effecting change) — not on breadth alone: a "
            "broad but revertible target (module, service, codebase) is medium at most, since the "
            "agent reads the repo and an over-touch is revertible. (See the base SEVERITY RUBRIC.)",
            "missing-external-environment is SUPPRESSED for tech-stack context — the agent reads "
            "project files (pyproject.toml, go.mod, package.json, requirements.txt) and detects "
            "language version, framework, and dependencies itself. Do NOT flag for information the "
            "agent can retrieve from project files. EXCEPTION: runtime hardware context that cannot "
            "be read from any project file — e.g. GPU vs CPU device, CUDA version, distributed "
            "training topology — should still be flagged when the task genuinely requires it.",
            "ambiguous-directive severity is REDUCED — agentic IDEs ask for clarification on "
            "genuinely ambiguous prompts rather than guessing silently.",
            "token-waste is HIGH PRIORITY — context window is a hard resource in agentic sessions. "
            "Redundant re-explanation of prior context directly degrades performance.",
        ],
    },
    "inline": {
        "label": "Inline IDE (Cursor / Copilot)",
        "guidance": [
            "Line-level precision matters most. Flag missing code location context (which function, "
            "which file, which line range) as higher severity than for agentic sessions.",
            "unspecified-output severity is HIGHER — the model fills code inline and ambiguity about "
            "what to keep vs replace leads directly to wrong completions.",
            "missing-external-environment: only flag when the language or framework version would "
            "materially change the completion (e.g., Python 2 vs 3 syntax differences).",
        ],
    },
    "chat": {
        "label": "Chat interface (GPT-4 web / Claude.ai / Gemini web)",
        "guidance": [
            "missing-external-environment is HIGH — no project file access; the model cannot read "
            "any file. Flag whenever stack or framework info is absent.",
            "missing-internal-context is HIGH — explicit code or schema pastes are required; the "
            "model has no ambient codebase access.",
            "unspecified-output is HIGH — the model returns text and format matters for how the "
            "user consumes and applies it.",
        ],
    },
    "long_context": {
        "label": "Long-context model (Gemini 1.5+ / large-window models)",
        "guidance": [
            "Full-file context pastes are acceptable cost — do not flag large context dumps as "
            "excessive-noise unless they are clearly irrelevant to the task.",
            "excessive-noise threshold is HIGHER — the model can handle larger inputs without meaningful degradation.",
            "Note: 'long-context' means the model tolerates more input than chat/inline modes; "
            "reliable attention is approximately 200k tokens, not the full advertised window.",
        ],
    },
}


def format_developer_profile_addendum(ctx: dict[str, Any]) -> str | None:
    """Build a developer profile addendum from session context.

    Returns None when no developer_profile key is present, confidence is
    below threshold, or both persona and tool_mode are unknown. The caller
    should skip appending when None is returned — the base rubric applies.
    """
    profile = ctx.get("developer_profile")
    if not profile or not isinstance(profile, dict):
        return None

    persona: str = profile.get("persona", "UNKNOWN")
    persona_conf: float = float(profile.get("persona_confidence") or 0.0)
    tool_mode: str = profile.get("tool_mode") or "unknown"

    if persona == "UNKNOWN" and tool_mode == "unknown":
        return None

    lines: list[str] = ["# DEVELOPER PROFILE", ""]

    # Persona section.
    persona_info = _PERSONA_GUIDANCE.get(persona)
    if persona_info and persona_conf >= _MIN_CONFIDENCE:
        lines += [
            f"Persona: {persona_info['label']} (confidence: {int(persona_conf * 100)}%)",
            persona_info["description"],
            "",
        ]
        if persona_info["amplify"]:
            lines.append("Persona-specific amplifications — apply on top of base rubric:")
            for item in persona_info["amplify"]:
                lines.append(f"- {item}")
            lines.append("")
        if persona_info["suppress"]:
            lines.append("Persona-specific suppressions:")
            for item in persona_info["suppress"]:
                lines.append(f"- {item}")
            lines.append("")
    elif persona_info is not None:
        # Known persona but below the confidence threshold: name it from the allowlisted label,
        # never the raw client-supplied persona string. An unrecognized persona (persona_info is
        # None) renders nothing — so an injected value with newlines/markdown cannot reach the
        # instruction channel the way the session_context fence prevents elsewhere.
        lines += [
            f"Persona: {persona_info['label']} "
            f"(low confidence: {int(persona_conf * 100)}% — base rubric applies for persona axis)",
            "",
        ]

    # Tool mode section.
    tool_info = _TOOL_MODE_GUIDANCE.get(tool_mode)
    if tool_info:
        lines += [
            f"AI Tool Mode: {tool_info['label']}",
            "",
            "Tool-mode scoring adjustments:",
        ]
        for item in tool_info["guidance"]:
            lines.append(f"- {item}")
        lines.append("")

    # Seniority — always UNKNOWN in Phase 1.
    lines += [
        "Seniority: UNKNOWN — Phase 1 does not infer seniority. Apply mid-level recommendation tone for all users.",
        "",
    ]

    return "\n".join(lines)
