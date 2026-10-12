// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// Event-timeline builders mirroring exactly what the desktop CC extractor
// (the desktop client's session-events extractor) emits: a TOOL_CALL
// carries call_id + the work summary (commandSummaryOf: command/file_path/pattern/…); a
// TOOL_RESULT carries parent_call_id (== the call's call_id) + the outcome and usually no
// summary; a COMPACTION is a positional boundary with no call_id; a PROMPT_REFERENCE
// carries the 1-based prompt_ordinal + author.
func evPromptRef(seq int64, ordinal int32, author sessionintelligencepb.SessionEventAuthor) *sessionintelligencepb.SessionEvent {
	return &sessionintelligencepb.SessionEvent{
		Sequence:      seq,
		Kind:          sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_PROMPT_REFERENCE,
		Outcome:       sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_SUCCEEDED,
		PromptOrdinal: ordinal,
		Author:        author,
	}
}

func evToolCall(seq int64, callID, op, summary string) *sessionintelligencepb.SessionEvent {
	return &sessionintelligencepb.SessionEvent{
		Sequence:        seq,
		Kind:            sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_TOOL_CALL,
		OperationName:   op,
		CallId:          callID,
		Outcome:         sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_UNKNOWN,
		RedactedSummary: summary,
	}
}

func evToolResult(seq int64, parentCallID string, outcome sessionintelligencepb.SessionEventOutcome) *sessionintelligencepb.SessionEvent {
	return &sessionintelligencepb.SessionEvent{
		Sequence:     seq,
		Kind:         sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_TOOL_RESULT,
		ParentCallId: parentCallID,
		Outcome:      outcome,
	}
}

func evCompaction(seq int64) *sessionintelligencepb.SessionEvent {
	return &sessionintelligencepb.SessionEvent{
		Sequence: seq,
		Kind:     sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_COMPACTION,
		Outcome:  sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_SUCCEEDED,
	}
}

// evSubagentStarted/evSubagentCompleted mirror the inline sub-agent span the Kiro extractor emits
// (kiroIdeExtractor sub_agent_start/complete → these kinds), the span's subSessionId as call_id.
func evSubagentStarted(seq int64, callID string) *sessionintelligencepb.SessionEvent {
	return &sessionintelligencepb.SessionEvent{
		Sequence: seq,
		Kind:     sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_SUBAGENT_STARTED,
		CallId:   callID,
		Outcome:  sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_SUCCEEDED,
	}
}

func evSubagentCompleted(seq int64, callID string) *sessionintelligencepb.SessionEvent {
	return &sessionintelligencepb.SessionEvent{
		Sequence: seq,
		Kind:     sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_SUBAGENT_COMPLETED,
		CallId:   callID,
		Outcome:  sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_SUCCEEDED,
	}
}

func assistantPrompt() *sessionintelligencepb.Prompt {
	return &sessionintelligencepb.Prompt{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{}}
}

// wantTool builds the expected per-invocation ToolCall the tools list should carry: name +
// call_id from the TOOL_CALL, outcome stamped from the paired TOOL_RESULT (UNSPECIFIED when a
// call had no paired result). failure_kind is always empty — no source event carries one.
func wantTool(name, callID string, outcome sessionintelligencepb.SessionEventOutcome) *sessionintelligencepb.ToolCall {
	return &sessionintelligencepb.ToolCall{Name: name, CallId: callID, Outcome: outcome}
}

const (
	authorHuman     = "SESSION_EVENT_AUTHOR_HUMAN"
	authorDelegated = "SESSION_EVENT_AUTHOR_DELEGATED_AGENT"
	authorAgent     = "SESSION_EVENT_AUTHOR_AGENT"
)

// TestApplyEventDerivedTurnColumns pins the second (event-derived) fold: tool_retry_count,
// compaction_count, and the precise authorship stamp, plus the session_events-presence
// zero-vs-null gate.
//
// The retry + compaction shapes are grounded in a REAL Claude Code transcript
// (a recorded coding-agent session): 2 compact_boundary system lines → 2 COMPACTION
// events, 18 is_error tool_results → FAILED outcomes, and 2 derivable retries where a failed
// Read/Edit was re-issued against the same file_path. The goldens below reconstruct that same
// event shape (a failed call re-issued with an equal, non-empty summary; a compaction inside a
// turn's span) as the extractor emits it.
func TestApplyEventDerivedTurnColumns(t *testing.T) {
	t.Parallel()

	// A 3-prompt, 2-turn conversation shared by the grounded cases: user(t0) → assistant(t0)
	// → user(t1). Ordinals are 1-based into this prompts slice.
	twoTurn := func(subagentT1 bool) []*sessionintelligencepb.Prompt {
		u1 := userPrompt()
		u2 := userPrompt()
		u2.Metadata.IsSubAgent = subagentT1
		return []*sessionintelligencepb.Prompt{u1, assistantPrompt(), u2}
	}

	tests := []struct {
		name    string
		prompts []*sessionintelligencepb.Prompt
		events  []*sessionintelligencepb.SessionEvent
		// wantRetry/wantCompaction are per-turn; a nil entry asserts the column is NULL.
		wantRetry      []*int32
		wantCompaction []*int32
		wantAuthor     []string // per-turn; "" = not asserted
		wantSubagent   []*bool  // per-turn; nil = not asserted
		// Tool family, per-turn. A nil wantCall/wantErr entry asserts the count column is NULL.
		// A nil wantTools entry asserts row.Tools is NULL; a non-nil ToolCallList (Items possibly
		// empty) asserts the exact per-invocation shape — an empty-but-non-nil list is a measured
		// zero, distinct from NULL.
		wantCall  []*int32
		wantErr   []*int32
		wantTools []*sessionintelligencepb.ToolCallList
	}{
		{
			// The real-CC golden: turn 0 has a failed Read re-issued against the same
			// file_path (a retry) and a compaction in its span; turn 1's opening reference
			// is a delegated agent, so authorship comes off the EVENT, not the message flag
			// (the message's is_sub_agent is false here to prove the event supersedes it).
			name:    "real-CC shape: retry + compaction + precise authorship",
			prompts: twoTurn(false),
			events: []*sessionintelligencepb.SessionEvent{
				evPromptRef(1, 1, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN),
				evToolCall(2, "c1", "Read", "/a/f.go"),
				evToolResult(3, "c1", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED),
				evToolCall(4, "c2", "Read", "/a/f.go"), // re-issue → retry
				evToolResult(5, "c2", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_SUCCEEDED),
				evPromptRef(6, 2, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_AGENT),
				evCompaction(7),
				evPromptRef(8, 3, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_DELEGATED_AGENT),
				evToolCall(9, "c3", "Bash", "go test ./..."),
				evToolResult(10, "c3", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED), // failed, no re-issue → no retry
			},
			wantRetry:      []*int32{i32(1), i32(0)},
			wantCompaction: []*int32{i32(1), i32(0)},
			wantAuthor:     []string{authorHuman, authorDelegated},
			wantSubagent:   []*bool{boolPtr(false), boolPtr(true)},
			// turn 0: two Read calls (one FAILED, one SUCCEEDED) → 2 calls, 1 error; turn 1: one
			// FAILED Bash → 1 call, 1 error. Outcomes are stamped from each call's paired result.
			wantCall: []*int32{i32(2), i32(1)},
			wantErr:  []*int32{i32(1), i32(1)},
			wantTools: []*sessionintelligencepb.ToolCallList{
				{Items: []*sessionintelligencepb.ToolCall{
					wantTool("Read", "c1", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED),
					wantTool("Read", "c2", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_SUCCEEDED),
				}},
				{Items: []*sessionintelligencepb.ToolCall{wantTool("Bash", "c3", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED)}},
			},
		},
		{
			// No timeline: the whole record's computed columns stay NULL ("couldn't observe").
			name:           "no timeline → NULL columns",
			prompts:        twoTurn(false),
			events:         nil,
			wantRetry:      []*int32{nil, nil},
			wantCompaction: []*int32{nil, nil},
			// No timeline: the whole tool family stays NULL too (tools nil, counts nil).
			wantCall:  []*int32{nil, nil},
			wantErr:   []*int32{nil, nil},
			wantTools: []*sessionintelligencepb.ToolCallList{nil, nil},
		},
		{
			// Timeline present but nothing matched: a real measured 0, not NULL.
			name:    "timeline present, nothing matched → measured zero",
			prompts: []*sessionintelligencepb.Prompt{userPrompt(), assistantPrompt()},
			events: []*sessionintelligencepb.SessionEvent{
				evPromptRef(1, 1, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN),
				evPromptRef(2, 2, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_AGENT),
			},
			wantRetry:      []*int32{i32(0)},
			wantCompaction: []*int32{i32(0)},
			wantAuthor:     []string{authorHuman},
			// No tool calls but the timeline WAS observed → measured zero + empty-but-non-nil list.
			wantCall:  []*int32{i32(0)},
			wantErr:   []*int32{i32(0)},
			wantTools: []*sessionintelligencepb.ToolCallList{{}},
		},
		{
			// A failed call whose summary is empty derives no retry, even when re-issued with
			// an equally-empty summary (the no-summary-no-retry rule). The tool family is
			// summary-independent: both calls still count, the FAILED one still errors, and the
			// result-less re-issue carries UNSPECIFIED (no paired outcome to stamp).
			name:    "no summary → no retry, but tools still counted",
			prompts: []*sessionintelligencepb.Prompt{userPrompt()},
			events: []*sessionintelligencepb.SessionEvent{
				evPromptRef(1, 1, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN),
				evToolCall(2, "c1", "Read", ""),
				evToolResult(3, "c1", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED),
				evToolCall(4, "c2", "Read", ""),
			},
			wantRetry:      []*int32{i32(0)},
			wantCompaction: []*int32{i32(0)},
			wantCall:       []*int32{i32(2)},
			wantErr:        []*int32{i32(1)},
			wantTools: []*sessionintelligencepb.ToolCallList{{Items: []*sessionintelligencepb.ToolCall{
				wantTool("Read", "c1", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED),
				wantTool("Read", "c2", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_UNSPECIFIED),
			}}},
		},
		{
			// The retry bar is WITHIN a turn: a failure in turn 0 re-issued in turn 1 is not
			// a retry for either turn.
			name:    "re-issue in a later turn is not a retry",
			prompts: []*sessionintelligencepb.Prompt{userPrompt(), userPrompt()},
			events: []*sessionintelligencepb.SessionEvent{
				evPromptRef(1, 1, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN),
				evToolCall(2, "c1", "Read", "/a/f.go"),
				evToolResult(3, "c1", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED),
				evPromptRef(4, 2, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN),
				evToolCall(5, "c2", "Read", "/a/f.go"), // next turn → not a retry
			},
			wantRetry:      []*int32{i32(0), i32(0)},
			wantCompaction: []*int32{i32(0), i32(0)},
			// The call/error counts are per-turn just like retry: turn 0's failed Read is its
			// error; turn 1's re-issue is turn 1's own (result-less) call.
			wantCall: []*int32{i32(1), i32(1)},
			wantErr:  []*int32{i32(1), i32(0)},
			wantTools: []*sessionintelligencepb.ToolCallList{
				{Items: []*sessionintelligencepb.ToolCall{wantTool("Read", "c1", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED)}},
				{Items: []*sessionintelligencepb.ToolCall{wantTool("Read", "c2", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_UNSPECIFIED)}},
			},
		},
		{
			// A TOOL_CALL with no call_id still counts as a call and lands in the list, but its
			// outcome can never be stamped (nothing to pair a result to) → UNSPECIFIED, no error.
			name:    "unpaired call (no call_id) counts, outcome unstamped",
			prompts: []*sessionintelligencepb.Prompt{userPrompt()},
			events: []*sessionintelligencepb.SessionEvent{
				evPromptRef(1, 1, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN),
				evToolCall(2, "", "Bash", "go build ./..."),
				evToolResult(3, "", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED), // no parent id to pair
			},
			wantRetry:      []*int32{i32(0)},
			wantCompaction: []*int32{i32(0)},
			wantCall:       []*int32{i32(1)},
			wantErr:        []*int32{i32(0)},
			wantTools: []*sessionintelligencepb.ToolCallList{{Items: []*sessionintelligencepb.ToolCall{
				wantTool("Bash", "", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_UNSPECIFIED),
			}}},
		},
		{
			// Outcome attribution is by the CALL's turn, not the result event's turn: turn 0's Read
			// is called, then turn 1 opens, then turn 0's FAILED result arrives. The error must land
			// on turn 0 (the call's turn), and turn 1 stays a clean measured zero. Pins that the
			// outcome stamp targets the invocation, unlike the within-turn retry bar.
			name:    "result in a later turn attributes the error to the call's turn",
			prompts: []*sessionintelligencepb.Prompt{userPrompt(), userPrompt()},
			events: []*sessionintelligencepb.SessionEvent{
				evPromptRef(1, 1, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN),
				evToolCall(2, "c1", "Read", "/a/f.go"),
				evPromptRef(3, 2, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN), // turn 1 opens
				evToolResult(4, "c1", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED),
			},
			wantRetry:      []*int32{i32(0), i32(0)},
			wantCompaction: []*int32{i32(0), i32(0)},
			wantCall:       []*int32{i32(1), i32(0)},
			wantErr:        []*int32{i32(1), i32(0)},
			wantTools: []*sessionintelligencepb.ToolCallList{
				{Items: []*sessionintelligencepb.ToolCall{wantTool("Read", "c1", sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED)}},
				{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sp := &spineResult{
				ideType:       sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
				prompts:       tt.prompts,
				sessionEvents: tt.events,
			}
			rows := buildMeasuredTurnRows("org-1", "rr-1", sp)
			require.Len(t, rows, len(tt.wantRetry))
			for i, row := range rows {
				assert.Equal(t, tt.wantRetry[i], row.ToolRetryCount, "turn %d tool_retry_count", i)
				assert.Equal(t, tt.wantCompaction[i], row.CompactionCount, "turn %d compaction_count", i)
				// api_retry_count is never derived — always NULL.
				assert.Nil(t, row.APIRetryCount, "turn %d api_retry_count stays NULL", i)
				if i < len(tt.wantCall) {
					assert.Equal(t, tt.wantCall[i], row.ToolCallCount, "turn %d tool_call_count", i)
				}
				if i < len(tt.wantErr) {
					assert.Equal(t, tt.wantErr[i], row.ToolErrorCount, "turn %d tool_error_count", i)
				}
				if i < len(tt.wantTools) {
					assertToolsEqual(t, tt.wantTools[i], row.Tools, i)
				}
				if i < len(tt.wantAuthor) && tt.wantAuthor[i] != "" {
					require.NotNil(t, row.Author, "turn %d author", i)
					assert.Equal(t, tt.wantAuthor[i], *row.Author, "turn %d author", i)
				}
				if i < len(tt.wantSubagent) && tt.wantSubagent[i] != nil {
					require.NotNil(t, row.IsSubagent, "turn %d is_subagent", i)
					assert.Equal(t, *tt.wantSubagent[i], *row.IsSubagent, "turn %d is_subagent", i)
				}
			}
		})
	}
}

// assertToolsEqual compares the tools column by field getters (never deep proto equality,
// which trips on the message's internal state): a nil want asserts row.Tools is NULL; a
// non-nil want (Items possibly empty) asserts the same length and per-invocation name/call_id/
// outcome, so an empty-but-non-nil measured zero is held distinct from NULL.
func assertToolsEqual(t *testing.T, want, got *sessionintelligencepb.ToolCallList, turn int) {
	t.Helper()
	if want == nil {
		assert.Nil(t, got, "turn %d tools should be NULL", turn)
		return
	}
	require.NotNil(t, got, "turn %d tools should be non-nil", turn)
	require.Len(t, got.GetItems(), len(want.GetItems()), "turn %d tools length", turn)
	for j, w := range want.GetItems() {
		g := got.GetItems()[j]
		assert.Equal(t, w.GetName(), g.GetName(), "turn %d tools[%d] name", turn, j)
		assert.Equal(t, w.GetCallId(), g.GetCallId(), "turn %d tools[%d] call_id", turn, j)
		assert.Equal(t, w.GetOutcome(), g.GetOutcome(), "turn %d tools[%d] outcome", turn, j)
		// failure_kind is always empty — no source event carries a failure categorization.
		assert.Empty(t, g.GetFailureKind(), "turn %d tools[%d] failure_kind stays empty", turn, j)
	}
}

// TestApplyEventDerivedTurnColumns_ToolFamilyRealCounts reconstructs the shape of a real Claude
// Code transcript whose tool counts were hand-derived: agent-a4a66db44ba4014d0.jsonl carried 32
// tool_use blocks and 32 tool_results — 22 is_error==true (→ FAILED), 10 false (→ SUCCEEDED) —
// all under one human prompt → one turn. This rebuilds that single-turn shape synthetically (a
// TOOL_CALL then its paired TOOL_RESULT, unique call ids) and asserts the grouper reproduces the
// hand counts: tool_call_count=32, tool_error_count=22, and a 32-entry tools list.
func TestApplyEventDerivedTurnColumns_ToolFamilyRealCounts(t *testing.T) {
	t.Parallel()
	const (
		totalCalls = 32
		failed     = 22
	)
	events := []*sessionintelligencepb.SessionEvent{
		evPromptRef(1, 1, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN),
	}
	seq := int64(1)
	for i := 0; i < totalCalls; i++ {
		callID := "call-" + strconv.Itoa(i)
		seq++
		events = append(events, evToolCall(seq, callID, "Bash", "cmd "+strconv.Itoa(i)))
		outcome := sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_SUCCEEDED
		if i < failed {
			outcome = sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED
		}
		seq++
		events = append(events, evToolResult(seq, callID, outcome))
	}

	sp := &spineResult{
		ideType:       sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		prompts:       []*sessionintelligencepb.Prompt{userPrompt()},
		sessionEvents: events,
	}
	rows := buildMeasuredTurnRows("org-1", "rr-1", sp)
	require.Len(t, rows, 1)
	row := rows[0]
	require.NotNil(t, row.ToolCallCount)
	assert.Equal(t, int32(totalCalls), *row.ToolCallCount, "tool_call_count matches the hand count")
	require.NotNil(t, row.ToolErrorCount)
	assert.Equal(t, int32(failed), *row.ToolErrorCount, "tool_error_count matches the hand count")
	require.NotNil(t, row.Tools)
	assert.Len(t, row.Tools.GetItems(), totalCalls, "one tools entry per invocation")
}

// TestApplyEventDerivedTurnColumns_SubagentCount pins subagent_count: the count of inline
// SUBAGENT_STARTED spans attributed to each turn, stamped ONLY for Kiro (the sole inline-span
// provider) — a non-Kiro row keeps it NULL even when a span is present, and a Kiro turn with no
// spans is a measured 0. The SUBAGENT_COMPLETED close is never counted (one dispatch per open).
func TestApplyEventDerivedTurnColumns_SubagentCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ide     sessionintelligencepb.IdeType
		prompts []*sessionintelligencepb.Prompt
		events  []*sessionintelligencepb.SessionEvent
		wantSub []*int32 // per turn; nil asserts NULL
	}{
		{
			// Two Kiro turns: turn 0 opens two spans (one also closes — the close is not counted),
			// turn 1 opens one. Ordinal 3 → the second human turn.
			name:    "Kiro: inline spans counted per turn, close not counted",
			ide:     sessionintelligencepb.IdeType_IDE_TYPE_KIRO,
			prompts: []*sessionintelligencepb.Prompt{userPrompt(), assistantPrompt(), userPrompt()},
			events: []*sessionintelligencepb.SessionEvent{
				evPromptRef(1, 1, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN),
				evSubagentStarted(2, "s1"),
				evSubagentStarted(3, "s2"),
				evSubagentCompleted(4, "s1"),
				evPromptRef(5, 3, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN),
				evSubagentStarted(6, "s3"),
			},
			wantSub: []*int32{i32(2), i32(1)},
		},
		{
			// A Kiro session whose timeline opened no spans is a measured zero, not NULL.
			name:    "Kiro: timeline present, no spans → measured zero",
			ide:     sessionintelligencepb.IdeType_IDE_TYPE_KIRO,
			prompts: []*sessionintelligencepb.Prompt{userPrompt()},
			events:  []*sessionintelligencepb.SessionEvent{evPromptRef(1, 1, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN)},
			wantSub: []*int32{i32(0)},
		},
		{
			// A non-Kiro session folds sub-agents into separate child records, so subagent_count is
			// NULL even though a SUBAGENT_STARTED happens to be present — the inline signal is not
			// this provider's.
			name:    "non-Kiro (CC): span present but gated out → NULL",
			ide:     sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
			prompts: []*sessionintelligencepb.Prompt{userPrompt()},
			events: []*sessionintelligencepb.SessionEvent{
				evPromptRef(1, 1, sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN),
				evSubagentStarted(2, "s1"),
			},
			wantSub: []*int32{nil},
		},
		{
			// No timeline → NULL for the whole record (couldn't observe), Kiro or not.
			name:    "Kiro: no timeline → NULL",
			ide:     sessionintelligencepb.IdeType_IDE_TYPE_KIRO,
			prompts: []*sessionintelligencepb.Prompt{userPrompt()},
			events:  nil,
			wantSub: []*int32{nil},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sp := &spineResult{ideType: tt.ide, prompts: tt.prompts, sessionEvents: tt.events}
			rows := buildMeasuredTurnRows("org-1", "rr-1", sp)
			require.Len(t, rows, len(tt.wantSub))
			for i, row := range rows {
				assert.Equal(t, tt.wantSub[i], row.SubagentCount, "turn %d subagent_count", i)
			}
		})
	}
}

// TestApplyEventDerivedTurnColumns_AuthorOriginationPairing pins that the precise stamp maps
// each author to its origination: HUMAN → HUMAN, AGENT/DELEGATED_AGENT → AGENT_DRIVEN, and
// only DELEGATED_AGENT flips is_subagent.
func TestApplyEventDerivedTurnColumns_AuthorOriginationPairing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		author          sessionintelligencepb.SessionEventAuthor
		wantAuthor      string
		wantSubagent    bool
		wantOrigination sessionintelligencepb.PromptOrigination
	}{
		{"human", sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN, authorHuman, false, sessionintelligencepb.PromptOrigination_PROMPT_ORIGINATION_HUMAN},
		{"agent (injected, not delegated)", sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_AGENT, authorAgent, false, sessionintelligencepb.PromptOrigination_PROMPT_ORIGINATION_AGENT_DRIVEN},
		{"delegated agent", sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_DELEGATED_AGENT, authorDelegated, true, sessionintelligencepb.PromptOrigination_PROMPT_ORIGINATION_AGENT_DRIVEN},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sp := &spineResult{
				ideType:       sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
				prompts:       []*sessionintelligencepb.Prompt{userPrompt()},
				sessionEvents: []*sessionintelligencepb.SessionEvent{evPromptRef(1, 1, tt.author)},
			}
			rows := buildMeasuredTurnRows("org-1", "rr-1", sp)
			require.Len(t, rows, 1)
			row := rows[0]
			require.NotNil(t, row.Author)
			assert.Equal(t, tt.wantAuthor, *row.Author)
			require.NotNil(t, row.IsSubagent)
			assert.Equal(t, tt.wantSubagent, *row.IsSubagent)
			require.NotNil(t, row.PromptOrigination)
			assert.Equal(t, tt.wantOrigination, *row.PromptOrigination)
		})
	}
}
