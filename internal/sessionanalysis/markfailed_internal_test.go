// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	daomocks "github.com/tetriz-ai/session-intelligence/internal/dao/mocks"
)

// markFailedTestConfig satisfies cfg for markFailed's simple path: a retryable (below-cap)
// record-class failure with the compat write off. The embedded nil Config is never reached —
// a non-transient cause short-circuits the outage lookups, and CompatWriteEnabled=false makes
// the compat stamp a no-op.
type markFailedTestConfig struct{ Config }

func (markFailedTestConfig) RetryCap() int32                   { return 3 }
func (markFailedTestConfig) CompatWriteEnabled() bool          { return false }
func (markFailedTestConfig) OutageSignalWindow() time.Duration { return 25 * time.Minute }
func (markFailedTestConfig) OutageSignalMinRecords() int       { return 5 }

// TestMarkFailed_StampsUnderFreshCtxWhenInputExpired is the regression guard for the silent
// re-pick loop (markFailed's own ctx-detach comment carries why the fresh ctx is required). It
// distinguishes the fix from the bug two ways at once (surviving-mutants): with an already-
// cancelled input ctx, TransitionToFailed must (a) still be called and (b) receive a LIVE ctx
// (Err()==nil). Reverting to the expired ctx either drops the call (RunInScope short-circuits)
// or delivers a cancelled ctx — both fail this test.
func TestMarkFailed_StampsUnderFreshCtxWhenInputExpired(t *testing.T) {
	ctrl := gomock.NewController(t)
	rawDAO := daomocks.NewMockIRawRecordDAO(ctrl)
	state := daomocks.NewMockISessionAnalysisStateDAO(ctrl)
	p := &SessionAnalysisProcessor{rawRecordDAO: rawDAO, sessionStateDAO: state, cfg: markFailedTestConfig{}}

	orgID := uuid.NewString()
	rawRecordID := uuid.NewString()
	dataDate := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)

	// An already-cancelled input ctx stands in for the expired per-record deadline.
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	rawDAO.EXPECT().CurrentContentDigest(gomock.Any(), rawRecordID, dataDate).Return("sha1", nil)
	rawDAO.EXPECT().CurrentSourceUpdatedAt(gomock.Any(), rawRecordID, dataDate).Return(time.Time{}, nil)

	var gotCtxErr error
	state.EXPECT().
		TransitionToFailed(gomock.Any(), rawRecordID, "tok", gomock.Any(), true, "sha1", gomock.Any()).
		DoAndReturn(func(sctx context.Context, _, _, _ string, _ bool, _ string, _ time.Time) (bool, int32, error) {
			gotCtxErr = sctx.Err()
			return true, int32(1), nil // below RetryCap → retryable, no JUDGE_FAILED path
		})

	p.markFailed(expired, orgID, rawRecordID, dataDate, "tok", errors.New("commit cut mid-flight"))

	require.NoError(t, gotCtxErr,
		"TransitionToFailed must run under a live (detached) ctx even though markFailed's input ctx had expired; a cancelled ctx here is the silent re-pick loop")
}
