// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionintelligence

import "strings"

// Plan-tier provenance buckets returned by ClassifyPlanTierOrigin.
const (
	PlanTierOriginReported    = "REPORTED"
	PlanTierOriginInferred    = "INFERRED"
	PlanTierOriginUnspecified = "UNSPECIFIED"
)

// ClassifyPlanTierOrigin maps a raw plan_tier_source extractor name (from
// usage_metadata.planTierSource on session_insights) to one of REPORTED,
// INFERRED, or UNSPECIFIED. Substring rules below match every value the client
// extractors emit.
//
// It lives here so both readers share one classifier: the seat-cost allocator
// (which drops INFERRED tiers from the money math) and the engine's badging.
//
// New extractor names that don't match the _inferred / detection-failure suffix
// conventions default to REPORTED. We'd rather under-badge than over-badge —
// surfacing an unbadged tier for a brand-new extractor is safer than slapping a
// false "(inferred)" on a value we don't understand.
//
// The REPORTED case is mirrored in SQL by dao.reportedPlanTierSourceSQL (the dao
// package can't import this one); keep the marker list in sync — dao's
// TestWithFoldedPlanTier exercises every marker to catch drift.
func ClassifyPlanTierOrigin(raw string) string {
	s := strings.ToLower(raw)
	switch {
	case s == "" || s == "unknown",
		strings.Contains(s, "_missing"),
		strings.Contains(s, "_parse_error"),
		strings.Contains(s, "_no_claims"),
		strings.Contains(s, "_runtime_error"),
		strings.Contains(s, "_unknown"):
		return PlanTierOriginUnspecified
	case strings.Contains(s, "_inferred"):
		return PlanTierOriginInferred
	default:
		return PlanTierOriginReported
	}
}
