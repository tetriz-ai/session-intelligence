// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

func ptr[T any](v T) *T { return &v }

func promptIssues(slugs ...sessionintelligencepb.PromptIssueSlug) *sessionintelligencepb.PromptIssueList {
	items := make([]*sessionintelligencepb.PromptIssue, 0, len(slugs))
	for _, s := range slugs {
		layer := ""
		if s == sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK ||
			s == sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_DB_CREDENTIAL_LEAK ||
			s == sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_PII_INJECTION {
			layer = "security"
		}
		items = append(items, &sessionintelligencepb.PromptIssue{
			Slug:     s,
			Severity: sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH,
			Layer:    layer,
		})
	}
	return &sessionintelligencepb.PromptIssueList{Items: items}
}

func TestApplyNewVocabRollup(t *testing.T) {
	t.Run("waste is bucketed by cause with the measured cost; taxonomy is flat and includes security", func(t *testing.T) {
		analysis := &model.SessionAnalysis{}
		sp := &spineResult{promptTokenCosts: []int64{100, 200, 300}} // indexed by prompt_index
		read := &ReadResult{Turns: []*TurnJudgment{
			// OVER_FETCH on turns 0 and 1 → 100 + 200 = 300; a security issue on turn 0.
			{
				PromptIndex: 0, WasteCause: ptr(sessionintelligencepb.WasteCause_WASTE_CAUSE_OVER_FETCH),
				PromptIssues: promptIssues(sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK),
			},
			{PromptIndex: 1, WasteCause: ptr(sessionintelligencepb.WasteCause_WASTE_CAUSE_OVER_FETCH)},
			// REDUNDANT_REREAD on turn 2 → 300; a non-security issue.
			{
				PromptIndex: 2, WasteCause: ptr(sessionintelligencepb.WasteCause_WASTE_CAUSE_REDUNDANT_REREAD),
				PromptIssues: promptIssues(sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_AMBIGUOUS_DIRECTIVE),
			},
		}}

		applyNewVocabRollup(analysis, sp, read)

		require.NotNil(t, analysis.WasteCategories)
		// Deterministic order: by WasteCause enum value (REDUNDANT_REREAD=2 before OVER_FETCH=3).
		// Category is the kebab slug (matching security_flags' vocabulary), not the enum name.
		got := analysis.WasteCategories.GetItems()
		require.Len(t, got, 2)
		assert.Equal(t, "redundant-reread", got[0].GetCategory())
		assert.Equal(t, int64(300), got[0].GetTokens())
		assert.Equal(t, "over-fetch", got[1].GetCategory())
		assert.Equal(t, int64(300), got[1].GetTokens())

		require.NotNil(t, analysis.TaxonomyIssues)
		issues := analysis.TaxonomyIssues.GetItems()
		require.Len(t, issues, 2, "flat collection: one taxonomy issue per per-turn prompt issue")
		// The security slug is INCLUDED on the taxonomy surface, carrying its layer; category_slug is
		// the kebab slug (matching security_flags' flag_type for this issue) and severity is lowercase.
		assert.Equal(t, "api-key-leak", issues[0].GetCategorySlug())
		assert.Equal(t, "high", issues[0].GetSeverity())
		assert.Equal(t, "security", issues[0].GetLayer())
		assert.Equal(t, "ambiguous-directive", issues[1].GetCategorySlug())
		// A new-session issue carries no per-issue wasted-tokens estimate.
		assert.Equal(t, int32(0), issues[0].GetEstimatedWastedTokens())
		// The taxonomy kebab derive and the security_flags kebab map agree for a security slug, so
		// the same issue surfaces identically on both surfaces.
		assert.Equal(t, "api-key-leak", issues[0].GetCategorySlug())
	})

	t.Run("UNSPECIFIED / NONE / nil causes contribute no waste bucket", func(t *testing.T) {
		analysis := &model.SessionAnalysis{}
		sp := &spineResult{promptTokenCosts: []int64{100, 200, 300}}
		read := &ReadResult{Turns: []*TurnJudgment{
			{PromptIndex: 0, WasteCause: ptr(sessionintelligencepb.WasteCause_WASTE_CAUSE_UNSPECIFIED)},
			{PromptIndex: 1, WasteCause: ptr(sessionintelligencepb.WasteCause_WASTE_CAUSE_NONE)},
			{PromptIndex: 2, WasteCause: nil},
		}}
		applyNewVocabRollup(analysis, sp, read)
		assert.Nil(t, analysis.WasteCategories, "no judged waste cause → column stays NULL")
		assert.Nil(t, analysis.TaxonomyIssues, "no per-turn issues → column stays NULL")
	})

	t.Run("a turn whose PromptIndex is out of the measured cost table is skipped, not indexed", func(t *testing.T) {
		analysis := &model.SessionAnalysis{}
		sp := &spineResult{promptTokenCosts: []int64{100}} // n=1, valid index 0 only
		read := &ReadResult{Turns: []*TurnJudgment{
			{PromptIndex: 5, WasteCause: ptr(sessionintelligencepb.WasteCause_WASTE_CAUSE_OVER_FETCH)}, // out of cut
			{PromptIndex: 0, WasteCause: ptr(sessionintelligencepb.WasteCause_WASTE_CAUSE_OVER_FETCH)}, // in cut, cost 100
		}}
		applyNewVocabRollup(analysis, sp, read)
		require.NotNil(t, analysis.WasteCategories)
		got := analysis.WasteCategories.GetItems()
		require.Len(t, got, 1)
		assert.Equal(t, int64(100), got[0].GetTokens(), "only the in-cut turn contributes; the out-of-cut turn is skipped")
	})

	t.Run("an in-cut turn whose measured cost is 0 mints no waste bucket", func(t *testing.T) {
		analysis := &model.SessionAnalysis{}
		sp := &spineResult{promptTokenCosts: []int64{0}} // in cut, but 0 cost
		read := &ReadResult{Turns: []*TurnJudgment{
			{PromptIndex: 0, WasteCause: ptr(sessionintelligencepb.WasteCause_WASTE_CAUSE_OVER_FETCH)},
		}}
		applyNewVocabRollup(analysis, sp, read)
		assert.Nil(t, analysis.WasteCategories, "a 0-cost in-cut cause carries no bucket")
	})

	t.Run("taxonomy is emitted in PromptIndex order even when turns arrive reversed", func(t *testing.T) {
		analysis := &model.SessionAnalysis{}
		sp := &spineResult{promptTokenCosts: []int64{100, 200}}
		// Turns in reverse PromptIndex order (LLM-response order, not index order).
		read := &ReadResult{Turns: []*TurnJudgment{
			{PromptIndex: 1, PromptIssues: promptIssues(sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_AMBIGUOUS_DIRECTIVE)},
			{PromptIndex: 0, PromptIssues: promptIssues(sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK)},
		}}
		applyNewVocabRollup(analysis, sp, read)
		require.NotNil(t, analysis.TaxonomyIssues)
		issues := analysis.TaxonomyIssues.GetItems()
		require.Len(t, issues, 2)
		assert.Equal(t, "api-key-leak", issues[0].GetCategorySlug(), "PromptIndex 0 first")
		assert.Equal(t, "ambiguous-directive", issues[1].GetCategorySlug(), "PromptIndex 1 second")
	})

	t.Run("no turns leaves both columns NULL", func(t *testing.T) {
		analysis := &model.SessionAnalysis{}
		applyNewVocabRollup(analysis, &spineResult{promptTokenCosts: []int64{100}}, &ReadResult{})
		assert.Nil(t, analysis.WasteCategories)
		assert.Nil(t, analysis.TaxonomyIssues)
	})

	t.Run("no turns but carried rollups set → columns equal the carried values, not nil", func(t *testing.T) {
		analysis := &model.SessionAnalysis{}
		carriedTaxonomy := &sessionintelligencepb.TaxonomyIssueList{Items: []*sessionintelligencepb.TaxonomyIssue{
			{CategorySlug: "api-key-leak", Severity: "high", Layer: "security"},
		}}
		carriedWaste := &sessionintelligencepb.TokenBreakdownList{Items: []*sessionintelligencepb.TokenBreakdown{
			{Category: "over-fetch", Tokens: 500},
		}}
		// A carried re-pick of a turn-grain-unsupported source: no turn rows, but the stored rollups
		// rehydrated onto read → re-written unchanged rather than NULLed by the empty flatten.
		read := &ReadResult{CarriedTaxonomyIssues: carriedTaxonomy, CarriedWasteCategories: carriedWaste}
		applyNewVocabRollup(analysis, &spineResult{promptTokenCosts: []int64{100}}, read)
		assert.Same(t, carriedTaxonomy, analysis.TaxonomyIssues, "carried taxonomy is re-written unchanged")
		assert.Same(t, carriedWaste, analysis.WasteCategories, "carried waste is re-written unchanged")
	})
}
