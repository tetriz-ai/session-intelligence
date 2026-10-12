// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionintelligence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"go.uber.org/zap"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	"github.com/tetriz-ai/session-intelligence/internal/lineage"
	"github.com/tetriz-ai/session-intelligence/internal/logger"
	"github.com/tetriz-ai/session-intelligence/internal/metrictenant"
	"github.com/tetriz-ai/session-intelligence/internal/s3"
)

// AssembleStaticUsage builds the static usage metrics (tokens, cost, activity) for a conversation.
func AssembleStaticUsage(ctx context.Context, orgID, recordID string, conv *sessionintelligencepb.Conversation, metrics *sessionintelligencepb.ConversationMetrics, repoName string) *sessionintelligencepb.UsageMetadata {
	unknownPriced := UnknownModelsWithBillableTokens(conv.GetPrompts())
	if len(unknownPriced) > 0 {
		logger.WarnWithCtx(ctx, "unknown_models_using_default_pricing",
			zap.String(logger.RecordID, recordID),
			zap.Strings("unknown_models", unknownPriced),
		)
		for _, m := range unknownPriced {
			unknownModelPricingTotal.WithLabelValues(metrictenant.OrgID(orgID), m, conv.GetIdeType().String()).Inc()
		}
	}

	usageMetadata := ExtractUsageMetadata(conv)
	contextUsage, unknownModels := computeContextUsagePercent(conv.GetPrompts())
	usageMetadata.ContextUsagePercent = contextUsage
	if len(unknownModels) > 0 {
		logger.WarnWithCtx(ctx, "unknown_models_using_default_context_window",
			zap.String(logger.RecordID, recordID),
			zap.Strings("unknown_models", unknownModels),
			zap.Int64("default_context_window", defaultContextWindow),
		)
	}
	usageMetadata.CacheHitRatePercent = computeCacheHitRate(conv.GetPrompts())
	usageMetadata.SessionDurationSeconds = computeSessionDuration(conv)
	ExtractSessionSignals(conv, usageMetadata)
	// Kiro records no tokens, so the estimate above is structurally $0 and the seat
	// carries no cost weight — allocation then falls to an even per-session split.
	// Overwrite rather than fill-if-empty: allocation divides each session's cost
	// by the seat's own total, so mixing a token-priced session in with
	// credit-priced ones compares incommensurable units.
	if conv.GetIdeType() == sessionintelligencepb.IdeType_IDE_TYPE_KIRO {
		if credits := usableVendorFigure(ctx, orgID, fieldBillingCreditsUsed, conv.GetSessionMeta().GetBillingCreditsUsed()); credits > 0 {
			metrics.EstimatedCostUsd = credits * kiroCreditOverageUSD
		}
	}
	// Vendor-reported actual cost (Cursor usageData) is authoritative over the
	// token-derived estimate — a Cursor session on an unpriced model estimates to
	// $0, but its reported cost is real. Overwrite the existing estimated_cost_usd
	// (no separate reported field) so every cost consumer (dashboards, seat-cost
	// weight) uses the true figure.
	reported := usableVendorFigure(ctx, orgID, fieldReportedCostUSD, conv.GetSessionMeta().GetReportedCostUsd())
	if reported > 0 {
		metrics.EstimatedCostUsd = reported
	}
	// Flag the cost as an estimate — gated on a non-zero token-derived cost so a
	// token-less surface isn't badged an "estimated $0". See costIsEstimated.
	usageMetadata.CostIsEstimated = costIsEstimated(reported, metrics.GetEstimatedCostUsd(), conv.GetIdeType(), len(unknownPriced))
	usageMetadata.CostIsInputOnly = costIsInputOnly(reported, metrics.GetEstimatedCostUsd(), metrics.GetInputTokens(), metrics.GetOutputTokens(), conv.GetIdeType())
	ExtractInstallationMetadata(ctx, conv, usageMetadata)
	ExtractAiAttribution(conv, usageMetadata)
	ExtractStatsCache(conv, usageMetadata)

	// repo_name from raw_record metadata (full "org/repo" resolved by the desktop app from
	// git remote origin); left as extracted when the metadata carried none.
	if repoName != "" {
		usageMetadata.RepoName = repoName
	}

	// Prime the normalized file/line churn before the value is serialized into session context
	// or read by a measured column: without it effectiveFilesChanged/effectiveLinesChanged are
	// proto3-zero and omitted by protojson. ComputeNormalizedFields recomputes it idempotently.
	setEffectiveChurn(usageMetadata)
	return usageMetadata
}

// processRecord handles a single raw_record: S3 fetch → parse → LLM analysis → write insights.
//
// Transaction phasing: the record's DB work runs in short scopes on either
// side of the LLM call, which runs outside any scope so no tenant transaction idles
// across it.

// StampChildCost reads a delegated child's cost from S3 and stamps it onto the child source.
func StampChildCost(ctx context.Context, s3Client s3.S3Client, rawRecordDAO dao.IRawRecordDAO, record *model.RawRecordModel, s3Key string) (*ChildSource, error) {
	content, err := s3Client.ReadObject(ctx, s3Key)
	if err != nil {
		return nil, fmt.Errorf("read s3 object %q: %w", s3Key, err)
	}
	conv, err := ParseConversation(content)
	if err != nil {
		return nil, fmt.Errorf("parse conversation: %w", err)
	}
	m := ExtractMetrics(conv.GetPrompts(), conv.GetIdeType())
	cost := m.GetEstimatedCostUsd()
	// childTokens matches the desktop lineage.tokens semantics (input + output +
	// cache) but is computed from the deduped metrics, so a repeated-id Claude
	// Code child is counted once. GetTotalTokens() is input+output only and would
	// drop cache volume; the flat CacheCreationInputTokens is the total (the
	// 5m/1h fields are its breakdown — summing them too would double-count).
	childTokens := m.GetInputTokens() + m.GetOutputTokens() +
		m.GetCacheCreationInputTokens() + m.GetCacheReadInputTokens()
	if unknownPriced := UnknownModelsWithBillableTokens(conv.GetPrompts()); len(unknownPriced) > 0 {
		logger.WarnWithCtx(ctx, "unknown_models_using_default_pricing_with_cache_tokens",
			zap.String(logger.RecordID, record.ID),
			zap.Strings("unknown_models", unknownPriced),
		)
		for _, m := range unknownPriced {
			unknownModelPricingTotal.WithLabelValues(metrictenant.OrgID(record.OrganizationID), m, conv.GetIdeType().String()).Inc()
		}
	}
	// Vendor-reported actual cost is authoritative over the token-derived
	// estimate — same precedence the primary path applies.
	if reported := usableVendorFigure(ctx, record.OrganizationID, fieldReportedCostUSD, conv.GetSessionMeta().GetReportedCostUsd()); reported > 0 {
		cost = reported
	}
	// Merge into the raw metadata map rather than round-tripping the typed
	// RawRecordMetadata struct — the metadata carries desktop-written lineage
	// keys the struct doesn't model, and a typed round-trip would drop them.
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(record.Metadata, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}
	if meta == nil {
		meta = map[string]json.RawMessage{}
	}
	meta[lineage.MetaKeyCostUsdMicros] = json.RawMessage(strconv.FormatInt(int64(math.Round(cost*1e6)), 10))
	// Overwrite the desktop's raw lineage.tokens in place with the deduped total.
	// The parent aggregation reads this same key, so no aggregation-SQL change;
	// CreateBatch carries it across an unchanged-payload re-upload alongside cost.
	meta[lineage.MetaKeyTokens] = json.RawMessage(strconv.FormatInt(childTokens, 10))
	merged, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	if err := rawRecordDAO.UpdateMetadata(ctx, record.ID, merged); err != nil {
		return nil, fmt.Errorf("update metadata: %w", err)
	}
	digest := sha256.Sum256(content)
	return &ChildSource{
		Conversation:        conv,
		OwnCostUsdMicros:    int64(math.Round(cost * 1e6)),
		SourceContentSHA256: hex.EncodeToString(digest[:]),
	}, nil
}

// ChildSource is what StampChildCost already had in hand: the parsed child
// transcript, its priced own cost, and the digest of the fetched bytes. Returning
// it lets the child loop path analyze without a second S3 read.
type ChildSource struct {
	Conversation        *sessionintelligencepb.Conversation
	OwnCostUsdMicros    int64
	SourceContentSHA256 string
}

// applySubagentUsage writes the aggregated subagent usage onto usage metadata:
// the usage, its last_child_update_at, and the delegation bump — a session with
// ≥1 delegated child delegated work, so an otherwise-interactive classification
// becomes assisted (background/task-agent sessions are already tagged
// delegated/assisted upstream). Shared by the primary's own run and the
// child-triggered / reconcile re-aggregations so usage attached on a
// late-arriving child classifies delegation identically to a child present on

// BuildSubagentUsage aggregates per-parent sub-agent usage rows into a SubagentUsage message.
func BuildSubagentUsage(rows []dao.SubagentUsageRow) *sessionintelligencepb.SubagentUsage {
	usage := &sessionintelligencepb.SubagentUsage{
		SpawnedAgents: make([]*sessionintelligencepb.SpawnedAgent, 0, len(rows)),
	}
	var totalCostMicros int64
	for _, row := range rows {
		usage.TotalTokens += row.Tokens
		usage.ChildCount += row.Count
		totalCostMicros += row.CostUsdMicros
		usage.SpawnedAgents = append(usage.SpawnedAgents, &sessionintelligencepb.SpawnedAgent{
			AgentType:        row.AgentType,
			PluginName:       row.PluginName,
			Count:            row.Count,
			Tokens:           row.Tokens,
			EstimatedCostUsd: float64(row.CostUsdMicros) / 1e6,
		})
	}
	usage.TotalEstimatedCostUsd = float64(totalCostMicros) / 1e6
	if usage.GetTotalTokens() < 0 {
		usage.TotalTokens = 0
	}
	if usage.GetTotalEstimatedCostUsd() < 0 {
		usage.TotalEstimatedCostUsd = 0
	}
	return usage
}
