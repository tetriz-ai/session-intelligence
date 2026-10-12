// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/logger"
	"github.com/tetriz-ai/session-intelligence/internal/sessionintelligence"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
)

// foldChildUsage folds this parent session's delegated children's cost and tokens into its
// measured row, matching the retained session path so the two pipelines' child folds cannot
// drift: own_estimated_cost_usd keeps the parent's OWN cost and estimated_cost_usd becomes
// own + Σ children, while the per-child breakdown (tokens, count, per-agent rows) lands on
// usage_metadata.subagent_usage. Without this, a delegated child's usage disappears from the
// measured numbers, because the parent spine reads only its own S3 body.
//
// A child that arrived BEFORE its parent is picked up here when the parent first runs — the
// aggregate reads every child raw_record tagged with this parent's record_key regardless of
// arrival order, and a child seen before its parent stays SKIPPED(CHILD) and folds now. A
// child that arrives AFTER the parent's children_seq snapshot leaves children_seq >
// folded_children_seq, which the child-refold scan leg re-admits to re-fold next tick (never a
// commit-time CAS). Aggregation is keyed on the PARENT's own
// person_id, never a triggering child's, so a child that forged another user's
// parent_record_key cannot add its tokens onto that user's row.
//
// Best-effort and non-fatal: a fold failure leaves the parent's own cost correct and the next
// tick re-folds. It runs outside any open transaction (its own short read scope).
//
// Returns whether the fold is complete for this children_seq — true when the children were
// folded (or there were none to fold), false ONLY when the aggregate read failed. The caller
// must gate the folded_children_seq advance on this: advancing it past a failed fold would mark
// those children folded and stop the child-refold scan leg from ever re-admitting them, so a
// transient read failure would silently and permanently undercount delegated cost.
func (p *SessionAnalysisProcessor) foldChildUsage(ctx context.Context, record *model.RawRecordModel, meta *rawRecordMetadata, metrics *sessionintelligencepb.ConversationMetrics, usage *sessionintelligencepb.UsageMetadata) bool {
	if metrics == nil {
		return true
	}
	// own_estimated_cost_usd anchors the parent's own cost so estimated_cost_usd can always be
	// recomputed as own + Σ children — here and on any later re-fold.
	own := metrics.GetEstimatedCostUsd()
	metrics.OwnEstimatedCostUsd = own

	childUsage, err := p.aggregateChildUsage(ctx, record, meta)
	if err != nil {
		logger.WarnWithCtx(ctx, "session analysis: subagent usage fold failed; parent keeps own-only cost, fold point held for re-fold",
			zap.String("raw_record_id", record.ID), zap.Error(err))
		return false
	}
	if childUsage == nil {
		return true
	}
	metrics.EstimatedCostUsd = own + childUsage.GetTotalEstimatedCostUsd()
	if usage != nil {
		usage.SubagentUsage = childUsage
	}
	return true
}

// aggregateChildUsage reads the parent's delegated children and builds their folded usage,
// returning nil when the session has no delegated children (so the caller leaves the parent's
// cost own-only). The read runs in its own tenant scope; raw_records is tenant-isolated.
func (p *SessionAnalysisProcessor) aggregateChildUsage(ctx context.Context, record *model.RawRecordModel, meta *rawRecordMetadata) (*sessionintelligencepb.SubagentUsage, error) {
	var childUsage *sessionintelligencepb.SubagentUsage
	if err := tenantdb.RunInScope(ctx, record.OrganizationID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		aggregated, aggErr := p.rawRecordDAO.AggregateSubagentUsageByParent(sctx, record.OrganizationID, meta.PersonID, record.RecordKey, record.DataDate)
		if aggErr != nil {
			return fmt.Errorf("aggregate subagent usage by parent (record %s): %w", record.ID, aggErr)
		}
		if len(aggregated) == 0 {
			return nil
		}
		childUsage = sessionintelligence.BuildSubagentUsage(aggregated)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("fold child usage scope: %w", err)
	}
	return childUsage, nil
}
