// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
	sa "github.com/tetriz-ai/session-intelligence/internal/sessionanalysis"
)

// compatOnConfig flips the phase-5 compat write ON (the cutover posture); testConfig has
// it OFF (the parallel run).
type compatOnConfig struct{ testConfig }

func (compatOnConfig) CompatWriteEnabled() bool { return true }

// TestProcessFlat_CompatWrite_PhaseGated is the M1 guardrail: the phase-5 compat write
// stamps the terminal raw_records.processing_status ONLY under the compat-write flag, and
// never during the parallel run. It drives an OUT_OF_WINDOW skip (a terminal SKIPPED state
// reached with no S3/LLM work) and asserts StampProcessingStatusOnly fires exactly when
// the flag is on, with 'processed' — reproducing the old processor's contract for a
// conditional skip.
func TestProcessFlat_CompatWrite_PhaseGated(t *testing.T) {
	orgID := uuid.NewString()

	tests := []struct {
		name        string
		compatOn    bool
		wantStamped bool
	}{
		{name: "parallel run — flag off, no processing_status write", compatOn: false, wantStamped: false},
		{name: "cutover — flag on, skip stamps processed", compatOn: true, wantStamped: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if tt.compatOn {
				h.proc = sa.NewSessionAnalysisProcessor(h.rawDAO, h.analysis, h.state, h.turn, h.phase, h.roundTripFault, h.secFlag, h.harnessArtifact, h.classification, h.s3, h.producer, nil, nil, compatOnConfig{})
				h.roundTripFault.EXPECT().ReplaceSet(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			}
			// An old-dated record under a 1-day window skips OUT_OF_WINDOW before any
			// S3/LLM work — a terminal SKIPPED reached in applySkip, where the compat
			// write lives.
			record := sessionRawRecord(t, orgID, uuid.NewString(), "sha1")
			ctx := planscoped.WithConfigForTest(context.Background(), planscoped.Config{SeatsLimit: -1, DataWindowDays: 1, OrgCreatedAt: time.Now()})

			h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
			h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
			h.state.EXPECT().MarkSkipped(gomock.Any(), gomock.Any(), sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_OUT_OF_WINDOW).Return(true, nil)
			// No paid work on a pre-fetch skip.
			h.s3.EXPECT().ReadObject(gomock.Any(), gomock.Any()).Times(0)
			h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Times(0)

			if tt.wantStamped {
				h.rawDAO.EXPECT().
					StampProcessingStatusOnly(gomock.Any(), record.ID, orgID, record.DataDate, model.ProcessingStatusProcessed).
					Return(nil).Times(1)
			} else {
				h.rawDAO.EXPECT().
					StampProcessingStatusOnly(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
					Times(0)
			}

			processed, err := h.proc.ProcessFlat(ctx, orgID, 50)
			require.NoError(t, err)
			assert.Equal(t, 1, processed)
		})
	}
}

// TestProcessFlat_CompatWrite_PricesChildBeforeStamp is the T10 guardrail: once the compat flag is
// on (Session retired), a delegated-CHILD skip must PRICE the child — write cost + deduped tokens
// to its raw_records.metadata — before stamping it processed, so the parent's fold reads real cost
// instead of zero. During the parallel run (flag off) the engine does NOT price (Session still
// owns it), so it writes no cost metadata — only the turn-grain body read still runs.
func TestProcessFlat_CompatWrite_PricesChildBeforeStamp(t *testing.T) {
	orgID := uuid.NewString()

	tests := []struct {
		name       string
		compatOn   bool
		wantPriced bool
	}{
		{name: "parallel run — engine does not price the child", compatOn: false, wantPriced: false},
		{name: "cutover — engine prices the child before the processed stamp", compatOn: true, wantPriced: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if tt.compatOn {
				h.proc = sa.NewSessionAnalysisProcessor(h.rawDAO, h.analysis, h.state, h.turn, h.phase, h.roundTripFault, h.secFlag, h.harnessArtifact, h.classification, h.s3, h.producer, nil, nil, compatOnConfig{})
				h.roundTripFault.EXPECT().ReplaceSet(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			}
			record := sessionRawRecord(t, orgID, uuid.NewString(), "sha1")
			record.Metadata = mustJSON(t, map[string]string{"person_id": uuid.NewString(), "relation_type": "SUBAGENT", "parent_record_key": "pk"})
			// A wide window + unlimited seats so the only pre-fetch gate that fires is CHILD.
			ctx := planscoped.WithConfigForTest(context.Background(), planscoped.Config{SeatsLimit: -1, DataWindowDays: 36500, OrgCreatedAt: time.Now()})

			h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
			h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
			h.state.EXPECT().MarkSkipped(gomock.Any(), gomock.Any(), sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD).Return(true, nil)
			// bumpParentFence looks up the parent (best-effort; not-found is a no-op).
			h.rawDAO.EXPECT().GetByRecordKey(gomock.Any(), orgID, sa.SessionConnectorID, "pk").Return(nil, dao.ErrNotFound).AnyTimes()
			h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Times(0)

			// A seated, in-window child always reads its body once to write its measured turn grain
			// (an empty body cuts to no turns) before the skip. When compat is on, StampChildCost
			// reads it a SECOND time to price the child before the processed stamp.
			if tt.wantPriced {
				h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(discardBodyJSON(t), nil).Times(2)
				h.rawDAO.EXPECT().UpdateMetadata(gomock.Any(), record.ID, gomock.Any()).Return(nil).Times(1)
				h.rawDAO.EXPECT().
					StampProcessingStatusOnly(gomock.Any(), record.ID, orgID, record.DataDate, model.ProcessingStatusProcessed).
					Return(nil).Times(1)
			} else {
				// The parallel run does NOT price (Session still owns it): no metadata write and no
				// pricing S3 read — only the turn-grain read.
				h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(discardBodyJSON(t), nil).Times(1)
				h.rawDAO.EXPECT().UpdateMetadata(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
				h.rawDAO.EXPECT().
					StampProcessingStatusOnly(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
					Times(0)
			}

			processed, err := h.proc.ProcessFlat(ctx, orgID, 50)
			require.NoError(t, err)
			assert.Equal(t, 1, processed)
		})
	}
}
