// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// TestApplyMeasuredColumns pins the code-owned measured mapping the S1 overview reads:
// the cost/token scalars, the token breakdown, session telemetry, the structural
// counts, and the vendor-string→enum conversions (including abstention on an unknown
// token).
func TestApplyMeasuredColumns(t *testing.T) {
	sp := &spineResult{
		turnCount: 5,
		repoName:  "org/repo",
		ideType:   sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		metrics: &sessionintelligencepb.ConversationMetrics{
			TotalPrompts:         5,
			TotalTokens:          1000,
			EstimatedCostUsd:     0.5,
			OwnEstimatedCostUsd:  0.4,
			InputTokens:          800,
			OutputTokens:         200,
			CacheReadInputTokens: 100,
		},
		usage: &sessionintelligencepb.UsageMetadata{
			IterationCount:      3,
			McpServerCount:      2,
			IsSessionEffective:  true,
			ProjectName:         "proj",
			ModelsUsed:          []string{"claude-sonnet"},
			DelegationLevel:     "assisted",
			EffectivenessSource: "cc_facets",
			SessionOutcome:      "fully_achieved",
		},
	}
	analysis := &model.SessionAnalysis{}
	applyMeasuredColumns(analysis, sp)

	require.NotNil(t, analysis.TotalPrompts)
	assert.Equal(t, int32(5), *analysis.TotalPrompts)
	require.NotNil(t, analysis.TotalTokens)
	assert.Equal(t, int64(1000), *analysis.TotalTokens)
	require.NotNil(t, analysis.EstimatedCostUsd)
	assert.InDelta(t, 0.5, *analysis.EstimatedCostUsd, 1e-9)
	require.NotNil(t, analysis.TokenBreakdown)
	assert.Equal(t, int64(800), analysis.TokenBreakdown.GetInputTokens())
	assert.Equal(t, int64(100), analysis.TokenBreakdown.GetCacheReadTokens())
	require.NotNil(t, analysis.IterationCount)
	assert.Equal(t, int32(3), *analysis.IterationCount)
	require.NotNil(t, analysis.McpServerCount)
	assert.Equal(t, int32(2), *analysis.McpServerCount)
	require.NotNil(t, analysis.IsSessionEffective)
	assert.True(t, *analysis.IsSessionEffective)
	require.NotNil(t, analysis.ProjectName)
	assert.Equal(t, "proj", *analysis.ProjectName)
	require.NotNil(t, analysis.ModelsUsed)
	assert.Equal(t, []string{"claude-sonnet"}, analysis.ModelsUsed.GetValues())
	require.NotNil(t, analysis.TurnCount)
	assert.Equal(t, int32(5), *analysis.TurnCount)
	require.NotNil(t, analysis.RepoName)
	assert.Equal(t, "org/repo", *analysis.RepoName)

	// Vendor-string → proto enum.
	require.NotNil(t, analysis.DelegationLevel)
	assert.Equal(t, sessionintelligencepb.DelegationLevel_DELEGATION_LEVEL_ASSISTED, *analysis.DelegationLevel)
	require.NotNil(t, analysis.EffectivenessSource)
	assert.Equal(t, sessionintelligencepb.EffectivenessSource_EFFECTIVENESS_SOURCE_CC_FACETS, *analysis.EffectivenessSource)
	require.NotNil(t, analysis.SessionOutcome)
	assert.Equal(t, sessionintelligencepb.SessionOutcome_SESSION_OUTCOME_FULLY_ACHIEVED, *analysis.SessionOutcome)

	// ide_type is stamped by buildSessionAnalysis via the enum-name conversion.
	require.NotNil(t, toCommonIdeType(sp.ideType))
	assert.Equal(t, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, *toCommonIdeType(sp.ideType))
}

// TestVendorEnumAbstains asserts an unrecognised vendor token maps to nil, not a
// plausible default.
func TestVendorEnumAbstains(t *testing.T) {
	got := vendorEnum("DELEGATION_LEVEL_", "not_a_real_level", sessionintelligencepb.DelegationLevel_value, func(v int32) sessionintelligencepb.DelegationLevel { return sessionintelligencepb.DelegationLevel(v) })
	assert.Nil(t, got)
	empty := vendorEnum("DELEGATION_LEVEL_", "", sessionintelligencepb.DelegationLevel_value, func(v int32) sessionintelligencepb.DelegationLevel { return sessionintelligencepb.DelegationLevel(v) })
	assert.Nil(t, empty)
	// A valid-but-zero token (the UNSPECIFIED member, which resolves in the value map to 0) must
	// also abstain: the zero enum is "no value", not a plausible default.
	unspecified := vendorEnum("DELEGATION_LEVEL_", "unspecified", sessionintelligencepb.DelegationLevel_value, func(v int32) sessionintelligencepb.DelegationLevel { return sessionintelligencepb.DelegationLevel(v) })
	assert.Nil(t, unspecified)
}

// TestToCommonIdeTypeAbstains asserts the zero (UNSPECIFIED) IdeType maps to nil, not a
// plausible default — mirroring the vendorEnum abstention.
func TestToCommonIdeTypeAbstains(t *testing.T) {
	assert.Nil(t, toCommonIdeType(sessionintelligencepb.IdeType_IDE_TYPE_UNSPECIFIED))
}
