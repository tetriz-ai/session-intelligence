// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"fmt"
	"sort"

	"github.com/tetriz-ai/session-intelligence/internal/refllmclient"
)

//go:generate go tool mockgen -source=detector.go -destination=mocks/mock_detector.go -package=mocks

// PairDetector is the seam the batch driver calls to score a session's guard-valid round-trip pairs
// . The Go spine enumerates + guards the pairs
// (enumerateGuardValidPairs) and hands the valid set here; the detector caps at the ceiling,
// scores via reference-llm, and returns the recall-oriented above-threshold candidates E's
// round-trip half consumes. A mock stands in for orchestration tests — the interface is
// exported-typed (no spineResult) so it mocks cleanly.
type PairDetector interface {
	// Detect scores the guard-valid pairs (up to the ceiling) and returns the top-N
	// above-threshold candidates by descending score. It returns an empty (non-nil) result when
	// no pairs are supplied. topN/ceiling are read per call (dynamic config). Concurrency across
	// a batch's records is bounded by the caller (a per-batch semaphore in the batch driver), not
	// here, so a single Detect call is self-contained. orgID labels the metrics; rawRecordID
	// names the error.
	Detect(ctx context.Context, orgID, rawRecordID string, pairs []refllmclient.RoundTripPair, topN, ceiling int) (*DetectorResult, error)
}

// DetectedPair is one above-threshold round-trip candidate the detector routes to E. PromptIndex
// is P's raw whole-session turn ordinal (the earlier prompt of the pair); Score is the detector's
// round-trip probability.
type DetectedPair struct {
	PromptIndex int32
	Score       float64
}

// DetectorResult is one session's detector output: the scored top-N candidates and whether the
// session's guard-valid pairs overflowed the ceiling (scored only up to it). Truncated is the
// legible degrade the same as D's overflow.
type DetectorResult struct {
	Candidates []DetectedPair
	Truncated  bool
}

// PairDetectorClient is the concrete PairDetector over the shared reference-llm transport. It owns
// the ceiling → score → top-N pipeline; only the transformer scoring lives in reference-llm. It is
// stateless and shared across a batch's concurrent records; the per-batch concurrency bound on
// those calls is held by the caller (the batch driver), not here.
type PairDetectorClient struct {
	client refllmclient.RoundTripDetector
}

// NewPairDetectorClient builds the detector client over the shared reference-llm round-trip surface.
func NewPairDetectorClient(client refllmclient.RoundTripDetector) *PairDetectorClient {
	return &PairDetectorClient{client: client}
}

// Detect scores the guard-valid pairs (capped at the pair ceiling, flagging a truncated
// session) and returns the top-N above-threshold candidates by descending score. The scored
// volume and a truncation are counted (metrics.go); the per-reason guard drops are counted by
// the caller at enumeration time. Concurrency across a batch is bounded by the caller.
func (d *PairDetectorClient) Detect(ctx context.Context, orgID, rawRecordID string, pairs []refllmclient.RoundTripPair, topN, ceiling int) (*DetectorResult, error) {
	result := &DetectorResult{}
	if ceiling > 0 && len(pairs) > ceiling {
		// A pathological whale session is scored only up to the ceiling and flagged — the same
		// legible degrade as D's overflow.
		pairs = pairs[:ceiling]
		result.Truncated = true
		roundTripSessionsTruncatedTotal.WithLabelValues(orgID).Inc()
	}
	if len(pairs) == 0 {
		return result, nil
	}
	roundTripPairsScoredTotal.WithLabelValues(orgID).Add(float64(len(pairs)))

	resp, err := d.client.DetectRoundTrips(ctx, refllmclient.DetectRoundTripsRequest{Pairs: pairs})
	if err != nil {
		return nil, fmt.Errorf("detect round trips for raw record %s (org %s): %w", rawRecordID, orgID, err)
	}
	result.Candidates = topNAboveThreshold(resp.Results, topN)
	return result, nil
}

// topNAboveThreshold keeps the detector's above-threshold pairs, highest score first, capped at
// topN — the recall-oriented candidate set E judges. A non-positive topN keeps every
// above-threshold pair (no cap).
func topNAboveThreshold(results []refllmclient.RoundTripPairResult, topN int) []DetectedPair {
	candidates := make([]DetectedPair, 0, len(results))
	for _, result := range results {
		if result.IsRoundTrip {
			candidates = append(candidates, DetectedPair{PromptIndex: result.PromptIndex, Score: result.RoundTripScore})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	if topN > 0 && len(candidates) > topN {
		candidates = candidates[:topN]
	}
	return candidates
}

// recordGuardDrops increments the per-reason guard-drop counters for one session.
func recordGuardDrops(orgID string, drops guardDrops) {
	inc := func(reason string, count int) {
		if count > 0 {
			roundTripPairsDroppedTotal.WithLabelValues(orgID, reason).Add(float64(count))
		}
	}
	inc(dropReasonNoAgentResponse, drops.noAgentResponse)
	inc(dropReasonPEmpty, drops.pEmpty)
	inc(dropReasonQEmpty, drops.qEmpty)
	inc(dropReasonQIdenticalToP, drops.qIdenticalToP)
	inc(dropReasonDuplicatePair, drops.duplicatePair)
}

var _ PairDetector = (*PairDetectorClient)(nil)
