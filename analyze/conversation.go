// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package analyze

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/sessionanalysis"
)

// MaxConversationFileBytes bounds the transcript file ReadConversationFile loads into
// memory. AnalyzeSession rejects a transcript over the single-call rune ceiling (~600k)
// anyway, and a single session's raw protojson stays well under this byte cap, so the cap is
// kept close to that bound: an oversized or malicious file is refused at read time — before
// the several-x protojson in-memory expansion — rather than parsed and then rejected.
// (ReadConversationDir reads files one at a time through this same cap.)
const MaxConversationFileBytes = 8 << 20 // 8 MiB

// Conversation is one coding-agent session transcript — the input AnalyzeSession analyses. It is the
// importable proto message (gen/sessionintelligence/v1); build one in memory, or parse one from the
// JSON a session is captured as with ParseConversation.
//
// The JSON is the protojson encoding of this message: an object with ide_type, prompts (each a role +
// content + metadata), and optional session_events / session_meta. See
// proto/sessionintelligence/v1/conversation.proto for the full contract. A record of the companion
// local-session-extractor (what `local-session-extractor show` prints) is read as well.
type Conversation = sessionintelligencepb.Conversation

// ParseConversation parses one transcript into a Conversation: a single JSON object in the Conversation
// protojson shape, or a local-session-extractor record (`show` output, one `export` line, or a store
// row), converted with its message metadata, session_meta and session events. It keeps the
// parse-boundary model backfill (an unnamed prompt inherits the conversation's sole model where the IDE
// reports conversation-wide).
func ParseConversation(content []byte) (*Conversation, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(content, &fields) == nil && isExtractorRecord(fields) {
		converted, err := conversationFromExtractorRecord(fields)
		if err != nil {
			return nil, fmt.Errorf("parse conversation: %w", err)
		}
		content = converted
	}
	return sessionanalysis.ParseConversation(content)
}

// ReadConversationFile reads and parses one transcript file from disk — a convenience over
// os.ReadFile + ParseConversation.
func ReadConversationFile(path string) (*Conversation, error) {
	content, err := readFileCapped(path, MaxConversationFileBytes)
	if err != nil {
		return nil, err
	}
	conv, err := ParseConversation(content)
	if err != nil {
		return nil, fmt.Errorf("parse conversation file %q: %w", path, err)
	}
	return conv, nil
}

// readFileCapped reads path into memory but never pulls in more than maxBytes: it reads at
// most maxBytes+1 (so an exactly-at-limit file is distinguishable from an over-limit one) and
// rejects the over-limit file instead of loading it whole.
func readFileCapped(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open conversation file %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	content, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read conversation file %q: %w", path, err)
	}
	if int64(len(content)) > maxBytes {
		return nil, fmt.Errorf("conversation file %q exceeds the %d-byte limit", path, maxBytes)
	}
	return content, nil
}

// ReadConversationDir reads and parses every .json transcript under root (recursively), returning the
// ones that parsed, ordered by path — the folder-reader convenience for analysing a directory of
// transcripts. A Conversation is ONE JSON object (protojson), not line-delimited, so .jsonl files are
// not matched. A file that fails to read or parse is skipped, not fatal: the good conversations are
// still returned alongside an aggregated error naming each bad file, so one malformed file never sinks
// the whole batch.
func ReadConversationDir(root string) ([]*Conversation, error) {
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".json" {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walk conversation dir %q: %w", root, err)
	}
	sort.Strings(paths)
	conversations := make([]*Conversation, 0, len(paths))
	var readErrs []error
	for _, path := range paths {
		conv, err := ReadConversationFile(path)
		if err != nil {
			readErrs = append(readErrs, err)
			continue
		}
		conversations = append(conversations, conv)
	}
	return conversations, errors.Join(readErrs...)
}
