// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package analyze_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tetriz-ai/session-intelligence/analyze"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// fakeProvider is an in-memory analyze.Provider: it returns the canned result for each task type and
// never touches the network, so AnalyzeSession runs fully offline.
type fakeProvider struct {
	resultByTask map[string]string
	err          error
}

func (f fakeProvider) GenerateContent(_ context.Context, req analyze.GenerateContentRequest) (*analyze.GenerateContentResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	result, ok := f.resultByTask[req.TaskType]
	if !ok {
		result = "{}"
	}
	return &analyze.GenerateContentResponse{Result: result, Model: "fake", Provider: "fake"}, nil
}

// twoHumanTurnConversation is a clean two-human-turn Claude Code session (no machine/tool turns), so
// the spine's taggable turn set is exactly the two human turns the fake read pass must cover.
func twoHumanTurnConversation() *analyze.Conversation {
	ts := timestamppb.Now()
	user := func(content string) *sessionintelligencepb.Prompt {
		return &sessionintelligencepb.Prompt{Role: sessionintelligencepb.Role_ROLE_USER, Content: content, Timestamp: ts}
	}
	assistant := func(content string) *sessionintelligencepb.Prompt {
		return &sessionintelligencepb.Prompt{
			Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Content: content, Timestamp: ts,
			Metadata: &sessionintelligencepb.PromptMetadata{Model: "claude-sonnet-4-5", InputTokens: 1000, OutputTokens: 200},
		}
	}
	return &analyze.Conversation{
		IdeType:               sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		SessionId:             "s1",
		ConversationStartedAt: ts,
		Prompts: []*sessionintelligencepb.Prompt{
			user("Add retry."), assistant("Done."),
			user("Add a test."), assistant("Done."),
		},
	}
}

func TestAnalyzeSession(t *testing.T) {
	t.Parallel()
	const verdictAndTurns = `{
      "session_analysis": {"underlying_goal": "Add retry + test", "goal_is_coding_work": true, "scores": {"overall": 8, "clarity": 7}},
      "turns": [{"prompt_index": 0}, {"prompt_index": 1}],
      "phases": [{"prompt_range": {"start_index": 0, "end_index": 1}}]
    }`
	const verdictWithSecurity = `{
      "session_analysis": {"underlying_goal": "Paste a key"},
      "turns": [
        {"prompt_index": 0, "prompt_issues": [{"slug": "api-key-leak", "severity": "high", "note": "key in config"}]},
        {"prompt_index": 1}
      ],
      "phases": [{"prompt_range": {"start_index": 0, "end_index": 1}}]
    }`
	// A planted secret-shaped note (an sk- token) must be masked on the public SecurityFlags — mirrors
	// security/reconcile_test.go TestReconcileMasksKnownSecretShapesInExcerpt.
	const verdictWithPlantedSecret = `{
      "session_analysis": {"underlying_goal": "Leak a key"},
      "turns": [
        {"prompt_index": 0, "prompt_issues": [{"slug": "api-key-leak", "severity": "high", "note": "committed sk-abc123SECRETxyz to the repo"}]},
        {"prompt_index": 1}
      ],
      "phases": [{"prompt_range": {"start_index": 0, "end_index": 1}}]
    }`
	// Two issues of the SAME type (different severities) must collapse to one flag at the highest
	// severity seen, with one occurrence per contributing issue.
	const verdictWithTwoSameType = `{
      "session_analysis": {"underlying_goal": "Leak twice"},
      "turns": [
        {"prompt_index": 0, "prompt_issues": [{"slug": "api-key-leak", "severity": "low", "note": "first"}]},
        {"prompt_index": 1, "prompt_issues": [{"slug": "api-key-leak", "severity": "high", "note": "second"}]}
      ],
      "phases": [{"prompt_range": {"start_index": 0, "end_index": 1}}]
    }`

	tests := []struct {
		name         string
		conversation *analyze.Conversation
		provider     analyze.Provider
		opts         analyze.Options
		wantErr      bool
		check        func(t *testing.T, analysis *analyze.Analysis)
	}{
		{
			name:         "verdict turns and phases",
			conversation: twoHumanTurnConversation(),
			provider:     fakeProvider{resultByTask: map[string]string{"analyze_read": verdictAndTurns}},
			check: func(t *testing.T, analysis *analyze.Analysis) {
				assert.Equal(t, "s1", analysis.SessionID)
				assert.Equal(t, "CLAUDE_CODE", analysis.IdeType)
				assert.Equal(t, "Add retry + test", analysis.Verdict.UnderlyingGoal)
				require.NotNil(t, analysis.Verdict.GoalIsCodingWork)
				assert.True(t, *analysis.Verdict.GoalIsCodingWork)
				require.NotNil(t, analysis.Verdict.ScoreOverall)
				assert.Equal(t, int32(8), *analysis.Verdict.ScoreOverall)
				assert.Len(t, analysis.Turns, 2)
				assert.Len(t, analysis.Phases, 1)
				assert.Empty(t, analysis.SecurityFlags)
			},
		},
		{
			name:         "security flag from read pass is reconciled",
			conversation: twoHumanTurnConversation(),
			provider:     fakeProvider{resultByTask: map[string]string{"analyze_read": verdictWithSecurity}},
			check: func(t *testing.T, analysis *analyze.Analysis) {
				require.Len(t, analysis.SecurityFlags, 1)
				assert.Equal(t, "api-key-leak", analysis.SecurityFlags[0].Type)
				assert.Equal(t, "high", analysis.SecurityFlags[0].Severity)
				require.Len(t, analysis.SecurityFlags[0].Occurrences, 1)
				assert.Equal(t, "key in config", analysis.SecurityFlags[0].Occurrences[0])
			},
		},
		{
			name:         "planted secret is masked on the public flag",
			conversation: twoHumanTurnConversation(),
			provider:     fakeProvider{resultByTask: map[string]string{"analyze_read": verdictWithPlantedSecret}},
			check: func(t *testing.T, analysis *analyze.Analysis) {
				require.Len(t, analysis.SecurityFlags, 1)
				require.Len(t, analysis.SecurityFlags[0].Occurrences, 1)
				excerpt := analysis.SecurityFlags[0].Occurrences[0]
				assert.NotContains(t, excerpt, "sk-abc123SECRETxyz", "the raw secret must not survive")
				assert.Contains(t, excerpt, "[REDACTED]")
				// The per-turn readout is masked too (it is raw-but-redacted, not reconciled).
				require.NotEmpty(t, analysis.Turns)
				require.NotEmpty(t, analysis.Turns[0].SecurityIssues)
				assert.NotContains(t, analysis.Turns[0].SecurityIssues[0].Note, "sk-abc123SECRETxyz")
			},
		},
		{
			name:         "same-type issues collapse to one flag at highest severity",
			conversation: twoHumanTurnConversation(),
			provider:     fakeProvider{resultByTask: map[string]string{"analyze_read": verdictWithTwoSameType}},
			check: func(t *testing.T, analysis *analyze.Analysis) {
				require.Len(t, analysis.SecurityFlags, 1)
				assert.Equal(t, "api-key-leak", analysis.SecurityFlags[0].Type)
				assert.Equal(t, "high", analysis.SecurityFlags[0].Severity)
				assert.Len(t, analysis.SecurityFlags[0].Occurrences, 2)
			},
		},
		{
			name:         "session id falls back to conversation",
			conversation: twoHumanTurnConversation(),
			provider:     fakeProvider{resultByTask: map[string]string{"analyze_read": verdictAndTurns}},
			opts:         analyze.Options{}, // no SessionID override
			check: func(t *testing.T, analysis *analyze.Analysis) {
				assert.Equal(t, "s1", analysis.SessionID)
			},
		},
		{
			name:         "nil conversation errors",
			conversation: nil,
			provider:     fakeProvider{},
			wantErr:      true,
		},
		{
			name:         "nil provider errors",
			conversation: twoHumanTurnConversation(),
			provider:     nil,
			wantErr:      true,
		},
		{
			name:         "provider error propagates",
			conversation: twoHumanTurnConversation(),
			provider:     fakeProvider{err: errors.New("boom")},
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := tt.opts
			opts.Provider = tt.provider
			analysis, err := analyze.AnalyzeSession(context.Background(), tt.conversation, opts)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, analysis)
			if tt.check != nil {
				tt.check(t, analysis)
			}
		})
	}
}
