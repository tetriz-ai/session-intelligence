// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Command analyze-raw-session runs the public single-session front door — analyze.AnalyzeSession —
// over a raw session transcript (the JSON a coding-agent session is captured as) and prints the
// structured analysis: the session verdict, input-quality scores, and the judged turn and phase
// counts.
//
// It is self-contained and offline: instead of a real reference-llm/Gemini provider it supplies a tiny
// in-process fake Provider that returns a fixed read-pass result, so the example runs with no
// network, no key and no database. Point analyze.NewReferenceLLMProvider at a real service (booted with
// your own LLM__GEMINI_API_KEY) to run the real pipeline — see the top-level README.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tetriz-ai/session-intelligence/analyze"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

func main() {
	conv, err := analyze.ParseConversation(buildRawSessionJSON())
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(1)
	}

	analysis, err := analyze.AnalyzeSession(context.Background(), conv, analyze.Options{
		Provider:  fakeProvider{},
		SessionID: "example-session",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "analyze:", err)
		os.Exit(1)
	}

	fmt.Printf("session: %s (%s)\n", analysis.SessionID, analysis.IdeType)
	fmt.Printf("goal: %s\n", analysis.Verdict.UnderlyingGoal)
	fmt.Printf("coding work: %v\n", deref(analysis.Verdict.GoalIsCodingWork))
	fmt.Printf("scores: overall %d, clarity %d\n", deref(analysis.Verdict.ScoreOverall), deref(analysis.Verdict.ScoreClarity))
	fmt.Printf("turns: %d judged, phases: %d\n", len(analysis.Turns), len(analysis.Phases))
	fmt.Printf("security flags: %d\n", len(analysis.SecurityFlags))
}

// fakeProvider is an in-process analyze.Provider: it returns a fixed analyze_read result and an empty
// envelope for every other task, so the example exercises the real public pipeline with no network.
type fakeProvider struct{}

func (fakeProvider) GenerateContent(_ context.Context, req analyze.GenerateContentRequest) (*analyze.GenerateContentResponse, error) {
	if req.TaskType != "analyze_read" {
		return &analyze.GenerateContentResponse{Result: "{}"}, nil
	}
	// One verdict per supplied (human) turn — the fixture has two — plus a single phase spanning them.
	const result = `{
      "session_analysis": {
        "underlying_goal": "Add retry with backoff and a unit test",
        "goal_is_coding_work": true,
        "scores": {"overall": 8, "clarity": 7}
      },
      "turns": [{"prompt_index": 0}, {"prompt_index": 1}],
      "phases": [{"prompt_range": {"start_index": 0, "end_index": 1}}]
    }`
	return &analyze.GenerateContentResponse{Result: result, Model: "fake", Provider: "fake"}, nil
}

func deref[T any](value *T) T {
	var zero T
	if value == nil {
		return zero
	}
	return *value
}

// buildRawSessionJSON returns a short coding session as the JSON a real transcript is stored as, so
// the example exercises the same parse-and-analyze path production uses.
func buildRawSessionJSON() []byte {
	ts := timestamppb.New(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	user := func(content string) *sessionintelligencepb.Prompt {
		return &sessionintelligencepb.Prompt{Role: sessionintelligencepb.Role_ROLE_USER, Content: content, Timestamp: ts}
	}
	assistant := func(content, model string, in, out, cacheRead int64, added, removed int32, files ...string) *sessionintelligencepb.Prompt {
		return &sessionintelligencepb.Prompt{
			Role:      sessionintelligencepb.Role_ROLE_ASSISTANT,
			Content:   content,
			Timestamp: ts,
			Metadata: &sessionintelligencepb.PromptMetadata{
				Model:                model,
				InputTokens:          in,
				OutputTokens:         out,
				CacheReadInputTokens: cacheRead,
				LinesAdded:           proto.Int32(added),
				LinesRemoved:         proto.Int32(removed),
				FilesEdited:          files,
			},
		}
	}
	conv := &sessionintelligencepb.Conversation{
		IdeType:               sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		SessionId:             "example-session",
		ConversationStartedAt: ts,
		Prompts: []*sessionintelligencepb.Prompt{
			user("Add a retry with exponential backoff to the HTTP client."),
			assistant("Added exponential backoff retry to client.go.", "claude-sonnet-4-5", 8200, 950, 15000, 34, 6, "client.go"),
			user("Now add a unit test for the retry path."),
			assistant("Added client_retry_test.go with three cases.", "claude-sonnet-4-5", 9100, 1200, 22000, 58, 2, "client_retry_test.go"),
		},
	}
	out, err := protojson.Marshal(conv)
	if err != nil {
		panic(err)
	}
	return out
}
