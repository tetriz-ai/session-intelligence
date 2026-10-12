// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// stateWithEvidenceStatus builds a state row whose EVIDENCE call carries the given status,
// pending artifact ids, and stamped body digest.
func stateWithEvidenceStatus(status sessionintelligencepb.CallStatus, artifactIDs []string, sha string) *model.SessionAnalysisState {
	return &model.SessionAnalysisState{
		CallState: &sessionintelligencepb.CallStateMap{Calls: map[string]*sessionintelligencepb.CallState{
			producerEvidence: {
				Status:              status,
				PendingArtifactIds:  artifactIDs,
				SourceContentSha256: sha,
			},
		}},
	}
}

func TestEvidenceRePickDecision(t *testing.T) {
	allResolved := func(string) (bool, error) { return true, nil }
	noneResolved := func(string) (bool, error) { return false, nil }
	onlyA := func(id string) (bool, error) { return id == "a", nil }
	readErr := func(string) (bool, error) { return false, errors.New("db down") }

	tests := []struct {
		name       string
		prior      *model.SessionAnalysisState
		currentSha string
		resolved   func(string) (bool, error)
		want       rePickDecision
		wantErr    bool
	}{
		{
			name:       "no evidence pending state resolves (nothing to wait on)",
			prior:      stateWithEvidenceStatus(sessionintelligencepb.CallStatus_CALL_STATUS_DONE, nil, "sha1"),
			currentSha: "sha1",
			resolved:   noneResolved,
			want:       rePickResolved,
		},
		{
			name:       "body digest changed since pending abandons the snapshot",
			prior:      stateWithEvidenceStatus(sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA, []string{"a", "b"}, "sha1"),
			currentSha: "sha2",
			resolved:   allResolved,
			want:       rePickResync,
		},
		{
			name:       "any pending artifact still unresolved re-skips cheaply",
			prior:      stateWithEvidenceStatus(sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA, []string{"a", "b"}, "sha1"),
			currentSha: "sha1",
			resolved:   onlyA,
			want:       rePickReSkip,
		},
		{
			name:       "every pending artifact resolved carries R and re-runs E",
			prior:      stateWithEvidenceStatus(sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA, []string{"a", "b"}, "sha1"),
			currentSha: "sha1",
			resolved:   allResolved,
			want:       rePickResolved,
		},
		{
			name:       "pending with no ids resolves (empty dependency set)",
			prior:      stateWithEvidenceStatus(sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA, nil, "sha1"),
			currentSha: "sha1",
			resolved:   noneResolved,
			want:       rePickResolved,
		},
		{
			name:       "a real intent-read DB error propagates (defer non-counting, not re-skip)",
			prior:      stateWithEvidenceStatus(sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA, []string{"a", "b"}, "sha1"),
			currentSha: "sha1",
			resolved:   readErr,
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := evidenceRePickDecision(tt.prior, tt.currentSha, tt.resolved)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestSetPendingDataAndFinalizeMasks pins that setPendingData stamps the pending snapshot and
// that finalize recomputes BOTH the flag-skipped and pending-data masks from call_state — a
// producer that resolves clears its pending bit (recompute, not clear-on-write).
func TestSetPendingDataAndFinalizeMasks(t *testing.T) {
	sp := &spineResult{}

	// E goes pending: EVIDENCE status + snapshot set, pending bit set, flag mask untouched.
	sp.setPendingData(producerEvidence, 3, []string{"a", "b"}, "sha1")
	call := sp.callState.GetCalls()[producerEvidence]
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA, call.GetStatus())
	assert.Equal(t, []string{"a", "b"}, call.GetPendingArtifactIds())
	assert.Equal(t, "sha1", call.GetSourceContentSha256())
	assert.Equal(t, int32(3), call.GetContractVersion())
	assert.Equal(t, bitEvidence, sp.pendingDataProducers)
	assert.Equal(t, int32(0), sp.flagSkippedProducers)

	// R is flag-dark alongside E-pending: finalize computes both masks in one pass.
	sp.setCallStatus(producerRead, sessionintelligencepb.CallStatus_CALL_STATUS_FLAG_SKIPPED, 1)
	assert.Equal(t, bitRead, sp.flagSkippedProducers)
	assert.Equal(t, bitEvidence, sp.pendingDataProducers)

	// E resolves on a later pass: its pending bit clears (recompute, not clear-on-write).
	sp.setCallStatus(producerEvidence, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, 3)
	assert.Equal(t, int32(0), sp.pendingDataProducers)
	assert.Equal(t, bitRead, sp.flagSkippedProducers)
}
