// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/tetriz-ai/session-intelligence/internal/dao"
	sa "github.com/tetriz-ai/session-intelligence/internal/sessionanalysis"
)

// TestProcessFlat_SwallowedFailureIncrementsMetric is the run-grain observability guard: a
// per-record failure is isolated to a nil return so it never fails the batch (ProcessFlat still
// returns success), but it MUST surface in records_failed_total — otherwise a swallowed failure is
// invisible under a "completed" processor_runs row. A unique org isolates the counter label so the
// before/after delta is exact.
func TestProcessFlat_SwallowedFailureIncrementsMetric(t *testing.T) {
	h := newHarness(t)
	orgID := uuid.NewString()
	record := sessionRawRecord(t, orgID, uuid.NewString(), "sha1")

	before := testutil.ToFloat64(sa.RecordsFailedTotalForTest.WithLabelValues(orgID))

	h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
	// A generic (non-deferred) load error fails processCandidate → the swallow branch, not the
	// deferred branch (which returns before the failed count). detector/intent-unavailable would
	// defer instead and must NOT increment; a plain error is the record's own failure.
	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(nil, errors.New("boom"))

	processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, err, "an isolated per-record failure never fails the batch")
	assert.Equal(t, 0, processed, "the failed record is not counted as processed")

	after := testutil.ToFloat64(sa.RecordsFailedTotalForTest.WithLabelValues(orgID))
	assert.Equal(t, before+1, after, "the swallowed per-record failure is surfaced in records_failed_total")
}
