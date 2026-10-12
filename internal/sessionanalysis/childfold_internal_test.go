// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	daomocks "github.com/tetriz-ai/session-intelligence/internal/dao/mocks"
)

// TestFoldChildUsage_CompletionContract pins foldChildUsage's bool return, which the spine uses
// to gate the folded_children_seq advance. The load-bearing case is the aggregate-read failure:
// it MUST report false so the caller holds the prior fold point, because advancing past a failed
// fold marks those children folded and stops the child-refold scan leg from ever re-admitting
// them — a transient read failure would then silently and permanently undercount delegated cost.
func TestFoldChildUsage_CompletionContract(t *testing.T) {
	const (
		orgID    = "org-fold"
		personID = "person-fold"
	)
	record := &model.RawRecordModel{
		ID:             "rec-fold",
		OrganizationID: orgID,
		RecordKey:      "parent-key",
		DataDate:       time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC),
	}
	meta := &rawRecordMetadata{PersonID: personID}

	tests := []struct {
		name          string
		metrics       *sessionintelligencepb.ConversationMetrics
		aggregateRows []dao.SubagentUsageRow
		aggregateErr  error
		expectCall    bool
		wantComplete  bool
		wantCost      float64 // expected metrics.EstimatedCostUsd after the fold (when metrics != nil)
	}{
		{
			// No measured metrics — nothing to fold onto; the fold is trivially complete and
			// never touches the aggregate.
			name:         "nil metrics is complete without reading",
			metrics:      nil,
			expectCall:   false,
			wantComplete: true,
		},
		{
			// The read failed: the fold is INCOMPLETE, so the caller must hold the fold point.
			// The parent keeps its own-only cost (own is stamped before the read).
			name:         "aggregate failure holds the fold point",
			metrics:      &sessionintelligencepb.ConversationMetrics{EstimatedCostUsd: 2.0},
			aggregateErr: errors.New("db down"),
			expectCall:   true,
			wantComplete: false,
			wantCost:     2.0,
		},
		{
			// No delegated children: complete, parent stays own-only.
			name:          "no children is complete own-only",
			metrics:       &sessionintelligencepb.ConversationMetrics{EstimatedCostUsd: 2.0},
			aggregateRows: nil,
			expectCall:    true,
			wantComplete:  true,
			wantCost:      2.0,
		},
		{
			// Children present: complete, own + Σ children folded into estimated_cost_usd.
			name:    "children fold into estimated cost",
			metrics: &sessionintelligencepb.ConversationMetrics{EstimatedCostUsd: 2.0},
			aggregateRows: []dao.SubagentUsageRow{
				{AgentType: "sub", Tokens: 100, CostUsdMicros: 1_500_000, Count: 1},
			},
			expectCall:   true,
			wantComplete: true,
			wantCost:     3.5, // 2.0 own + 1.5 child
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			rawDAO := daomocks.NewMockIRawRecordDAO(ctrl)
			if tt.expectCall {
				rawDAO.EXPECT().
					AggregateSubagentUsageByParent(gomock.Any(), orgID, personID, record.RecordKey, record.DataDate).
					Return(tt.aggregateRows, tt.aggregateErr)
			}
			processor := &SessionAnalysisProcessor{rawRecordDAO: rawDAO}

			usage := &sessionintelligencepb.UsageMetadata{}
			complete := processor.foldChildUsage(context.Background(), record, meta, tt.metrics, usage)

			assert.Equal(t, tt.wantComplete, complete)
			if tt.metrics != nil {
				assert.InDelta(t, tt.wantCost, tt.metrics.GetEstimatedCostUsd(), 1e-9)
			}
		})
	}
}
