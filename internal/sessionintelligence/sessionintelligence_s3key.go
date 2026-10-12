// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionintelligence

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// S3KeyParts holds the parsed components of an S3 key.
type S3KeyParts struct {
	OrgID          string
	PersonID       string
	ConversationID string
	StartedAt      time.Time
}

const s3KeyPrefix = "session/conversations/"

// BuildS3Key constructs an S3 key from its components.
// Output format: session/conversations/<org_id>/<person_id>/<started_at>_<conversation_id>.jsonl
func BuildS3Key(orgID, personID, conversationID string, startedAt time.Time) string {
	return fmt.Sprintf("%s%s/%s/%s_%s.jsonl",
		s3KeyPrefix, orgID, personID,
		startedAt.UTC().Format("20060102T150405"), conversationID,
	)
}

// ParseS3Key parses the S3 key to extract org_id, person_id, conversation_id,
// and started_at.
//
// Expected format: session/conversations/<org_id>/<person_id>/<started_at>_<client_id>.jsonl
// Example: session/conversations/org_abc/person_123/20260223T110000_conv_456.jsonl
func ParseS3Key(s3Key string) (*S3KeyParts, error) {
	s3Key = strings.TrimPrefix(s3Key, s3KeyPrefix)
	parts := strings.SplitN(s3Key, "/", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid s3 key format: expected <org>/<person>/<filename>, got %q", s3Key)
	}

	orgID := parts[0]
	personID := parts[1]
	filename := parts[2]

	// Remove .jsonl extension.
	base := strings.TrimSuffix(path.Base(filename), ".jsonl")
	if base == filename {
		return nil, fmt.Errorf("invalid s3 key: filename must end with .jsonl, got %q", filename)
	}

	// Split on first underscore: <started_at>_<client_id>
	idx := strings.Index(base, "_")
	if idx < 0 {
		return nil, fmt.Errorf("invalid s3 key filename: expected <started_at>_<client_id>, got %q", base)
	}

	startedAtStr := base[:idx]
	conversationID := base[idx+1:]

	startedAt, err := time.Parse("20060102T150405", startedAtStr)
	if err != nil {
		return nil, fmt.Errorf("invalid started_at in s3 key %q: %w", startedAtStr, err)
	}

	return &S3KeyParts{
		OrgID:          orgID,
		PersonID:       personID,
		ConversationID: conversationID,
		StartedAt:      startedAt,
	}, nil
}
