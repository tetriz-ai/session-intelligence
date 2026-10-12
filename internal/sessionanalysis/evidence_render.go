// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"fmt"
	"sort"
	"strings"

	coreaidlcv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/aidlc/v1"
	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
)

// buildEvidenceContent assembles the compact, E-shaped request the Evidence producer reads — NOT
// R's full raw transcript, which E never re-reads (that is E's whole cost argument). It matches the
// ANALYZE_EVIDENCE_FENCE's block vocabulary: the artifact LEDGER (identity + invocation counts),
// the per-invocation INVOCATIONS block (with any injected body), the candidate round-trip PAIRS the
// detector routed to E, and the CAPABILITIES block. The whole assembly then gets the SAME
// anonymizePaths send-transform sp.llmContent gets, so file paths never reach the model
// (the LLM output contract).
//
// It takes orgID + rawRecordID because the PAIRS block re-enumerates the guard-valid pairs the same
// way the detector did (buildMeasuredTurnRows is keyed by them) and keeps only the ones the detector
// scored above threshold (sp.roundTripCandidates). It does not take R's ReadResult: E receives no
// upstream verdict — the contract E-input contract is ledgers, invocations, excerpts, pairs and
// capabilities, none of them R's output.
//
// contract_gate_checks (a fence block) is intentionally omitted: no gate-check arithmetic exists in
// the session spine to render, so an empty block would assert a coverage the data cannot back.
// roundTripOnly renders the round-trip half's inputs alone (the pairs), with the artifact blocks
// empty, so E adjudicates only the pairs on a pass whose artifact ledger is pending intent
// (only the artifact half waits); the full ledger renders on the
// intent-arrival re-run.
// resolvedIntent carries each invocation slug's classified intent so E identifies reviewers by
// classified intent, not slug name (empty when nothing is classified, or on a round-trip-only pass).
func buildEvidenceContent(orgID, rawRecordID string, sp *spineResult, roundTripOnly bool, resolvedIntent map[string]coreaidlcv1.ArtifactIntentClass) string {
	var b strings.Builder
	invocations := sp.ledgers.invocations
	if roundTripOnly {
		invocations = nil
		resolvedIntent = nil
	}
	renderEvidenceLedger(&b, invocations, resolvedIntent)
	renderInvocations(&b, invocations, resolvedIntent)
	renderEvidencePairs(&b, orgID, rawRecordID, sp)
	renderCapabilities(&b, capabilityFor(sp.prompts))
	return anonymizePaths(b.String())
}

// shortIntent renders a classified intent for E's request; a slug with no classified intent renders
// nothing (E treats it as unknown rather than reading a false class). AMBIGUOUS is the classifier's
// terminal abstain — a real verdict (so the sweep does not re-classify it), but not one E surfaces —
// so it renders nothing too, exactly like UNSPECIFIED (never-classified).
func shortIntent(resolvedIntent map[string]coreaidlcv1.ArtifactIntentClass, slug string) string {
	intent, ok := resolvedIntent[slug]
	if !ok ||
		intent == coreaidlcv1.ArtifactIntentClass_ARTIFACT_INTENT_CLASS_UNSPECIFIED ||
		intent == coreaidlcv1.ArtifactIntentClass_ARTIFACT_INTENT_CLASS_AMBIGUOUS {
		return ""
	}
	return shortEnum(intent.String(), "ARTIFACT_INTENT_CLASS_")
}

// renderEvidenceLedger writes the LEDGER block — one line per distinct (artifact kind, slug) with
// its invocation count, the identity E adjudicates against and may never re-count. An empty
// invocation set renders the header with an explicit no-artifact line so the block is never
// ambiguous (E may still fire on a round-trip pair alone).
func renderEvidenceLedger(b *strings.Builder, invocations []invocationDetail, resolvedIntent map[string]coreaidlcv1.ArtifactIntentClass) {
	b.WriteString("=== LEDGER ===\n")
	counts := map[ledgerKey]int{}
	var order []ledgerKey
	for _, invocation := range invocations {
		key := ledgerKey{artifactType: invocation.artifactType, slug: invocation.slug}
		if _, seen := counts[key]; !seen {
			order = append(order, key)
		}
		counts[key]++
	}
	if len(order) == 0 {
		b.WriteString("no artifact invocations\n")
		return
	}
	// Stable order (kind then slug) so the rendered LEDGER is deterministic across runs.
	sort.Slice(order, func(i, j int) bool {
		if order[i].artifactType != order[j].artifactType {
			return order[i].artifactType < order[j].artifactType
		}
		return order[i].slug < order[j].slug
	})
	for _, key := range order {
		fmt.Fprintf(b, "artifact: %s kind=%s invocations=%d",
			key.slug, shortEnum(key.artifactType.String(), "HARNESS_ARTIFACT_TYPE_"), counts[key])
		if intent := shortIntent(resolvedIntent, key.slug); intent != "" {
			fmt.Fprintf(b, " intent=%s", intent)
		}
		fmt.Fprintf(b, " [%s]\n", measuredTag)
	}
}

// ledgerKey is the (kind, slug) identity the LEDGER groups invocations by.
type ledgerKey struct {
	artifactType commonv1.HarnessArtifactType
	slug         string
}

// renderInvocations writes the INVOCATIONS block — one entry per invocation carrying its slug, its
// artifact kind, the human turn it fired at (only when the timeline recorded one), and its injected
// body (only when the source captured a bounded description). An empty set renders the header with
// an explicit no-invocation line.
func renderInvocations(b *strings.Builder, invocations []invocationDetail, resolvedIntent map[string]coreaidlcv1.ArtifactIntentClass) {
	b.WriteString("=== INVOCATIONS ===\n")
	if len(invocations) == 0 {
		b.WriteString("no invocations\n")
		return
	}
	for _, invocation := range invocations {
		fmt.Fprintf(b, "invocation: %s kind=%s",
			invocation.slug, shortEnum(invocation.artifactType.String(), "HARNESS_ARTIFACT_TYPE_"))
		if invocation.promptIndex > 0 {
			fmt.Fprintf(b, " prompt_index=%d", invocation.promptIndex)
		}
		if intent := shortIntent(resolvedIntent, invocation.slug); intent != "" {
			fmt.Fprintf(b, " intent=%s", intent)
		}
		b.WriteString("\n")
		if body := strings.TrimSpace(invocation.injectedBody); body != "" {
			fmt.Fprintf(b, "body: %s\n", body)
		}
	}
}

// renderEvidencePairs writes the PAIRS block — the P / agent response / Q triples the detector
// scored above threshold. It re-enumerates the session's guard-valid pairs (the same enumeration
// that fed the detector) and keeps only those whose P-turn index is in sp.roundTripCandidates, so E
// judges the recall-oriented top-N the detector routed to it and nothing else. An empty candidate
// set (detector dark, or no scored pair) renders the header with an explicit no-pair line.
func renderEvidencePairs(b *strings.Builder, orgID, rawRecordID string, sp *spineResult) {
	b.WriteString("=== PAIRS ===\n")
	selected := make(map[int32]struct{}, len(sp.roundTripCandidates))
	for _, candidate := range sp.roundTripCandidates {
		selected[candidate.PromptIndex] = struct{}{}
	}
	if len(selected) == 0 {
		b.WriteString("no round-trip pairs\n")
		return
	}
	pairs, _ := enumerateGuardValidPairs(orgID, rawRecordID, sp)
	wrote := false
	for _, pair := range pairs {
		if _, ok := selected[pair.PromptIndex]; !ok {
			continue
		}
		fmt.Fprintf(b, "pair prompt_index=%d:\n", pair.PromptIndex)
		fmt.Fprintf(b, "P: %s\n", pair.P)
		fmt.Fprintf(b, "response: %s\n", pair.AgentResponse)
		fmt.Fprintf(b, "Q: %s\n", pair.Q)
		wrote = true
	}
	if !wrote {
		b.WriteString("no round-trip pairs\n")
	}
}
