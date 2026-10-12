// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// TestCompatProcessingStatus pins's mapping: JUDGED and every conditional
// SKIPPED reproduce the old processor's 'processed' (its seat/window/repo scoping was
// serving-side), only a hard FAILED maps to 'failed', and every non-terminal /
// not-this-generation state is left unstamped so it reads 'pending' exactly as an
// in-flight row did. Mapping a skip to 'skipped' would shrink the loops-retry + BQ sets,
// which the negative assertions here guard against.
func TestCompatProcessingStatus(t *testing.T) {
	tests := []struct {
		name       string
		status     sessionintelligencepb.SessionAnalysisStatus
		wantStatus model.ProcessingStatus
		wantOK     bool
	}{
		{"judged → processed", sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, model.ProcessingStatusProcessed, true},
		{"skipped → processed", sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, model.ProcessingStatusProcessed, true},
		{"failed → failed", sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_FAILED, model.ProcessingStatusFailed, true},
		{"pending → unstamped", sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_PENDING, "", false},
		{"spine_done → unstamped", sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SPINE_DONE, "", false},
		{"superseded → unstamped", sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SUPERSEDED, "", false},
		{"unspecified → unstamped", sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_UNSPECIFIED, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := compatProcessingStatus(tt.status)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantStatus, got)
			// A skip must never map to 'skipped' — that would shrink the loops-retry set.
			assert.NotEqual(t, model.ProcessingStatusSkipped, got)
		})
	}
}
