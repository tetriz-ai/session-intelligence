// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

// Regression guard for the ownership fix (by design, E and R own separate fields):
// applyEvidence must route E's output into R's ReadResult's own E-owned fields
// (SkillAssessments, RoundTripFaults, EvidenceSecurityIssues) and
// must NOT mutate R's Verdict.InvokedSkills, the turn rows, or a turn's RoundTripFault —
// R stays byte-immutable under E.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// TestApplyEvidence_DoesNotMutateRImmutableFields is the byte-immutability guardrail:
// R's invoked_skills list and turn rows must be the identical objects, unchanged, after
// applyEvidence folds in an E result that adjudicates the same invocation_ref and the
// same P turn's round trip.
func TestApplyEvidence_DoesNotMutateRImmutableFields(t *testing.T) {
	rInvokedSkills := &sessionintelligencepb.InvokedSkillJudgmentList{
		Items: []*sessionintelligencepb.InvokedSkillJudgment{
			{InvocationRef: "skill-1", Role: sessionintelligencepb.SkillRole_SKILL_ROLE_PRODUCER},
		},
	}
	// A snapshot of R's invoked_skills taken before applyEvidence runs, so the assertion
	// below proves R's list was never touched rather than merely ending up equal.
	rInvokedSkillsSnapshot := proto.Clone(rInvokedSkills).(*sessionintelligencepb.InvokedSkillJudgmentList)

	rTurn := &TurnJudgment{PromptIndex: 0}
	read := &ReadResult{
		Verdict: SessionVerdict{InvokedSkills: rInvokedSkills},
		Turns:   []*TurnJudgment{rTurn},
	}

	fault := sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_AGENT
	evidencePromptIndex := int32(1)
	role2 := sessionintelligencepb.SkillRole_SKILL_ROLE_UNSPECIFIED
	evidence := &EvidenceResult{
		Adjudications: &sessionintelligencepb.ArtifactAdjudicationList{},
		SkillAssessments: []*SkillAssessment{
			{InvocationRef: "skill-1", Role: &role2},
		},
		RoundTrips: []*RoundTripJudgment{
			{PromptIndex: 0, Fault: &fault, EvidencePromptIndex: &evidencePromptIndex},
		},
	}

	applyEvidence(read, evidence)

	// R's invoked_skills is the SAME object, byte-unchanged — applyEvidence must not
	// merge E's assessment onto it (that is the pre- behavior this fix removed).
	assert.Same(t, rInvokedSkills, read.Verdict.InvokedSkills, "R's invoked_skills object identity must be untouched")
	assert.True(t, proto.Equal(rInvokedSkillsSnapshot, read.Verdict.InvokedSkills), "R's invoked_skills content must be byte-unchanged")

	// R's turn row is the SAME object and its RoundTripFault stays nil — the fault must
	// land in RoundTripFaults (persisted to the E-owned table), never stamped on the turn.
	require.Len(t, read.Turns, 1)
	assert.Same(t, rTurn, read.Turns[0])
	assert.Nil(t, read.Turns[0].RoundTripFault, "R's turn row must not be stamped with E's fault")
	assert.Nil(t, read.Turns[0].RoundTripEvidencePromptIndex)

	// E's own fields are filled.
	require.NotNil(t, read.SkillAssessments)
	require.Len(t, read.SkillAssessments.GetItems(), 1)
	assert.Equal(t, "skill-1", read.SkillAssessments.GetItems()[0].GetInvocationRef())
	require.Len(t, read.RoundTripFaults, 1)
	assert.Equal(t, int32(0), read.RoundTripFaults[0].PromptIndex)
	require.NotNil(t, read.RoundTripFaults[0].Fault)
	assert.Equal(t, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_AGENT, *read.RoundTripFaults[0].Fault)

	// The verdict's ArtifactAdjudications field IS E-owned (it lives on Verdict by
	// design, per producers.go's doc comment) — applyEvidence sets it, which is not a
	// violation of R's immutability.
	assert.Same(t, evidence.Adjudications, read.Verdict.ArtifactAdjudications)
}

// TestApplyEvidence_RoundTripSecurityIssueRoutesToEvidenceSecurityIssues asserts a
// security-layer slug E caught on a round trip lands in EvidenceSecurityIssues, not R's
// SecurityIssues.
func TestApplyEvidence_RoundTripSecurityIssueRoutesToEvidenceSecurityIssues(t *testing.T) {
	read := &ReadResult{}
	securityIssue := &sessionintelligencepb.PromptIssue{Note: "sk-*** detected in prompt"}
	evidence := &EvidenceResult{
		RoundTrips: []*RoundTripJudgment{
			{PromptIndex: 0, SecurityIssue: securityIssue},
			{PromptIndex: 1}, // no security issue on this pair
		},
	}

	applyEvidence(read, evidence)

	require.Len(t, read.EvidenceSecurityIssues, 1, "only the round trip carrying a security issue contributes")
	assert.Same(t, securityIssue, read.EvidenceSecurityIssues[0])
	assert.Empty(t, read.SecurityIssues, "R's SecurityIssues is untouched by E's round-trip findings")
}

// TestApplyEvidence_NilGuards asserts the no-op guard on a nil read or a nil evidence
// (E did not run), so a caller does not need to branch before calling.
func TestApplyEvidence_NilGuards(t *testing.T) {
	t.Run("nil read is a no-op", func(t *testing.T) {
		assert.NotPanics(t, func() { applyEvidence(nil, &EvidenceResult{}) })
	})

	t.Run("nil evidence leaves the ReadResult untouched", func(t *testing.T) {
		read := &ReadResult{Verdict: SessionVerdict{}}
		applyEvidence(read, nil)
		assert.Nil(t, read.SkillAssessments)
		assert.Nil(t, read.RoundTripFaults)
	})
}

// TestApplyEvidence_EmptySkillAssessments asserts buildSkillAssessmentList's contract
// (surfaced through applyEvidence): an E run with no assessments leaves the E-owned
// column nil rather than an empty list, so a NULL/empty distinction survives to the DB.
func TestApplyEvidence_EmptySkillAssessments(t *testing.T) {
	read := &ReadResult{}
	applyEvidence(read, &EvidenceResult{})
	assert.Nil(t, read.SkillAssessments)
}
