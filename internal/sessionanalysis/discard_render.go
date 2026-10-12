// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"fmt"
	"strings"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// buildDiscardContent assembles the compact, D-shaped request the Discard producer reads — NOT
// R's full raw transcript. It matches the ANALYZE_DISCARD_FENCE's vocabulary: the underlying GOAL,
// the PHASES (each with its phase_theme and intent), the turn body (the same turn-grouped body R
// reads, decorated per turn with R's judged downstream_impact tag the fence explicitly consults), a
// measured BRANCH LEDGER, and the CAPABILITIES block. It carries no certification/fingerprint and no
// re-pick edge — the fence reads none. The whole assembly then gets the SAME anonymizePaths send-
// transform sp.llmContent gets, so file paths never reach the model.
// read is R's settled result (guaranteed non-nil at D's call site); a turn R left unjudged simply
// renders its body with no downstream_impact line.
func buildDiscardContent(sp *spineResult, read *ReadResult) string {
	var b strings.Builder

	b.WriteString("=== GOAL ===\n")
	if goal := read.Verdict.UnderlyingGoal; goal != nil && *goal != "" {
		b.WriteString(*goal + "\n")
	}

	b.WriteString("=== PHASES ===\n")
	for _, phase := range read.Phases {
		renderDiscardPhase(&b, phase)
	}

	// The turn body — the SAME turn-grouped blocks the spine cut once (sp.turnBlocks), so D judges
	// over the identical turn set R read and neither re-parses the conversation — decorated with each
	// turn's judged downstream_impact. read.Turns is keyed by PromptIndex, already rebased to the raw
	// whole-session ordinal that equals a block's Index.
	impactByIndex := downstreamImpactByIndex(read.Turns)
	for _, block := range sp.turnBlocks {
		renderTurnBlock(&b, block)
		if impact, ok := impactByIndex[block.Index]; ok {
			fmt.Fprintf(&b, "downstream_impact: %s\n", shortEnum(impact.String(), "DOWNSTREAM_IMPACT_"))
		}
	}

	b.WriteString(renderBranchLedger(sp.ledgers.discardLedger))

	renderCapabilities(&b, capabilityFor(sp.prompts))

	return anonymizePaths(b.String())
}

// renderDiscardPhase writes one phase row for D — its ordinal, its phase_theme, and its intent
// (phase_objective) — the fields the fence's INTENT guardrail reads to partition discardable from
// non-discardable work. A phase with no judged theme or no objective renders the fields it has
// rather than a false value.
func renderDiscardPhase(b *strings.Builder, phase *PhaseJudgment) {
	fmt.Fprintf(b, "phase %d:", phase.PhaseOrdinal)
	if phase.PhaseTheme != nil {
		b.WriteString(" phase_theme=" + shortEnum(phase.PhaseTheme.String(), "PHASE_THEME_"))
	}
	if phase.PhaseObjective != nil && *phase.PhaseObjective != "" {
		b.WriteString(" intent=" + *phase.PhaseObjective)
	}
	b.WriteString("\n")
}

// downstreamImpactByIndex indexes R's judged downstream_impact tags by prompt_index so the turn
// body can look each up by its block's raw ordinal. A turn R left unjudged (nil impact) is absent
// from the map, so its body renders with no tag rather than a spurious one.
func downstreamImpactByIndex(turns []*TurnJudgment) map[int32]sessionintelligencepb.DownstreamImpact {
	byIndex := make(map[int32]sessionintelligencepb.DownstreamImpact, len(turns))
	for _, turn := range turns {
		if turn.DownstreamImpact != nil {
			byIndex[turn.PromptIndex] = *turn.DownstreamImpact
		}
	}
	return byIndex
}
