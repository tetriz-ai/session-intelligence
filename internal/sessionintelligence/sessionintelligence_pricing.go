// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionintelligence

import (
	"strings"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// Model name constants for the pricing and context window tables.
const (
	ModelClaudeFable5_1 = "claude-fable-5-1"
	ModelClaudeFable5   = "claude-fable-5"
	ModelClaudeSonnet   = "claude-sonnet-4-6"
	ModelClaudeSonnet45 = "claude-sonnet-4-5"
	ModelClaudeSonnet5  = "claude-sonnet-5"
	ModelClaudeOpus     = "claude-opus-4-6"
	ModelClaudeOpus4_5  = "claude-opus-4-5"
	ModelClaudeOpus4_7  = "claude-opus-4-7"
	ModelClaudeOpus4_8  = "claude-opus-4-8"
	ModelClaudeOpus5    = "claude-opus-5"
	ModelClaudeHaiku    = "claude-haiku-4-5"
	ModelGPT4o          = "gpt-4o"
	ModelGPT4oMini      = "gpt-4o-mini"
	ModelGPT54          = "gpt-5.4"
	ModelGPT54Mini      = "gpt-5.4-mini"
	ModelGPT55          = "gpt-5.5"
	ModelGPT41          = "gpt-4.1"
	ModelGPT52          = "gpt-5.2"
	ModelGPT4           = "gpt-4"
	ModelO1             = "o1"
	ModelO1Mini         = "o1-mini"
	ModelO3Mini         = "o3-mini"
	ModelCursorDefault  = "cursor-default"
	// Cursor-native models. Cursor emits these ids in composerData.modelName.
	// composer-* and gpt-5.2-codex arrive verbatim (their Cursor spelling is the
	// canonical key); the ones needing normalization go through cursorModelAlias.
	ModelComposer1      = "composer-1"
	ModelComposer25     = "composer-2.5"
	ModelCursorAuto     = "cursor-auto"
	ModelGemini25Flash  = "gemini-2.5-flash"
	ModelGPT52Codex     = "gpt-5.2-codex"
	ModelGrok45         = "grok-4.5"
	ModelGrok46         = "grok-4.6"
	ModelGPT56Luna      = "gpt-5.6-luna"
	ModelGPT56Sol       = "gpt-5.6-sol"
	ModelGPT56Terra     = "gpt-5.6-terra"
	ModelGPT53Codex     = "gpt-5.3-codex"
	ModelGemini35Flash  = "gemini-3.5-flash"
	ModelKimiK25        = "kimi-k2.5"
	ModelKimiK3         = "kimi-k3"
	ModelComposer2Fast  = "composer-2-fast"
	ModelComposer25Fast = "composer-2.5-fast"
)

// modelPricing holds per-million-token prices for a model.
//
// Cache token semantics differ by provider:
// - Anthropic (Claude*): explicit cache_creation (1.25× input for 5m TTL,
// 2× input for 1h TTL) and cache_read (0.1× input) buckets, populated
// by Claude Code and Copilot extractors. The Claude Code extractor reads
// the nested usage.cache_creation.ephemeral_{5m,1h}_input_tokens object
// so 5m and 1h writes are priced separately. Pre-split-extractor
// conversations only carry the flat cache_creation_input_tokens — those
// are priced at the 5m rate (CacheCreate5mPerMillion) as a conservative
// fallback.
// - OpenAI (GPT*/o*): caching is automatic, with no cache_write billing on
// most SKUs — the gpt-5.6 family is the exception (it bills cache writes,
// carried on its rows as the 5m tier since OpenAI has no TTL split) — and
// cache_read is priced at 0.5× input for the 4o/o-series; newer models
// (e.g. gpt-5.4-mini) drop to 0.1×, so trust the per-row value, not this
// rule of thumb. Populated by Codex (via cached_input_tokens) and by
// Copilot when it routes to OpenAI models.
// - Cursor / Antigravity / Kiro: their extractors populate no explicit cache
// buckets. For Cursor, ExtractMetrics infers a cached-prefix split from the
// re-sent full context (see splitCursorInput) and prices that inferred
// prefix at CacheReadPerMillion precisely because no buckets are present —
// so the cache-read rate is load-bearing for cache-blind Cursor, not inert.
// Antigravity / Kiro get no split and price input whole, so their cache
// rates go unused.
type modelPricing struct {
	InputPerMillion         float64
	OutputPerMillion        float64
	CacheCreate5mPerMillion float64
	CacheCreate1hPerMillion float64
	CacheReadPerMillion     float64
}

// modelPricingTable maps model names to their pricing.
// Sources: the vendors' published rate cards —
// Anthropic prompt-caching rates (5m + 1h ephemeral write tiers, 1.25× and
// 2× input respectively); OpenAI cached-input pricing (50% discount for
// GPT-4o + o-series).
var modelPricingTable = map[string]modelPricing{
	// Fable 5.1 succeeds Fable 5 in the same tier at the same per-token rate
	// ($10 / $50) and the same 1.25× / 2× cache-write tiers, but its cache READ is
	// a published $0.25 — 0.025× input, NOT the 0.1× the rows below derive. That
	// is a deliberate discount off the family rule, not a transcription slip, so
	// the row is exempted from the fixed cache_read multiplier and pinned to the
	// published figure in claudeCacheReadExempt instead.
	ModelClaudeFable5_1: {InputPerMillion: 10.00, OutputPerMillion: 50.00, CacheCreate5mPerMillion: 12.50, CacheCreate1hPerMillion: 20.00, CacheReadPerMillion: 0.25},
	// Every Claude row below takes its cache tiers from Anthropic's standard
	// multipliers off input (5m 1.25×, 1h 2×, read 0.1×), same derivation as the
	// Opus rows. See https://platform.claude.com/docs/en/about-claude/pricing.
	ModelClaudeFable5: {InputPerMillion: 10.00, OutputPerMillion: 50.00, CacheCreate5mPerMillion: 12.50, CacheCreate1hPerMillion: 20.00, CacheReadPerMillion: 1.00},
	ModelClaudeSonnet: {InputPerMillion: 3.00, OutputPerMillion: 15.00, CacheCreate5mPerMillion: 3.75, CacheCreate1hPerMillion: 6.00, CacheReadPerMillion: 0.30},
	// Sonnet 4.5 shares Sonnet 4.6's $3/$15 rate (standard Anthropic cache
	// multipliers) but a smaller 200K context window — a distinct row so the
	// Cursor "4.5 sonnet" alias resolves to the right window, not 4.6's 1M.
	ModelClaudeSonnet45: {InputPerMillion: 3.00, OutputPerMillion: 15.00, CacheCreate5mPerMillion: 3.75, CacheCreate1hPerMillion: 6.00, CacheReadPerMillion: 0.30},
	// Sonnet 5 launched at an introductory rate below Sonnet 4.6 ($2/$10 vs $3/$15) —
	// looks like a transposition but matches OpenRouter + LiteLLM; the drift test
	// flags it if/when the published rate moves. Standard Anthropic cache multipliers.
	ModelClaudeSonnet5: {InputPerMillion: 2.00, OutputPerMillion: 10.00, CacheCreate5mPerMillion: 2.50, CacheCreate1hPerMillion: 4.00, CacheReadPerMillion: 0.20},
	// Opus 4.5+ uses a different rate card from the deprecated Opus 4 / 4.1
	// ($15 / $75 with $1.50 cache read). See https://platform.claude.com/docs/en/about-claude/pricing.
	ModelClaudeOpus: {InputPerMillion: 5.00, OutputPerMillion: 25.00, CacheCreate5mPerMillion: 6.25, CacheCreate1hPerMillion: 10.00, CacheReadPerMillion: 0.50},
	// Opus 4.5 shares the 4.5+ Opus rate card; its window is 200K, not the 1M the
	// later Opus rows carry, so it is a distinct row rather than an alias.
	ModelClaudeOpus4_5: {InputPerMillion: 5.00, OutputPerMillion: 25.00, CacheCreate5mPerMillion: 6.25, CacheCreate1hPerMillion: 10.00, CacheReadPerMillion: 0.50},
	ModelClaudeOpus4_7: {InputPerMillion: 5.00, OutputPerMillion: 25.00, CacheCreate5mPerMillion: 6.25, CacheCreate1hPerMillion: 10.00, CacheReadPerMillion: 0.50},
	ModelClaudeOpus4_8: {InputPerMillion: 5.00, OutputPerMillion: 25.00, CacheCreate5mPerMillion: 6.25, CacheCreate1hPerMillion: 10.00, CacheReadPerMillion: 0.50},
	ModelClaudeOpus5:   {InputPerMillion: 5.00, OutputPerMillion: 25.00, CacheCreate5mPerMillion: 6.25, CacheCreate1hPerMillion: 10.00, CacheReadPerMillion: 0.50},
	ModelClaudeHaiku:   {InputPerMillion: 1.00, OutputPerMillion: 5.00, CacheCreate5mPerMillion: 1.25, CacheCreate1hPerMillion: 2.00, CacheReadPerMillion: 0.10},
	ModelGPT4o:         {InputPerMillion: 2.50, OutputPerMillion: 10.00, CacheReadPerMillion: 1.25},
	ModelGPT4oMini:     {InputPerMillion: 0.15, OutputPerMillion: 0.60, CacheReadPerMillion: 0.075},
	// OpenAI auto-caches, so no cache_write tier (like the gpt-4o rows). cache_read
	// here is 0.1× input ($0.075) — NOT the 0.5× the gpt-4o family uses. Values are
	// guarded by TestPricingMatchesUpstream against the live OpenRouter catalog.
	ModelGPT54Mini: {InputPerMillion: 0.75, OutputPerMillion: 4.50, CacheReadPerMillion: 0.075},
	// gpt-5.4 (full tier, distinct SKU from gpt-5.4-mini): same OpenAI auto-cache
	// shape — no cache_write tier, cache_read 0.1× input ($0.25). Source: OpenAI's
	// own model docs — absent from both OpenRouter and LiteLLM today, so
	// TestPricingMatchesUpstream fail-soft skips it (see upstreamCatalogRefs)
	// until either catalog lists it; guarded by the pinned unit test meanwhile.
	ModelGPT54: {InputPerMillion: 2.50, OutputPerMillion: 15.00, CacheReadPerMillion: 0.25},
	// gpt-5.5 (frontier, not the -mini tier): same OpenAI auto-cache shape — no
	// cache_write tier, cache_read 0.1× input ($0.50). Guarded by TestPricingMatchesUpstream.
	ModelGPT55:         {InputPerMillion: 5.00, OutputPerMillion: 30.00, CacheReadPerMillion: 0.50},
	ModelGPT4:          {InputPerMillion: 30.00, OutputPerMillion: 60.00},
	ModelO1:            {InputPerMillion: 15.00, OutputPerMillion: 60.00, CacheReadPerMillion: 7.50},
	ModelO1Mini:        {InputPerMillion: 1.10, OutputPerMillion: 4.40, CacheReadPerMillion: 0.55},
	ModelO3Mini:        {InputPerMillion: 1.10, OutputPerMillion: 4.40, CacheReadPerMillion: 0.55},
	ModelCursorDefault: {},
	// Cursor rate card (cursor.com/docs/models-and-pricing, fetched 2026-07-09).
	// Cursor's API pool is provider pass-through at list price, so pass-through
	// models (claude-*/gpt-*) reuse the rows above via cursorModelAlias — only
	// Cursor's first-party pool and models we don't already carry are added here.
	// These are NOT claude-prefixed, so the fixed Anthropic cache-multiplier
	// assertion in TestModelTablesConsistent does not apply; cache_read carries
	// Cursor's published rate and is applied to the inferred cached-prefix split
	// when a Cursor call has NO real cache buckets (see the modelPricing doc and
	// splitCursorInput). composer-* have no cache_write tier.
	ModelComposer1:     {InputPerMillion: 1.25, OutputPerMillion: 10.00, CacheReadPerMillion: 0.125},
	ModelComposer25:    {InputPerMillion: 0.50, OutputPerMillion: 2.50, CacheReadPerMillion: 0.20},
	ModelCursorAuto:    {InputPerMillion: 1.25, OutputPerMillion: 6.00, CacheCreate5mPerMillion: 1.25, CacheReadPerMillion: 0.25},
	ModelGemini25Flash: {InputPerMillion: 0.30, OutputPerMillion: 2.50, CacheReadPerMillion: 0.03},
	// gpt-5.2-codex: OpenRouter openai/gpt-5.2-codex ($1.75/$14, cache_read
	// $0.175). OpenAI auto-caches, so no cache_write tier. Cross-checked live in
	// upstreamCatalogRefs (drift test surfaced the cache_read the docs page omitted).
	ModelGPT52Codex: {InputPerMillion: 1.75, OutputPerMillion: 14.00, CacheReadPerMillion: 0.175},
	// xAI grok-4.5 base (<200K-token) tier; not claude-prefixed, so no Anthropic
	// cache-write tiers apply. Rates per docs.x.ai (2026-08), cross-checked live in
	// upstreamCatalogRefs. LiteLLM's xai/grok-4.5 cache_read ($0.50) disagrees with
	// both docs.x.ai and OpenRouter ($0.30) — the two agreeing sources win.
	ModelGrok45: {InputPerMillion: 2.00, OutputPerMillion: 6.00, CacheReadPerMillion: 0.30},
	// xAI grok-4.6 base (<200K-token) tier, same shape as the grok-4.5 row above.
	// Input/output match 4.5 exactly; cache_read does NOT — 4.6 reads at $0.50
	// (0.25x input) where 4.5 reads at $0.30, so the two rows are not
	// interchangeable. docs.x.ai and OpenRouter agree on all three; xAI bills no
	// cache writes, hence no create tiers.
	ModelGrok46: {InputPerMillion: 2.00, OutputPerMillion: 6.00, CacheReadPerMillion: 0.50},
	// gpt-5.6-luna standard (<=272K-input) tier: cache_read 0.1× input ($0.02).
	// Unlike gpt-5.4/5.5 (auto-cache, no write billing), the 5.6 family bills
	// cache writes — $0.25/M (1.25× input) per LiteLLM, openai/codex-security,
	// and openclaw. Carried as the 5m tier: the Copilot extractor forwards the
	// flat cacheCreationInputTokens bucket for whatever model a request used,
	// and ExtractMetrics prices that flat bucket at CacheCreate5mPerMillion.
	// No 1h tier — OpenAI has no TTL split. Rates per the LiteLLM catalog
	// (2026-08, post the 2026-07-30 price cut), corroborated by
	// simonw/llm-prices and openai/codex-security; OpenRouter agrees, so this row
	// rides the default price check.
	ModelGPT56Luna: {InputPerMillion: 0.20, OutputPerMillion: 1.20, CacheCreate5mPerMillion: 0.25, CacheReadPerMillion: 0.02},
	// gpt-5.6-sol / -terra: the 5.6 family's larger tiers, same billing shape as the
	// luna row above (cache writes ARE billed at 1.25x input, carried as the 5m tier;
	// no 1h tier because OpenAI has no TTL split). The family holds read at 0.1x input
	// and write at 1.25x throughout.
	//
	// sol takes LiteLLM's OpenAI-direct `gpt-5.6-sol` key ($4.00/$20.00/$0.40).
	// OpenRouter is the lone outlier here, listing every sol variant (`:batch` included)
	// at exactly half; it agrees with us on luna and terra, so the divergence is
	// sol-specific rather than family-wide. sol's price is therefore checked against
	// LiteLLM (see priceFrom in model_catalog_drift_test.go); OpenRouter's halved listing
	// is not compared.
	//
	// The rate moved down from $5.00/$30.00 with the family ratios intact — read is still
	// 0.1x input and write still 1.25x — which is what distinguishes a list-price cut
	// from a catalog we are reading wrong. It no longer matches the gpt-5.5 row, so the
	// flagship tiers are no longer one price.
	ModelGPT56Sol:   {InputPerMillion: 4.00, OutputPerMillion: 20.00, CacheCreate5mPerMillion: 5.00, CacheReadPerMillion: 0.40},
	ModelGPT56Terra: {InputPerMillion: 2.00, OutputPerMillion: 12.00, CacheCreate5mPerMillion: 2.50, CacheReadPerMillion: 0.20},
	// gpt-4.1 / gpt-5.2 / gpt-5.3-codex: older auto-cache shape (no cache_write tier,
	// cache_read published directly). gpt-5.2 and gpt-5.3-codex share gpt-5.2-codex's
	// rate exactly. Rates from the OpenRouter catalog.
	ModelGPT41:      {InputPerMillion: 2.00, OutputPerMillion: 8.00, CacheReadPerMillion: 0.50},
	ModelGPT52:      {InputPerMillion: 1.75, OutputPerMillion: 14.00, CacheReadPerMillion: 0.175},
	ModelGPT53Codex: {InputPerMillion: 1.75, OutputPerMillion: 14.00, CacheReadPerMillion: 0.175},
	// gemini-3.5-flash: $1.50/$9.00/$0.15 read — Google's list rate per LiteLLM's
	// vertex_ai key, which is the price authority for this row (OpenRouter resells the
	// billed tiers at a uniform +10%). No cache-WRITE tier: LiteLLM publishes none, and OpenRouter's
	// $0.0833 is $1.00/M divided by 12 — the shape of Google's per-hour cache STORAGE
	// price, which this field cannot express (it is multiplied by a token count once,
	// with no time term). Gemini reaches us cache-blind anyway, so a write tier would
	// not be applied.
	ModelGemini35Flash: {InputPerMillion: 1.50, OutputPerMillion: 9.00, CacheReadPerMillion: 0.15},
	// Moonshot kimi: no cache-write billing, so no create tiers. Both rows carry
	// Moonshot's USD list rate from its own rate cards (platform.kimi.ai/docs/pricing:
	// k2.5 $0.60/$3.00/$0.10, k3 $3.00/$15.00/$0.30), which LiteLLM's moonshot/ keys
	// mirror; the live OpenRouter API returns a provider discount off list for both, so
	// neither row is checked against OpenRouter. Moonshot's China platform publishes a
	// separate CNY card that is not an FX of this one.
	ModelKimiK25: {InputPerMillion: 0.60, OutputPerMillion: 3.00, CacheReadPerMillion: 0.10},
	ModelKimiK3:  {InputPerMillion: 3.00, OutputPerMillion: 15.00, CacheReadPerMillion: 0.30},
	// Cursor's "fast" Composer tiers are separately priced SKUs, NOT effort variants:
	// composer-2-fast bills $1.50/$7.50 against composer-2's $0.50/$2.50, and
	// composer-2.5-fast $3.00/$15.00 against 2.5's $0.50/$2.50. They must not ride the
	// base rows (cursor.com/blog/composer-2, cursor.com/docs/models-and-pricing).
	// Cursor publishes a cache-read only for the 2.5 fast tier ($0.50 on $3.00 input,
	// 0.167x). composer-2-fast's is DERIVED, not published: the same 0.167x applied to
	// its $1.50 input, giving $0.25. Zero is not a safe stand-in — a cache-blind Cursor
	// call prices its INFERRED cached prefix at this rate (see the modelPricing doc), so
	// a zero would bill those tokens free — and defaultPricing's $0.50 would be 0.33x,
	// double the vendor's own fast-tier ratio.
	ModelComposer2Fast:  {InputPerMillion: 1.50, OutputPerMillion: 7.50, CacheReadPerMillion: 0.25},
	ModelComposer25Fast: {InputPerMillion: 3.00, OutputPerMillion: 15.00, CacheReadPerMillion: 0.50},
}

// defaultPricing is used for models not in the pricing table. Cache rates use
// Anthropic's standard multipliers off the input rate (5m 1.25×, 1h 2×, read
// 0.1×) so an unlisted model isn't billed $0 for cache — which dominates
// Claude Code token volume.
var defaultPricing = modelPricing{
	InputPerMillion:         5.00,
	OutputPerMillion:        15.00,
	CacheCreate5mPerMillion: 6.25,
	CacheCreate1hPerMillion: 10.00,
	CacheReadPerMillion:     0.50,
}

// stripModelDateSuffix drops a trailing Anthropic snapshot suffix ("-YYYYMMDD")
// so a dated id (claude-opus-4-8-20260514) resolves to its base model.
func stripModelDateSuffix(model string) string {
	const suffixLen = 9 // "-" + 8 date digits
	if len(model) < suffixLen || model[len(model)-suffixLen] != '-' {
		return model
	}
	for _, c := range model[len(model)-suffixLen+1:] {
		if c < '0' || c > '9' {
			return model
		}
	}
	return model[:len(model)-suffixLen]
}

// cursorModelAlias maps Cursor's model spellings (composerData.modelName) to
// canonical modelPricingTable / modelContextWindowTable keys. Cursor's API pool
// is provider pass-through at list price, so a Cursor Claude/Gemini id resolves
// to the same-rate canonical row; the spelling may differ (e.g. Cursor
// "4.5 sonnet" → claude-sonnet-4-5) but the $/token is identical, which is what
// cost depends on. `default`/`auto` are Cursor's Auto router → the cursor-auto
// pool rate (NOT the cursor-default subscription sentinel). New Cursor spellings
// (e.g. a future gemini -preview date) that miss both this map and the tables
// surface via unknownModelPricingTotal rather than silently mispricing.
var cursorModelAlias = map[string]string{
	"claude-4.5-sonnet-thinking": ModelClaudeSonnet45,
	"claude-4.5-sonnet":          ModelClaudeSonnet45,
	// Cursor's "claude-4-sonnet" (Sonnet 4) rides the Sonnet 4.6 row on purpose:
	// the $/token is identical, and the only field that differs — the context
	// window — is 4.6's 1M here vs Sonnet 4's standard 200K. The wider window is
	// a deliberate choice (contrast the 200K Sonnet 4.5 row above); its effect is
	// masked because Cursor sessions carry their own contextUsagePercent from the
	// extractor, so this row rarely drives the UI.
	"claude-4-sonnet-thinking": ModelClaudeSonnet,
	"claude-4-sonnet":          ModelClaudeSonnet,
	// composer-2's published base rate ($0.50/$2.50, cursor.com/blog/composer-2) is
	// identical to composer-2.5's, so it rides that row exactly rather than
	// approximately. Its FAST tier is not identical — see the composer-*-fast rows.
	"composer-2": ModelComposer25,
	"default":    ModelCursorAuto,
	"auto":       ModelCursorAuto,
	// "auto-smart" is one of Cursor Router's optimization modes, not a model — it
	// carries no published rate of its own, so it rides the same cursor-auto row as
	// "auto"/"default".
	"auto-smart": ModelCursorAuto,
	// Cursor re-orders the version ("claude-4.6-opus" for claude-opus-4-6) and appends
	// the reasoning EFFORT ("-high", "-medium", "-xhigh", "-thinking"). Effort changes
	// how many tokens a request spends, never the $/token, so each variant rides its
	// base model's row.
	//
	// Enumerated deliberately rather than stripped by a generic suffix rule: "-fast"
	// looks like the same kind of modifier but is a separately priced SKU for Composer
	// (see the composer-*-fast rows), so a blind stripper would bill those at the
	// wrong rate. Every entry here is a reviewed decision.
	"claude-4.6-opus-high":              ModelClaudeOpus,
	"claude-4.6-opus-high-thinking":     ModelClaudeOpus,
	"claude-4.6-sonnet-medium-thinking": ModelClaudeSonnet,
	"claude-4.5-opus-high":              ModelClaudeOpus4_5,
	"claude-4.5-opus-high-thinking":     ModelClaudeOpus4_5,
	"claude-opus-4-7-thinking-high":     ModelClaudeOpus4_7,
	"claude-opus-4-7-thinking-xhigh":    ModelClaudeOpus4_7,
	"claude-opus-4-8-thinking-high":     ModelClaudeOpus4_8,
	"claude-opus-4-8-thinking-medium":   ModelClaudeOpus4_8,
	"claude-sonnet-5-thinking-high":     ModelClaudeSonnet5,
	// Cursor prefixes xAI models it serves from its own pool.
	"cursor-grok-4.5-high":        ModelGrok45,
	"cursor-grok-4.5-high-fast":   ModelGrok45,
	"cursor-grok-4.5-medium-fast": ModelGrok45,
	// Effort variants of models already carried at the base id.
	"gpt-5.5-medium":       ModelGPT55,
	"gpt-5.6-terra-medium": ModelGPT56Terra,
}

// cursorModelPrefixAlias maps a Cursor model-name PREFIX to a canonical key,
// for families where the vendor appends a rolling date/preview suffix we can't
// enumerate ahead of time (e.g. gemini-2.5-flash-preview-05-20,
// gemini-2.5-flash-preview-09-2025). Checked after the exact table, date-suffix,
// and exact-alias lookups, so a fully-specified id always wins over a prefix
// match. A miss still surfaces via unknownModelPricingTotal rather than
// silently mispricing.
var cursorModelPrefixAlias = []struct {
	prefix string
	canon  string
}{
	{"gemini-2.5-flash-preview", ModelGemini25Flash},
}

// lookupModelTable resolves a model id against a table, retrying on the
// date-suffix-stripped base id so dated snapshots match, then on the Cursor
// alias so Cursor's own spellings hit the canonical row. Found maps to
// (value, matched-key, true) — the matched-key is the canonical table key the id
// resolved to, so a caller can group by the same identity pricing uses; a miss
// returns the zero value and "" so callers apply their own default. Aliasing
// lives here (not only in resolveModelPricing) so pricing and context-window
// lookups normalize identically and cannot diverge.
func lookupModelTable[V any](table map[string]V, model string) (V, string, bool) {
	if v, ok := table[model]; ok {
		return v, model, true
	}
	if base := stripModelDateSuffix(model); base != model {
		if v, ok := table[base]; ok {
			return v, base, true
		}
	}
	if canon, ok := cursorModelAlias[model]; ok {
		if v, ok := table[canon]; ok {
			return v, canon, true
		}
	}
	for _, pa := range cursorModelPrefixAlias {
		if strings.HasPrefix(model, pa.prefix) {
			if v, ok := table[pa.canon]; ok {
				return v, pa.canon, true
			}
		}
	}
	var zero V
	return zero, "", false
}

// resolveModelPricing returns a model's pricing and whether the table matched;
// an unknown model falls to defaultPricing (callers flag it via
// UnknownModelsWithBillableTokens).
func resolveModelPricing(model string) (modelPricing, bool) {
	if pr, _, ok := lookupModelTable(modelPricingTable, model); ok {
		return pr, true
	}
	return defaultPricing, false
}

// longContextTier is a vendor's second flat rate, charged once a request's
// prompt reaches ThresholdTokens. It replaces the base rate for the WHOLE
// request rather than pricing only the tokens above the threshold — that is how
// xAI bills it, and it is why this is a rate swap and not a marginal band.
type longContextTier struct {
	ThresholdTokens int64
	Pricing         modelPricing
}

// modelLongContextTable holds the models that bill a second tier. Absence means
// single-tier, so a model needs a row here only when its vendor publishes one.
// Rates and thresholds per docs.x.ai (longContextThreshold +
// promptTextTokenPriceLongContext and siblings), corroborated by OpenRouter's
// min_prompt_tokens override.
var modelLongContextTable = map[string]longContextTier{
	ModelGrok45: {ThresholdTokens: 200_000, Pricing: modelPricing{InputPerMillion: 4.00, OutputPerMillion: 12.00, CacheReadPerMillion: 0.60}},
	ModelGrok46: {ThresholdTokens: 200_000, Pricing: modelPricing{InputPerMillion: 4.00, OutputPerMillion: 12.00, CacheReadPerMillion: 1.00}},
}

// resolvePricingForPrompt returns the rate to bill one request at, upgrading to
// the model's long-context tier when promptTokens reaches its threshold.
//
// promptTokens must be the request's WHOLE prompt — fresh input plus any cached
// prefix — because the vendor thresholds on what was sent, not on what was
// billed as fresh. Callers therefore pass input+cache_read+cache_create, which
// is the full prompt on every IDE: Claude Code reports a fresh input slice with
// the prefix in the cache buckets, while Cursor reports the whole re-sent
// context in input with the cache buckets empty.
func resolvePricingForPrompt(model string, promptTokens int64) modelPricing {
	pricing, _ := resolveModelPricing(model)
	if lc, _, ok := lookupModelTable(modelLongContextTable, model); ok && promptTokens >= lc.ThresholdTokens {
		return lc.Pricing
	}
	return pricing
}

// modelContextWindowTable maps model names to their context window sizes in tokens.
var modelContextWindowTable = map[string]int64{
	ModelClaudeFable5_1: 1_000_000,
	ModelClaudeFable5:   1_000_000,
	ModelClaudeSonnet:   1_000_000,
	// Sonnet 4.5's standard window is 200K (1M only via the beta header).
	ModelClaudeSonnet45: 200_000,
	ModelClaudeSonnet5:  1_000_000,
	ModelClaudeOpus:     1_000_000,
	ModelClaudeOpus4_7:  1_000_000,
	ModelClaudeOpus4_8:  1_000_000,
	ModelClaudeOpus5:    1_000_000,
	ModelClaudeHaiku:    200_000,
	ModelGPT4o:          128_000,
	ModelGPT4oMini:      128_000,
	// LiteLLM lists the OpenAI-direct gpt-5.4-mini at 272k (provider variants
	// differ: OpenRouter 400k, azure 1.05M). Guarded by TestContextWindowMatchesLiteLLM.
	ModelGPT54Mini: 272_000,
	// gpt-5.4 (full tier) window is OpenAI-docs-sourced — absent from LiteLLM
	// today, so TestContextWindowMatchesLiteLLM fail-soft skips it (see
	// upstreamCatalogRefs) until it's listed; guarded by the pinned unit test
	// meanwhile.
	ModelGPT54: 1_050_000,
	// gpt-5.5 window is OpenRouter-sourced (top_provider.context_length); LiteLLM
	// carries no gpt-5.5 entry, and TestContextWindowMatchesLiteLLM cross-checks
	// only listed models, so this row is guarded by the pinned unit test, not the
	// LiteLLM drift check.
	ModelGPT55:         1_050_000,
	ModelGPT4:          128_000,
	ModelO1:            200_000,
	ModelO1Mini:        128_000,
	ModelO3Mini:        200_000,
	ModelCursorDefault: 128_000,
	// Cursor-native models (paired with the pricing rows above; the 1:1 table
	// consistency guard requires a window for every priced model). gpt-5.2-codex
	// (400K) and gemini-2.5-flash (~1.05M) are provider-published; the composer-*
	// / cursor-auto windows are Cursor-internal and unpublished — set to 200K as a
	// conservative best-effort (not cross-checked; Cursor sessions carry their own
	// contextUsagePercent from the extractor, so this fallback rarely drives UI).
	ModelComposer1:     200_000,
	ModelComposer25:    200_000,
	ModelCursorAuto:    200_000,
	ModelGemini25Flash: 1_048_576,
	// LiteLLM lists gpt-5.2-codex at 272K (OpenRouter advertises 400K — same
	// provider-variance the gpt-5.4-mini row notes); match LiteLLM so
	// TestContextWindowMatchesLiteLLM stays green.
	ModelGPT52Codex: 272_000,
	ModelGrok45:     500_000,
	// docs.x.ai maxPromptLength, corroborated by OpenRouter. LiteLLM carries no
	// grok-4.6 entry, so TestContextWindowMatchesLiteLLM fail-soft skips this row.
	ModelGrok46: 500_000,
	// LiteLLM lists the gpt-5.6 family at 922K (max_input_tokens); guarded by
	// TestContextWindowMatchesLiteLLM.
	ModelGPT56Luna: 922_000,
	// Opus 4.5's standard window is 200K — the later Opus rows carry 1M.
	ModelClaudeOpus4_5: 200_000,
	ModelGPT56Sol:      922_000,
	ModelGPT56Terra:    922_000,
	// OpenRouter context_length for gpt-4.1.
	ModelGPT41: 1_047_576,
	// gpt-5.2 / gpt-5.3-codex match gpt-5.2-codex's provider-variance note above:
	// LiteLLM's 272K, not OpenRouter's 400K.
	ModelGPT52:      272_000,
	ModelGPT53Codex: 272_000,
	// Windows from LiteLLM's vertex_ai/gemini-3.5-flash, moonshot/kimi-k2.5 and
	// moonshot/kimi-k3 keys.
	ModelGemini35Flash: 1_048_576,
	ModelKimiK25:       262_144,
	ModelKimiK3:        1_048_576,
	// Cursor-internal and unpublished, same conservative 200K as the other composer-*
	// rows above.
	ModelComposer2Fast:  200_000,
	ModelComposer25Fast: 200_000,
}

const defaultContextWindow int64 = 200_000

// contextWindowForModel returns the context window size for a model.
// Returns the hardcoded value from modelContextWindowTable, or defaultContextWindow (200k)
// for unknown models. The table is hand-maintained; there is no runtime metadata-API lookup.
func contextWindowForModel(model string) (int64, bool) {
	if cw, _, ok := lookupModelTable(modelContextWindowTable, model); ok {
		return cw, true
	}
	return defaultContextWindow, false
}

// effectiveContextTokens is the whole prompt a message occupied in the context window:
// fresh input plus the cached prefix (cache_read + cache_creation). This is the single
// definition of "effective context" — both the session-max percent (computeContextUsagePercent)
// and the per-turn percent (ContextOccupancyPercentForMessage) derive from it, so the two
// cannot drift apart.
func effectiveContextTokens(md *sessionintelligencepb.PromptMetadata) int64 {
	return md.GetInputTokens() + md.GetCacheReadInputTokens() + md.GetCacheCreationInputTokens()
}

// ContextOccupancyPercentForMessage returns one message's context occupancy as 0–100 —
// effectiveContextTokens over the model's context window from the authoritative,
// LiteLLM-drift-guarded window table (TestContextWindowMatchesLiteLLM) — and whether the
// model was known. known=false (unknown/foreign model id, or no context-bearing tokens)
// lets the caller emit honest-unset rather than divide by the 200K default. The value is
// physical-window occupancy, not a client's usable-window indicator (which reserves output
// / auto-compact budget), so it is not directly comparable to a vendor context %; and for a
// turn that spans models it is a per-message figure, so the turn's peak is a worst-case
// occupancy, not the dominant model's. Clamped to 100 because heavy cache_read can exceed
// the physical window on long sessions (as computeContextUsagePercent clamps its session-max).
func ContextOccupancyPercentForMessage(md *sessionintelligencepb.PromptMetadata) (float64, bool) {
	window, known := contextWindowForModel(md.GetModel())
	if !known || window <= 0 {
		return 0, false
	}
	effective := effectiveContextTokens(md)
	if effective <= 0 {
		return 0, false
	}
	pct := float64(effective) / float64(window) * 100
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

// The constants below parameterize the active-duration cascade. The full
// per-tier flow is documented on ComputeNormalizedFields; each constant's own
// doc explains its role.

// isSyntheticModel reports whether the given model name is a Claude Code
// locally-generated placeholder (e.g., "<synthetic>") rather than a real
// model. Synthetic messages represent client-side system events (API errors
// like "Prompt is too long", interruptions, compaction placeholders) and
// have zero token usage. They should not be tracked as real models used.
func isSyntheticModel(model string) bool {
	return model == "<synthetic>"
}
