// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

// Regression guard for the ownership fix:
// writeRoundTripFaults must touch session_round_trip_faults ONLY on a pass E actually ran
// on. A full R re-judge where E did not fire (no artifact/round-trip trigger) must preserve
// the prior generation's fault set rather than replacing it with an empty one.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao/mocks"
)

// TestBodyChangedSinceClaim pins the commit-time freshness re-read that gates the Decision-37
// stale-security skip + fast re-admit. Both branches: a hash-bearing body compares the current
// payload content_sha256 to the digest the producers analyzed; a LEGACY body (current digest "")
// falls back to comparing raw_records.updated_at against the claim-time value. Each branch must
// report unchanged (false) on a match and changed (true) on a move.
func TestBodyChangedSinceClaim(t *testing.T) {
	claimedSha := "sha-analyzed"
	claimedAt := time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC)
	movedAt := claimedAt.Add(time.Hour)

	tests := []struct {
		name           string
		currentDigest  string
		currentUpdated time.Time // consulted only on the legacy branch (currentDigest == "")
		wantChanged    bool
	}{
		{name: "hash-bearing unchanged", currentDigest: claimedSha, wantChanged: false},
		{name: "hash-bearing changed", currentDigest: "sha-reuploaded", wantChanged: true},
		{name: "legacy unchanged (updated_at matches)", currentDigest: "", currentUpdated: claimedAt, wantChanged: false},
		{name: "legacy changed (updated_at moved)", currentDigest: "", currentUpdated: movedAt, wantChanged: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			rawDAO := mocks.NewMockIRawRecordDAO(ctrl)
			record := &model.RawRecordModel{ID: "raw-1", DataDate: claimedAt}
			rawDAO.EXPECT().CurrentContentDigest(gomock.Any(), "raw-1", record.DataDate).Return(tt.currentDigest, nil)
			if tt.currentDigest == "" {
				// Only the legacy branch re-reads the source updated_at.
				rawDAO.EXPECT().CurrentSourceUpdatedAt(gomock.Any(), "raw-1", record.DataDate).Return(tt.currentUpdated, nil)
			} else {
				rawDAO.EXPECT().CurrentSourceUpdatedAt(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
			}
			p := &SessionAnalysisProcessor{rawRecordDAO: rawDAO}

			changed, err := p.bodyChangedSinceClaim(context.Background(), record, claimedSha, claimedAt)
			require.NoError(t, err)
			assert.Equal(t, tt.wantChanged, changed)
		})
	}
}

// TestWriteRoundTripFaults_EvidenceDidNotRun_PreservesPriorFaults asserts an R-only
// re-pass (read != nil, read.EvidenceRan == false) never calls ReplaceSet — the E-owned
// fault set from a prior generation where E DID fire must survive a later pass that
// re-judges the session without E refiring.
func TestWriteRoundTripFaults_EvidenceDidNotRun_PreservesPriorFaults(t *testing.T) {
	ctrl := gomock.NewController(t)
	roundTripFaultDAO := mocks.NewMockISessionRoundTripFaultDAO(ctrl)
	// No EXPECT() on ReplaceSet — any call fails the test via gomock's unexpected-call panic.
	p := &SessionAnalysisProcessor{sessionRoundTripFaultDAO: roundTripFaultDAO}

	record := &model.RawRecordModel{ID: "raw-1", OrganizationID: "org-1"}
	sp := &spineResult{conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	// read != nil (a full R re-judge ran) but EvidenceRan is false — R re-caught the
	// session without triggering E this time.
	read := &ReadResult{EvidenceRan: false}

	err := p.writeRoundTripFaults(context.Background(), "org-1", record, "session-1", sp, read)
	if err != nil {
		t.Fatalf("writeRoundTripFaults returned an error: %v", err)
	}
}

// TestWriteRoundTripFaults_EvidenceRan_CallsReplaceSet is the positive-path sibling: when
// E DID run this pass, writeRoundTripFaults must call ReplaceSet — including with an empty
// set when E ran and found no faults, which correctly clears any prior generation's rows.
func TestWriteRoundTripFaults_EvidenceRan_CallsReplaceSet(t *testing.T) {
	ctrl := gomock.NewController(t)
	roundTripFaultDAO := mocks.NewMockISessionRoundTripFaultDAO(ctrl)
	roundTripFaultDAO.EXPECT().
		ReplaceSet(gomock.Any(), "org-1", "session-1", gomock.Any()).
		Return(nil).
		Times(1)
	p := &SessionAnalysisProcessor{sessionRoundTripFaultDAO: roundTripFaultDAO}

	record := &model.RawRecordModel{ID: "raw-1", OrganizationID: "org-1"}
	sp := &spineResult{conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	read := &ReadResult{EvidenceRan: true}

	if err := p.writeRoundTripFaults(context.Background(), "org-1", record, "session-1", sp, read); err != nil {
		t.Fatalf("writeRoundTripFaults returned an error: %v", err)
	}
}

// TestBuildSessionAnalysis_DiscardDisposition pins D's readiness: on a judged pass (read != nil)
// it is PRODUCED when D ran (discard != nil) and NOT_APPLICABLE when D did not fire (a settled
// absence — D never pends); a measured-only build (read == nil) leaves it nil so the measured
// upserts never publish a disposition for an unevaluated producer.
func TestBuildSessionAnalysis_DiscardDisposition(t *testing.T) {
	produced := sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_PRODUCED
	notApplicable := sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_NOT_APPLICABLE
	tests := []struct {
		name    string
		read    *ReadResult
		discard *DiscardResult
		want    *sessionintelligencepb.ProducerReadiness
	}{
		{name: "judged, D ran -> PRODUCED", read: &ReadResult{}, discard: &DiscardResult{}, want: &produced},
		{name: "judged, D not fired -> NOT_APPLICABLE", read: &ReadResult{}, discard: nil, want: &notApplicable},
		{name: "measured-only (read nil) -> nil", read: nil, discard: nil, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := &model.RawRecordModel{ID: "raw-1", OrganizationID: "org-1"}
			sp := &spineResult{
				conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				disposition:           sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PRODUCED,
			}
			analysis := buildSessionAnalysisRow("org-1", record, "person-1", sp, tt.read, tt.discard, true, buildMeasuredTurnRows("org-1", record.ID, sp))
			assert.Equal(t, tt.want, analysis.DiscardDisposition)
			//: engine-produced rows are stamped LIVE (vs the copy backfill's SESSION_COPY).
			if assert.NotNil(t, analysis.Origin, "buildSessionAnalysis stamps origin") {
				assert.Equal(t, sessionintelligencepb.SessionAnalysisOrigin_SESSION_ANALYSIS_ORIGIN_LIVE, *analysis.Origin)
			}
		})
	}
}

// measuredWriteTestConfig embeds Config (nil); writeMeasuredTurns consults no config method, so
// the test needs no full Config stub — the embedded nil panics loudly on any unexpected call.
type measuredWriteTestConfig struct{ Config }

// TestWriteMeasuredTurns_EmptyReparseRetires pins the empty-re-parse retire: a re-parse that
// yields zero prompts but is NOT spine-only must still run SoftDeleteMeasuredBeyond to retire
// the whole prior tail (maxPromptIndex stays -1 → every turn > -1 is soft-deleted). Only a
// genuine spine-only reprocess (sp.spineOnly) short-circuits and leaves prior turns intact.
func TestWriteMeasuredTurns_EmptyReparseRetires(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		sp             *spineResult
		wantSoftDelete bool
	}{
		{
			name:           "empty re-parse (not spine-only) retires the whole prior tail",
			sp:             &spineResult{prompts: nil, spineOnly: false, ideType: sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE},
			wantSoftDelete: true,
		},
		{
			name:           "spine-only reprocess leaves prior turns untouched",
			sp:             &spineResult{prompts: nil, spineOnly: true},
			wantSoftDelete: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			turnDAO := mocks.NewMockISessionTurnAnalysisDAO(ctrl)
			if tt.wantSoftDelete {
				// No turns to upsert; the tail retire runs with maxPromptIndex -1 (retire all).
				turnDAO.EXPECT().
					SoftDeleteMeasuredBeyond(gomock.Any(), "org-1", "rr-1", int32(-1)).
					Return(int64(2), nil)
				turnDAO.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Times(0)
			} else {
				turnDAO.EXPECT().SoftDeleteMeasuredBeyond(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
				turnDAO.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Times(0)
			}

			p := &SessionAnalysisProcessor{sessionTurnDAO: turnDAO, cfg: measuredWriteTestConfig{}}
			record := &model.RawRecordModel{ID: "rr-1", OrganizationID: "org-1"}
			err := p.writeMeasuredTurns(context.Background(), "org-1", record, tt.sp, buildMeasuredTurnRows("org-1", record.ID, tt.sp))
			if err != nil {
				t.Fatalf("writeMeasuredTurns: %v", err)
			}
		})
	}
}
