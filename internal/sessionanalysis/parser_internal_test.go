// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// validReadBody is copied verbatim from reference-llm's AnalyzeReadResponse VALID_READ
// fixture — the deployed wire shape, with
// lowercase StrEnum values, `turns`/`phases`/`session_analysis` keys, and prompt_issues
// carried inside turns (no top-level security block). A parser that does not match this
// shape decodes to an empty struct, which is exactly the bug this pins.
const validReadBody = `{
  "turns": [
    {
      "prompt_index": 0, "prompt_type": "creation", "user_steering": "active",
      "prompt_completeness": "complete",
      "prompt_issues": [{"slug": "api-key-leak", "severity": "critical", "note": "AKIA..."}],
      "prompt_objective_met": "met", "agent_helpfulness": "helpful",
      "agent_initiative": "assumed_and_proceeded", "tool_use": "appropriate",
      "was_retry": "fresh", "failure_class": "none", "token_locus": "agent_reasoning",
      "waste_cause": "none", "decision_type": "approach", "downstream_impact": "built_on",
      "feedback_target": "artifact", "feedback_disposition": "accept",
      "referenced_artifact": "spec.md", "referenced_finding_ids": ["f1"], "evidence_seq": 3
    },
    {
      "prompt_index": 1, "prompt_type": "trivial", "user_steering": null,
      "prompt_completeness": null, "prompt_issues": [],
      "prompt_objective_met": "not_applicable", "agent_helpfulness": "unclear",
      "agent_initiative": "followed_spec", "tool_use": "unclear", "was_retry": null,
      "failure_class": null, "token_locus": "unknown", "waste_cause": null,
      "decision_type": null, "downstream_impact": null, "feedback_target": null,
      "feedback_disposition": null, "referenced_artifact": null,
      "referenced_finding_ids": [], "evidence_seq": null
    }
  ],
  "phases": [
    {
      "prompt_range": {"start_index": 0, "end_index": 1}, "boundary_overridden": true,
      "override_reason": "folded a one-prompt theme change", "phase_theme": "creation",
      "sdlc_stage": ["Code", "Test & QA"], "task_complexity": "moderate",
      "user_steering": "active", "phase_objective": "ship the parser",
      "phase_objective_met": "met", "agent_helpfulness": "helpful", "smoothness": "smooth",
      "friction_cause": "none", "speed": "fast", "went_well": ["clear spec"],
      "went_wrong": [], "could_be_better": []
    }
  ],
  "session_analysis": {
    "underlying_goal": "build a parser", "session_steering": "mostly_active",
    "goal_achieved": "fully", "smoothness": "smooth", "friction_cause": "none",
    "speed": "fast", "token_efficiency": "efficient", "goal_is_coding_work": true,
    "scores": {"overall": 8, "clarity": 9, "specificity": 7, "context_provision": 8,
               "actionability": 8, "completeness": 7, "efficiency": 9},
    "invoked_skills": [], "harness_findings": []
  }
}`

// TestParseReadResponse_RealFixture asserts the parser decodes
// W's actual AnalyzeReadResponse into a fully-populated ReadResult — verdict, scores,
// turns (with per-turn enums + phase linkage), phases, and the security readout — so
// the engine never commits an empty PRODUCED row.
func TestParseReadResponse_RealFixture(t *testing.T) {
	got, err := parseReadResponse(validReadBody)
	require.NoError(t, err)
	require.NotNil(t, got)

	// Verdict + scores.
	require.NotNil(t, got.Verdict.GoalAchieved)
	assert.Equal(t, sessionintelligencepb.GoalAchieved_GOAL_ACHIEVED_FULLY, *got.Verdict.GoalAchieved)
	require.NotNil(t, got.Verdict.SessionSteering)
	assert.Equal(t, sessionintelligencepb.SessionSteering_SESSION_STEERING_MOSTLY_ACTIVE, *got.Verdict.SessionSteering)
	require.NotNil(t, got.Verdict.TokenEfficiency)
	assert.Equal(t, sessionintelligencepb.TokenEfficiency_TOKEN_EFFICIENCY_EFFICIENT, *got.Verdict.TokenEfficiency)
	require.NotNil(t, got.Verdict.GoalIsCodingWork)
	assert.True(t, *got.Verdict.GoalIsCodingWork)
	require.NotNil(t, got.Verdict.UnderlyingGoal)
	assert.Equal(t, "build a parser", *got.Verdict.UnderlyingGoal)
	require.NotNil(t, got.Verdict.ScoreOverall)
	assert.Equal(t, int32(8), *got.Verdict.ScoreOverall)
	require.NotNil(t, got.Verdict.ScoreEfficiency)
	assert.Equal(t, int32(9), *got.Verdict.ScoreEfficiency)
	// work_type is the dominant phase's theme.
	require.NotNil(t, got.Verdict.WorkType)
	assert.Equal(t, sessionintelligencepb.PhaseTheme_PHASE_THEME_CREATION, *got.Verdict.WorkType)

	// Turns — the full judged contract: every field the schema defines threads
	// through, not just a subset.
	require.Len(t, got.Turns, 2)
	require.NotNil(t, got.Turns[0].PromptType)
	assert.Equal(t, sessionintelligencepb.PromptType_PROMPT_TYPE_CREATION, *got.Turns[0].PromptType)
	require.NotNil(t, got.Turns[0].UserSteering)
	assert.Equal(t, sessionintelligencepb.TurnUserSteering_TURN_USER_STEERING_ACTIVE, *got.Turns[0].UserSteering)
	require.NotNil(t, got.Turns[0].PromptCompleteness)
	assert.Equal(t, sessionintelligencepb.PromptCompleteness_PROMPT_COMPLETENESS_COMPLETE, *got.Turns[0].PromptCompleteness)
	require.NotNil(t, got.Turns[0].PromptObjectiveMet)
	assert.Equal(t, sessionintelligencepb.PromptObjectiveMet_PROMPT_OBJECTIVE_MET_MET, *got.Turns[0].PromptObjectiveMet)
	require.NotNil(t, got.Turns[0].AgentInitiative)
	assert.Equal(t, sessionintelligencepb.AgentInitiative_AGENT_INITIATIVE_ASSUMED_AND_PROCEEDED, *got.Turns[0].AgentInitiative)
	require.NotNil(t, got.Turns[0].ToolUse)
	assert.Equal(t, sessionintelligencepb.ToolUse_TOOL_USE_APPROPRIATE, *got.Turns[0].ToolUse)
	require.NotNil(t, got.Turns[0].FailureClass)
	assert.Equal(t, sessionintelligencepb.FailureClass_FAILURE_CLASS_NONE, *got.Turns[0].FailureClass)
	require.NotNil(t, got.Turns[0].TokenLocus)
	assert.Equal(t, sessionintelligencepb.TokenLocus_TOKEN_LOCUS_AGENT_REASONING, *got.Turns[0].TokenLocus)
	require.NotNil(t, got.Turns[0].WasteCause)
	assert.Equal(t, sessionintelligencepb.WasteCause_WASTE_CAUSE_NONE, *got.Turns[0].WasteCause)
	require.NotNil(t, got.Turns[0].DecisionType)
	assert.Equal(t, sessionintelligencepb.DecisionType_DECISION_TYPE_APPROACH, *got.Turns[0].DecisionType)
	require.NotNil(t, got.Turns[0].DownstreamImpact)
	assert.Equal(t, sessionintelligencepb.DownstreamImpact_DOWNSTREAM_IMPACT_BUILT_ON, *got.Turns[0].DownstreamImpact)
	require.NotNil(t, got.Turns[0].FeedbackTarget)
	assert.Equal(t, sessionintelligencepb.FeedbackTarget_FEEDBACK_TARGET_ARTIFACT, *got.Turns[0].FeedbackTarget)
	require.NotNil(t, got.Turns[0].FeedbackDisposition)
	assert.Equal(t, sessionintelligencepb.FeedbackDisposition_FEEDBACK_DISPOSITION_ACCEPT, *got.Turns[0].FeedbackDisposition)
	require.NotNil(t, got.Turns[0].ReferencedArtifact)
	assert.Equal(t, "spec.md", *got.Turns[0].ReferencedArtifact)
	require.NotNil(t, got.Turns[0].ReferencedFindingIDs)
	assert.Equal(t, []string{"f1"}, got.Turns[0].ReferencedFindingIDs.GetValues())
	require.NotNil(t, got.Turns[0].EvidenceSeq)
	assert.Equal(t, int32(3), *got.Turns[0].EvidenceSeq)
	require.NotNil(t, got.Turns[0].WasRetry)
	assert.Equal(t, sessionintelligencepb.WasRetry_WAS_RETRY_FRESH, *got.Turns[0].WasRetry)
	require.NotNil(t, got.Turns[0].PhaseOrdinal)
	assert.Equal(t, int32(0), *got.Turns[0].PhaseOrdinal, "turn 0 links to phase ordinal 0 by prompt range")
	// A nullable turn field decodes to nil, not a bogus zero enum.
	assert.Nil(t, got.Turns[1].UserSteering)
	assert.Nil(t, got.Turns[1].WasRetry)
	assert.Nil(t, got.Turns[1].FailureClass)
	assert.Nil(t, got.Turns[1].WasteCause)
	assert.Nil(t, got.Turns[1].DecisionType)
	assert.Nil(t, got.Turns[1].EvidenceSeq)

	// Phases — the full phase contract, including the boundary-override signal and the
	// Title-case SDLC set mapping.
	require.Len(t, got.Phases, 1)
	assert.Equal(t, int32(0), got.Phases[0].PhaseOrdinal)
	require.NotNil(t, got.Phases[0].PhaseTheme)
	assert.Equal(t, sessionintelligencepb.PhaseTheme_PHASE_THEME_CREATION, *got.Phases[0].PhaseTheme)
	require.NotNil(t, got.Phases[0].TaskComplexity)
	assert.Equal(t, sessionintelligencepb.TaskComplexity_TASK_COMPLEXITY_MODERATE, *got.Phases[0].TaskComplexity)
	require.NotNil(t, got.Phases[0].RangeEnd)
	assert.Equal(t, int32(1), *got.Phases[0].RangeEnd)
	require.NotNil(t, got.Phases[0].BoundaryOverridden)
	assert.True(t, *got.Phases[0].BoundaryOverridden)
	require.NotNil(t, got.Phases[0].OverrideReason)
	assert.Equal(t, "folded a one-prompt theme change", *got.Phases[0].OverrideReason)
	require.NotNil(t, got.Phases[0].UserSteering)
	assert.Equal(t, sessionintelligencepb.PhaseUserSteering_PHASE_USER_STEERING_ACTIVE, *got.Phases[0].UserSteering)
	require.NotNil(t, got.Phases[0].PhaseObjectiveMet)
	assert.Equal(t, sessionintelligencepb.PhaseObjectiveMet_PHASE_OBJECTIVE_MET_MET, *got.Phases[0].PhaseObjectiveMet)
	require.NotNil(t, got.Phases[0].SdlcStageSet)
	assert.Equal(t,
		[]sessionintelligencepb.SdlcStage{sessionintelligencepb.SdlcStage_SDLC_STAGE_CODE, sessionintelligencepb.SdlcStage_SDLC_STAGE_TEST_AND_QA},
		got.Phases[0].SdlcStageSet.GetStages())
	require.NotNil(t, got.Phases[0].WentWell)
	assert.Equal(t, []string{"clear spec"}, got.Phases[0].WentWell.GetValues())

	// Security readout — extracted from the turn's prompt_issues, keyed by slug.
	require.Len(t, got.SecurityIssues, 1)
	assert.Equal(t, sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK, got.SecurityIssues[0].GetSlug())
	assert.Equal(t, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_CRITICAL, got.SecurityIssues[0].GetSeverity())

	// The turn also carries the issue in its own prompt_issues list.
	require.NotNil(t, got.Turns[0].PromptIssues)
	require.Len(t, got.Turns[0].PromptIssues.GetItems(), 1)
}

// readBodyWithLists exercises the JSONB judged lists R emits: a phase could_be_better
// note and the session-grain invoked_skills / harness_findings.
const readBodyWithLists = `{
  "turns": [
    {"prompt_index": 0, "prompt_type": "planning", "prompt_objective_met": "met",
     "agent_helpfulness": "helpful", "agent_initiative": "followed_spec",
     "tool_use": "appropriate", "token_locus": "agent_reasoning", "evidence_seq": 1}
  ],
  "phases": [
    {"prompt_range": {"start_index": 0, "end_index": 0}, "boundary_overridden": false,
     "override_reason": null, "phase_theme": "planning", "sdlc_stage": ["Plan"],
     "task_complexity": "moderate", "user_steering": "active", "phase_objective": "plan it",
     "phase_objective_met": "met", "agent_helpfulness": "helpful", "smoothness": "smooth",
     "friction_cause": "none", "speed": "fast", "went_well": [], "went_wrong": [],
     "could_be_better": [{"root_cause": "no plan file", "suggestion": "write a plan",
       "lever": "planning", "axes": ["speed", "tokens"], "target": "PLAN.md",
       "evidence_prompt_index": 0}]}
  ],
  "session_analysis": {
    "underlying_goal": "plan the work", "session_steering": "active", "goal_achieved": "fully",
    "smoothness": "smooth", "friction_cause": "none", "speed": "fast",
    "token_efficiency": "efficient", "goal_is_coding_work": true,
    "scores": {"overall": 7, "clarity": 7, "specificity": 7, "context_provision": 7,
               "actionability": 7, "completeness": 7, "efficiency": 7},
    "invoked_skills": [{"invocation_ref": "skill:plan", "baseline_intent": "produce a plan",
      "input_contract": "task", "contract_satisfied": true, "contribution": "wrote the plan",
      "evidence_prompt_index": 0}],
    "harness_findings": [{"preference_signal": "wants a plan file", "agent_default": "inline plan",
      "author_preference": "PLAN.md", "both_valid": true, "attribution": "harness",
      "invoked_skill": "plan", "artifact_type": "command", "artifact_relation": "should_have_been_invoked",
      "phase_ref": 0, "phase_impact": "faster", "visible_via": "inferred", "evidence_prompt_index": 0}]
  }
}`

// TestParseReadResponse_JudgedLists pins the JSONB list mappings: the phase
// coaching note and the session invoked_skills / harness_findings must thread through
// with their enums resolved, not drop to NULL.
func TestParseReadResponse_JudgedLists(t *testing.T) {
	got, err := parseReadResponse(readBodyWithLists)
	require.NoError(t, err)

	require.NotNil(t, got.Phases[0].Coaching)
	require.Len(t, got.Phases[0].Coaching.GetItems(), 1)
	coaching := got.Phases[0].Coaching.GetItems()[0]
	assert.Equal(t, "no plan file", coaching.GetRootCause())
	assert.Equal(t, sessionintelligencepb.CoachingLever_COACHING_LEVER_PLANNING, coaching.GetLever())
	assert.Equal(t, "PLAN.md", coaching.GetTarget())
	assert.Equal(t,
		[]sessionintelligencepb.CoachingAxis{sessionintelligencepb.CoachingAxis_COACHING_AXIS_SPEED, sessionintelligencepb.CoachingAxis_COACHING_AXIS_TOKENS},
		coaching.GetAxes())

	require.NotNil(t, got.Verdict.InvokedSkills)
	require.Len(t, got.Verdict.InvokedSkills.GetItems(), 1)
	skill := got.Verdict.InvokedSkills.GetItems()[0]
	assert.Equal(t, "skill:plan", skill.GetInvocationRef())
	assert.Equal(t, sessionintelligencepb.InputContract_INPUT_CONTRACT_TASK, skill.GetInputContract())
	assert.True(t, skill.GetContractSatisfied())

	require.NotNil(t, got.Verdict.HarnessFindings)
	require.Len(t, got.Verdict.HarnessFindings.GetItems(), 1)
	finding := got.Verdict.HarnessFindings.GetItems()[0]
	assert.Equal(t, sessionintelligencepb.FindingAttributionKind_FINDING_ATTRIBUTION_KIND_HARNESS, finding.GetAttribution())
	assert.Equal(t, sessionintelligencepb.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND, finding.GetArtifactType())
	assert.Equal(t, sessionintelligencepb.ArtifactRelation_ARTIFACT_RELATION_SHOULD_HAVE_BEEN_INVOKED, finding.GetArtifactRelation())
	assert.Equal(t, sessionintelligencepb.PhaseImpact_PHASE_IMPACT_FASTER, finding.GetPhaseImpact())
	assert.Equal(t, sessionintelligencepb.VisibleVia_VISIBLE_VIA_INFERRED, finding.GetVisibleVia())
}

// TestParseDiscardResponse decodes D's analyze_discard shape into DiscardedRange values
// (both bounds inclusive); token_sum stays zero because code sums it at commit time.
func TestParseDiscardResponse(t *testing.T) {
	const body = `{"discarded_ranges": [
		{"prompt_range": {"start_index": 2, "end_index": 4}, "reason": "reverted approach",
		 "evidence_prompt_index": 5}
	]}`
	got, err := parseDiscardResponse(body)
	require.NoError(t, err)
	require.NotNil(t, got.DiscardedRanges)
	require.Len(t, got.DiscardedRanges.GetItems(), 1)
	rng := got.DiscardedRanges.GetItems()[0]
	assert.Equal(t, int32(2), rng.GetRangeStart())
	assert.Equal(t, int32(4), rng.GetRangeEnd())
	assert.Equal(t, int32(5), rng.GetEvidencePromptIndex())
	assert.Equal(t, "reverted approach", rng.GetReason())
	assert.Equal(t, int64(0), rng.GetTokenSum(), "token_sum is code-summed at commit, not parsed")

	// An empty array (the common answer) yields no ranges.
	empty, err := parseDiscardResponse(`{"discarded_ranges": []}`)
	require.NoError(t, err)
	assert.Nil(t, empty.DiscardedRanges)
}
