// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package refllmclient

// This file centralizes the package's constant values.

// reference-llm endpoint paths.
const (
	generateContentAPI  = "/api/v1/generate-content"
	detectRoundTripsAPI = "/api/v1/detect-round-trips"
)

// Task-type identifiers, mirroring reference-llm's TASK_REGISTRY keys. Callers set these
// on a generate-content request's TaskType.
const (
	// The three session-analysis engine producers: the merged read pass, the
	// artifact/round-trip evidence pass, and the discard tail.
	TaskTypeAnalyzeRead     = "analyze_read"
	TaskTypeAnalyzeEvidence = "analyze_evidence"
	TaskTypeAnalyzeDiscard  = "analyze_discard"
)

// Transport size limits.
const (
	// DefaultMaxBody is the 10 MB response-body ceiling for every endpoint.
	DefaultMaxBody = 10 << 20
)
