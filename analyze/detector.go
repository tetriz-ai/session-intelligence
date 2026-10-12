// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package analyze

import "github.com/tetriz-ai/session-intelligence/internal/sessionanalysis"

// RoundTripDetector is the optional round-trip detection seam AnalyzeSession feeds E's round-trip
// half. The spine enumerates the guard-valid pairs and hands them here; the detector scores them and
// returns the top-N above-threshold candidates. Supply your own, or get the reference-llm-backed one
// from NewReferenceLLMDetector. When nil, the round-trip half simply contributes no candidates.
type RoundTripDetector = sessionanalysis.RoundTripDetector

// RoundTripPair is one guard-valid adjacent-human-turn pair the detector scores. P is the earlier
// human prompt, Q the later one, AgentResponse the assistant turns between them, Prior the human turn
// before P.
type RoundTripPair = sessionanalysis.RoundTripPair

// DetectedPair is one above-threshold candidate: P's turn index and the detector's score.
type DetectedPair = sessionanalysis.DetectedPair

// DetectorResult is one session's detector output: the scored candidates and whether the pairs
// overflowed the ceiling. Return a non-nil empty result when no pairs are supplied.
type DetectorResult = sessionanalysis.DetectorResult

// NewReferenceLLMDetector returns a RoundTripDetector backed by a reference-llm service at baseURL. The
// shipped classifier is an uncalibrated placeholder, so /detect-round-trips returns 503 unless the
// detector bundle is baked and armed; most callers either omit the detector or plug their own.
func NewReferenceLLMDetector(baseURL string) RoundTripDetector {
	return sessionanalysis.NewReferenceLLMRoundTripDetector(baseURL)
}
