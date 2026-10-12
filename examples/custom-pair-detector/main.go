// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Command custom-pair-detector shows the round-trip detection extension point. AnalyzeSession takes
// any analyze.RoundTripDetector; the bundled one calls a reference-llm ML service, but you can plug your
// own. This one scores a pair by a trivial keyword rule and returns the top-N above a threshold —
// enough to see the public interface a real detector implements. It runs with no service and no model.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tetriz-ai/session-intelligence/analyze"
)

// keywordDetector implements analyze.RoundTripDetector. It scores a pair by whether the follow-up
// prompt signals a correction ("actually", "no,", "that's wrong"), a cheap stand-in for a fitted model.
type keywordDetector struct{ threshold float64 }

func (d keywordDetector) Detect(_ context.Context, _, _ string, pairs []analyze.RoundTripPair, topN, ceiling int) (*analyze.DetectorResult, error) {
	truncated := false
	if ceiling > 0 && len(pairs) > ceiling {
		pairs = pairs[:ceiling]
		truncated = true
	}
	var candidates []analyze.DetectedPair
	for _, pair := range pairs {
		score := 0.2
		if hasCorrectionSignal(pair.Q) {
			score = 0.9
		}
		if score >= d.threshold {
			candidates = append(candidates, analyze.DetectedPair{PromptIndex: pair.PromptIndex, Score: score})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	if topN > 0 && len(candidates) > topN {
		candidates = candidates[:topN]
	}
	return &analyze.DetectorResult{Candidates: candidates, Truncated: truncated}, nil
}

func hasCorrectionSignal(q string) bool {
	q = strings.ToLower(q)
	for _, signal := range []string{"actually", "no,", "that's wrong", "not what i meant"} {
		if strings.Contains(q, signal) {
			return true
		}
	}
	return false
}

func main() {
	var detector analyze.RoundTripDetector = keywordDetector{threshold: 0.5}

	pairs := []analyze.RoundTripPair{
		{PromptIndex: 0, P: "Rename the field.", Q: "Looks good, thanks."},
		{PromptIndex: 1, P: "Add validation.", Q: "Actually, validate the other field."},
		{PromptIndex: 2, P: "Write a test.", Q: "No, that's wrong — test the error path."},
	}

	result, err := detector.Detect(context.Background(), "org-1", "session-1", pairs, 5, 100)
	if err != nil {
		panic(err)
	}
	fmt.Printf("round-trip candidates: %d (truncated=%v)\n", len(result.Candidates), result.Truncated)
	for _, candidate := range result.Candidates {
		fmt.Printf("- prompt %d: score %.1f\n", candidate.PromptIndex, candidate.Score)
	}
}
