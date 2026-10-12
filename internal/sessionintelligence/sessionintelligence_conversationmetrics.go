// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionintelligence

import (
	"sort"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/harnesside"
)

// dedupPromptsByMessageID collapses the per-content-block lines Claude Code
// writes for one billed response — each repeating the whole response's usage —
// into a single prompt, so per-line-usage consumers count each response once.
// It keeps the first prompt for each non-empty message_id and drops later ones;
// prompts with an empty message_id are all kept, which makes it a no-op for
// sources that emit one prompt per turn (Cursor, Codex, …) and for legacy data
// carrying no id. Keep-first is exact because Anthropic stamps the whole
// response's usage byte-identically on every content-block line, so the first
// surviving line's usage is the response's usage. Order is preserved.
func dedupPromptsByMessageID(prompts []*sessionintelligencepb.Prompt) []*sessionintelligencepb.Prompt {
	seen := make(map[string]struct{}, len(prompts))
	deduped := make([]*sessionintelligencepb.Prompt, 0, len(prompts))
	for _, p := range prompts {
		id := p.GetMetadata().GetMessageId()
		if id != "" {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
		}
		deduped = append(deduped, p)
	}
	return deduped
}

// computeCacheHitRate returns the session-aggregate cache hit rate (0–100).
// Computed as total cache_read / total effective_context across all prompts.
// Returns 0 if no effective context (e.g., Cursor sessions with no cache tokens).
func computeCacheHitRate(prompts []*sessionintelligencepb.Prompt) float64 {
	// Count each billed response once — otherwise a response's cache ratio is
	// weighted by its content-block count.
	prompts = dedupPromptsByMessageID(prompts)
	var totalCacheRead, totalEffective int64
	for _, p := range prompts {
		m := p.GetMetadata()
		totalCacheRead += m.GetCacheReadInputTokens()
		totalEffective += m.GetInputTokens() + m.GetCacheReadInputTokens() + m.GetCacheCreationInputTokens()
	}
	if totalEffective == 0 {
		return 0
	}
	return float64(totalCacheRead) / float64(totalEffective) * 100
}

// splitCursorInput estimates one Cursor call's fresh-input vs cached-prefix
// breakdown from its full-context inputTokens (curr) and the previous priced
// call's full context (prev). Cursor's local store has no cache buckets, so we
// infer: a grown-or-equal context (curr >= prev, prev > 0) re-read the whole
// prior context from cache and added (curr - prev) fresh tokens; a shrunk
// context — Cursor compaction or a cache reset — is treated as all-fresh (a new
// cache lineage). This is an approximation: it omits the cache-write premium,
// assumes one stable prefix, and treats ANY decrease as a reset (so it cannot
// tell rare tokenizer jitter from a real compaction — real drops in observed
// data are large compactions, not single-token noise). It therefore only
// approximates Cursor's true billed split, and is reconciled against Cursor's
// usage export rather than trusted as exact.
func splitCursorInput(curr, prev int64) (fresh, cachedRead int64) {
	if prev > 0 && curr >= prev {
		return curr - prev, prev
	}
	return curr, 0
}

// isCostBlindIDE reports whether an IDE's extractor populates no cache-token detail,
// so a token-derived cost for it is an approximation (priced without the cheap
// cache-read discount, from full-context input). See the modelPricing doc; Claude Code
// / Copilot report real cache buckets and price exactly. Used to mark
// cost_is_estimated. Delegates to harnesside.IsCostBlindIDE via the same
// value-preserving cast commonIdeType uses. Deliberately NOT the input-side-blind set:
// Codex reports cacheRead, so its cost prices exactly even though its input-side sum
// is a floor.
func isCostBlindIDE(ide sessionintelligencepb.IdeType) bool {
	return harnesside.IsCostBlindIDE(commonIdeType(ide))
}

// costIsEstimated reports whether estimatedCost is an approximation the UI must
// caveat. True only when there IS a cost (> 0) that no vendor figure overrode
// (reported <= 0), reached via an approximating path: a cache-blind IDE
// (Cursor/Antigravity price re-sent context through an estimated split; Kiro is
// priced from credits at the marginal overage rate, a floor — see
// kiroCreditOverageUSD) OR a model absent from the rate card (defaultPricing,
// unknownPricedCount > 0).
//
// The estimatedCost > 0 guard is load-bearing for token-less surfaces: a Cursor
// terminal-CLI session (IDE_TYPE_CURSOR, but its ~/.cursor/chats store logs no
// tokens or cost) prices to 0 with no vendor figure — there is no cost figure to
// caveat, so it is NOT an estimate. Without the guard it would be flagged an
// "estimated $0", which reads as a real (approximate) cost rather than absent
// data.
func costIsEstimated(reported, estimatedCost float64, ide sessionintelligencepb.IdeType, unknownPricedCount int) bool {
	return reported <= 0 && estimatedCost > 0 &&
		(isCostBlindIDE(ide) || unknownPricedCount > 0)
}

// costIsInputOnly reports whether estimated_cost_usd covers input tokens only:
// a cache-blind surface that priced input context but recorded no output/
// generation tokens, with no vendor figure overriding it. Cursor CLI is the
// structural case (its store keeps per-turn input context but no generation
// count); a cache-blind session that produced no assistant response also
// qualifies, since its cost genuinely was input-only. The cache-blind gate
// (matching costIsEstimated) keeps it off non-cache-blind IDEs — Claude Code /
// Copilot / Codex always record output, so a zero-output turn there is a
// degenerate abort, not an input-only cost basis. UI labels this
// "API-equivalent (input-only)".
//
// The estimatedCost > 0 guard mirrors costIsEstimated: a cursor-default
// (subscription sentinel, zero per-token rate) session prices to $0 even with
// input tokens recorded — there is no cost figure to label input-only, so it
// must not be flagged.
func costIsInputOnly(reported, estimatedCost float64, inputTokens, outputTokens int64, ide sessionintelligencepb.IdeType) bool {
	return reported <= 0 && estimatedCost > 0 && inputTokens > 0 && outputTokens == 0 && isCostBlindIDE(ide)
}

// ExtractMetrics computes aggregate metrics from parsed conversation prompts.
//
// Cost is the sum of input, output, cache-create and cache-read tokens priced
// at each model's per-bucket rate. Anthropic's API reports the three input
// counters as DISJOINT buckets — per the prompt-caching docs:
//
//	total_input_tokens = cache_read_input_tokens + cache_creation_input_tokens + input_tokens
//
// (input_tokens is only the tokens after the last cache breakpoint.) So
// summing the buckets here is correct and does not double-count.
// See: https://platform.claude.com/docs/en/build-with-claude/prompt-caching
// ("Tracking cache performance" → "Understanding the token breakdown").
//
// total_tokens deliberately excludes cache_* — it feeds the waste-cost ratio
// in session_insight_repo.go which divides wasted_tokens (also
// input+output-only) by this value.
//
// ideType gates the Cursor cache-split estimate: Cursor records inputTokens as
// the full re-sent context per call (its local store has no cache buckets),
// whereas Claude Code's inputTokens is already the post-breakpoint fresh slice.
// For Cursor we infer a fresh/cache-read split (see splitCursorInput) so cost is
// comparable to Claude Code instead of billing re-sent context at the full input
// rate. The estimate adjusts COST only; the reported token counters keep the
// recorded full-context values.
func ExtractMetrics(prompts []*sessionintelligencepb.Prompt, ideType sessionintelligencepb.IdeType) *sessionintelligencepb.ConversationMetrics {
	estimateCursorSplit := ideType == sessionintelligencepb.IdeType_IDE_TYPE_CURSOR
	var prevCursorContext int64 // full-context inputTokens of the previous priced call
	var prevCursorModel string  // model that established the current baseline (prevCursorContext)
	// Prompt size of the preceding row, held for the completion row of a producer
	// that splits one request in two (see the pendingPromptTokens use below).
	var pendingPromptTokens int64
	var pendingPromptModel string
	var totalTokens, totalInput, totalOutput, totalCacheCreate, totalCacheRead int64
	var totalCacheCreate5m, totalCacheCreate1h int64
	var totalCostUSD float64
	userPrompts := 0
	// Per-model buckets, accumulated in the same priced loop so their sum
	// reconciles with the aggregates below. Keyed by the canonical model
	// (canonicalModelKey), so dated/aliased spellings of one model collapse into
	// one bucket and a session's per-model cost adds back to totalCostUSD exactly.
	perModel := make(map[string]*modelUsageBucket)

	// Count each billed response once. Empty-id prompts (Cursor et al.) are all
	// kept, so the Cursor cost-split baseline below advances unchanged.
	prompts = dedupPromptsByMessageID(prompts)

	for _, p := range prompts {
		m := p.GetMetadata()
		inputTokens := m.GetInputTokens()
		outputTokens := m.GetOutputTokens()
		cacheCreate := m.GetCacheCreationInputTokens()
		cacheCreate5m := m.GetCacheCreation_5MInputTokens()
		cacheCreate1h := m.GetCacheCreation_1HInputTokens()
		cacheRead := m.GetCacheReadInputTokens()

		totalInput += inputTokens
		totalOutput += outputTokens
		totalCacheCreate += cacheCreate
		totalCacheCreate5m += cacheCreate5m
		totalCacheCreate1h += cacheCreate1h
		totalCacheRead += cacheRead
		totalTokens += inputTokens + outputTokens

		// The split and flat cache-create fields are alternative spellings of the
		// same volume, so take whichever this row uses — double-counting both
		// would inflate the prompt size and trip the tier early.
		cacheCreateTokens := cacheCreate
		if cacheCreate5m > 0 || cacheCreate1h > 0 {
			cacheCreateTokens = cacheCreate5m + cacheCreate1h
		}

		// The threshold is on the whole prompt as sent, so the cached prefix
		// counts toward it even though it bills at the cache rate.
		promptTokens := inputTokens + cacheRead + cacheCreateTokens

		// The tier belongs to the REQUEST, but some producers split one request
		// across two rows — Copilot's reconstructPrompts puts the prompt and cache
		// buckets on the user row and only completionTokens on the assistant row.
		// Pricing that completion row on its own zero prompt would bill a
		// long-context response at the base output rate. So a row carrying no
		// prompt inherits the preceding row's size, once: consuming it keeps a
		// request whose user row was dropped (empty text) from inheriting an
		// older, unrelated request's tier.
		if promptTokens > 0 {
			pendingPromptTokens, pendingPromptModel = promptTokens, m.GetModel()
		} else if m.GetModel() == pendingPromptModel {
			promptTokens = pendingPromptTokens
			pendingPromptTokens, pendingPromptModel = 0, ""
		}

		pricing := resolvePricingForPrompt(m.GetModel(), promptTokens)

		// Prefer the TTL-split fields when populated (post-extractor-upgrade
		// data). Fall back to the flat counter at the 5m rate for legacy
		// rows uploaded before the extractor was widened — conservative
		// because 5m is the cheaper of the two write tiers; if any of that
		// volume was actually 1h, we under-bill rather than over-bill.
		var cacheCreateCost float64
		if cacheCreate5m > 0 || cacheCreate1h > 0 {
			cacheCreateCost = float64(cacheCreate5m)/1_000_000*pricing.CacheCreate5mPerMillion +
				float64(cacheCreate1h)/1_000_000*pricing.CacheCreate1hPerMillion
		} else {
			cacheCreateCost = float64(cacheCreate) / 1_000_000 * pricing.CacheCreate5mPerMillion
		}

		// For Cursor, inputTokens is the full re-sent context; split it into an
		// estimated fresh slice (priced at input) and a cached prefix (priced at
		// cache-read) so cost matches how Claude Code bills re-sent context. Other
		// IDEs price inputTokens whole (it is already the fresh slice). Only cost
		// is affected — totalInput/totalTokens above keep the recorded values.
		// The estimate applies only when a call carries no real cache buckets; a
		// call that does is priced by those buckets below, so the split never
		// double-counts. prevCursorContext tracks the previous priced call's full
		// context and is advanced on every priced Cursor call — including one that
		// skipped the estimate — so the next call's split compares against the
		// immediately preceding call, never a stale earlier one.
		hasRealCacheData := cacheRead > 0 || cacheCreate > 0 || cacheCreate5m > 0 || cacheCreate1h > 0
		inputCost := float64(inputTokens) / 1_000_000 * pricing.InputPerMillion
		if estimateCursorSplit && inputTokens > 0 {
			if !hasRealCacheData {
				// A mid-session model switch (Cursor allows per-message model
				// changes) breaks cache continuity: the prior model's context is
				// not in the new model's cache. Treat it as a fresh baseline
				// (prev = 0) so the full context is priced at the new model's
				// input rate, not undercounted at its cache-read rate.
				prev := prevCursorContext
				if m.GetModel() != prevCursorModel {
					prev = 0
				}
				fresh, cachedRead := splitCursorInput(inputTokens, prev)
				inputCost = float64(fresh)/1_000_000*pricing.InputPerMillion +
					float64(cachedRead)/1_000_000*pricing.CacheReadPerMillion
			}
			prevCursorContext = inputTokens
			prevCursorModel = m.GetModel()
		}

		promptCost := inputCost +
			float64(outputTokens)/1_000_000*pricing.OutputPerMillion +
			cacheCreateCost +
			float64(cacheRead)/1_000_000*pricing.CacheReadPerMillion
		totalCostUSD += promptCost

		isHuman := IsHumanPrompt(p)
		if isHuman {
			userPrompts++
		}

		// Key on the canonical model pricing resolved against — the same
		// normalization (snapshot-date strip, Cursor alias) — so two dated or
		// aliased spellings of one model land in one bucket, not two rows that
		// render identically. Grouping is sum-invariant, so Σ(bucket) is unchanged.
		modelKey := canonicalModelKey(m.GetModel())
		b := perModel[modelKey]
		if b == nil {
			b = &modelUsageBucket{}
			perModel[modelKey] = b
		}
		b.inputTokens += inputTokens
		b.outputTokens += outputTokens
		// The EFFECTIVE cache-write count (split 5m+1h when populated, else the
		// flat field) — the same value priced above. The flat cacheCreate is 0 for
		// a split-only prompt, so bucketing it would record zero cache-write tokens
		// against a cost that includes the split (ModelUsageBreakdown contract).
		b.cacheCreateTokens += cacheCreateTokens
		b.cacheReadTokens += cacheRead
		b.costUSD += promptCost
		if isHuman {
			b.promptCount++
		}
	}

	return &sessionintelligencepb.ConversationMetrics{
		TotalPrompts:                int32(userPrompts),
		TotalTokens:                 totalTokens,
		EstimatedCostUsd:            totalCostUSD,
		InputTokens:                 totalInput,
		OutputTokens:                totalOutput,
		CacheCreationInputTokens:    totalCacheCreate,
		CacheCreation_5MInputTokens: totalCacheCreate5m,
		CacheCreation_1HInputTokens: totalCacheCreate1h,
		CacheReadInputTokens:        totalCacheRead,
		PerModel:                    buildPerModelBreakdown(perModel),
	}
}

// modelUsageBucket accumulates one model's token and cost totals across a
// conversation's priced prompts. Internal to ExtractMetrics.
type modelUsageBucket struct {
	promptCount       int32
	inputTokens       int64
	outputTokens      int64
	cacheCreateTokens int64
	cacheReadTokens   int64
	costUSD           float64
}

// canonicalModelKey groups a prompt under the model identity pricing resolves it
// to, so snapshot-dated ids (claude-opus-4-8-20260514 vs …-20260601) and Cursor
// alias spellings collapse into one per-model row rather than several that render
// identically. An empty name maps to the unspecified sentinel (as it is priced),
// and an unknown model keeps its date-stripped id so distinct unknowns stay apart.
func canonicalModelKey(model string) string {
	if model == "" {
		return unspecifiedModelLabel
	}
	if _, key, ok := lookupModelTable(modelPricingTable, model); ok {
		return key
	}
	return stripModelDateSuffix(model)
}

// maxModelBreakdownEntries caps the per-model rows so a corrupt or adversarial
// upload with many distinct model strings cannot grow the metrics JSONB blob,
// the wire message, or the panel list without bound. 12 sits far above any real
// session (the observed fleet max is 4 distinct models), so it bounds only
// pathological input; the overflow tail rolls into one unspecified bucket so Σ
// still reconciles.
const maxModelBreakdownEntries = 12

// buildPerModelBreakdown flattens the per-model buckets into the wire message,
// ordered by descending cost (model name breaks ties) so the output is stable
// across runs — a map iteration order would otherwise churn the JSONB blob and
// the panel's row order on every re-parse. Buckets with no billed contribution
// are dropped (a model-less/synthetic turn must not pad the list or trip the
// panel's >1 gate). Returns nil when nothing survives.
func buildPerModelBreakdown(perModel map[string]*modelUsageBucket) []*sessionintelligencepb.ModelUsageBreakdown {
	out := make([]*sessionintelligencepb.ModelUsageBreakdown, 0, len(perModel))
	for model, b := range perModel {
		if b.promptCount == 0 && b.inputTokens == 0 && b.outputTokens == 0 &&
			b.cacheCreateTokens == 0 && b.cacheReadTokens == 0 && b.costUSD == 0 {
			continue
		}
		out = append(out, &sessionintelligencepb.ModelUsageBreakdown{
			Model:                    model,
			PromptCount:              b.promptCount,
			InputTokens:              b.inputTokens,
			OutputTokens:             b.outputTokens,
			CacheCreationInputTokens: b.cacheCreateTokens,
			CacheReadInputTokens:     b.cacheReadTokens,
			EstimatedCostUsd:         b.costUSD,
		})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GetEstimatedCostUsd() != out[j].GetEstimatedCostUsd() {
			return out[i].GetEstimatedCostUsd() > out[j].GetEstimatedCostUsd()
		}
		return out[i].GetModel() < out[j].GetModel()
	})
	if len(out) > maxModelBreakdownEntries {
		kept := out[:maxModelBreakdownEntries-1]
		// Fold the tail into a kept "<unspecified>" bucket when one is already in
		// the top slice, so the rollup never renders as a second row with the same
		// sentinel label; otherwise the rollup becomes that bucket.
		rollup := (*sessionintelligencepb.ModelUsageBreakdown)(nil)
		for _, e := range kept {
			if e.GetModel() == unspecifiedModelLabel {
				rollup = e
				break
			}
		}
		appendRollup := rollup == nil
		if appendRollup {
			rollup = &sessionintelligencepb.ModelUsageBreakdown{Model: unspecifiedModelLabel}
		}
		for _, e := range out[maxModelBreakdownEntries-1:] {
			rollup.PromptCount += e.GetPromptCount()
			rollup.InputTokens += e.GetInputTokens()
			rollup.OutputTokens += e.GetOutputTokens()
			rollup.CacheCreationInputTokens += e.GetCacheCreationInputTokens()
			rollup.CacheReadInputTokens += e.GetCacheReadInputTokens()
			rollup.EstimatedCostUsd += e.GetEstimatedCostUsd()
		}
		out = kept
		if appendRollup {
			out = append(out, rollup)
		}
	}
	return out
}

// hasBillableTokens reports whether a prompt's usage carries any token we bill for —
// input, output, or either cache bucket including the TTL split. It gates both the
// unknown-model surfacing and the model backfill, so the two cannot disagree about
// which prompts have a price worth resolving.
func hasBillableTokens(m *sessionintelligencepb.PromptMetadata) bool {
	return m.GetInputTokens() > 0 || m.GetOutputTokens() > 0 ||
		m.GetCacheCreationInputTokens() > 0 || m.GetCacheReadInputTokens() > 0 ||
		m.GetCacheCreation_5MInputTokens() > 0 || m.GetCacheCreation_1HInputTokens() > 0
}

// unspecifiedModelLabel stands in for a prompt whose billable tokens carry no
// model name. ExtractMetrics still prices such volume at defaultPricing, so it
// is mispriced exactly like a named-but-unknown SKU; it is surfaced under this
// sentinel (rather than a bare "") so the unknownModelPricingTotal alert names
// it legibly. Angle-bracket style matches the "<synthetic>" placeholder.
const unspecifiedModelLabel = "<unspecified>"

// UnknownModelsWithBillableTokens returns distinct model names that resolve to
// defaultPricing AND carried billable tokens (input, output, or cache) — a real
// SKU being mispriced at the generic default rate. The gate is any billable
// token, not just cache: Cursor's own models never carry cache tokens locally,
// so a cache-only gate silently missed an unrecognized Cursor spelling mispriced
// on its input/output volume. Flagging them lets ops add the exact rate card
// before the misprice compounds. Resolution goes through resolveModelPricing so
// dated snapshots / Cursor aliases of a known model are not falsely flagged.
//
// An empty model name that carries billable tokens is also flagged (under
// unspecifiedModelLabel): ExtractMetrics prices it at defaultPricing too, so
// without this it would be default-priced yet badged an EXACT cost.
func UnknownModelsWithBillableTokens(prompts []*sessionintelligencepb.Prompt) []string {
	set := make(map[string]struct{})
	for _, p := range prompts {
		m := p.GetMetadata()
		model := m.GetModel()
		if isSyntheticModel(model) {
			continue
		}
		if _, known := resolveModelPricing(model); known {
			continue
		}
		if hasBillableTokens(m) {
			if model == "" {
				model = unspecifiedModelLabel
			}
			set[model] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// ExtractUsageMetadata extracts routing and display metadata from a conversation.
//
// files_edited is the union of per-prompt relevant_files and session_meta
// tracked_file_paths, both populated by the desktop app:
// - Cursor: relevant_files from SQLite bubble.relevantFiles.
// - Claude Code: relevant_files from write-tool (Edit/Write/MultiEdit/
// NotebookEdit) file_path inputs, plus tracked_file_paths from
// file-history-snapshot events.
// - Antigravity: not populated — API trajectory steps carry no file references.
//
// Backfill for existing Cursor sessions:
//
//	The conversation proto in S3 already contains relevant_files (populated at upload time).
//	A processor re-run (reset processing_status='pending' on session raw_records) will
//	re-fetch from S3, re-parse with this code, and write files_edited into session_insights.
//	No desktop app re-upload needed — the data is already in S3.
//
//	Production backfill SQL:
//	 WITH dates AS (
//	 SELECT DISTINCT ON (data_date) id
//	 FROM raw_records
//	 WHERE connector_id = 'session' AND processing_status = 'processed' AND deleted_at IS NULL
//	 ORDER BY data_date, id
//	 )
//	 UPDATE raw_records SET processing_status = 'pending', updated_at = now()
//	 WHERE id IN (SELECT id FROM dates);
func ExtractUsageMetadata(conv *sessionintelligencepb.Conversation) *sessionintelligencepb.UsageMetadata {
	modelsSet := make(map[string]struct{})
	filesSet := make(map[string]struct{})
	iterationCount := 0

	for _, p := range conv.GetPrompts() {
		if m := p.GetMetadata().GetModel(); m != "" && !isSyntheticModel(m) {
			modelsSet[m] = struct{}{}
		}
		if IsHumanPrompt(p) {
			iterationCount++
		}
		// Collect relevant_files from per-prompt metadata (Cursor bubble relevantFiles, CC tool_use blocks).
		for _, f := range p.GetMetadata().GetRelevantFiles() {
			stripped := stripPathPrefix(f)
			if stripped != "" {
				filesSet[stripped] = struct{}{}
			}
		}
	}

	// Merge tracked_file_paths from session_meta (CC: file-history-snapshot, Cursor: codeBlockData URIs).
	for _, f := range conv.GetSessionMeta().GetTrackedFilePaths() {
		stripped := stripPathPrefix(f)
		if stripped != "" {
			filesSet[stripped] = struct{}{}
		}
	}

	models := make([]string, 0, len(modelsSet))
	for m := range modelsSet {
		models = append(models, m)
	}

	files := make([]string, 0, len(filesSet))
	for f := range filesSet {
		files = append(files, f)
	}

	// EstimatedLocChanged is not populated server-side. The desktop app strips tool_use
	// content before uploading (replacing it with "[Tool: edit_file]" placeholders without
	// file paths or diffs), so LOC cannot be extracted from conversation content here.
	// This field is a forward-looking placeholder for client-side extraction by the desktop app.
	return &sessionintelligencepb.UsageMetadata{
		IdeType:        conv.GetIdeType().String(),
		SessionId:      conv.GetSessionId(),
		ProjectName:    conv.GetProjectName(),
		ModelsUsed:     models,
		IterationCount: int32(iterationCount),
		FilesEdited:    files,
	}
}
