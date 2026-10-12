// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"testing"

	"github.com/stretchr/testify/assert"

	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

func skillEvent(name string) *sessionintelligencepb.SessionEvent {
	return &sessionintelligencepb.SessionEvent{Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_SKILL_CALL, OperationName: name}
}

func toolEvent(name string, outcome sessionintelligencepb.SessionEventOutcome) *sessionintelligencepb.SessionEvent {
	return &sessionintelligencepb.SessionEvent{Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_TOOL_CALL, OperationName: name, Outcome: outcome}
}

// TestBuildSpineLedgers covers the static E/D fire signals: the artifact ledger counts
// harness invocations (and excludes generic tool calls), and the branch/revert ledger
// counts revert/abandon evidence — from the timeline when present, else the SessionMeta
// aggregates a no-timeline client reports.
func TestBuildSpineLedgers(t *testing.T) {
	tests := []struct {
		name         string
		conv         *sessionintelligencepb.Conversation
		wantArtifact int
		wantRevert   int
	}{
		{
			name:         "empty conversation has no ledger evidence",
			conv:         &sessionintelligencepb.Conversation{},
			wantArtifact: 0,
			wantRevert:   0,
		},
		{
			name: "skill and command events count as artifacts; generic tool calls do not",
			conv: &sessionintelligencepb.Conversation{SessionEvents: []*sessionintelligencepb.SessionEvent{
				skillEvent("code-review"),
				{Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_COMMAND, OperationName: "/ship"},
				{Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_HOOK_CALL, OperationName: "pre-commit"},
				toolEvent("Bash", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_SUCCEEDED),
				toolEvent("Read", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_SUCCEEDED),
			}},
			wantArtifact: 3,
			wantRevert:   0,
		},
		{
			name: "delegated sub-agent runs count as artifacts on the timeline path too",
			conv: &sessionintelligencepb.Conversation{
				SessionEvents: []*sessionintelligencepb.SessionEvent{skillEvent("planner")},
				SessionMeta:   &sessionintelligencepb.SessionMeta{SubAgentRuns: []*sessionintelligencepb.SubAgentRun{{Name: "explorer"}}},
			},
			wantArtifact: 2,
			wantRevert:   0,
		},
		{
			name: "no timeline falls back to SessionMeta harness aggregates",
			conv: &sessionintelligencepb.Conversation{SessionMeta: &sessionintelligencepb.SessionMeta{
				SpawnedAgentNames: []string{"a", "b"},
				SpecCount:         1,
				HookCount:         1,
			}},
			wantArtifact: 4,
			wantRevert:   0,
		},
		{
			name: "cancelled/failed outcomes and revert operations count as revert evidence",
			conv: &sessionintelligencepb.Conversation{SessionEvents: []*sessionintelligencepb.SessionEvent{
				toolEvent("Edit", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_CANCELLED),
				toolEvent("Bash", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED),
				toolEvent("git revert HEAD", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_SUCCEEDED),
			}},
			wantArtifact: 0,
			wantRevert:   3, // one cancelled + one failed + one revert-op match
		},
		{
			name: "SessionMeta interruptions, deleted files, abandoned outcome and multiple branches count",
			conv: &sessionintelligencepb.Conversation{
				SessionMeta:   &sessionintelligencepb.SessionMeta{UserInterruptions: 2, GitBranches: []string{"main", "feature"}},
				SessionFacets: &sessionintelligencepb.SessionFacets{Outcome: "abandoned"},
				AiAttribution: &sessionintelligencepb.AiAttribution{DeletedFilesCount: 3},
			},
			wantArtifact: 0,
			wantRevert:   5, // interruptions(2) + abandoned(1) + deleted-files(1) + multi-branch(1)
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledgers := buildSpineLedgers(tt.conv)
			assert.Equal(t, tt.wantArtifact, ledgers.artifactInvocations, "artifact ledger count")
			assert.Equal(t, tt.wantRevert, ledgers.revertSignals, "revert ledger count")
		})
	}
}

// TestFiresEvidence covers E's fire gate: a non-empty artifact ledger fires it, or a
// round-trip pair R confirmed via a turn tag (was_retry / user_steering correcting).
func TestFiresEvidence(t *testing.T) {
	retrySame := sessionintelligencepb.WasRetry_WAS_RETRY_RETRY_SAME
	correcting := sessionintelligencepb.TurnUserSteering_TURN_USER_STEERING_CORRECTING
	tests := []struct {
		name    string
		ledgers spineLedgers
		read    *ReadResult
		want    bool
	}{
		{name: "no artifact, no read", ledgers: spineLedgers{}, read: nil, want: false},
		{name: "artifact ledger fires E regardless of read", ledgers: spineLedgers{artifactInvocations: 1}, read: &ReadResult{}, want: true},
		{name: "confirmed retry turn fires E", ledgers: spineLedgers{}, read: &ReadResult{Turns: []*TurnJudgment{{PromptIndex: 1, WasRetry: &retrySame}}}, want: true},
		{name: "correcting steering fires E", ledgers: spineLedgers{}, read: &ReadResult{Turns: []*TurnJudgment{{PromptIndex: 1, UserSteering: &correcting}}}, want: true},
		{name: "clean read with no artifact does not fire E", ledgers: spineLedgers{}, read: &ReadResult{Turns: []*TurnJudgment{{PromptIndex: 1}}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.ledgers.firesEvidence(tt.read))
		})
	}
}

// TestFiresDiscard covers D's fire gate: a non-empty branch/revert ledger.
func TestFiresDiscard(t *testing.T) {
	assert.False(t, spineLedgers{}.firesDiscard(), "no revert evidence does not fire D")
	assert.True(t, spineLedgers{revertSignals: 1}.firesDiscard(), "any revert evidence fires D")
}

// namedSkillEvent builds a SKILL_CALL carrying a sequence, so the timeline walk can place it on
// its surrounding human turn (skillEvent above omits the sequence the walk needs).
func namedSkillEvent(seq int64, name string) *sessionintelligencepb.SessionEvent {
	return &sessionintelligencepb.SessionEvent{Sequence: seq, Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_SKILL_CALL, OperationName: name}
}

// TestBuildInvocationDetailsPromptIndex pins the fix for the prompt_ordinal-on-skill-events bug:
// prompt_ordinal is set only on PROMPT_REFERENCE events (zero on skill/command/hook), so an
// invocation's turn must be resolved by walking the timeline to the surrounding PROMPT_REFERENCE
// and translating its ordinal to the raw whole-session human-turn index — never read off the
// invocation event's own (always-zero) prompt_ordinal. A SessionMeta sub-agent run carries no
// timeline position, so its promptIndex stays 0.
func TestBuildInvocationDetailsPromptIndex(t *testing.T) {
	tests := []struct {
		name string
		conv *sessionintelligencepb.Conversation
		want []invocationDetail
	}{
		{
			name: "skill after a second-turn prompt reference resolves to raw turn 1, not 0",
			conv: &sessionintelligencepb.Conversation{
				// groups: g0={p0,p1}, g1={p2,p3}
				Prompts: []*sessionintelligencepb.Prompt{userPrompt(), asstPrompt(), userPrompt(), asstPrompt()},
				SessionEvents: []*sessionintelligencepb.SessionEvent{
					promptRef(1, 1),                   // user p0 → cur g0
					promptRef(2, 2),                   // asst p1 → still g0
					promptRef(3, 3),                   // user p2 → cur g1
					namedSkillEvent(4, "code-review"), // fired in g1
					promptRef(5, 4),                   // asst p3 → still g1
				},
			},
			want: []invocationDetail{{
				artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
				slug:         "code-review",
				promptIndex:  1,
			}},
		},
		{
			name: "command and hook events resolve to the surrounding turn, ordered by sequence not slice order",
			conv: &sessionintelligencepb.Conversation{
				Prompts: []*sessionintelligencepb.Prompt{userPrompt(), asstPrompt(), userPrompt()},
				// deliberately out of slice order to prove the walk sorts by sequence
				SessionEvents: []*sessionintelligencepb.SessionEvent{
					{Sequence: 4, Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_HOOK_CALL, OperationName: "pre-commit"}, // g1
					promptRef(3, 3), // user p2 → cur g1
					{Sequence: 2, Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_COMMAND, OperationName: "/ship"}, // g0
					promptRef(1, 1), // user p0 → cur g0
				},
			},
			// details keep the original slice order; only the resolved promptIndex changes.
			want: []invocationDetail{
				{artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK, slug: "pre-commit", promptIndex: 1},
				{artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND, slug: "/ship", promptIndex: 0},
			},
		},
		{
			name: "a skill with no surrounding prompt reference stays at 0",
			conv: &sessionintelligencepb.Conversation{
				Prompts: []*sessionintelligencepb.Prompt{userPrompt()},
				SessionEvents: []*sessionintelligencepb.SessionEvent{
					namedSkillEvent(1, "planner"), // no prompt ref before it → no turn
				},
			},
			want: []invocationDetail{{
				artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
				slug:         "planner",
				promptIndex:  0,
			}},
		},
		{
			name: "a SessionMeta sub-agent run has no timeline position, so promptIndex stays 0",
			conv: &sessionintelligencepb.Conversation{
				Prompts: []*sessionintelligencepb.Prompt{userPrompt(), asstPrompt()},
				SessionEvents: []*sessionintelligencepb.SessionEvent{
					promptRef(1, 1),
					namedSkillEvent(2, "code-review"),
				},
				SessionMeta: &sessionintelligencepb.SessionMeta{SubAgentRuns: []*sessionintelligencepb.SubAgentRun{{Name: "explorer"}}},
			},
			want: []invocationDetail{
				{artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, slug: "code-review", promptIndex: 0},
				{artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT, slug: "explorer", promptIndex: 0},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, buildInvocationDetails(tt.conv))
		})
	}
}
