// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"testing"

	"github.com/stretchr/testify/assert"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// userPrompt is defined in turngrouper_test.go (same package); asstPrompt is the assistant
// counterpart this test needs.
func asstPrompt() *sessionintelligencepb.Prompt {
	return &sessionintelligencepb.Prompt{Role: sessionintelligencepb.Role_ROLE_ASSISTANT}
}

func promptRef(seq int64, ordinal int32) *sessionintelligencepb.SessionEvent {
	return &sessionintelligencepb.SessionEvent{
		Sequence:      seq,
		Kind:          sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_PROMPT_REFERENCE,
		PromptOrdinal: ordinal,
	}
}

func dispatch(seq int64, kind sessionintelligencepb.SessionEventKind, op, callID string) *sessionintelligencepb.SessionEvent {
	return &sessionintelligencepb.SessionEvent{Sequence: seq, Kind: kind, OperationName: op, CallId: callID}
}

// TestDeriveDispatchPromptIndex validates the #7 derivation over a synthesized
// session_events timeline that mirrors the real extractor's shape: a message's tool events
// are emitted just before that message's PROMPT_REFERENCE, and a Claude Code sub-agent
// dispatch is a SKILL_CALL whose call_id is the dispatch tool-use id. (The linkage and the
// prompt_ordinal→group mapping are separately validated against a real parent+subagent dump
// in the PR body.)
func TestDeriveDispatchPromptIndex(t *testing.T) {
	skill := sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_SKILL_CALL
	tool := sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_TOOL_CALL
	subStart := sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_SUBAGENT_STARTED

	tests := []struct {
		name    string
		prompts []*sessionintelligencepb.Prompt
		events  []*sessionintelligencepb.SessionEvent
		want    map[string]int32
	}{
		{
			name:    "no timeline yields nil",
			prompts: []*sessionintelligencepb.Prompt{userPrompt(), asstPrompt()},
			events:  nil,
			want:    nil,
		},
		{
			name:    "skill-call dispatches attribute to their human turn",
			prompts: []*sessionintelligencepb.Prompt{userPrompt(), asstPrompt(), userPrompt(), asstPrompt()},
			// groups: g0={p0,p1}, g1={p2,p3}
			events: []*sessionintelligencepb.SessionEvent{
				promptRef(1, 1),                         // user p0 → cur g0
				dispatch(2, skill, "Explore", "call-a"), // dispatch in g0 (emitted before asst ref)
				promptRef(3, 2),                         // asst p1 → still g0
				promptRef(4, 3),                         // user p2 → cur g1
				dispatch(5, skill, "general", "call-b"), // dispatch in g1
				promptRef(6, 4),                         // asst p3 → still g1
			},
			want: map[string]int32{"call-a": 0, "call-b": 1},
		},
		{
			name:    "tool-call Task and subagent-span count; a plain tool call does not",
			prompts: []*sessionintelligencepb.Prompt{userPrompt(), asstPrompt()},
			events: []*sessionintelligencepb.SessionEvent{
				promptRef(1, 1),                        // user p0 → cur g0
				dispatch(2, tool, "Task", "call-task"), // Task tool call → counts
				dispatch(3, tool, "Read", "call-read"), // plain tool → ignored
				dispatch(4, subStart, "", "call-span"), // inline subagent span → counts
				promptRef(5, 2),
			},
			want: map[string]int32{"call-task": 0, "call-span": 0},
		},
		{
			name:    "a dispatch with no preceding prompt reference is dropped",
			prompts: []*sessionintelligencepb.Prompt{userPrompt()},
			events: []*sessionintelligencepb.SessionEvent{
				dispatch(1, skill, "Explore", "call-early"), // no prompt ref yet → cur -1 → dropped
			},
			want: nil,
		},
		{
			name:    "events are ordered by sequence, not slice order",
			prompts: []*sessionintelligencepb.Prompt{userPrompt(), asstPrompt(), userPrompt()},
			// deliberately out of order in the slice
			events: []*sessionintelligencepb.SessionEvent{
				promptRef(3, 3),                         // user p2 → g1
				dispatch(2, skill, "Explore", "call-a"), // after ref seq1 (user p0 → g0)
				promptRef(1, 1),
			},
			want: map[string]int32{"call-a": 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conv := &sessionintelligencepb.Conversation{Prompts: tt.prompts, SessionEvents: tt.events}
			got := deriveDispatchPromptIndex(conv)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestPromptPositionGroupIndex checks the prompts-array-position → splitHumanTurns group
// index mapping stays consistent with splitHumanTurns, including leading non-user prompts.
func TestPromptPositionGroupIndex(t *testing.T) {
	tests := []struct {
		name    string
		prompts []*sessionintelligencepb.Prompt
		want    []int32
	}{
		{
			name:    "leading assistant folds into group 0",
			prompts: []*sessionintelligencepb.Prompt{asstPrompt(), userPrompt(), asstPrompt(), userPrompt()},
			want:    []int32{0, 0, 0, 1},
		},
		{
			name:    "all non-user stays group 0",
			prompts: []*sessionintelligencepb.Prompt{asstPrompt(), asstPrompt()},
			want:    []int32{0, 0},
		},
		{
			name:    "empty",
			prompts: nil,
			want:    []int32{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, promptPositionGroupIndex(tt.prompts))
		})
	}
}
