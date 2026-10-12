// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package analyze

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

// A record of the companion local-session-extractor (what `local-session-extractor show <source> -- <id>`
// prints, one line of its `export`, or one of its store rows) is a different JSON shape from the
// Conversation protojson: the messages are a `conversation` array with plain roles, the source is a name
// such as `codex`, and the session-level objects may be stored as JSON-encoded strings. ParseConversation
// recognises that shape and converts it here, keeping every field the Conversation carries: message
// metadata, session_meta, the session events and their coverage, and the other session objects.

// extractorIdeTypes maps the extractor's source names to the Conversation's IdeType enum names.
var extractorIdeTypes = map[string]string{
	"claude-code": "IDE_TYPE_CLAUDE_CODE",
	"cursor":      "IDE_TYPE_CURSOR",
	"copilot":     "IDE_TYPE_COPILOT",
	"antigravity": "IDE_TYPE_ANTIGRAVITY",
	"codex":       "IDE_TYPE_CODEX",
	"kiro":        "IDE_TYPE_KIRO",
}

var extractorRoles = map[string]string{
	"user":      "ROLE_USER",
	"assistant": "ROLE_ASSISTANT",
}

// extractorPassThrough are the session-level fields whose content is already in the Conversation's
// shape; only their spelling (nested JSON or a JSON-encoded string) differs.
var extractorPassThrough = []string{
	"session_meta", "session_facets", "installation_metadata", "stats_cache", "ai_attribution", "session_events",
}

// isExtractorRecord reports whether fields hold an extractor record: messages under `conversation`, and
// no Conversation `prompts`.
func isExtractorRecord(fields map[string]json.RawMessage) bool {
	_, hasConversation := fields["conversation"]
	_, hasPrompts := fields["prompts"]
	return hasConversation && !hasPrompts
}

// conversationFromExtractorRecord returns the Conversation protojson for an extractor record.
func conversationFromExtractorRecord(fields map[string]json.RawMessage) ([]byte, error) {
	var source string
	if err := json.Unmarshal(fields["ide_type"], &source); err != nil {
		return nil, fmt.Errorf("extractor record: ide_type: %w", err)
	}
	ideType, ok := extractorIdeTypes[source]
	if !ok {
		return nil, fmt.Errorf("extractor record: unknown source %q", source)
	}
	out := map[string]any{"ide_type": ideType}

	var sessionID string
	if err := unmarshalOptional(fields["session_id"], &sessionID); err != nil {
		return nil, fmt.Errorf("extractor record: session_id: %w", err)
	}
	out["session_id"] = sessionID

	var projectPath string
	if err := unmarshalOptional(fields["project_path"], &projectPath); err != nil {
		return nil, fmt.Errorf("extractor record: project_path: %w", err)
	}
	if projectPath != "" {
		out["project_name"] = path.Base(projectPath)
	}

	prompts, startedAt, err := promptsFromExtractorConversation(fields["conversation"])
	if err != nil {
		return nil, err
	}
	out["prompts"] = prompts

	// The start is the first message's timestamp, else the time the source says it created the session.
	if startedAt == "" {
		var createdMs *int64
		if err := unmarshalOptional(fields["conversation_source_created_at_ms"], &createdMs); err != nil {
			return nil, fmt.Errorf("extractor record: conversation_source_created_at_ms: %w", err)
		}
		if createdMs != nil {
			startedAt = time.UnixMilli(*createdMs).UTC().Format(time.RFC3339Nano)
		}
	}
	if startedAt != "" {
		out["conversation_started_at"] = startedAt
	}

	var lastMtimeMs *int64
	if err := unmarshalOptional(fields["last_mtime_ms"], &lastMtimeMs); err != nil {
		return nil, fmt.Errorf("extractor record: last_mtime_ms: %w", err)
	}
	if lastMtimeMs != nil && *lastMtimeMs > 0 {
		out["last_updated_at"] = time.UnixMilli(*lastMtimeMs).UTC().Format(time.RFC3339Nano)
	}

	var extractorVersion *int32
	if err := unmarshalOptional(fields["extractor_version"], &extractorVersion); err != nil {
		return nil, fmt.Errorf("extractor record: extractor_version: %w", err)
	}
	if extractorVersion != nil {
		out["extractor_version"] = *extractorVersion
	}

	var eventCoverage string
	if err := unmarshalOptional(fields["event_coverage"], &eventCoverage); err != nil {
		return nil, fmt.Errorf("extractor record: event_coverage: %w", err)
	}
	if eventCoverage != "" {
		out["event_coverage"] = eventCoverage
	}

	for _, name := range extractorPassThrough {
		value, err := decodeJSONField(fields[name])
		if err != nil {
			return nil, fmt.Errorf("extractor record: %s: %w", name, err)
		}
		if value != nil {
			out[name] = value
		}
	}

	return json.Marshal(out)
}

// promptsFromExtractorConversation converts the extractor's messages to Conversation prompts, and returns
// the first message timestamp as the conversation's start.
func promptsFromExtractorConversation(raw json.RawMessage) ([]map[string]any, string, error) {
	value, err := decodeJSONField(raw)
	if err != nil {
		return nil, "", fmt.Errorf("extractor record: conversation: %w", err)
	}
	var messages []struct {
		Role      string          `json:"role"`
		Content   string          `json:"content"`
		Timestamp string          `json:"timestamp"`
		Metadata  json.RawMessage `json:"metadata"`
	}
	if value != nil {
		if err := json.Unmarshal(value, &messages); err != nil {
			return nil, "", fmt.Errorf("extractor record: conversation: %w", err)
		}
	}
	prompts := make([]map[string]any, 0, len(messages))
	startedAt := ""
	for _, message := range messages {
		role, ok := extractorRoles[message.Role]
		if !ok {
			role = "ROLE_UNSPECIFIED"
		}
		prompt := map[string]any{"role": role, "content": message.Content}
		// A timestamp the Timestamp type cannot hold is left off that message, so it never fails the
		// whole conversation.
		if timestamp, ok := rfc3339(message.Timestamp); ok {
			prompt["timestamp"] = timestamp
			if startedAt == "" {
				startedAt = timestamp
			}
		}
		if len(message.Metadata) > 0 && !bytes.Equal(message.Metadata, []byte("null")) {
			prompt["metadata"] = message.Metadata
		}
		prompts = append(prompts, prompt)
	}
	return prompts, startedAt, nil
}

// decodeJSONField returns a field's JSON, whether it is nested JSON or a JSON-encoded string, or nil when
// the field is absent, null or an empty string.
func decodeJSONField(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if raw[0] != '"' {
		return raw, nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, err
	}
	if strings.TrimSpace(encoded) == "" {
		return nil, nil
	}
	if !json.Valid([]byte(encoded)) {
		return nil, fmt.Errorf("not valid JSON")
	}
	return json.RawMessage(encoded), nil
}

// unmarshalOptional unmarshals raw into target unless the field is absent or null.
func unmarshalOptional(raw json.RawMessage, target any) error {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	return json.Unmarshal(raw, target)
}

// rfc3339 returns timestamp in the RFC 3339 form a protobuf Timestamp parses, accepting the space-separated
// form some sources write, or false when it cannot be read.
func rfc3339(timestamp string) (string, bool) {
	if timestamp == "" {
		return "", false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00"} {
		if parsed, err := time.Parse(layout, timestamp); err == nil {
			return parsed.UTC().Format(time.RFC3339Nano), true
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999"} {
		if parsed, err := time.ParseInLocation(layout, timestamp, time.UTC); err == nil {
			return parsed.Format(time.RFC3339Nano), true
		}
	}
	return "", false
}
