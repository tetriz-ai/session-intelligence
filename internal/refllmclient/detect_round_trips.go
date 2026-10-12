// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package refllmclient

import (
	"context"
	"fmt"
)

// DetectRoundTripsRequest is the PAIRS-detector scoring request: the guard-valid P/A/Q
// triples the Go spine enumerated. The endpoint carries no task_type (it selects the one
// resident detector), so the request is just the pair batch.
type DetectRoundTripsRequest struct {
	Pairs []RoundTripPair `json:"pairs"`
}

// RoundTripPair is one guard-valid adjacent-human-turn pair. P is the earlier human prompt,
// Q the later one, AgentResponse the assistant turns between them, and Prior the human turn
// before P ("" for a session's first pair). Field names mirror the reference-llm wire contract
// (app/models/detect_round_trips.py) verbatim, capitals included.
type RoundTripPair struct {
	PromptIndex   int32  `json:"prompt_index"`
	P             string `json:"P"`
	AgentResponse string `json:"agent_response"`
	Q             string `json:"Q"`
	Prior         string `json:"prior"`
}

// DetectRoundTripsResponse carries the per-pair round-trip scores plus the detector's
// operating point + model provenance. Threshold is bound to the fitted classifier (in the
// versioned artifact, never runtime config), so the caller reads it rather than owning it.
type DetectRoundTripsResponse struct {
	Threshold float64               `json:"threshold"`
	Encoder   string                `json:"encoder"`
	NLIModel  string                `json:"nli_model"`
	Results   []RoundTripPairResult `json:"results"`
}

// RoundTripPairResult is one pair's score. IsRoundTrip is the detector's own
// score>=threshold verdict (a recall-oriented router signal); Features carries the 19-feature
// vector for calibration/debugging. PromptIndex echoes the request pair's P index.
type RoundTripPairResult struct {
	PromptIndex    int32              `json:"prompt_index"`
	RoundTripScore float64            `json:"round_trip_score"`
	IsRoundTrip    bool               `json:"is_round_trip"`
	Features       map[string]float64 `json:"features"`
}

// DetectRoundTrips POSTs the guard-valid pairs to the detector and returns the scored set. An
// empty pair batch is a valid no-op call the caller can skip; this returns the empty result.
// The detector is a local, token-free ML model, so — like classify-themes — it fires no
// SuccessHook (there is no per-call billed completion to emit).
func (c *Client) DetectRoundTrips(ctx context.Context, req DetectRoundTripsRequest) (*DetectRoundTripsResponse, error) {
	var response DetectRoundTripsResponse
	if _, err := c.postJSON(ctx, detectRoundTripsAPI, req, &response, DefaultMaxBody); err != nil {
		return nil, fmt.Errorf("detect-round-trips: %w", err)
	}
	return &response, nil
}
