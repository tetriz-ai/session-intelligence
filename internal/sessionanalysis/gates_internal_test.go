// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	"github.com/tetriz-ai/session-intelligence/internal/lineage"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
)

// TestPrefetchGate exercises every gate branch, including REPO_UNTRACKED_METADATA
// (whose tracked-repo resolution is not yet wired in ProcessFlat, so it is covered
// here at the gate level). The gates are tri-state: an inactive gate admits the
// record.
func TestPrefetchGate(t *testing.T) {
	proc := &SessionAnalysisProcessor{}
	oldDate := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seatedCtx := func() context.Context {
		c := planscoped.WithConfigForTest(context.Background(), planscoped.Config{SeatsLimit: 1, DataWindowDays: -1})
		return planscoped.WithSeats(c, planscoped.SeatsContext{PersonIDs: []string{"p1"}})
	}

	tests := []struct {
		name       string
		ctx        context.Context
		meta       *rawRecordMetadata
		params     dao.SessionAnalysisScanParams
		wantReason sessionintelligencepb.SessionAnalysisSkipReason
		wantGated  bool
	}{
		{
			name:       "child",
			ctx:        context.Background(),
			meta:       &rawRecordMetadata{PersonID: "p1", RelationType: lineage.RelationTypeSubagent, ParentRecordKey: "pk"},
			wantReason: sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD,
			wantGated:  true,
		},
		{
			// A genuine child hook run folds too — lineage.IsChild's positive list.
			name:       "child hook",
			ctx:        seatedCtx(),
			meta:       &rawRecordMetadata{PersonID: "p1", RelationType: lineage.RelationTypeHook},
			wantReason: sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD,
			wantGated:  true,
		},
		{
			// Finding 3: an explicit PRIMARY is a top-level session, never folded — even
			// with a parent_record_key present. The presence-check gate used to drop it.
			name:      "primary is not a child",
			ctx:       seatedCtx(),
			meta:      &rawRecordMetadata{PersonID: "p1", RelationType: lineage.RelationTypePrimary, ParentRecordKey: "pk"},
			wantGated: false,
		},
		{
			// A bare parent_record_key with no child relation_type is not a child either —
			// classification is relation_type-only (the session fold contract).
			name:      "parent key without relation type is not a child",
			ctx:       seatedCtx(),
			meta:      &rawRecordMetadata{PersonID: "p1", ParentRecordKey: "pk"},
			wantGated: false,
		},
		{
			name:       "out of window",
			ctx:        planscoped.WithConfigForTest(context.Background(), planscoped.Config{SeatsLimit: -1, DataWindowDays: 1, OrgCreatedAt: time.Now()}),
			meta:       &rawRecordMetadata{PersonID: "p1"},
			wantReason: sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_OUT_OF_WINDOW,
			wantGated:  true,
		},
		{
			name:       "no seat",
			ctx:        seatedCtx(),
			meta:       &rawRecordMetadata{PersonID: "unseated"},
			wantReason: sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_NO_SEAT,
			wantGated:  true,
		},
		{
			name:       "repo untracked",
			ctx:        context.Background(),
			meta:       &rawRecordMetadata{PersonID: "p1", RepoName: "org/untracked"},
			params:     dao.SessionAnalysisScanParams{RepoFilterEnabled: true, TrackedRepos: []string{"org/tracked"}},
			wantReason: sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_REPO_UNTRACKED_METADATA,
			wantGated:  true,
		},
		{
			name:      "repo tracked passes",
			ctx:       context.Background(),
			meta:      &rawRecordMetadata{PersonID: "p1", RepoName: "org/tracked"},
			params:    dao.SessionAnalysisScanParams{RepoFilterEnabled: true, TrackedRepos: []string{"org/tracked"}},
			wantGated: false,
		},
		{
			name:      "repo filter inactive fails open",
			ctx:       context.Background(),
			meta:      &rawRecordMetadata{PersonID: "p1", RepoName: "org/anything"},
			params:    dao.SessionAnalysisScanParams{RepoFilterEnabled: false},
			wantGated: false,
		},
		{
			name:      "fully clear",
			ctx:       seatedCtx(),
			meta:      &rawRecordMetadata{PersonID: "p1"},
			wantGated: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := &model.RawRecordModel{DataDate: oldDate}
			reason, gated := proc.prefetchGate(tt.ctx, record, tt.meta, tt.params)
			assert.Equal(t, tt.wantGated, gated)
			if tt.wantGated {
				assert.Equal(t, tt.wantReason, reason)
			}
		})
	}
}
