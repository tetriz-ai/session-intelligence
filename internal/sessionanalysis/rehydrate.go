// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"github.com/tetriz-ai/session-intelligence/dao/model"
)

// rehydrateReadResult reconstructs R's ReadResult from R's committed rows — the
// reverse of the commit builders (buildSessionAnalysis, buildTurnRow, buildPhaseRows)
// — so a per-producer re-pick can carry R by rehydration instead of re-inferring it
// . It reads ONLY R-owned columns: forward-commit
// and reverse-rehydrate must stay exact inverses or a carried R is silently wrong,
// defeating the R-immutability guarantee, so the coupling is guarded by the
// rehydrate(read(commit(R))) == R property test and the column-set-parity test in
// rehydrate_internal_test.go.
//
// The rehydrated result is a PURE-R result: E's and D's outputs are deliberately not
// reconstructed here (EvidenceRan stays false; SkillAssessments / RoundTripFaults /
// EvidenceSecurityIssues / Verdict.ArtifactAdjudications stay nil), because a re-pick
// carries R and RE-RUNS E/D — a rehydrated E would be double-written. Usage and
// ScorerModel are call telemetry, never persisted as R content, so they are not carried.
//
// The three input row sets come from the existing readers:
// SessionAnalysisRepo.GetByRawRecordID, SessionTurnAnalysisRepo.ListByRawRecordID, and
// SessionPhaseAnalysisRepo.ListBySessionAnalysisID. Turns must belong to the same live
// generation as analysis (the readers already scope to the live, non-deleted rows).
func rehydrateReadResult(analysis *model.SessionAnalysis, turns []*model.SessionTurnAnalysis, phases []*model.SessionPhaseAnalysis) *ReadResult {
	if analysis == nil {
		return nil
	}
	read := &ReadResult{
		Verdict: rehydrateVerdict(analysis),
		Phases:  rehydratePhases(phases),
	}
	// A turn stores its phase link as phase_id (a UUID resolved at commit from the fresh
	// phase set), not phase_ordinal, so invert through the phase set to recover the
	// ordinal buildTurnRow started from.
	phaseIDToOrdinal := make(map[string]int32, len(phases))
	for _, phase := range phases {
		if phase.ID != "" && phase.PhaseOrdinal != nil {
			phaseIDToOrdinal[phase.ID] = *phase.PhaseOrdinal
		}
	}
	read.Turns = rehydrateTurns(turns, phaseIDToOrdinal)
	// R's security readout: recover it from the per-turn prompt_issues the turn rows carry
	// (exactly as parseReadResponse flattened it), so a carried re-commit's security reconcile
	// is a no-op on the unchanged R grain rather than wiping the flag set. When no turn rows
	// exist — a source whose turn grain is not stored (supportsTurnGrouping == false), where the
	// flatten would yield an empty set and the last-writer-wins reconcile would then delete R's
	// flags — fall back to the row's persisted read_security_issues column, which the commit
	// wrote from this same list.
	if len(read.Turns) > 0 {
		for _, turn := range read.Turns {
			read.SecurityIssues = append(read.SecurityIssues, securityIssuesOf(turn.PromptIssues)...)
		}
	} else {
		read.SecurityIssues = analysis.ReadSecurityIssues.GetItems()
		// Same no-turn-rows case for the session-grain rollups: with no turns to recompute
		// from, carry the stored taxonomy_issues / waste_categories so a re-commit re-writes
		// them unchanged rather than NULLing them via an empty turn-flatten.
		read.CarriedTaxonomyIssues = analysis.TaxonomyIssues
		read.CarriedWasteCategories = analysis.WasteCategories
	}
	return read
}

// rehydrateDiscardResult reconstructs D's DiscardResult from the committed waste ledger
// (discarded_ranges) so an E-only resolved re-pick — where R is carried and the body is
// unchanged — carries a settled D forward instead of re-billing AnalyzeDiscard and risking a
// nondeterministic result overwriting the settled ledger.
// The stored ranges are already rebased to raw whole-session indices and token-summed, so the
// commit's applyWasteLedger recomputes an identical ledger from the unchanged body and never
// re-rebases them. It is called only when D's prior call was DONE, so the result is deliberately
// non-nil even when the ledger is empty (D ran and found no waste — PRODUCED, not NOT_APPLICABLE);
// carrying nil there would regress D's disposition. Only DiscardedRanges is carried:
// TokenEfficiencyDowngrade never rewrites R's immutable verdict and Usage is call telemetry.
func rehydrateDiscardResult(analysis *model.SessionAnalysis) *DiscardResult {
	if analysis == nil {
		return nil
	}
	return &DiscardResult{DiscardedRanges: analysis.DiscardedRanges}
}

// rehydrateVerdict inverts buildSessionAnalysis's R-owned session-grain columns.
// E-owned columns (artifact_adjudications, skill_assessments), the readiness
// dispositions, R's waste ledger (D-owned discarded_ranges/wasted_tokens), and every
// measured column are deliberately NOT read — see the R-owned allowlist in
// rehydrate_internal_test.go.
func rehydrateVerdict(analysis *model.SessionAnalysis) SessionVerdict {
	return SessionVerdict{
		SessionSteering:       analysis.SessionSteering,
		GoalAchieved:          analysis.GoalAchieved,
		Smoothness:            analysis.Smoothness,
		FrictionCause:         analysis.FrictionCause,
		Speed:                 analysis.Speed,
		TokenEfficiency:       analysis.TokenEfficiency,
		GoalIsCodingWork:      analysis.GoalIsCodingWork,
		UnderlyingGoal:        analysis.UnderlyingGoal,
		WorkType:              analysis.WorkType,
		ScoreOverall:          analysis.ScoreOverall,
		ScoreClarity:          analysis.ScoreClarity,
		ScoreSpecificity:      analysis.ScoreSpecificity,
		ScoreContextProvision: analysis.ScoreContextProvision,
		ScoreActionability:    analysis.ScoreActionability,
		ScoreCompleteness:     analysis.ScoreCompleteness,
		ScoreEfficiency:       analysis.ScoreEfficiency,
		InvokedSkills:         analysis.InvokedSkills,
		HarnessFindings:       analysis.HarnessFindings,
	}
}

// rehydrateTurns inverts buildTurnRow's R-owned turn columns, one TurnJudgment per live
// turn row. round_trip_fault / round_trip_evidence_prompt_index are E-owned (P1 moved
// the fault set into session_round_trip_faults) and are NOT read; judged_content_sha256
// is the content digest, not an R judgment, and is likewise excluded.
func rehydrateTurns(turns []*model.SessionTurnAnalysis, phaseIDToOrdinal map[string]int32) []*TurnJudgment {
	if len(turns) == 0 {
		return nil
	}
	judgments := make([]*TurnJudgment, 0, len(turns))
	for _, turn := range turns {
		judgment := &TurnJudgment{
			PromptIndex:          turn.PromptIndex,
			PromptType:           turn.PromptType,
			UserSteering:         turn.UserSteering,
			PromptCompleteness:   turn.PromptCompleteness,
			PromptObjectiveMet:   turn.PromptObjectiveMet,
			AgentHelpfulness:     turn.AgentHelpfulness,
			AgentInitiative:      turn.AgentInitiative,
			ToolUse:              turn.ToolUse,
			WasRetry:             turn.WasRetry,
			FailureClass:         turn.FailureClass,
			TokenLocus:           turn.TokenLocus,
			WasteCause:           turn.WasteCause,
			DecisionType:         turn.DecisionType,
			DownstreamImpact:     turn.DownstreamImpact,
			FeedbackTarget:       turn.FeedbackTarget,
			FeedbackDisposition:  turn.FeedbackDisposition,
			ReferencedArtifact:   turn.ReferencedArtifact,
			ReferencedFindingIDs: turn.ReferencedFindingIDs,
			EvidenceSeq:          turn.EvidenceSeq,
			PromptIssues:         turn.PromptIssues,
			ChunkIndex:           turn.ChunkIndex,
		}
		if turn.PhaseID != nil {
			if ordinal, ok := phaseIDToOrdinal[*turn.PhaseID]; ok {
				judgment.PhaseOrdinal = int32Ptr(ordinal)
			}
		}
		judgments = append(judgments, judgment)
	}
	return judgments
}

// rehydratePhases inverts buildPhaseRows's R-owned phase columns. hitl_gate, tokens and
// tool_call_count ARE written by buildPhaseRows, but recomputed fresh from measured turn state
// (per-turn gates / token costs / tool-call counts) each commit — not from R's output — so they are
// not rehydrated; segmentation_suspect is likewise code-owned/measured and not an R judgment. Reading
// any of them back would let a stale stored value shadow the freshly recomputed one.
func rehydratePhases(phases []*model.SessionPhaseAnalysis) []*PhaseJudgment {
	if len(phases) == 0 {
		return nil
	}
	judgments := make([]*PhaseJudgment, 0, len(phases))
	for _, phase := range phases {
		judgment := &PhaseJudgment{
			RangeStart:         phase.RangeStart,
			RangeEnd:           phase.RangeEnd,
			BoundaryOverridden: phase.BoundaryOverridden,
			OverrideReason:     phase.OverrideReason,
			PhaseTheme:         phase.PhaseTheme,
			SdlcStageSet:       phase.SdlcStageSet,
			TaskComplexity:     phase.TaskComplexity,
			UserSteering:       phase.UserSteering,
			PhaseObjective:     phase.PhaseObjective,
			PhaseObjectiveMet:  phase.PhaseObjectiveMet,
			AgentHelpfulness:   phase.AgentHelpfulness,
			Smoothness:         phase.Smoothness,
			FrictionCause:      phase.FrictionCause,
			Speed:              phase.Speed,
			WentWell:           phase.WentWell,
			WentWrong:          phase.WentWrong,
			Coaching:           phase.Coaching,
			TurnCount:          phase.TurnCount,
		}
		if phase.PhaseOrdinal != nil {
			judgment.PhaseOrdinal = *phase.PhaseOrdinal
		}
		judgments = append(judgments, judgment)
	}
	return judgments
}
