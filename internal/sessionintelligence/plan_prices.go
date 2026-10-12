// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionintelligence

// AI coding-assistant seat list prices (USD/seat/month).
//
// Lives in this package (alongside MapClaudeCodeSeatTier) so
// the parser can resolve and STAMP a conversation's seat_monthly_price_usd at
// ingestion (point-in-time pricing) without an import cycle —
// the engine imports this package, not the reverse. The engine's read-time
// consumers (seat-allocation, plan-tier table) call LookupSeatMonthlyPriceUSD
// here too, as the fallback for rows ingested before the stamp existed.
//
// Values are published list prices sourced from each vendor's pricing page.
// Each vendor block carries its own verification date.
// Enterprise tiers are usually custom contracts — represented as an unpriced
// entry so callers render an em-dash rather than a misleading zero.
//
// A tier carries BOTH published bases wherever both are known, because vendors
// discount annual billing materially — Cursor by a flat 20% on every tier — and
// the same seat is a different number depending on how the customer bought it.
// One number cannot serve both, and a table that holds one number per tier
// silently means a different thing for each vendor that priced it differently.
//
// A caller that knows the seat's basis passes it and gets that rate. Nothing
// exposes the basis today — no vendor writes it to any file the desktop agent
// reads, and only Cursor exposes it over an API at all — so every
// production lookup arrives with BillingPeriodUnknown and takes the owning
// vendor's declared defaultBasis.
//
// defaultBasis is per vendor, not global, because these tables were written on
// different bases: Claude Code's figures are annual rates, every other vendor's
// are monthly. Recording that per vendor makes it auditable and keeps a price
// correction a deliberate, isolated change to one field.
//
// Keys are normalized:
// - ideType: the sessionintelligencepb.IdeType proto string (e.g., "IDE_TYPE_CURSOR").
// - planTier: the canonical tier — for Claude Code and Cursor the values
// emitted by MapClaudeCodeSeatTier / MapCursorMembershipTier (the
// *MapsToPricingKey tests guard that every mapper output is a key here);
// for other IDEs the desktop plan-tier detector's lowercase strings.
//
// Lookup semantics (see LookupSeatPriceUSD): unknown ideType, unknown planTier,
// or a recognised pair with no published price on any basis all return
// (0, false) = "price unavailable"; a concrete price returns (price, true).
var planTierSeatPriceUSD = map[string]vendorSeatPrices{
	// Anthropic Claude Code — https://claude.com/pricing (verified 2026-06).
	// userRateLimitTier is price-bearing: a claude_team seat on a Max-class rate
	// limit is a Team *Premium* seat, not Standard. Keys are the canonical tiers
	// emitted by MapClaudeCodeSeatTier (plan_tier_map.go).
	"IDE_TYPE_CLAUDE_CODE": {defaultBasis: BillingPeriodAnnual, tiers: map[string]seatPrice{
		"free":          flat(0),
		"pro":           priced(17, 20),
		"max_5x":        monthlyOnly(100),
		"max_20x":       monthlyOnly(200),
		"team_standard": priced(20, 25),   // 1.25x Pro.
		"team_premium":  priced(100, 125), // 6.25x Pro ≈ Max 5x.
		"enterprise":    unpriced,         // Custom contract.
		// Legacy keys emitted by the pre-rate-limit-aware mapper. The
		// dashboard read path no longer resolves these directly — it canonicalizes
		// planTier first (see the plantier package), so a persisted "team"/"max"/
		// "business" row prices via its canonical key. They stay load-bearing
		// anyway: an equivalence class must be price-identical to its canonical
		// twin, which TestCanonicalTiersArePricePreserving enforces against these
		// very entries, and the ingestion-time price stamp still resolves raw
		// values. Legacy "team" can't tell Premium → priced as Standard; "max" →
		// Max 5x; "business" was the team alias.
		"team":        priced(20, 25),
		"max":         monthlyOnly(100),
		"business":    priced(20, 25),
		"pro_or_free": unpriced,
		"unknown":     unpriced,
	}},
	// Cursor — https://cursor.com, verified 2026-07-28. Canonical keys are the
	// tiers MapCursorMembershipTier emits (plan_tier_map.go); below them are the
	// raw `cursorAuth/stripeMembershipType` strings kept as legacy aliases.
	// Annual is a flat 20% off monthly on every tier.
	//
	// The raw keys are load-bearing, not cosmetic. A client build reporting no
	// team/enterprise signal leaves planTier on Cursor's raw membership string
	// (MapCursorMembershipTier declines to map it, so raw values keep arriving
	// indefinitely), and both the ingestion-time price stamp and the read-time
	// fallback resolve from that raw value — drop a key and those rows go unpriced.
	//
	// `free` and `pro` are additionally registered as equivalence-class aliases of
	// their canonical twins (see the plantier package), so a persisted `free`/`pro`
	// row is canonicalized before the read-time lookup and prices via its twin. That
	// makes their price-identity to those twins a requirement, not a coincidence —
	// TestCanonicalTiersArePricePreserving enforces it. Raw `enterprise` is NOT such
	// an alias (see below), so it prices through this key directly.
	//
	// `enterprise` and `enterprise_custom` are deliberately distinct keys, because the
	// raw string does not mean what it says: Cursor reports `stripeMembershipType =
	// enterprise` for ANY paid Teams seat, self-serve Standard included, and pairs it
	// with teamMembershipType "SELF_SERVE". So the raw `enterprise` key prices at the
	// Teams Standard rate by pricing convention — what such a row overwhelmingly is,
	// and NOT because it is canonicalized into team_standard — while the canonical
	// `enterprise_custom`, emitted only when an enterprise flag is set, stays
	// **unpriced**: the vendor has told us that subscription is not list-priced, so
	// any list figure on it is one the seat does not pay. A disclosed floor beats a
	// confident wrong number, which is the trade the owning decision accepts (D4).
	// Accepted consequence: a genuine Enterprise-Custom org's rows that carry no
	// enterprise flag price as Standard rather than rendering "—".
	"IDE_TYPE_CURSOR": {defaultBasis: BillingPeriodMonthly, tiers: map[string]seatPrice{
		"individual_free":     flat(0),
		"individual_pro":      priced(16, 20),
		"individual_pro_plus": priced(48, 60),
		"individual_ultra":    priced(160, 200),
		"team_standard":       priced(32, 40),
		"team_premium":        priced(96, 120),
		// Unpaid Admin — Cursor's free team seat for IT/finance admins who manage
		// the team without product access. A known $0, not unpriced: conflating
		// the two would pull the seat out of PlanBasedCost and can drop the org
		// from ROI calibration over a price we know exactly.
		"team_unpaid_admin": flat(0),
		"enterprise_custom": unpriced, // Negotiated contract, no list price.
		"free":              flat(0),  // Legacy raw alias.
		"pro":               priced(16, 20),
		"enterprise":        priced(32, 40), // Legacy raw key, NOT a class alias — any paid Teams seat; see above.
		"unknown":           unpriced,
		// No alias for pro_plus / ultra / business: a fleet-wide scan of every
		// stored Cursor tier returned only free, pro, enterprise and empty, so no
		// row resolves through them. The mapper still canonicalizes pro_plus and
		// ultra if Cursor starts reporting them.
	}},
	// GitHub Copilot — https://github.com/features/copilot/plans. Verified
	// 2026-04-24. "Copilot Individual" was retired and rebranded to
	// "Copilot Pro" — our "pro" key already reflects that. The desktop detector's
	// copilotSkuToTier collapses both Business and Enterprise SKUs onto "team", so
	// "team" is the key paid Copilot seats actually arrive under; business/
	// enterprise are kept for legacy/stamped rows and the Enterprise price.
	// Annual rates are unverified, so these are recorded monthly-only.
	"IDE_TYPE_COPILOT": {defaultBasis: BillingPeriodMonthly, tiers: map[string]seatPrice{
		"free":       flat(0),
		"pro":        monthlyOnly(10), // ex-Individual.
		"pro_plus":   monthlyOnly(39),
		"team":       monthlyOnly(19), // Detector key for Business/Enterprise; priced as Business (the dominant paid tier — Enterprise seats under-count by $20 until the detector distinguishes them,).
		"business":   monthlyOnly(19),
		"enterprise": monthlyOnly(39), // Excludes the $21/seat GHEC base.
		"unknown":    unpriced,
	}},
	// OpenAI Codex — https://developers.openai.com/codex/pricing (Codex is bundled
	// into ChatGPT plans; we price the seat by the plan that unlocks it). Keys are
	// the tiers the desktop detector's tierMap emits: plus/pro → "pro",
	// team/business → "team", "go", "free", "enterprise" (readCodexPlanTier).
	// API-key Codex is token-billed with no seat, so it reports tier "unknown"
	// (unpriced). Verified 2026-07-08.
	"IDE_TYPE_CODEX": {defaultBasis: BillingPeriodMonthly, tiers: map[string]seatPrice{
		"free":       flat(0),
		"go":         monthlyOnly(8),  // ChatGPT Go.
		"pro":        monthlyOnly(20), // ChatGPT Plus (the detector collapses Plus and the $100 Pro plan onto "pro"; priced at Plus).
		"team":       priced(20, 25),  // ChatGPT Business/Team.
		"business":   priced(20, 25),  // Legacy key; the detector now emits "team" for Business.
		"enterprise": unpriced,        // Custom contract.
		"unknown":    unpriced,
	}},
	// AWS Kiro — https://kiro.dev/pricing. Keys are the tiers mapKiroProfileToTier
	// emits from the Q Developer profile (free / pro / pro_plus / pro_power /
	// enterprise), plus "free_trial" which the detector emits (REPORTED, via
	// kiro_cli_profile) for a "free" profile on an active trial. Verified 2026-07-08.
	"IDE_TYPE_KIRO": {defaultBasis: BillingPeriodMonthly, tiers: map[string]seatPrice{
		"free":       flat(0),
		"free_trial": flat(0),          // Active free trial — no charge, but a known (not unpriced) seat.
		"pro":        monthlyOnly(20),  // Kiro Pro.
		"pro_plus":   monthlyOnly(40),  // Kiro Pro+.
		"pro_power":  monthlyOnly(200), // Kiro Power.
		"enterprise": unpriced,         // AWS-provisioned / custom contract.
		"unknown":    unpriced,
	}},
	// Google Antigravity — https://antigravity.google/ (verified 2026-07-08:
	// still gated / preview, no public pricing page).
	"IDE_TYPE_ANTIGRAVITY": {defaultBasis: BillingPeriodMonthly, tiers: map[string]seatPrice{
		"free":       flat(0),
		"pro":        unpriced,
		"business":   unpriced,
		"enterprise": unpriced,
		"unknown":    unpriced,
	}},
}

// BillingPeriod is the basis a seat was purchased on. Determining it is the
// point: a caller that knows passes it and gets that vendor's published rate
// for that basis. No source exposes it yet, so production passes
// BillingPeriodUnknown and takes the monthly default —.
type BillingPeriod int

// Billing periods.
const (
	// BillingPeriodUnknown is the label for an unrecognized billing period.
	BillingPeriodUnknown BillingPeriod = iota
	BillingPeriodAnnual
	BillingPeriodMonthly
)

// vendorSeatPrices is one vendor's tier table plus the basis to assume when the
// caller cannot determine the seat's own. Both are properties of this table, not
// claims about the vendor: defaultBasis records which basis these figures were
// sourced on, so a lookup that cannot know better returns what this table has
// always returned.
type vendorSeatPrices struct {
	defaultBasis BillingPeriod
	tiers        map[string]seatPrice
}

// kiroCreditOverageUSD converts one Kiro credit to USD — Kiro meters in credits,
// not tokens, so a rate is the only way to price it. It values usage at
// API-equivalent rates and says nothing about the org's bill (that is the seat
// price, resolved separately), so it applies on every tier, free included.
//
// 0.04 is the MARGINAL rate GetUsageLimitsCommand reports, identical on KIRO PRO
// and KIRO FREE, so a credits-derived cost is a floor — inside the plan allowance
// the effective rate is higher. The vendor reports the rate per account and we do
// not capture it; allocation is unaffected either way, since a uniform
// scale cancels within a seat-month.
const kiroCreditOverageUSD = 0.04

// seatPrice holds a tier's published per-month rate on each billing basis. A
// nil field means that basis has no published rate — either the tier is sold
// only on the other one, or (both nil) it has no list price at all.
type seatPrice struct {
	annual  *float64
	monthly *float64
}

// resolve returns the per-month rate for the requested basis, falling back as
// the package doc describes.
func (p seatPrice) resolve(period BillingPeriod) (float64, bool) {
	switch period {
	case BillingPeriodAnnual:
		if p.annual != nil {
			return *p.annual, true
		}
	case BillingPeriodMonthly:
		if p.monthly != nil {
			return *p.monthly, true
		}
	case BillingPeriodUnknown:
		// No basis to prefer — drop straight to the shared fallback below.
	}
	if p.monthly != nil {
		return *p.monthly, true
	}
	if p.annual != nil {
		return *p.annual, true
	}
	return 0, false
}

// priced is a tier published on both bases. Both arguments are per-MONTH rates:
// annual first, so `priced(32, 40)` is "$32/mo billed annually, $40 billed
// monthly". TestAnnualNeverExceedsMonthly guards the ordering.
func priced(annual, monthly float64) seatPrice {
	return seatPrice{annual: &annual, monthly: &monthly}
}

// flat is a tier that costs the same however it is billed — the free tiers.
func flat(v float64) seatPrice { return seatPrice{annual: &v, monthly: &v} }

// monthlyOnly records only a monthly rate — either the tier is genuinely sold
// monthly-only, or its annual rate is unverified. The owning vendor block says
// which.
func monthlyOnly(v float64) seatPrice { return seatPrice{monthly: &v} }

// unpriced is a recognised tier with no list price on any basis — a custom
// contract, or a vendor with no public pricing page yet. Distinct from an
// absent key: the tier is known, its price is not.
var unpriced = seatPrice{}

// LookupSeatMonthlyPriceUSD returns the per-month seat list price for the given
// ideType / planTier, for a caller that does not know the seat's billing basis.
// Identical to LookupSeatPriceUSD with BillingPeriodUnknown.
func LookupSeatMonthlyPriceUSD(ideType, planTier string) (float64, bool) {
	return LookupSeatPriceUSD(ideType, planTier, BillingPeriodUnknown)
}

// LookupSeatPriceUSD returns the per-month seat list price for the given
// ideType / planTier on the seat's billing basis. The second return is false
// when no price is known (the tuple is absent, or present with no published
// rate on any basis — enterprise/custom). Callers treat (0, false) as "price
// unavailable": never add zero to a rollup; propagate the missing signal so the
// UI renders "—".
func LookupSeatPriceUSD(ideType, planTier string, period BillingPeriod) (float64, bool) {
	vendor, ok := planTierSeatPriceUSD[ideType]
	if !ok {
		return 0, false
	}
	p, ok := vendor.tiers[planTier]
	if !ok {
		return 0, false
	}
	if period == BillingPeriodUnknown {
		period = vendor.defaultBasis
	}
	return p.resolve(period)
}
