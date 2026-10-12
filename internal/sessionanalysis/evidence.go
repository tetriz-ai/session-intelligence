// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// confirmedRoundTrip reports whether R's turn tags confirm a round-trip pair — the
// tag-informed confirmation applied after R (one
// detection algorithm, confirmed against R's turn tags, not two independent detectors). A
// pair is confirmed by a turn R tagged as a re-run of an earlier ask (was_retry
// retry_same/retry_refined) or a human correcting the agent (user_steering correcting) —
// these are the pairs E attributes fault to. It is the round-trip half of E's fire gate;
// the artifact half is the spine's code-built ledger (spineLedgers.firesEvidence).
func confirmedRoundTrip(read *ReadResult) bool {
	if read == nil {
		return false
	}
	for _, turn := range read.Turns {
		if turn.WasRetry != nil {
			switch *turn.WasRetry {
			case sessionintelligencepb.WasRetry_WAS_RETRY_RETRY_SAME, sessionintelligencepb.WasRetry_WAS_RETRY_RETRY_REFINED:
				return true
			default:
				// fresh / unspecified: not a re-run, not a confirmed pair.
			}
		}
		if turn.UserSteering != nil && *turn.UserSteering == sessionintelligencepb.TurnUserSteering_TURN_USER_STEERING_CORRECTING {
			return true
		}
	}
	return false
}

// applyEvidence routes E's output into R's ReadResult's E-OWNED fields (E runs after R):
// the artifact adjudications and the skill
// assessments land in their own E-owned session_analyses columns, the per-pair faults in
// RoundTripFaults (persisted to the E-owned session_round_trip_faults table), and any
// security-layer slug E alone caught on a round trip in EvidenceSecurityIssues. R's verdict,
// invoked_skills, and turn rows are NOT mutated, so R stays byte-immutable under E.
func applyEvidence(read *ReadResult, evidence *EvidenceResult) {
	if read == nil || evidence == nil {
		return
	}
	read.Verdict.ArtifactAdjudications = evidence.Adjudications
	read.SkillAssessments = buildSkillAssessmentList(evidence.SkillAssessments)
	read.RoundTripFaults = evidence.RoundTrips
	read.EvidenceRan = true
	for _, roundTrip := range evidence.RoundTrips {
		if roundTrip.SecurityIssue != nil {
			read.EvidenceSecurityIssues = append(read.EvidenceSecurityIssues, roundTrip.SecurityIssue)
		}
	}
}

// buildSkillAssessmentList builds E's E-owned skill-assessment list — one InvokedSkillJudgment
// per assessment carrying its invocation_ref and E's six adjudicated dimensions. It reuses the
// InvokedSkillJudgment message shape but is stored in the separate skill_assessments column, so
// R's invoked_skills is never touched. Returns nil for an empty set so the column stays NULL.
func buildSkillAssessmentList(assessments []*SkillAssessment) *sessionintelligencepb.InvokedSkillJudgmentList {
	if len(assessments) == 0 {
		return nil
	}
	items := make([]*sessionintelligencepb.InvokedSkillJudgment, 0, len(assessments))
	for _, assessment := range assessments {
		skill := &sessionintelligencepb.InvokedSkillJudgment{InvocationRef: assessment.InvocationRef}
		stampAssessment(skill, assessment)
		items = append(items, skill)
	}
	return &sessionintelligencepb.InvokedSkillJudgmentList{Items: items}
}

// stampAssessment writes E's assessment dimensions onto one invoked-skill judgment,
// leaving each unresolved (nil) dimension at its proto zero (UNSPECIFIED).
func stampAssessment(skill *sessionintelligencepb.InvokedSkillJudgment, assessment *SkillAssessment) {
	if assessment.Role != nil {
		skill.Role = *assessment.Role
	}
	if assessment.ContractCompliance != nil {
		skill.ContractCompliance = *assessment.ContractCompliance
	}
	if assessment.OutputQuality != nil {
		skill.OutputQuality = *assessment.OutputQuality
	}
	if assessment.ExecutionReliability != nil {
		skill.ExecutionReliability = *assessment.ExecutionReliability
	}
	if assessment.HumanAlignment != nil {
		skill.HumanAlignment = *assessment.HumanAlignment
	}
	if assessment.SkillVerdict != nil {
		skill.SkillVerdict = *assessment.SkillVerdict
	}
}
