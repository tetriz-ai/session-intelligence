// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionintelligence

import (
	"github.com/prometheus/client_golang/prometheus"
)

// unknownModelPricingTotal counts conversations that carried billable tokens
// (input, output, or cache) for a model name absent from modelPricingTable.
// Each such conversation is mispriced at the generic defaultPricing rate — alert
// on any non-zero rate so we add the exact SKU before the misprice compounds.
// The gate is any billable token, not just cache: Cursor's own models never
// carry cache tokens, so a cache-only gate missed an unrecognized Cursor
// spelling. Label cardinality is intentionally model-keyed: the metric exists to
// name the unknown SKUs; cardinality is bounded because once a model is added to
// the pricing table this counter stops firing for it.
//
// ide_type names the tool that produced the unpriceable model. It is the dimension
// that makes the unspecifiedModelLabel residue actionable: after backfillPromptModels
// resolves what it can, whatever still arrives with no model name is a client-side
// reporting gap, and this label says which client to look at rather than leaving it to
// code archaeology. Cardinality is the closed IdeType enum, as on
// activeDayMissingTimestampTotal below.
var unknownModelPricingTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "session_intelligence_unknown_model_pricing_total",
		Help: "Total conversations processed with billable tokens for a model not in modelPricingTable. Any non-zero rate means we are mispricing at the default rate — add the SKU to the table.",
	},
	[]string{"organization_id", "model", "ide_type"},
)

// modelBackfilledTotal counts conversations whose unnamed prompts had their model
// resolved from the conversation's sole model, and those where the resolution declined
// because several models were named. It is the other half of unknownModelPricingTotal:
// without it a fall in `model="<unspecified>"` is ambiguous between the resolution
// working and clients starting to report models, and an attribution that is WRONG is
// invisible — the prompt stops being unknown at exactly the moment it becomes
// confidently mispriced. outcome="ambiguous" is the volume deliberately left on
// defaultPricing.
//
// Counted per PARSE, not per conversation: ParseConversation is also called by the
// analytics detail read and a operational job backfill, so one conversation can be counted
// more than once. Read it as a rate against unknownModelPricingTotal, not as a session
// count. Cardinality is the closed IdeType enum times two outcomes.
var modelBackfilledTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "session_intelligence_model_backfilled_total",
		Help: "Conversations whose unnamed prompts were attributed to the conversation's sole model (outcome=filled), or left unattributed because several models were named (outcome=ambiguous).",
	},
	[]string{"ide_type", "outcome"},
)

const (
	backfillOutcomeFilled    = "filled"
	backfillOutcomeAmbiguous = "ambiguous"
)

// vendorFigureRejectedTotal counts vendor-supplied cost/credit figures dropped
// by usableVendorFigure because they were non-finite or too large for the micros
// representation — never the ordinary absent/zero case, which is not a fault.
//
// Steady state is flat zero: a well-behaved client cannot produce one. So any
// sustained rate means either a desktop-app regression writing a garbage figure
// (the field's sessions are then silently pricing at $0 — check the named field
// against that IDE's extractor) or someone probing the ingestion path with a
// crafted upload, which is a security escalation rather than a pricing one.
// Label cardinality is bounded: organization_id (existing convention, see
// unknownModelPricingTotal) × the fixed set of vendor figure fields.
var vendorFigureRejectedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "session_intelligence_vendor_figure_rejected_total",
		Help: "Vendor-supplied cost/credit figures rejected as non-finite or unrepresentable. Steady state is zero; any sustained rate is a client regression or a crafted upload.",
	},
	[]string{"organization_id", "field"},
)

// RegisterMetrics registers this package's parser and plan-tier metrics with reg. They are
// created unregistered — see analyze.RegisterMetrics for the public wire-once contract. The
// plan-tier counters are declared in plan_tier_map.go.
func RegisterMetrics(reg prometheus.Registerer) {
	reg.MustRegister(
		unknownModelPricingTotal,
		modelBackfilledTotal,
		vendorFigureRejectedTotal,
		unmappedSeatTierTotal,
		unmappedCursorMembershipTotal,
		cursorEnterpriseNoTeamSignalTotal,
		unmappedCursorBillingTierTotal,
		cursorTeamSeatWithoutVendorTierTotal,
		cursorTeamSeatVendorAnsweredTotal,
		overlongCursorVendorStringTotal,
		unrecognisedCursorTeamMembershipTotal,
		divergentCursorTierTotal,
		expectedCursorTierRefinementTotal,
	)
}
