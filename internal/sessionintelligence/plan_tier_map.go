// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionintelligence

import (
	"context"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/logger"
	"github.com/tetriz-ai/session-intelligence/internal/reqcontext"
)

// PlanTierSourceCCOauthAccount marks a plan_tier derived from the authoritative
// ~/.claude.json `oauthAccount` record (vs. the inferred cc_telemetry_inferred /
// cc_model_inferred sources). It contains no "_inferred" suffix, so
// ClassifyPlanTierOrigin classifies it as REPORTED. Exported because
// the engine gates seat-cost pricing on this authoritative source (an inferred
// tier is a guess, not a billing fact, so it must not drive a seat price).
const PlanTierSourceCCOauthAccount = "cc_oauth_account"

// PlanTierSourceCursorAPI2Dashboard marks a Cursor plan_tier the vendor itself
// reported over api2.cursor.sh, as opposed to one derived from local Statsig /
// state.vscdb signals. It carries no "_inferred" suffix, so
// ClassifyPlanTierOrigin classifies it REPORTED and lets it drive a
// seat price.
//
// It is stamped HERE, never by the writer that supplied the raw vendor values:
// provenance must follow the tier that actually won. A client that stamped it up
// front would destroy the local origin (Cursor emits several) on every row,
// including the rows whose vendor tier this mapper REJECTS — laundering an
// unknown origin into REPORTED, which passes the reported filter and earns a
// frozen price stamp.
const PlanTierSourceCursorAPI2Dashboard = "cursor_api2_dashboard"

// maxCursorVendorStringLen bounds the raw Cursor vendor strings that land in
// usage_metadata. It is headroom rather than a measurement — the longest real
// value ("TEAM_MEMBER_BILLING_TIER_TIER_1000") is 34 characters — and the
// exposure it closes is JSONB bloat from an older or modified client, which is
// why it is enforced where the value lands rather than as a proto rule (the
// message is unmarshalled from an S3 object, so no validating interceptor ever
// runs).
//
// Half of a matched pair: cursorSeatTier.ts applies the same ceiling as a cheap
// early cut. Changing this number means changing it there too.
const maxCursorVendorStringLen = 128

// Canonical plan-tier keys reused across the mapping/pricing tables below.
const (
	planTierTeamStandard     = "team_standard"
	planTierTeamUnpaidAdmin  = "team_unpaid_admin"
	planTierEnterpriseCustom = "enterprise_custom"
	planTierEnterprise       = "enterprise"
)

// cursorBillingTierToSeatTier is the exact-string allowlist from Cursor's
// per-member billingTier enum onto canonical seat tiers. Exact strings only,
// never a passthrough or a prefix test: inventing a tier from an unrecognised
// string is the one failure mode that both parses and is wrong.
//
// APPEND-ONLY. Rows persist carrying whatever Cursor called the tier when they
// were ingested, so retiring a spelling silently unprices every historical row
// holding it. A spelling is also never silently re-pointed at a different tier.
var cursorBillingTierToSeatTier = map[string]string{
	"TEAM_MEMBER_BILLING_TIER_TIER_1000": planTierTeamStandard,
	"TEAM_MEMBER_BILLING_TIER_TIER_2000": "team_premium",
}

// cursorVendorPricedTiers is the set of canonical tiers a vendor-reported
// billingTier can name — the allowlist's values, derived rather than restated so
// appending a spelling cannot leave the new tier landing on the divergence alarm.
// enterprise_custom is absent by construction: it comes from the contract
// branch, never from a billingTier.
var cursorVendorPricedTiers = func() map[string]bool {
	priced := make(map[string]bool, len(cursorBillingTierToSeatTier))
	for _, tier := range cursorBillingTierToSeatTier {
		priced[tier] = true
	}
	return priced
}()

// cursorUnconfirmedTeamTierSpellings are the tiers that mean "a paid Cursor Teams
// seat" WITHOUT naming Standard-vs-Premium, so a published price for one is the
// Standard rate on a seat that may be Premium — the doubt the interim disclosure
// exists to state. Append-only, like the allowlist: historical rows keep the
// spelling they were ingested with.
//
// Restated rather than derived, and deliberately NOT folded into
// cursorVendorPricedTiers: recordCursorTierDivergence reads that set to decide what
// the divergence alarm counts, so widening it would move the alarm's population as a
// side effect of a disclosure change.
//
// A member must be a Cursor price-table key priced at the Standard rate, or it
// downgrades nothing — the predicate is reached only for an already-priced tier.
// TestCursorUnconfirmedSpellingsArePricedAtStandard enforces that.
var cursorUnconfirmedTeamTierSpellings = map[string]bool{
	planTierEnterprise: true,
}

// cursorContractMembershipTypes is the set of teamMembershipType spellings that
// mean a negotiated contract — a subscription whose seats have no list price at
// all. Only a spelling IN this set may unprice a seat: inferring a contract from
// non-membership of the self-serve set would let a rename unprice the fleet.
//
// Half of a matched pair: cursorSeatTier.ts holds the same set to decide which
// seats it need not look a tier up for. Append-only, like the tier allowlist.
var cursorContractMembershipTypes = map[string]bool{
	"ENTERPRISE": true,
	// A negotiated org contract billed by tokens rather than list-priced seats —
	// no per-member billingTier exists for a seat under this spelling.
	"ORG_TOKEN_BASED_CONTRACT": true,
}

// cursorSelfServeMembershipTypes is the set of teamMembershipType spellings that
// mean a self-serve Teams subscription — the ones whose seats have a list price.
//
// Recognising both kinds explicitly is what makes a rename observable: a value in
// neither set is counted rather than resolved, so it reaches
// unrecognisedCursorTeamMembershipTotal instead of quietly changing how seats
// price. Append-only for the same reason as the tier allowlist.
var cursorSelfServeMembershipTypes = map[string]bool{
	"SELF_SERVE": true,
}

// unmappedSeatTierTotal counts conversations whose oauthAccount carried a
// non-empty seat_tier that MapClaudeCodeSeatTier could not classify. A non-zero
// rate is the signal that Anthropic shipped a seat-tier string we don't map yet
// — alert on it, then read the raw seat_tier from usage_metadata to learn the
// new value and extend the switch below. No label: seat_tier is open-ended
// vendor input, so labelling it would risk unbounded cardinality.
var unmappedSeatTierTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "session_intelligence_unmapped_seat_tier_total",
	Help: "Claude Code conversations with a non-empty oauthAccount seat_tier that did not map to a canonical plan tier.",
})

// unmappedCursorMembershipTotal counts Cursor conversations whose
// stripeMembershipType is a string MapCursorMembershipTier does not recognise.
// A non-zero rate is the signal that Cursor shipped a membership value we don't
// map yet — read the raw plan_tier from usage_metadata to learn the new value
// and extend the switch below. The values Cursor is known to emit (including
// the tier-less ones) are excluded, so this counter stays at zero in steady
// state. No label: the membership string is open-ended vendor input, so
// labelling it would risk unbounded cardinality.
var unmappedCursorMembershipTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "session_intelligence_unmapped_cursor_membership_total",
	Help: "Cursor conversations whose stripeMembershipType did not map to a canonical plan tier.",
})

// cursorEnterpriseNoTeamSignalTotal counts Cursor conversations reporting the
// `enterprise` membership — which Cursor emits for any paid team seat — while
// carrying no team id and no enterprise flag. Such a seat cannot be told apart
// from a negotiated contract, so it stays on its own raw `enterprise` price-table
// key, charged at the Teams Standard rate by pricing convention (it is deliberately
// not canonicalized into team_standard — see the plantier package doc);
// a Premium seat in this bucket is under-counted and a genuine
// Enterprise-Custom one is priced when it should read "—". Separate from
// unmappedCursorMembershipTotal because the membership string IS recognised —
// what is missing is the account detail that disambiguates it. A decaying rate
// is the expected shape as clients that predate the team-field capture age out;
// a flat or rising one means the extraction path is not delivering those fields.
var cursorEnterpriseNoTeamSignalTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "session_intelligence_cursor_enterprise_no_team_signal_total",
	Help: "Cursor conversations reporting enterprise membership with no team id and no enterprise flag.",
})

// unmappedCursorBillingTierTotal counts Cursor conversations carrying a
// persisted cursor_billing_tier spelling the allowlist does not recognise. The
// price-bearing field failed to resolve and the seat fell back to the local
// derivation.
//
// ZERO-BASELINE TRIPWIRE. Its response depends on the value being readable, which
// is why an over-long value is counted separately: this one's raw string IS
// persisted, so triage is "read cursor_billing_tier from usage_metadata and
// append the new spelling to the allowlist".
//
// No label: the vendor string is open-ended, so labelling it would risk unbounded
// cardinality — and an unlabelled counter cannot leak an identifier by accident.
// The same holds for every counter below.
var unmappedCursorBillingTierTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "session_intelligence_unmapped_cursor_billing_tier_total",
	Help: "Cursor conversations whose persisted billing-tier spelling is not in the allowlist. Zero-baseline tripwire — alert on any non-zero rate, then read cursor_billing_tier from usage_metadata and append the spelling.",
})

// cursorTeamSeatWithoutVendorTierTotal counts Cursor conversations where the
// vendor confirmed a NON-CONTRACT team subscription but named no per-member tier,
// so the seat falls back to the local derivation — which for a team seat can only
// ever say Standard. This is the population at risk of the Premium under-count,
// and it is the only counter that moves when the vendor's response SHAPE changes:
// a renamed or removed `billingTier` KEY yields absence, not a spelling, so
// unmappedCursorBillingTierTotal cannot see it and the desktop reports the lookup
// as a success.
//
// NOT a zero-baseline tripwire. It moves for benign reasons too — a call-2
// failure preserves the membership string from call 1, and a seat resolved before
// the lookup ran has neither field — so it is a RATE to trend and alert on as a
// share of Cursor team seats, not an any-increment page. That breadth is the
// point: it measures the impact (seats priced without a vendor tier) rather than
// one cause, so it stays true when a cause nobody predicted appears.
//
// It cannot fire while an org's gate is off: with no lookup there is no
// membership string either, so such rows never reach the branch that counts.
var cursorTeamSeatWithoutVendorTierTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "session_intelligence_cursor_team_seat_without_vendor_tier_total",
	Help: "Cursor conversations on a vendor-confirmed non-contract team subscription that carry no per-member billing tier, so the seat is priced by local derivation (Standard) and a Premium seat under-counts. Alert on the RATE as a share of Cursor team seats, not on any increment.",
})

// cursorTeamSeatVendorAnsweredTotal is the denominator for the counter above:
// Cursor conversations where the vendor confirmed a non-contract team
// subscription, whether or not it also named a tier. Contract seats are excluded
// from both, because for them a missing tier is expected and not a fault.
//
// It exists so the alert can be a SHARE rather than an absolute count — an
// absolute threshold is a proxy for org size, so it either pages a large customer
// constantly or never fires for a small one, and the number it should be set to
// is unknowable before the feature carries real traffic.
var cursorTeamSeatVendorAnsweredTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "session_intelligence_cursor_team_seat_vendor_answered_total",
	Help: "Cursor conversations where the vendor confirmed a non-contract team subscription, with or without a per-member billing tier. Denominator for cursor_team_seat_without_vendor_tier_total.",
})

// overlongCursorVendorStringTotal counts each raw Cursor vendor string refused at
// the length ceiling and dropped before persistence — one increment per string,
// so a row with two bad values reports two.
//
// ZERO-BASELINE TRIPWIRE, and deliberately NOT folded into
// unmappedCursorBillingTierTotal even though a dropped billing tier also fails to
// resolve: the drop persists neither the value nor its length, so "read the raw
// value from usage_metadata" is impossible here. The response is to triage the
// client build writing them, since no vendor value is anywhere near the ceiling.
var overlongCursorVendorStringTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "session_intelligence_overlong_cursor_vendor_string_total",
	Help: "Raw Cursor vendor strings dropped for exceeding the length ceiling. Zero-baseline tripwire — alert on any non-zero rate; the value is not persisted, so triage the client build, not usage_metadata.",
})

// unrecognisedCursorTeamMembershipTotal counts Cursor conversations whose
// vendor-reported teamMembershipType is neither a recognised self-serve nor a
// recognised contract spelling.
//
// ZERO-BASELINE TRIPWIRE, and the one that makes a Cursor rename visible instead
// of silent. An unrecognised spelling asserts nothing — it cannot unprice a seat
// — so the row itself degrades gracefully and nothing else would ever report the
// vocabulary change. Response: read cursor_team_membership_type from
// usage_metadata and append the spelling to whichever set it belongs in.
//
// Separate from unmappedCursorMembershipTotal, which names state.vscdb's
// stripeMembershipType: two vendor fields, two responses.
var unrecognisedCursorTeamMembershipTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "session_intelligence_unrecognised_cursor_team_membership_total",
	Help: "Cursor conversations whose vendor-reported teamMembershipType is in neither the self-serve nor the contract set. Zero-baseline tripwire — alert on any non-zero rate, then read cursor_team_membership_type from usage_metadata and append the spelling.",
})

// divergentCursorTierTotal counts the residue: a vendor-reported tier that
// disagrees with the local derivation in a way this design does NOT expect.
//
// ZERO-BASELINE TRIPWIRE, and the only one of these that can page. Sustained
// non-zero means one of the two derivations has drifted, and because such a
// change both parses and passes the allowlist, nothing else catches it. Paired
// with a warn line: the counter can page, the line says what disagreed.
var divergentCursorTierTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "session_intelligence_divergent_cursor_tier_total",
	Help: "Cursor conversations whose vendor-reported seat tier disagrees with the local derivation in an unexpected way. Zero-baseline tripwire — alert on any non-zero rate, then read the paired warn line for the two tiers.",
})

// expectedCursorTierRefinementTotal MEASURES THE FEATURE WORKING — it is not a
// tripwire and must never be alerted on. It counts the two corrections this
// design exists to produce, both of them a crossing of the priced/unpriced line
// in the direction where the LOCAL answer is the unreliable one: a local
// team_standard the vendor unprices (the Statsig contract flag was never set),
// and a local enterprise_custom the vendor list-prices (the local enterprise
// heuristic over-fires on genuinely self-serve teams). Either runs on every
// conversation of every affected org.
//
// Kept separate from divergentCursorTierTotal precisely so that alarm can keep a
// zero baseline instead of paging an on-call for an expected, correct refinement.
var expectedCursorTierRefinementTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "session_intelligence_expected_cursor_tier_refinement_total",
	Help: "Cursor conversations where the vendor-reported seat tier corrected the local derivation as designed, in either direction across the priced/unpriced line. Measures the feature working — expected non-zero, never alert on this.",
})

// The counters above are created unregistered and registered via RegisterMetrics
// (sessionintelligence_metrics.go), so importing this package has no global prometheus
// side effect.

// MapClaudeCodeSeatTier maps Claude Code's authoritative account signals
// (oauthAccount.seatTier / organizationType / userRateLimitTier) onto the
// canonical plan-tier strings keyed by the plan-price table
// (free | pro | max_5x | max_20x | team_standard | team_premium | enterprise —
// see ai_leverage_plan_prices.go). TestClaudeCodeSeatTierMapsToPricingKey
// this package guards that every value this returns is a key there.
//
// It returns (tier, true) only on a confident match. Anything unrecognized
// returns ("", false) so the caller keeps the desktop-derived inferred tier
// rather than regressing it to an empty/unknown value. Matching is
// substring/prefix based (like isOpusModel / copilotSkuToTier) so future tier
// suffixes (e.g. "team_tier_2") don't require code churn.
//
// userRateLimitTier is PRICE-BEARING. Anthropic's Team plan has two seat types —
// Standard (1.25x Pro, $20) and Premium (6.25x Pro ≈ Max 5x, $100) — and a
// Team seat's rate-limit tier is what distinguishes them; individual Max has
// 5x ($100) vs 20x ($200). So we check the rate-limit tier within the org
// branch (team → premium vs standard) and to split individual Max. Enterprise
// (custom price) still wins outright. NOTE: check max_20x before the broader
// max/claude_max test, since "default_claude_max_20x" contains both.
func MapClaudeCodeSeatTier(seatTier, organizationType, userRateLimitTier string) (string, bool) {
	seat := strings.ToLower(strings.TrimSpace(seatTier))
	orgType := strings.ToLower(strings.TrimSpace(organizationType))
	userRateLimit := strings.ToLower(strings.TrimSpace(userRateLimitTier))

	// "pro_or_free" / "unknown" are desktop inference artifacts that must never
	// reach the authoritative seat_tier — guard them explicitly so the
	// substring "pro" check below can't misclassify "pro_or_free" as "pro".
	if seat == "pro_or_free" || seat == toolModeUnknown {
		return "", false
	}

	isMax20x := strings.Contains(userRateLimit, "max_20x") || strings.Contains(seat, "max_20x")
	// A Max-class rate limit (5x or unspecified "claude_max") or a seat tier that
	// itself names "max". max_20x is a strict subset already captured above.
	isMaxClass := isMax20x ||
		strings.Contains(userRateLimit, "claude_max") || strings.Contains(userRateLimit, "max_5x") ||
		strings.Contains(seat, "max")

	switch {
	case strings.Contains(orgType, "enterprise"):
		return "enterprise", true
	case orgType == "claude_team" || strings.HasPrefix(seat, "team"):
		// Team seat: a Max-class rate limit means a Premium seat ($100); a
		// standard rate limit means a Standard seat ($20). Team has no separate
		// 20x SKU, so any Max-class signal maps to Premium.
		if isMaxClass {
			return "team_premium", true
		}
		return planTierTeamStandard, true
	case isMax20x:
		return "max_20x", true
	case isMaxClass:
		return "max_5x", true
	case strings.Contains(seat, "pro"):
		return "pro", true
	case strings.Contains(seat, "free"):
		return "free", true
	default:
		return "", false
	}
}

// TierOverridesSeatMonth reports whether observing tier anywhere on a seat-month
// settles that seat-month's tier outright, beating every other tier the seat
// reported there — including one on a later session date.
//
// Seat tier is normally decided by recency (the latest session date wins), which
// is correct for a plan CHANGE. It is wrong when two tiers on one seat disagree
// about the seat's contract TYPE rather than its plan level, because then only one
// of them can be true and recency does not say which. Cursor's enterprise_custom
// rests on an account flag the client reports only once it ships the team-field
// capture, so the same seat's earlier rows carry raw `enterprise` — the self-serve
// spelling at a concrete $40 — and a recency-decided FEE swings between $40 and
// unpriced on nothing but which spelling happens to land last in the window. The
// flag's absence on an earlier row is MISSING DATA, not evidence of self-serve, so
// it must never outvote a positive account fact. The label moves with it, but the
// fee is the reason this override exists: `enterprise_custom` carries no list price
// while the raw `enterprise` spelling prices at the Teams Standard rate.
//
// This decides only WHICH tier wins, never what it costs: the winner is priced
// through the same table as any other tier, so whether enterprise_custom carries a
// list price is decided in that table, not here.

// CursorTierNeedsVendorConfirmation reports whether a resolved tier's Standard-vs-
// Premium granularity is one ONLY the vendor can settle, so a price for it is a
// published rate on a guessed tier. Locally a Standard and a Premium team seat are
// indistinguishable, and both candidate spellings price at the Standard rate.
//
// True for two populations, which is why it reads two sets: the canonical team tiers
// a vendor billingTier names, and the raw spellings that mean a paid Teams seat
// without naming which one. The question is whether the GRANULARITY was confirmed,
// not whether the tier string came from the vendor — a vendor-reported `enterprise`
// is a definite label that still leaves the priced tier a guess.
//
// Individual tiers are absent by construction: the local membership string names those.
func CursorTierNeedsVendorConfirmation(ideType, tier string) bool {
	if ideType != sessionintelligencepb.IdeType_IDE_TYPE_CURSOR.String() {
		return false
	}
	return cursorVendorPricedTiers[tier] || cursorUnconfirmedTeamTierSpellings[tier]
}

// TierOverridesSeatMonth reports whether the given IDE tier overrides the default monthly seat price.
func TierOverridesSeatMonth(ideType, tier string) bool {
	if ideType != sessionintelligencepb.IdeType_IDE_TYPE_CURSOR.String() {
		return false
	}
	// enterprise_custom: a negotiated-contract account fact must not lose to a
	// later self-serve spelling. team_unpaid_admin: a $0 free-admin seat must
	// not lose to pickCurrentTier's max-price preference against team_standard
	// / team_premium on the same seat-month.
	return tier == planTierEnterpriseCustom || tier == planTierTeamUnpaidAdmin
}

// customContractTierByIDE names, per IDE, the one canonical tier that vendor sells
// only by negotiated contract — the sole tier an org's own contracted rate may
// price.
//
// Cursor's entry is `enterprise_custom`, NOT the raw `enterprise` spelling: raw
// `enterprise` is any paid Teams seat, self-serve included, and already prices at
// the Teams Standard rate. Copilot is absent because its Enterprise tier has a
// published list rate.
//
// Every tier here must be unpriced on both bases in the seat price table.
// TestCustomContractTiersAreUnpriced enforces that, and it is what makes a
// contracted rate incapable of contradicting a list price or an ingestion-time
// price stamp: those exist only for tiers the vendor publishes.
var customContractTierByIDE = map[string]string{
	sessionintelligencepb.IdeType_IDE_TYPE_CURSOR.String():      "enterprise_custom",
	sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE.String(): "enterprise",
	sessionintelligencepb.IdeType_IDE_TYPE_CODEX.String():       "enterprise",
	sessionintelligencepb.IdeType_IDE_TYPE_KIRO.String():        "enterprise",
	sessionintelligencepb.IdeType_IDE_TYPE_ANTIGRAVITY.String(): "enterprise",
}

// TierMayBeContractSeat reports whether a seat on this PAID tier could in truth be a
// seat on the vendor's negotiated contract, so a list price for it under-states what a
// contracted org actually pays.
//
// It is the raw `enterprise` spelling plus the tiers only a vendor billingTier can
// name. All three mean "a paid Cursor Teams seat" and none of them proves the
// subscription is self-serve: `enterprise` is what Cursor reports for every paid Teams
// seat, and a team tier reached without a vendor answer is a default rather than a
// statement. A client that has not shipped the enterprise-flag capture leaves a
// contract org's seats on exactly these tiers.
//
// Distinct from CursorTierNeedsVendorConfirmation, which asks whether the tier's
// Standard-vs-Premium GRANULARITY is unconfirmed. This asks whether a CONTRACT
// supersedes the price. The two questions are independent — a tier could be
// granular-certain yet contract-suspect, or the reverse — so their memberships
// coinciding over today's Cursor tiers is incidental and must not be relied on:
// keep the two sets and the two predicates separate.
func TierMayBeContractSeat(ideType, tier string) bool {
	if ideType != sessionintelligencepb.IdeType_IDE_TYPE_CURSOR.String() {
		return false
	}
	return tier == planTierEnterprise || cursorVendorPricedTiers[tier]
}

// CustomContractTier returns the tier an org's negotiated seat contract prices for
// the given IDE. The second return is false for an IDE that sells no
// contract-only tier, whose seats therefore always price at list.
func CustomContractTier(ideType string) (string, bool) {
	tier, ok := customContractTierByIDE[ideType]
	return tier, ok
}

// CursorTierSignals is every signal MapCursorMembershipTier reads about one
// Cursor seat, split by who reported it.
//
// It is a struct rather than a parameter list because two of its fields are
// different vendor vocabularies with near-identical names — TeamMembershipType
// (the API's subscription kind) and MembershipType (state.vscdb's per-seat
// string, which is not a tier at all) — and transposing them positionally would
// silently invert the precedence the mapper exists to enforce.
type CursorTierSignals struct {
	// BillingTier is Cursor's per-member billingTier, verbatim. Empty is not
	// evidence of any tier: it means no vendor lookup ran, none succeeded, or the
	// value was refused as implausible.
	BillingTier string
	// TeamMembershipType is Cursor's teamMembershipType, verbatim — the
	// subscription KIND (self-serve versus a negotiated contract), not a tier.
	// Empty is not evidence of any kind.
	TeamMembershipType string
	// MembershipType is cursorAuth/stripeMembershipType from state.vscdb. Despite
	// the name it does not name a tier for team seats: Cursor reports `enterprise`
	// for every paid Teams seat, self-serve included.
	MembershipType string
	// TeamID is the Statsig-reported team id. Non-empty is what identifies a team
	// seat locally; the membership string is not.
	TeamID string
	// IsEnterpriseUser / IsEnterpriseTrialUser are Statsig account flags. They are
	// a local heuristic for a negotiated contract, not a vendor statement of one.
	IsEnterpriseUser      bool
	IsEnterpriseTrialUser bool
	// LocalPlanTierSource is the plan_tier_source the row arrived with — which
	// local detector produced MembershipType/TeamID. Carried only so a divergence
	// line can name it; it never affects the mapping.
	LocalPlanTierSource string
	// TeamRole is cursor_team_role from the Statsig blob (or any other surface
	// that persisted it). Price-bearing: Cursor's unpaid-admin role is a $0
	// seat. Empty means no role was observed.
	TeamRole string
}

// restampMutedKey marks a context in which MapCursorMembershipTier must NOT touch
// its ingestion metrics.
type restampMutedKey struct{}

// WithRestampMuted marks ctx so MapCursorMembershipTier re-derives a tier WITHOUT
// incrementing its ingestion counters or emitting its divergence alarm. Those
// signals mean "new vendor vocabulary / a vendor-vs-local split seen at INGESTION";
// a re-stamp re-deriving an already-stored row is neither new nor ingestion, so it
// must be silent. Only the seat-price re-stamp sets this — the ingestion path never
// does, so its counting behaviour is unchanged.
func WithRestampMuted(ctx context.Context) context.Context {
	return context.WithValue(ctx, restampMutedKey{}, true)
}

func ingestionMetricsMuted(ctx context.Context) bool {
	muted, _ := ctx.Value(restampMutedKey{}).(bool)
	return muted
}

// MapCursorMembershipTier maps Cursor's account signals onto the canonical
// plan-tier strings keyed by the Cursor price table (individual_free |
// individual_pro | individual_pro_plus | individual_ultra | team_standard |
// team_premium | team_unpaid_admin | enterprise_custom — see plan_prices.go).
// TestCursorMembershipMapsToPricingKey guards that every value this returns is a
// key there, and TestEmittableCursorTiersRoundTripThroughPlantier that each one
// is already the canonical spelling of its equivalence class.
//
// It returns (tier, source, true) only on a confident match. source is
// PlanTierSourceCursorAPI2Dashboard when a VENDOR-reported value decided the
// tier and "" when the local signals did, so a caller stamps provenance by
// following what actually won rather than deciding again. Anything unconfident
// returns ("", "", false) so the caller keeps the raw membership string, which
// the price table's legacy aliases still price.
//
// The LOCAL precedence exists because the membership string is not a tier: Cursor
// reports `enterprise` for every paid Teams seat, self-serve included. So among the
// local signals an enterprise flag wins (a confirmed account fact, not the ambiguous
// self-serve spelling); failing that, a non-empty teamID — not the membership string
// — is what identifies a team seat; only a seat with neither is an individual
// subscription, and only then does the membership string name the tier. A
// VENDOR-reported tier outranks all three — see the order below, which is the
// function's actual precedence.
//
// Precedence, most authoritative first:
//
// A recognised unpaid-admin TeamRole wins outright. Cursor's Unpaid Admin seat is
// free ($0); the role is a seat-TYPE fact, not a billing-tier fact, and a free
// admin can still arrive with a vendor BillingTier of TIER_1000 on the wire
// (see the seam fixture that pins). Letting BillingTier decide would keep
// charging $40 for a seat that does not exist. Provenance stays local: the role
// is read from Statsig / persisted cursor_team_role, never from the vendor API
// (design D8 / D15).
//
// A RECOGNISED contract TeamMembershipType wins outright, beside a team id. The
// vendor has stated the subscription is not list-priced, so any tier on the same
// row is a figure that seat does not pay: the unpriced enterprise_custom beats
// confidently reporting $40 (or $120) for a seat we have been told is neither.
//
// Failing that, a recognised BillingTier decides — it is the only signal that can
// tell Teams Standard from Teams Premium, which is indistinguishable in every
// signal Cursor writes to disk.
//
// An UNRECOGNISED membership spelling sits BELOW the tier branch and asserts
// nothing, which is the load-bearing half of the ordering. Were it read as a
// contract, a vendor rename of SELF_SERVE would unprice every self-serve team
// fleet-wide while a perfectly good BillingTier sat unused on the row — and
// because enterprise_custom is a TierOverridesSeatMonth tier, ONE such row
// settles the whole seat-month, so no later correct row could rescue it. So a
// rename costs nothing beyond a tripwire increment until the sets learn it.
//
// TeamMembershipType is read only beside a non-empty TeamID: it names the kind of
// a TEAM subscription, so without a team there is no subscription for it to
// describe.
//
// Below the vendor branches sit the local signals unchanged: an enterprise flag,
// then a non-empty TeamID (always team_standard, since nothing local refines it),
// then the raw membership string for a seat with neither.
func MapCursorMembershipTier(ctx context.Context, s CursorTierSignals) (string, string, bool) {
	muted := ingestionMetricsMuted(ctx)

	if isCursorUnpaidAdminRole(s.TeamRole) {
		return planTierTeamUnpaidAdmin, "", true
	}

	localTier, localOK := cursorLocalSignalTier(s)

	if vendorTier, ok := cursorVendorReportedTier(ctx, s); ok {
		if localOK && localTier != vendorTier {
			recordCursorTierDivergence(ctx, localTier, vendorTier, s.LocalPlanTierSource)
		}
		return vendorTier, PlanTierSourceCursorAPI2Dashboard, true
	}
	if localOK {
		return localTier, "", true
	}

	// No signal named a tier. Count the vocabulary gap that explains why, so a new
	// Cursor membership string stays discoverable — but only on this path: a row
	// the vendor resolved paid no price for the gap. A re-stamp re-deriving a stored
	// row (muted) makes no such observation, so it must not count.
	switch strings.ToLower(strings.TrimSpace(s.MembershipType)) {
	case "enterprise":
		// A paid team seat whose team/enterprise facts never arrived. Canonicalizing
		// it would assert a Standard-vs-Custom distinction the input does not carry,
		// so it stays raw and prices through its own price-table key. Counted
		// separately — the vocabulary is recognised, the disambiguating detail is
		// what is missing.
		if !muted {
			cursorEnterpriseNoTeamSignalTotal.Inc()
		}
	case "", toolModeUnknown:
		// No membership signal at all, so nothing to name and nothing to count as
		// new vocabulary.
	default:
		if !muted {
			unmappedCursorMembershipTotal.Inc()
		}
	}
	return "", "", false
}

// cursorVendorReportedTier resolves the vendor-reported half of the precedence.
//
// Both unrecognised-vocabulary paths are counted and then abandoned rather than
// guessed at, and neither suppresses the other: a BillingTier and a
// TeamMembershipType are independent vendor statements, so a row can report a new
// spelling of one while the other still resolves.
//
// A recognised contract spelling returns before the tier is even examined, so an
// unrecognised tier alongside one goes uncounted. That is the right trade: the
// only population it silences is seats whose price we have just been told we
// cannot know, and forcing the count would mean resolving the tier we are about
// to discard.
func cursorVendorReportedTier(ctx context.Context, s CursorTierSignals) (string, bool) {
	muted := ingestionMetricsMuted(ctx)
	// Only meaningful beside a team id — see MapCursorMembershipTier's doc.
	membership := ""
	if strings.TrimSpace(s.TeamID) != "" {
		membership = strings.TrimSpace(s.TeamMembershipType)
	}
	// enterprise_custom, NOT the raw `enterprise` key: those are two distinct
	// price-table entries and `enterprise` is a $40 legacy alias, so the wrong one
	// yields a plausible-looking number instead of an em-dash.
	if cursorContractMembershipTypes[membership] {
		return planTierEnterpriseCustom, true
	}
	if !muted && membership != "" && !cursorSelfServeMembershipTypes[membership] {
		unrecognisedCursorTeamMembershipTotal.Inc()
	}
	if !muted && membership != "" {
		cursorTeamSeatVendorAnsweredTotal.Inc()
	}
	if tier := strings.TrimSpace(s.BillingTier); tier != "" {
		if mapped, ok := cursorBillingTierToSeatTier[tier]; ok {
			return mapped, true
		}
		if !muted {
			unmappedCursorBillingTierTotal.Inc()
		}
	} else if !muted && membership != "" {
		// The vendor confirmed a non-contract team subscription and named no tier.
		// Counted here rather than at the caller because this is the only point that
		// knows both facts, and it is the sole signal for a response whose
		// `billingTier` KEY changed: absence carries no spelling to reject, and the
		// desktop scores such a lookup a success.
		cursorTeamSeatWithoutVendorTierTotal.Inc()
	}
	return "", false
}

// cursorLocalSignalTier resolves the local-signal half of the precedence — the
// Statsig flags, the team id, then the raw state.vscdb membership string.
//
// Pure: the counters that explain a non-match are incremented by the one caller
// that actually falls back to this result, so a vendor-resolved row does not
// report a local vocabulary gap that cost it nothing.
func cursorLocalSignalTier(s CursorTierSignals) (string, bool) {
	if s.IsEnterpriseUser || s.IsEnterpriseTrialUser {
		return planTierEnterpriseCustom, true
	}
	if strings.TrimSpace(s.TeamID) != "" {
		return planTierTeamStandard, true
	}
	switch strings.ToLower(strings.TrimSpace(s.MembershipType)) {
	case "free":
		return "individual_free", true
	case "pro":
		return "individual_pro", true
	case "pro_plus":
		return "individual_pro_plus", true
	case "ultra":
		return "individual_ultra", true
	default:
		return "", false
	}
}

// isCursorUnpaidAdminRole reports whether role names Cursor's free Unpaid Admin
// seat. Two vocabularies exist for the same concept: the Statsig blob uses a
// lowercase short form, the DashboardService API uses the full uppercase enum
// name (TEAM_ROLE_*). Match case-insensitively and accept both shapes — the
// unpaid-admin spelling has not been seen on a live free-admin install, so
// guessing one casing would silently miss.
//
// TEAM_ROLE_REMOVED is deliberately NOT matched here: volume is unknown and a
// removed member is a separate decision from a free admin.
func isCursorUnpaidAdminRole(role string) bool {
	r := strings.ToLower(strings.TrimSpace(role))
	r = strings.TrimPrefix(r, "team_role_")
	switch r {
	case "free_owner", "freeowner":
		return true
	default:
		return false
	}
}

// recordCursorTierDivergence splits a vendor/local disagreement by whether this
// design expects it, because "any disagreement" is far too wide to alarm on — it
// includes the case the vendor branch exists to produce, and alarming there would
// page an on-call because the feature worked.
//
// A local team_standard refined to team_premium is not even counted: it is the
// primary effect, on every conversation of every Premium seat. Both crossings of
// the priced/unpriced line are MEASURED, not alarmed, because in both the local
// side is the unreliable one and the vendor lookup exists precisely to correct it:
// a local team_standard the vendor unprices (the Statsig contract flag was never
// set), and a local enterprise_custom the vendor list-prices (the local enterprise
// heuristic over-fires on genuinely self-serve teams). Alarming on the second
// would put ordinary vendor-confirmed traffic on a zero-baseline alarm, which
// trains an on-call to ignore it and costs more than the case it would catch.
//
// Everything else is the residue — an individual-tier local answer against a
// vendor team tier, say — where neither side has a known-weaker claim: it
// increments the alarm and emits the triage line.
//
// The line carries no person: no seat id, no user id, no email, no vendor
// response content — including none the logger would attach from ctx, which is why
// the person id is cleared rather than merely omitted here. That is not reduced
// fidelity: every row persists both the raw vendor tier and the derived one, so
// the affected seats are a tenant-scoped query away in a surface that already has
// access control, and putting a person in a log line to save that query would move
// identity into the least access-controlled place we have.
func recordCursorTierDivergence(ctx context.Context, localTier, vendorTier, localPlanTierSource string) {
	// A re-stamp re-deriving a stored row observes no live divergence — stay silent.
	if ingestionMetricsMuted(ctx) {
		return
	}
	switch {
	case localTier == planTierTeamStandard && vendorTier == "team_premium":
		// The refinement the feature exists to produce, not a disagreement.
	case localTier == planTierTeamStandard && vendorTier == planTierEnterpriseCustom,
		localTier == planTierEnterpriseCustom && cursorVendorPricedTiers[vendorTier]:
		expectedCursorTierRefinementTotal.Inc()
	default:
		divergentCursorTierTotal.Inc()
		logger.WarnWithCtx(reqcontext.WithPersonID(ctx, ""), "cursor_vendor_tier_diverges_from_local_derivation",
			zap.String("local_plan_tier", localTier),
			zap.String("vendor_plan_tier", vendorTier),
			zap.String("plan_tier_source", localPlanTierSource),
		)
	}
}

// acceptCursorVendorString bounds a raw Cursor vendor string at maxCursorVendorStringLen,
// returning ("", false) for an over-long one.
//
// It DROPS rather than truncates: a truncated string would contradict the field's
// persisted-verbatim contract and would later read as raw evidence to anything
// re-deriving a tier from it. Nothing is lost — an over-long value cannot resolve
// to a tier anyway.
func acceptCursorVendorString(value string) (string, bool) {
	if len(value) > maxCursorVendorStringLen {
		return "", false
	}
	return value, true
}
