// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/tetriz-ai/session-intelligence/internal/refllmclient"
	pymocks "github.com/tetriz-ai/session-intelligence/internal/refllmclient/mocks"
)

func result(index int32, score float64, isRoundTrip bool) refllmclient.RoundTripPairResult {
	return refllmclient.RoundTripPairResult{PromptIndex: index, RoundTripScore: score, IsRoundTrip: isRoundTrip}
}

// TestTopNAboveThreshold pins that only above-threshold pairs survive, highest score first,
// capped at topN — the recall-oriented candidate set E judges.
func TestTopNAboveThreshold(t *testing.T) {
	tests := []struct {
		name        string
		results     []refllmclient.RoundTripPairResult
		topN        int
		wantIndexes []int32
	}{
		{
			name: "below-threshold pairs dropped, sorted by score desc",
			results: []refllmclient.RoundTripPairResult{
				result(0, 0.9, true),
				result(1, 0.2, false), // below threshold — dropped
				result(2, 0.6, true),
				result(3, 0.7, true),
			},
			topN:        0, // no cap
			wantIndexes: []int32{0, 3, 2},
		},
		{
			name: "top-N cap keeps the highest scores",
			results: []refllmclient.RoundTripPairResult{
				result(0, 0.5, true),
				result(1, 0.9, true),
				result(2, 0.7, true),
			},
			topN:        2,
			wantIndexes: []int32{1, 2},
		},
		{
			name:        "no above-threshold pairs → empty",
			results:     []refllmclient.RoundTripPairResult{result(0, 0.1, false)},
			topN:        5,
			wantIndexes: []int32{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := topNAboveThreshold(tt.results, tt.topN)
			gotIndexes := make([]int32, 0, len(got))
			for _, candidate := range got {
				gotIndexes = append(gotIndexes, candidate.PromptIndex)
			}
			assert.Equal(t, tt.wantIndexes, gotIndexes)
		})
	}
}

func pair(index int32) refllmclient.RoundTripPair {
	return refllmclient.RoundTripPair{PromptIndex: index, P: "p", AgentResponse: "r", Q: "q"}
}

// TestPairDetectorClient_Detect_Ceiling pins that a session whose guard-valid pairs exceed the
// ceiling is scored only up to it and flagged detector-truncated (the legible degrade the same
// as D's overflow), and that only the ceiling-many pairs are sent to reference-llm.
func TestPairDetectorClient_Detect_Ceiling(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := pymocks.NewMockRoundTripDetector(ctrl)

	pairs := []refllmclient.RoundTripPair{pair(0), pair(1), pair(2), pair(3)}
	client.EXPECT().DetectRoundTrips(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, req refllmclient.DetectRoundTripsRequest) (*refllmclient.DetectRoundTripsResponse, error) {
			// Ceiling is 2, so only the first two pairs cross to the detector.
			require.Len(t, req.Pairs, 2)
			assert.Equal(t, int32(0), req.Pairs[0].PromptIndex)
			assert.Equal(t, int32(1), req.Pairs[1].PromptIndex)
			return &refllmclient.DetectRoundTripsResponse{
				Results: []refllmclient.RoundTripPairResult{result(0, 0.8, true), result(1, 0.3, false)},
			}, nil
		})

	detector := NewPairDetectorClient(client)
	got, err := detector.Detect(context.Background(), "org-1", "raw-1", pairs, 0, 2)
	require.NoError(t, err)
	assert.True(t, got.Truncated)
	require.Len(t, got.Candidates, 1)
	assert.Equal(t, int32(0), got.Candidates[0].PromptIndex)
}

// TestPairDetectorClient_Detect_AtCeiling pins the boundary: a session whose guard-valid pair
// count exactly equals the ceiling is scored in full and NOT flagged truncated — only a count
// strictly above the ceiling truncates.
func TestPairDetectorClient_Detect_AtCeiling(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := pymocks.NewMockRoundTripDetector(ctrl)

	pairs := []refllmclient.RoundTripPair{pair(0), pair(1)}
	client.EXPECT().DetectRoundTrips(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, req refllmclient.DetectRoundTripsRequest) (*refllmclient.DetectRoundTripsResponse, error) {
			require.Len(t, req.Pairs, 2, "all pairs sent when count == ceiling")
			return &refllmclient.DetectRoundTripsResponse{
				Results: []refllmclient.RoundTripPairResult{result(0, 0.8, true), result(1, 0.9, true)},
			}, nil
		})

	detector := NewPairDetectorClient(client)
	got, err := detector.Detect(context.Background(), "org-1", "raw-1", pairs, 0, 2)
	require.NoError(t, err)
	assert.False(t, got.Truncated, "count == ceiling is not a truncation")
	require.Len(t, got.Candidates, 2)
}

// TestPairDetectorClient_Detect_NoPairs pins that a session with no guard-valid pair makes no
// detector call and returns an empty, non-truncated result.
func TestPairDetectorClient_Detect_NoPairs(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := pymocks.NewMockRoundTripDetector(ctrl)
	// No DetectRoundTrips expectation — a call would fail the test.

	detector := NewPairDetectorClient(client)
	got, err := detector.Detect(context.Background(), "org-1", "raw-1", nil, 50, 1000)
	require.NoError(t, err)
	assert.False(t, got.Truncated)
	assert.Empty(t, got.Candidates)
}

// TestPairDetectorClient_Detect_TopN pins that the top-N cap is applied to the detector's
// above-threshold results.
func TestPairDetectorClient_Detect_TopN(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := pymocks.NewMockRoundTripDetector(ctrl)
	client.EXPECT().DetectRoundTrips(gomock.Any(), gomock.Any()).Return(
		&refllmclient.DetectRoundTripsResponse{
			Results: []refllmclient.RoundTripPairResult{
				result(0, 0.5, true), result(1, 0.9, true), result(2, 0.7, true),
			},
		}, nil)

	detector := NewPairDetectorClient(client)
	got, err := detector.Detect(context.Background(), "org-1", "raw-1",
		[]refllmclient.RoundTripPair{pair(0), pair(1), pair(2)}, 2, 1000)
	require.NoError(t, err)
	assert.False(t, got.Truncated)
	require.Len(t, got.Candidates, 2)
	assert.Equal(t, int32(1), got.Candidates[0].PromptIndex) // 0.9 first
	assert.Equal(t, int32(2), got.Candidates[1].PromptIndex) // 0.7 next
}
