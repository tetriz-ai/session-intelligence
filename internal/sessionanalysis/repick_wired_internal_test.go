// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	daomocks "github.com/tetriz-ai/session-intelligence/internal/dao/mocks"
)

// errResolverProbe is a sentinel the resolver-error case returns, to prove a resolver fault falls
// through to the full path rather than short-circuiting.
var errResolverProbe = errors.New("resolver probe error")

// recordWithSha builds a raw record whose payload carries the given content_sha256, the anchor
// pendingDataStillUnresolved compares against without reading S3.
func recordWithSha(orgID, rawRecordID, sha string) *model.RawRecordModel {
	payload, _ := json.Marshal(map[string]string{"s3_key": "s3://b/k", "content_sha256": sha})
	return &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID, Payload: payload}
}

// repickTestConfig implements only the Config methods the re-pick paths in runProducers touch;
// the embedded (nil) Config makes any un-touched method panic loudly rather than pass silently.
type repickTestConfig struct{ Config }

func (repickTestConfig) ReadEnabled(string) bool        { return true }
func (repickTestConfig) EvidenceEnabled(string) bool    { return false }
func (repickTestConfig) UseSystemPromptCache() bool     { return false }
func (repickTestConfig) ContractVersionRead() int32     { return 1 }
func (repickTestConfig) ContractVersionEvidence() int32 { return 1 }
func (repickTestConfig) ContractVersionDiscard() int32  { return 1 }

// countingProducer is a hand-written LLMProducer (the generated mock cannot be used from an
// in-package test — mocks imports this package, which would cycle). It counts calls so the
// re-pick tests can assert exactly which producers ran.
type countingProducer struct {
	readCalls, evidenceCalls, discardCalls int
	readResult                             *ReadResult
	// evidenceErr/discardErr, when set, make the matching producer error — so a test can drive the
	// per-producer failure isolation (R survives, only the failed producer is marked FAILED).
	evidenceErr, discardErr error
}

func (c *countingProducer) AnalyzeRead(context.Context, ProducerRequest) (*ReadResult, error) {
	c.readCalls++
	return c.readResult, nil
}

func (c *countingProducer) AnalyzeEvidence(context.Context, ProducerRequest) (*EvidenceResult, error) {
	c.evidenceCalls++
	if c.evidenceErr != nil {
		return nil, c.evidenceErr
	}
	return &EvidenceResult{}, nil
}

func (c *countingProducer) AnalyzeDiscard(context.Context, ProducerRequest) (*DiscardResult, error) {
	c.discardCalls++
	if c.discardErr != nil {
		return nil, c.discardErr
	}
	return &DiscardResult{}, nil
}

// priorWithEvidencePending builds a JUDGED state row whose E half is SKIPPED_PENDING_DATA,
// carrying the persisted dependency snapshot the re-pick reads. D is recorded settled (NOT_FIRED
// at the current contract) so a re-pick sees E as the only unsettled producer; a case that needs a
// re-runnable D overrides this entry.
func priorWithEvidencePending(orgID, rawRecordID, sha string, artifactIDs []string) *model.SessionAnalysisState {
	return &model.SessionAnalysisState{
		OrganizationID: orgID,
		RawRecordID:    rawRecordID,
		Status:         sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED,
		CallState: &sessionintelligencepb.CallStateMap{Calls: map[string]*sessionintelligencepb.CallState{
			producerRead: {Status: sessionintelligencepb.CallStatus_CALL_STATUS_DONE, ContractVersion: 1},
			producerEvidence: {
				Status:              sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA,
				ContractVersion:     1,
				PendingArtifactIds:  artifactIDs,
				SourceContentSha256: sha,
			},
			producerDiscard: {Status: sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED, ContractVersion: 1},
		}},
	}
}

// TestRunProducers_EvidenceRePick_ReSkip: the intent is still unresolved and the body is
// unchanged, so the re-pick re-skips cheaply — no producer runs, E stays SKIPPED_PENDING_DATA
// with its snapshot refreshed, the pending bit stays set, and the measured-reaffirm commit path
// is taken (errProducersSkipped + spineOnly).
func TestRunProducers_EvidenceRePick_ReSkip(t *testing.T) {
	orgID, rawRecordID := "org", "rr"
	prior := priorWithEvidencePending(orgID, rawRecordID, "sha1", []string{"a1"})
	prod := &countingProducer{}
	p := &SessionAnalysisProcessor{
		producer:         prod,
		cfg:              repickTestConfig{},
		evidenceResolver: func(context.Context, string, string) (bool, error) { return false, nil }, // nothing resolves
	}
	sp := &spineResult{contentSha256: "sha1"} // unchanged body

	read, discard, err := p.runProducers(context.Background(), orgID, &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, prior, sp, nil)

	require.ErrorIs(t, err, errProducersSkipped)
	assert.Nil(t, read)
	assert.Nil(t, discard)
	assert.Equal(t, 0, prod.readCalls, "R must not re-infer on a re-skip")
	assert.Equal(t, 0, prod.evidenceCalls, "E must not run on a re-skip (no detector/LLM)")
	assert.True(t, sp.spineOnly, "re-skip takes the measured-reaffirm commit path")
	assert.Equal(t, bitEvidence, sp.pendingDataProducers, "the E pending bit stays set")
	got := sp.callState.GetCalls()[producerEvidence]
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA, got.GetStatus())
	assert.Equal(t, []string{"a1"}, got.GetPendingArtifactIds(), "the pending snapshot is re-persisted")
	assert.Equal(t, sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PARTIAL, sp.disposition,
		"an E-pending record is PARTIAL (E has not committed), never PRODUCED")
}

// TestRunProducers_EvidenceRePick_Resolved: the intent has landed, so R is carried by
// rehydration (never re-inferred) and E's now-resolvable half runs; the pending bit clears.
func TestRunProducers_EvidenceRePick_Resolved(t *testing.T) {
	ctrl := gomock.NewController(t)
	orgID, rawRecordID, sessionAnalysisID := "org", "rr", "said"
	prior := priorWithEvidencePending(orgID, rawRecordID, "sha1", []string{"a1"})
	prod := &countingProducer{}

	analysisDAO := daomocks.NewMockISessionAnalysisDAO(ctrl)
	turnDAO := daomocks.NewMockISessionTurnAnalysisDAO(ctrl)
	phaseDAO := daomocks.NewMockISessionPhaseAnalysisDAO(ctrl)
	analysisDAO.EXPECT().GetByRawRecordID(gomock.Any(), rawRecordID).
		Return(&model.SessionAnalysis{ID: sessionAnalysisID, OrganizationID: orgID, RawRecordID: rawRecordID}, nil)
	turnDAO.EXPECT().ListByRawRecordID(gomock.Any(), rawRecordID).Return(nil, nil)
	phaseDAO.EXPECT().ListBySessionAnalysisID(gomock.Any(), sessionAnalysisID).Return(nil, nil)

	p := &SessionAnalysisProcessor{
		producer:           prod,
		cfg:                repickTestConfig{},
		evidenceResolver:   func(context.Context, string, string) (bool, error) { return true, nil }, // a1 now resolves
		sessionAnalysisDAO: analysisDAO,
		sessionTurnDAO:     turnDAO,
		sessionPhaseDAO:    phaseDAO,
	}
	// artifactInvocations>0 fires E regardless of the carried turns.
	sp := &spineResult{contentSha256: "sha1", ledgers: spineLedgers{artifactInvocations: 1}}

	read, _, err := p.runProducers(context.Background(), orgID, &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, prior, sp, nil)

	require.NoError(t, err)
	require.NotNil(t, read, "R is carried by rehydration")
	assert.Equal(t, 0, prod.readCalls, "R must NOT be re-inferred — it is carried by rehydration")
	assert.Equal(t, 1, prod.evidenceCalls, "E's now-resolvable half runs")
	assert.Equal(t, int32(0), sp.pendingDataProducers, "the pending bit clears once E runs")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, sp.callState.GetCalls()[producerEvidence].GetStatus())
}

// TestRunProducers_EvidenceRePick_Resolved_CarriesSettledDiscard: on the resolved re-pick where D
// already completed (DONE, PRODUCED), R and D are BOTH carried while only E re-runs — the body is
// unchanged, so re-firing D would re-bill AnalyzeDiscard and could overwrite the committed waste
// ledger with a nondeterministic result. The carried discard returns the prior generation's ranges
// so the commit re-writes an identical ledger, and D's call state stays DONE.
func TestRunProducers_EvidenceRePick_Resolved_CarriesSettledDiscard(t *testing.T) {
	ctrl := gomock.NewController(t)
	orgID, rawRecordID, sessionAnalysisID := "org", "rr", "said"
	prior := priorWithEvidencePending(orgID, rawRecordID, "sha1", []string{"a1"})
	prior.CallState.Calls[producerDiscard] = &sessionintelligencepb.CallState{Status: sessionintelligencepb.CallStatus_CALL_STATUS_DONE, ContractVersion: 1}
	prod := &countingProducer{}

	priorRanges := &sessionintelligencepb.DiscardedRangeList{Items: []*sessionintelligencepb.DiscardedRange{{RangeStart: 2, RangeEnd: 3}}}
	analysisDAO := daomocks.NewMockISessionAnalysisDAO(ctrl)
	turnDAO := daomocks.NewMockISessionTurnAnalysisDAO(ctrl)
	phaseDAO := daomocks.NewMockISessionPhaseAnalysisDAO(ctrl)
	// GetByRawRecordID is read twice: once by carryReadDone (R rehydrate) and once by carryDiscardDone
	// (D rehydrate). The second read is what supplies the committed ranges the carried discard returns.
	analysisDAO.EXPECT().GetByRawRecordID(gomock.Any(), rawRecordID).
		Return(&model.SessionAnalysis{ID: sessionAnalysisID, OrganizationID: orgID, RawRecordID: rawRecordID, DiscardedRanges: priorRanges}, nil).
		Times(2)
	turnDAO.EXPECT().ListByRawRecordID(gomock.Any(), rawRecordID).Return(nil, nil)
	phaseDAO.EXPECT().ListBySessionAnalysisID(gomock.Any(), sessionAnalysisID).Return(nil, nil)

	p := &SessionAnalysisProcessor{
		producer:           prod,
		cfg:                repickTestConfig{},
		evidenceResolver:   func(context.Context, string, string) (bool, error) { return true, nil }, // a1 now resolves
		sessionAnalysisDAO: analysisDAO,
		sessionTurnDAO:     turnDAO,
		sessionPhaseDAO:    phaseDAO,
	}
	// artifactInvocations>0 fires E; revertSignals>0 would fire D on a fresh run, but the carry must
	// short-circuit that so D never re-runs.
	sp := &spineResult{contentSha256: "sha1", ledgers: spineLedgers{artifactInvocations: 1, revertSignals: 1}}

	read, discard, err := p.runProducers(context.Background(), orgID, &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, prior, sp, nil)

	require.NoError(t, err)
	require.NotNil(t, read, "R is carried by rehydration")
	assert.Equal(t, 0, prod.readCalls, "R must NOT be re-inferred")
	assert.Equal(t, 1, prod.evidenceCalls, "only E re-runs")
	assert.Equal(t, 0, prod.discardCalls, "a settled DONE D is carried, never re-billed on the resolved re-pick")
	require.NotNil(t, discard, "the carried discard is returned so the commit re-writes the same ledger")
	require.NotNil(t, discard.DiscardedRanges)
	require.Len(t, discard.DiscardedRanges.GetItems(), 1, "the prior generation's committed ranges are carried")
	assert.Equal(t, int32(2), discard.DiscardedRanges.GetItems()[0].GetRangeStart())
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, sp.callState.GetCalls()[producerDiscard].GetStatus(), "D stays DONE")
}

// TestRunProducers_EvidenceRePick_Resync: the body changed since E went pending, so the pending
// snapshot is abandoned and R + E re-run in full; the pending bit clears.
func TestRunProducers_EvidenceRePick_Resync(t *testing.T) {
	orgID, rawRecordID := "org", "rr"
	prior := priorWithEvidencePending(orgID, rawRecordID, "sha1", []string{"a1"})
	prod := &countingProducer{
		readResult: &ReadResult{Turns: []*TurnJudgment{{PromptIndex: 0}}},
	}
	p := &SessionAnalysisProcessor{
		producer:         prod,
		cfg:              repickTestConfig{},
		evidenceResolver: func(context.Context, string, string) (bool, error) { return true, nil }, // irrelevant: sha changed first
	}
	// contentSha256 differs from the pending snapshot's "sha1" → resync. turnIndexMap maps the
	// single rendered turn (sequence 0) back to raw index 0 so the coverage check passes.
	sp := &spineResult{contentSha256: "sha2", turnIndexMap: []int32{0}, ledgers: spineLedgers{artifactInvocations: 1}}

	read, _, err := p.runProducers(context.Background(), orgID, &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, prior, sp, nil)

	require.NoError(t, err)
	require.NotNil(t, read)
	assert.Equal(t, 1, prod.readCalls, "a changed body re-infers R in full")
	assert.Equal(t, 1, prod.evidenceCalls, "E re-runs in full")
	assert.Equal(t, int32(0), sp.pendingDataProducers, "the pending bit clears on a full re-run")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, sp.callState.GetCalls()[producerRead].GetStatus())
}

// repickReadOffConfig is repickTestConfig with the master read gate off, exercising the
// flag-skip early-return path in runProducers.
type repickReadOffConfig struct{ repickTestConfig }

func (repickReadOffConfig) ReadEnabled(string) bool { return false }

// TestRunProducers_EvidenceRePick_ReadOffPreservesPending: when the master read gate is off, R is
// flag-skipped and no producer runs, but E's pending snapshot + bit must SURVIVE the commit so
// the scan keeps re-admitting it (rather than dropping it and forcing a full paid re-run when
// the org is re-enabled).
func TestRunProducers_EvidenceRePick_ReadOffPreservesPending(t *testing.T) {
	orgID, rawRecordID := "org", "rr"
	prior := priorWithEvidencePending(orgID, rawRecordID, "sha1", []string{"a1"})
	prod := &countingProducer{}
	p := &SessionAnalysisProcessor{
		producer:         prod,
		cfg:              repickReadOffConfig{},
		evidenceResolver: func(context.Context, string, string) (bool, error) { return true, nil }, // read-off short-circuits before this matters
	}
	sp := &spineResult{contentSha256: "sha1"}

	read, discard, err := p.runProducers(context.Background(), orgID, &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, prior, sp, nil)

	require.ErrorIs(t, err, errProducersSkipped)
	assert.Nil(t, read)
	assert.Nil(t, discard)
	assert.Equal(t, 0, prod.readCalls)
	assert.Equal(t, 0, prod.evidenceCalls)
	assert.Equal(t, bitEvidence, sp.pendingDataProducers, "the E pending bit survives the telemetry-off cycle")
	e := sp.callState.GetCalls()[producerEvidence]
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA, e.GetStatus())
	assert.Equal(t, []string{"a1"}, e.GetPendingArtifactIds(), "the pending dependency snapshot survives")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_FLAG_SKIPPED, sp.callState.GetCalls()[producerRead].GetStatus())
	assert.Equal(t, sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PARTIAL, sp.disposition)
}

// TestRunProducers_EvidenceRePick_ReSkipRunsUnsettledD: a record that is BOTH E-pending AND
// D-unsettled (D just enabled) runs D independently — a pending E must not block D — while E
// stays SKIPPED_PENDING_DATA and R is carried, not re-inferred.
func TestRunProducers_EvidenceRePick_ReSkipRunsUnsettledD(t *testing.T) {
	ctrl := gomock.NewController(t)
	orgID, rawRecordID, sessionAnalysisID := "org", "rr", "said"
	prior := priorWithEvidencePending(orgID, rawRecordID, "sha1", []string{"a1"})
	// A prior generation left D flag-skipped; category 9 re-admits it, so it is independently
	// unsettled and re-runs on this pass.
	prior.CallState.Calls[producerDiscard] = &sessionintelligencepb.CallState{Status: sessionintelligencepb.CallStatus_CALL_STATUS_FLAG_SKIPPED, ContractVersion: 1}
	prod := &countingProducer{}

	analysisDAO := daomocks.NewMockISessionAnalysisDAO(ctrl)
	turnDAO := daomocks.NewMockISessionTurnAnalysisDAO(ctrl)
	phaseDAO := daomocks.NewMockISessionPhaseAnalysisDAO(ctrl)
	analysisDAO.EXPECT().GetByRawRecordID(gomock.Any(), rawRecordID).
		Return(&model.SessionAnalysis{ID: sessionAnalysisID, OrganizationID: orgID, RawRecordID: rawRecordID}, nil)
	turnDAO.EXPECT().ListByRawRecordID(gomock.Any(), rawRecordID).Return(nil, nil)
	phaseDAO.EXPECT().ListBySessionAnalysisID(gomock.Any(), sessionAnalysisID).Return(nil, nil)

	p := &SessionAnalysisProcessor{
		producer:           prod,
		cfg:                repickTestConfig{},
		evidenceResolver:   func(context.Context, string, string) (bool, error) { return false, nil }, // E still unresolved
		sessionAnalysisDAO: analysisDAO,
		sessionTurnDAO:     turnDAO,
		sessionPhaseDAO:    phaseDAO,
	}
	// revertSignals>0 fires D; artifact ledger empty so E's fire gate is irrelevant (E stays pending).
	sp := &spineResult{contentSha256: "sha1", ledgers: spineLedgers{revertSignals: 1}}

	read, discard, err := p.runProducers(context.Background(), orgID, &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, prior, sp, nil)

	require.NoError(t, err)
	require.NotNil(t, read, "R is carried by rehydration")
	require.NotNil(t, discard, "D re-ran independently of the pending E")
	assert.Equal(t, 0, prod.readCalls, "R is carried, not re-inferred")
	assert.Equal(t, 0, prod.evidenceCalls, "E stays pending — a pending E does not run")
	assert.Equal(t, 1, prod.discardCalls, "D re-runs even while E is pending")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA, sp.callState.GetCalls()[producerEvidence].GetStatus())
	assert.Equal(t, bitEvidence, sp.pendingDataProducers, "the E pending bit is kept")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, sp.callState.GetCalls()[producerDiscard].GetStatus(), "D cleared its flag-skipped state")
}

// repickReadBumpConfig is repickTestConfig with a HIGHER current READ contract than the prior
// generation recorded (1), so a re-pick sees R as unsettled at the new contract.
type repickReadBumpConfig struct{ repickTestConfig }

func (repickReadBumpConfig) ContractVersionRead() int32 { return 2 }

// TestRunProducers_ReadOff_PreservesSettledDiscard: a read-off (master-gate-off) commit carries the
// prior generation's FULL call_state, so a DONE D (and a pending E) survive rather than being
// dropped by the fresh spine map — a dropped entry would read as unsettled next pass and force a
// wasteful re-run. R is stamped FLAG_SKIPPED on top.
func TestRunProducers_ReadOff_PreservesSettledDiscard(t *testing.T) {
	orgID, rawRecordID := "org", "rr"
	prior := priorWithEvidencePending(orgID, rawRecordID, "sha1", []string{"a1"})
	prior.CallState.Calls[producerDiscard] = &sessionintelligencepb.CallState{Status: sessionintelligencepb.CallStatus_CALL_STATUS_DONE, ContractVersion: 1}
	prod := &countingProducer{}
	p := &SessionAnalysisProcessor{
		producer:         prod,
		cfg:              repickReadOffConfig{},
		evidenceResolver: func(context.Context, string, string) (bool, error) { return true, nil },
	}
	sp := &spineResult{contentSha256: "sha1"}

	read, discard, err := p.runProducers(context.Background(), orgID, &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, prior, sp, nil)

	require.ErrorIs(t, err, errProducersSkipped)
	assert.Nil(t, read)
	assert.Nil(t, discard)
	assert.Equal(t, 0, prod.readCalls)
	assert.Equal(t, 0, prod.discardCalls)
	discardCall := sp.callState.GetCalls()[producerDiscard]
	require.NotNil(t, discardCall, "the settled D entry must survive — not be dropped by the fresh spine map")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, discardCall.GetStatus(), "D stays DONE")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA, sp.callState.GetCalls()[producerEvidence].GetStatus(), "E stays pending")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_FLAG_SKIPPED, sp.callState.GetCalls()[producerRead].GetStatus(), "R is flag-skipped on top")
}

// TestRunProducers_EvidenceRePick_ReSkip_ReadContractBumped: E is still unresolved (reskip) but the
// READ contract bumped since R last ran, so R is RE-INFERRED (not carried on the reaffirm fast
// path) and stamped at the new contract, while E stays SKIPPED_PENDING_DATA.
func TestRunProducers_EvidenceRePick_ReSkip_ReadContractBumped(t *testing.T) {
	orgID, rawRecordID := "org", "rr"
	prior := priorWithEvidencePending(orgID, rawRecordID, "sha1", []string{"a1"})
	prod := &countingProducer{readResult: &ReadResult{Turns: []*TurnJudgment{{PromptIndex: 0}}}}
	p := &SessionAnalysisProcessor{
		producer:         prod,
		cfg:              repickReadBumpConfig{},                                                    // ContractVersionRead()==2 > prior's 1; prior D settled
		evidenceResolver: func(context.Context, string, string) (bool, error) { return false, nil }, // E still unresolved
	}
	// Body unchanged (sha1) so the decision is reskip; turnIndexMap maps the single rendered turn.
	sp := &spineResult{contentSha256: "sha1", turnIndexMap: []int32{0}}

	read, discard, err := p.runProducers(context.Background(), orgID, &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, prior, sp, nil)

	require.NoError(t, err)
	require.NotNil(t, read, "R re-inferred on the bumped contract")
	assert.Nil(t, discard, "D does not fire — no revert/abandon evidence")
	assert.Equal(t, 1, prod.readCalls, "R is re-inferred, not carried, when its contract bumped")
	assert.False(t, sp.spineOnly, "a contract-bumped R must not take the measured-reaffirm fast path")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA, sp.callState.GetCalls()[producerEvidence].GetStatus(), "E stays pending")
	readCall := sp.callState.GetCalls()[producerRead]
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, readCall.GetStatus())
	assert.Equal(t, int32(2), readCall.GetContractVersion(), "the re-inferred R is stamped at the new current contract")
}

// TestRunProducers_EvidenceRePick_Resolved_ReadContractBumped: the pending data landed (resolved),
// but the READ contract bumped, so R is RE-INFERRED (not carried by rehydration) and stamped at the
// new contract. No DAO mocks are set — a rehydrate call would fail, proving R is not carried.
func TestRunProducers_EvidenceRePick_Resolved_ReadContractBumped(t *testing.T) {
	orgID, rawRecordID := "org", "rr"
	prior := priorWithEvidencePending(orgID, rawRecordID, "sha1", []string{"a1"})
	prod := &countingProducer{readResult: &ReadResult{Turns: []*TurnJudgment{{PromptIndex: 0}}}}
	p := &SessionAnalysisProcessor{
		producer:         prod,
		cfg:              repickReadBumpConfig{},                                                   // ContractVersionRead()==2 > prior's 1
		evidenceResolver: func(context.Context, string, string) (bool, error) { return true, nil }, // a1 now resolves ⇒ resolved
	}
	// artifactInvocations>0 fires E on the re-inferred read; turnIndexMap maps the single turn.
	sp := &spineResult{contentSha256: "sha1", turnIndexMap: []int32{0}, ledgers: spineLedgers{artifactInvocations: 1}}

	read, _, err := p.runProducers(context.Background(), orgID, &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, prior, sp, nil)

	require.NoError(t, err)
	require.NotNil(t, read, "R re-inferred on the bumped contract")
	assert.Equal(t, 1, prod.readCalls, "R is re-inferred, not carried by rehydration, when its contract bumped")
	assert.Equal(t, 1, prod.evidenceCalls, "E's now-resolvable half runs")
	readCall := sp.callState.GetCalls()[producerRead]
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, readCall.GetStatus())
	assert.Equal(t, int32(2), readCall.GetContractVersion(), "the re-inferred R is stamped at the new current contract")
}

// TestPendingDataStillUnresolved covers the #6 short-circuit predicate: it re-skips a pending-E
// record from the persisted state alone (no S3) only when the pending intent is still unresolved on
// the UNCHANGED body and R and D are both settled; every other shape falls through to the full path.
func TestPendingDataStillUnresolved(t *testing.T) {
	orgID, rawRecordID := "org", "rr"
	const sha = "sha1"

	tests := []struct {
		name     string
		mutate   func(*model.SessionAnalysisState)
		record   *model.RawRecordModel
		resolver func(context.Context, string, string) (bool, error)
		want     bool
	}{
		{
			name:     "pending E, unchanged body, intent unresolved -> short-circuit",
			record:   recordWithSha(orgID, rawRecordID, sha),
			resolver: func(context.Context, string, string) (bool, error) { return false, nil },
			want:     true,
		},
		{
			name:     "intent now resolved -> full path (resolved re-pick must run)",
			record:   recordWithSha(orgID, rawRecordID, sha),
			resolver: func(context.Context, string, string) (bool, error) { return true, nil },
			want:     false,
		},
		{
			name:     "body changed -> full path (resync must re-run the spine)",
			record:   recordWithSha(orgID, rawRecordID, "sha2"),
			resolver: func(context.Context, string, string) (bool, error) { return false, nil },
			want:     false,
		},
		{
			name:     "no content anchor -> full path (needs the S3 body to compare)",
			record:   recordWithSha(orgID, rawRecordID, ""),
			resolver: func(context.Context, string, string) (bool, error) { return false, nil },
			want:     false,
		},
		{
			name:     "R contract bumped (R unsettled) -> full path (R must re-infer)",
			mutate:   func(s *model.SessionAnalysisState) { s.CallState.Calls[producerRead].ContractVersion = 0 },
			record:   recordWithSha(orgID, rawRecordID, sha),
			resolver: func(context.Context, string, string) (bool, error) { return false, nil },
			want:     false,
		},
		{
			name: "D flag-skipped (D unsettled) -> full path (D must run once enabled)",
			mutate: func(s *model.SessionAnalysisState) {
				s.CallState.Calls[producerDiscard].Status = sessionintelligencepb.CallStatus_CALL_STATUS_FLAG_SKIPPED
			},
			record:   recordWithSha(orgID, rawRecordID, sha),
			resolver: func(context.Context, string, string) (bool, error) { return false, nil },
			want:     false,
		},
		{
			name: "E not pending (already DONE) -> full path (nothing to re-skip)",
			mutate: func(s *model.SessionAnalysisState) {
				s.CallState.Calls[producerEvidence].Status = sessionintelligencepb.CallStatus_CALL_STATUS_DONE
			},
			record:   recordWithSha(orgID, rawRecordID, sha),
			resolver: func(context.Context, string, string) (bool, error) { return false, nil },
			want:     false,
		},
		{
			name:     "resolver error -> full path (defer non-counting there)",
			record:   recordWithSha(orgID, rawRecordID, sha),
			resolver: func(context.Context, string, string) (bool, error) { return false, errResolverProbe },
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			state := priorWithEvidencePending(orgID, rawRecordID, sha, []string{"a1"})
			if tt.mutate != nil {
				tt.mutate(state)
			}
			stateDAO := daomocks.NewMockISessionAnalysisStateDAO(ctrl)
			stateDAO.EXPECT().GetByRawRecordID(gomock.Any(), rawRecordID).Return(state, nil)
			p := &SessionAnalysisProcessor{
				cfg:              repickTestConfig{},
				sessionStateDAO:  stateDAO,
				evidenceResolver: tt.resolver,
			}
			got := p.pendingDataStillUnresolved(context.Background(), orgID, tt.record)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestRunProducers_EvidenceFailure_PreservesRead: E errors while R succeeded — the record must NOT
// fail wholesale. R is returned (servable), E is stamped FAILED with its failed_producers bit set and
// the attempt counted, and the disposition is PARTIAL. #1 per-producer failure isolation.
func TestRunProducers_EvidenceFailure_PreservesRead(t *testing.T) {
	orgID, rawRecordID := "org", "rr"
	prod := &countingProducer{
		readResult:  &ReadResult{Turns: []*TurnJudgment{{PromptIndex: 0}}},
		evidenceErr: errors.New("evidence boom"),
	}
	p := &SessionAnalysisProcessor{producer: prod, cfg: repickTestConfig{}}
	// artifactInvocations>0 fires E; no revert signal so D stays NOT_FIRED.
	sp := &spineResult{contentSha256: "sha1", turnIndexMap: []int32{0}, ledgers: spineLedgers{artifactInvocations: 1}}

	read, discard, err := p.runProducers(context.Background(), orgID, &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, nil, sp, nil)

	require.NoError(t, err, "an E failure must NOT fail the whole record")
	require.NotNil(t, read, "R is preserved and served")
	assert.Nil(t, discard)
	assert.Equal(t, 1, prod.readCalls, "R ran")
	assert.Equal(t, 1, prod.evidenceCalls, "E ran and errored")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_FAILED, sp.callState.GetCalls()[producerEvidence].GetStatus())
	assert.Equal(t, bitEvidence, sp.failedProducers, "E's failed_producers bit is set")
	assert.True(t, sp.failedProducerIncrementAttempt, "a non-transient failure counts toward the retry cap")
	assert.Equal(t, sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PARTIAL, sp.disposition)
}

// TestRunProducers_DiscardFailure_PreservesReadAndEvidence: D errors while R (and E) succeeded — the
// record commits R, D is stamped FAILED (its bit set), and only D retries.
func TestRunProducers_DiscardFailure_PreservesReadAndEvidence(t *testing.T) {
	orgID, rawRecordID := "org", "rr"
	prod := &countingProducer{
		readResult: &ReadResult{Turns: []*TurnJudgment{{PromptIndex: 0}}},
		discardErr: errors.New("discard boom"),
	}
	p := &SessionAnalysisProcessor{producer: prod, cfg: repickTestConfig{}}
	// revertSignals>0 fires D; no artifact so E stays NOT_FIRED.
	sp := &spineResult{contentSha256: "sha1", turnIndexMap: []int32{0}, ledgers: spineLedgers{revertSignals: 1}}

	read, discard, err := p.runProducers(context.Background(), orgID, &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, nil, sp, nil)

	require.NoError(t, err, "a D failure must NOT fail the whole record")
	require.NotNil(t, read, "R is preserved and served")
	assert.Nil(t, discard, "D produced no ledger this pass")
	assert.Equal(t, 1, prod.discardCalls, "D ran and errored")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_FAILED, sp.callState.GetCalls()[producerDiscard].GetStatus())
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED, sp.callState.GetCalls()[producerEvidence].GetStatus())
	assert.Equal(t, bitDiscard, sp.failedProducers)
	assert.Equal(t, sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PARTIAL, sp.disposition)
}

// priorWithFailedProducer builds a JUDGED row where R is DONE (its input hash matching sha) and the
// named producer is FAILED, for driving the failed-producer retry. otherStatus sets the third
// producer's state.
func priorWithFailedProducer(orgID, rawRecordID, sha, failed string, otherStatus sessionintelligencepb.CallStatus) *model.SessionAnalysisState {
	calls := map[string]*sessionintelligencepb.CallState{
		producerRead:     {Status: sessionintelligencepb.CallStatus_CALL_STATUS_DONE, ContractVersion: 1, InputHash: ReadInputHash(sha, 1)},
		producerEvidence: {Status: otherStatus, ContractVersion: 1},
		producerDiscard:  {Status: otherStatus, ContractVersion: 1},
	}
	calls[failed].Status = sessionintelligencepb.CallStatus_CALL_STATUS_FAILED
	return &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawRecordID,
		Status:    sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED,
		CallState: &sessionintelligencepb.CallStateMap{Calls: calls},
	}
}

// TestRunProducers_FailedProducerRetry_Evidence: a re-admitted row whose E FAILED (D already DONE)
// carries R AND the settled D by rehydration and re-runs ONLY E — never re-billing R or D.
func TestRunProducers_FailedProducerRetry_Evidence(t *testing.T) {
	ctrl := gomock.NewController(t)
	orgID, rawRecordID, sessionAnalysisID := "org", "rr", "said"
	prior := priorWithFailedProducer(orgID, rawRecordID, "sha1", producerEvidence, sessionintelligencepb.CallStatus_CALL_STATUS_DONE)
	prod := &countingProducer{} // E succeeds this pass

	analysisDAO := daomocks.NewMockISessionAnalysisDAO(ctrl)
	turnDAO := daomocks.NewMockISessionTurnAnalysisDAO(ctrl)
	phaseDAO := daomocks.NewMockISessionPhaseAnalysisDAO(ctrl)
	// GetByRawRecordID twice: carryReadDone (R) + carryDiscardDone (settled D).
	analysisDAO.EXPECT().GetByRawRecordID(gomock.Any(), rawRecordID).
		Return(&model.SessionAnalysis{ID: sessionAnalysisID, OrganizationID: orgID, RawRecordID: rawRecordID}, nil).Times(2)
	turnDAO.EXPECT().ListByRawRecordID(gomock.Any(), rawRecordID).Return(nil, nil)
	phaseDAO.EXPECT().ListBySessionAnalysisID(gomock.Any(), sessionAnalysisID).Return(nil, nil)

	p := &SessionAnalysisProcessor{producer: prod, cfg: repickTestConfig{}, sessionAnalysisDAO: analysisDAO, sessionTurnDAO: turnDAO, sessionPhaseDAO: phaseDAO}
	sp := &spineResult{contentSha256: "sha1", ledgers: spineLedgers{artifactInvocations: 1}}

	read, _, err := p.runProducers(context.Background(), orgID, &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, prior, sp, nil)

	require.NoError(t, err)
	require.NotNil(t, read, "R is carried by rehydration")
	assert.Equal(t, 0, prod.readCalls, "R must NOT be re-inferred")
	assert.Equal(t, 1, prod.evidenceCalls, "only E re-runs")
	assert.Equal(t, 0, prod.discardCalls, "the settled DONE D is carried, not re-billed")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, sp.callState.GetCalls()[producerEvidence].GetStatus(), "E succeeded on retry")
	assert.Zero(t, sp.failedProducers, "E's failed bit clears on a successful retry")
}

// TestRunProducers_FailedProducerRetry_Discard: a re-admitted row whose D FAILED (E already DONE)
// carries R AND the settled E and re-runs ONLY D — E's grain is preserved, never re-billed.
func TestRunProducers_FailedProducerRetry_Discard(t *testing.T) {
	ctrl := gomock.NewController(t)
	orgID, rawRecordID, sessionAnalysisID := "org", "rr", "said"
	prior := priorWithFailedProducer(orgID, rawRecordID, "sha1", producerDiscard, sessionintelligencepb.CallStatus_CALL_STATUS_DONE)
	prod := &countingProducer{} // D succeeds this pass

	analysisDAO := daomocks.NewMockISessionAnalysisDAO(ctrl)
	turnDAO := daomocks.NewMockISessionTurnAnalysisDAO(ctrl)
	phaseDAO := daomocks.NewMockISessionPhaseAnalysisDAO(ctrl)
	// Only carryReadDone reads (R); E is carried by re-stamping DONE (no DAO), D re-runs (no carry).
	analysisDAO.EXPECT().GetByRawRecordID(gomock.Any(), rawRecordID).
		Return(&model.SessionAnalysis{ID: sessionAnalysisID, OrganizationID: orgID, RawRecordID: rawRecordID}, nil)
	turnDAO.EXPECT().ListByRawRecordID(gomock.Any(), rawRecordID).Return(nil, nil)
	phaseDAO.EXPECT().ListBySessionAnalysisID(gomock.Any(), sessionAnalysisID).Return(nil, nil)

	p := &SessionAnalysisProcessor{producer: prod, cfg: repickTestConfig{}, sessionAnalysisDAO: analysisDAO, sessionTurnDAO: turnDAO, sessionPhaseDAO: phaseDAO}
	// revertSignals fires D; artifactInvocations present but E is carried DONE (not re-run).
	sp := &spineResult{contentSha256: "sha1", ledgers: spineLedgers{revertSignals: 1, artifactInvocations: 1}}

	read, discard, err := p.runProducers(context.Background(), orgID, &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, prior, sp, nil)

	require.NoError(t, err)
	require.NotNil(t, read, "R is carried by rehydration")
	require.NotNil(t, discard, "D re-ran and produced a result")
	assert.Equal(t, 0, prod.readCalls, "R must NOT be re-inferred")
	assert.Equal(t, 0, prod.evidenceCalls, "E is carried, never re-billed")
	assert.Equal(t, 1, prod.discardCalls, "only D re-runs")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, sp.callState.GetCalls()[producerDiscard].GetStatus(), "D succeeded on retry")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, sp.callState.GetCalls()[producerEvidence].GetStatus(), "E stays DONE (carried)")
	assert.Zero(t, sp.failedProducers, "D's failed bit clears on a successful retry")
}
