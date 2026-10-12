// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package analyze_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/analyze"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// The Agent Desk fixtures are records of the companion local-session-extractor: export.jsonl is its
// sanitized golden export (one record a line, 31 sessions across Claude Code, Codex, Copilot, Cursor and
// Kiro), and show-codex.json is one of those records printed the way `local-session-extractor show`
// prints it. They pin the handoff `local-session-extractor show <source> -- <id> > session.json`, then
// `analyze-real session.json`.

// extractorRecord is the part of an extractor record the assertions compare against.
type extractorRecord struct {
	IdeType       string            `json:"ide_type"`
	SessionID     string            `json:"session_id"`
	ProjectPath   *string           `json:"project_path"`
	CreatedAtMs   *int64            `json:"conversation_source_created_at_ms"`
	Conversation  []json.RawMessage `json:"conversation"`
	SessionMeta   json.RawMessage   `json:"session_meta"`
	SessionEvents []json.RawMessage `json:"session_events"`
	EventCoverage *string           `json:"event_coverage"`
}

var extractorIdeTypes = map[string]sessionintelligencepb.IdeType{
	"claude-code": sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
	"codex":       sessionintelligencepb.IdeType_IDE_TYPE_CODEX,
	"copilot":     sessionintelligencepb.IdeType_IDE_TYPE_COPILOT,
	"cursor":      sessionintelligencepb.IdeType_IDE_TYPE_CURSOR,
	"kiro":        sessionintelligencepb.IdeType_IDE_TYPE_KIRO,
}

func exportLines(t *testing.T) [][]byte {
	t.Helper()
	content, err := os.ReadFile("testdata/agentdesk/export.jsonl")
	require.NoError(t, err)
	var lines [][]byte
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for scanner.Scan() {
		if line := bytes.TrimSpace(scanner.Bytes()); len(line) > 0 {
			lines = append(lines, append([]byte(nil), line...))
		}
	}
	require.NoError(t, scanner.Err())
	require.Len(t, lines, 31)
	return lines
}

func TestParseConversationReadsEveryAgentDeskExportRecord(t *testing.T) {
	for _, line := range exportLines(t) {
		var record extractorRecord
		require.NoError(t, json.Unmarshal(line, &record))
		t.Run(record.IdeType+"/"+record.SessionID, func(t *testing.T) {
			conv, err := analyze.ParseConversation(line)
			require.NoError(t, err)

			assert.Equal(t, extractorIdeTypes[record.IdeType], conv.GetIdeType())
			assert.Equal(t, record.SessionID, conv.GetSessionId())
			require.Len(t, conv.GetPrompts(), len(record.Conversation), "every message becomes a prompt")
			anyTimestamp := false
			for i, raw := range record.Conversation {
				var message struct {
					Role      string `json:"role"`
					Content   string `json:"content"`
					Timestamp string `json:"timestamp"`
				}
				require.NoError(t, json.Unmarshal(raw, &message))
				want := map[string]sessionintelligencepb.Role{"user": sessionintelligencepb.Role_ROLE_USER, "assistant": sessionintelligencepb.Role_ROLE_ASSISTANT}[message.Role]
				assert.Equal(t, want, conv.GetPrompts()[i].GetRole(), "prompt %d role", i)
				assert.Equal(t, message.Content, conv.GetPrompts()[i].GetContent(), "prompt %d content", i)
				assert.Equal(t, message.Timestamp != "", conv.GetPrompts()[i].GetTimestamp() != nil, "prompt %d timestamp", i)
				anyTimestamp = anyTimestamp || message.Timestamp != ""
			}
			assert.Len(t, conv.GetSessionEvents(), len(record.SessionEvents), "every session event is kept")
			if record.EventCoverage != nil {
				assert.Equal(t, *record.EventCoverage, conv.GetEventCoverage().String())
			}
			if len(record.SessionMeta) > 0 && string(record.SessionMeta) != "null" {
				assert.NotNil(t, conv.GetSessionMeta(), "session_meta is kept")
			}
			if record.ProjectPath != nil && *record.ProjectPath != "" {
				assert.Equal(t, path.Base(*record.ProjectPath), conv.GetProjectName())
			}
			// The start comes from the first message timestamp, else the source's creation time; a record
			// with neither has no start rather than an invented one.
			assert.Equal(t, anyTimestamp || record.CreatedAtMs != nil, conv.GetConversationStartedAt() != nil, "start time")
		})
	}
}

func TestReadConversationFileReadsShowOutput(t *testing.T) {
	conv, err := analyze.ReadConversationFile("testdata/agentdesk/show-codex.json")
	require.NoError(t, err)

	assert.Equal(t, sessionintelligencepb.IdeType_IDE_TYPE_CODEX, conv.GetIdeType())
	assert.Equal(t, "thread-fixture-001", conv.GetSessionId())
	assert.Equal(t, "project", conv.GetProjectName())
	require.Len(t, conv.GetPrompts(), 2)

	user, assistant := conv.GetPrompts()[0], conv.GetPrompts()[1]
	assert.Equal(t, sessionintelligencepb.Role_ROLE_USER, user.GetRole())
	assert.Equal(t, "Write a sort function", user.GetContent())
	assert.Equal(t, time.Date(2026, 4, 1, 10, 0, 5, 0, time.UTC), user.GetTimestamp().AsTime())
	assert.Nil(t, assistant.GetTimestamp(), "a message with no timestamp keeps none")

	// Token metadata survives the conversion: the engine's cost and context metrics read it.
	assert.Equal(t, "o3", assistant.GetMetadata().GetModel())
	assert.EqualValues(t, 20, assistant.GetMetadata().GetInputTokens())
	assert.EqualValues(t, 50, assistant.GetMetadata().GetOutputTokens())
	assert.EqualValues(t, 5, assistant.GetMetadata().GetCacheReadInputTokens())

	// Session events and their coverage survive: loop and event-based metrics read them.
	require.Len(t, conv.GetSessionEvents(), 9)
	assert.Equal(t, sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_TOOL_CALL, conv.GetSessionEvents()[0].GetKind())
	assert.Equal(t, sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_TURN_COMPLETED, conv.GetSessionEvents()[8].GetKind())
	assert.Equal(t, sessionintelligencepb.EventCoverage_EVENT_COVERAGE_COMPLETE, conv.GetEventCoverage())

	assert.NotEmpty(t, conv.GetSessionMeta().GetGitBranches(), "session_meta is kept")
	assert.Equal(t, time.Date(2026, 4, 1, 10, 0, 5, 0, time.UTC), conv.GetConversationStartedAt().AsTime(),
		"the start is the first message's timestamp")
	assert.Equal(t, time.UnixMilli(1743494430000).UTC(), conv.GetLastUpdatedAt().AsTime(),
		"the last update is the record's last_mtime_ms")
}

// The library's own store rows (SessionStore.listConversations) carry the same fields as JSON-encoded
// strings rather than nested JSON; both spellings convert the same way.
func TestParseConversationReadsStringEncodedStoreRow(t *testing.T) {
	shown, err := os.ReadFile("testdata/agentdesk/show-codex.json")
	require.NoError(t, err)
	var record map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(shown, &record))
	for _, field := range []string{"conversation", "session_meta", "session_events"} {
		encoded, err := json.Marshal(string(record[field]))
		require.NoError(t, err)
		record[field] = encoded
	}
	storeRow, err := json.Marshal(record)
	require.NoError(t, err)

	fromStoreRow, err := analyze.ParseConversation(storeRow)
	require.NoError(t, err)
	fromShow, err := analyze.ParseConversation(shown)
	require.NoError(t, err)
	assert.Len(t, fromStoreRow.GetPrompts(), 2)
	assert.Len(t, fromStoreRow.GetSessionEvents(), 9)
	assert.Equal(t, fromShow.GetSessionMeta().GetGitBranches(), fromStoreRow.GetSessionMeta().GetGitBranches())
}

func TestParseConversationStillReadsTheConversationFormat(t *testing.T) {
	conv, err := analyze.ReadConversationFile("../examples/transcript.sample.json")
	require.NoError(t, err)
	assert.NotEqual(t, sessionintelligencepb.IdeType_IDE_TYPE_UNSPECIFIED, conv.GetIdeType())
	assert.NotEmpty(t, conv.GetPrompts())
}

func TestParseConversationRejectsAnUnknownExtractorSource(t *testing.T) {
	_, err := analyze.ParseConversation([]byte(`{"ide_type":"notepad","session_id":"s","conversation":[]}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"notepad"`)
}

// The handoff end to end: a `show` record goes straight into AnalyzeSession, offline.
func TestAnalyzeSessionRunsOnShowOutput(t *testing.T) {
	conv, err := analyze.ReadConversationFile("testdata/agentdesk/show-codex.json")
	require.NoError(t, err)
	const verdict = `{
      "session_analysis": {"underlying_goal": "Write a sort function", "goal_is_coding_work": true},
      "turns": [{"prompt_index": 0}],
      "phases": [{"prompt_range": {"start_index": 0, "end_index": 0}}]
    }`
	analysis, err := analyze.AnalyzeSession(context.Background(), conv, analyze.Options{
		Provider: fakeProvider{resultByTask: map[string]string{"analyze_read": verdict}},
	})
	require.NoError(t, err)
	assert.Equal(t, "thread-fixture-001", analysis.SessionID)
	assert.Equal(t, "CODEX", analysis.IdeType)
	assert.Equal(t, "Write a sort function", analysis.Verdict.UnderlyingGoal)
	assert.Len(t, analysis.Turns, 1)
}
