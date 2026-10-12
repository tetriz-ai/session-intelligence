// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	daomocks "github.com/tetriz-ai/session-intelligence/internal/dao/mocks"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
	"github.com/tetriz-ai/session-intelligence/internal/refllmclient"
	s3mocks "github.com/tetriz-ai/session-intelligence/internal/s3/mocks"
	sa "github.com/tetriz-ai/session-intelligence/internal/sessionanalysis"
	producermocks "github.com/tetriz-ai/session-intelligence/internal/sessionanalysis/mocks"
)

// callKeyRead is the CallStateMap key for the merged read pass (the wire contract).
const callKeyRead = "READ"

// discardBodyJSON is a minimal conversation body carrying revert/abandon evidence (a user
// interruption), so the spine's branch/revert ledger fires D — the tests that assert D runs
// need a body the D gate admits, since D no longer fires unconditionally on every session.
func discardBodyJSON(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		// A supported ide_type so the measured turn grain is cut (supportsTurnGrouping);
		// the source is incidental to what these tests exercise (D firing, commit paths).
		"ideType":     "IDE_TYPE_CLAUDE_CODE",
		"sessionMeta": map[string]any{"userInterruptions": 1},
	})
	require.NoError(t, err)
	return body
}

// twoHumanTurnDiscardBodyJSON carries the same revert/abandon evidence as discardBodyJSON
// (so D still fires) but over two REAL human turns, so R's tagged prompt_index (a rendered
// Sequence position) actually resolves to a raw whole-session index through the spine's
// Sequence->Index map — discardBodyJSON's empty prompt list renders zero turns, which
// resolves every tagged index to none.
func twoHumanTurnDiscardBodyJSON(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		// A supported ide_type so the measured turn grain is cut (supportsTurnGrouping);
		// the source is incidental to what this fixture exercises (D + turn-index resolution).
		"ideType": "IDE_TYPE_CLAUDE_CODE",
		"prompts": []map[string]any{
			{"role": "ROLE_USER", "content": "do the thing"},
			{"role": "ROLE_ASSISTANT", "content": "on it"},
			{"role": "ROLE_USER", "content": "now the other thing"},
			{"role": "ROLE_ASSISTANT", "content": "done"},
		},
		"sessionMeta": map[string]any{"userInterruptions": 1},
	})
	require.NoError(t, err)
	return body
}

// humanTurnsWithSubAgentDiscardBodyJSON has two human turns and a trailing SUB-AGENT
// (machine-authored) user turn — three splitHumanTurns groups, but only two survive the
// machine-turn filter (prefixHumanTurnFilter drops the is_sub_agent group). So the count
// supplied to R (len(turnIndexMap) = 2) is strictly below the raw group count (3): the
// fixture that separates the filtered supplied count from the raw one in the rows==turns
// guard. Still carries the userInterruption so D fires.
func humanTurnsWithSubAgentDiscardBodyJSON(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		// A supported ide_type so the measured turn grain is cut (supportsTurnGrouping); the
		// measured writer emits one row per unfiltered group while R's supplied set is the
		// machine-turn-filtered subset.
		"ideType": "IDE_TYPE_CLAUDE_CODE",
		"prompts": []map[string]any{
			{"role": "ROLE_USER", "content": "do the thing"},
			{"role": "ROLE_ASSISTANT", "content": "on it"},
			{"role": "ROLE_USER", "content": "now the other thing"},
			{"role": "ROLE_ASSISTANT", "content": "done"},
			{"role": "ROLE_USER", "content": "sub-agent fan-out", "metadata": map[string]any{"isSubAgent": true}},
			{"role": "ROLE_ASSISTANT", "content": "sub work"},
		},
		"sessionMeta": map[string]any{"userInterruptions": 1},
	})
	require.NoError(t, err)
	return body
}

// testConfig is a fixed Config for the orchestration tests: one current version of
// each stamp, both effective from the epoch, all producers enabled, caching on.
type testConfig struct{}

func (testConfig) Enabled(string) bool                            { return true }
func (testConfig) BatchSize() int                                 { return 50 }
func (testConfig) RecordConcurrency() int                         { return 20 }
func (testConfig) GlobalBudgetPerOrgTick() int                    { return 200 }
func (testConfig) RetryCap() int32                                { return 3 }
func (testConfig) ReprocessMinInterval() time.Duration            { return 12 * time.Hour }
func (testConfig) ChildRefoldMinInterval() time.Duration          { return 12 * time.Hour }
func (testConfig) PerRecordTimeout() time.Duration                { return 15 * time.Minute }
func (testConfig) OutageSignalWindow() time.Duration              { return 10 * time.Minute }
func (testConfig) OutageSignalMinRecords() int                    { return 5 }
func (testConfig) ProviderRetryBackoff() time.Duration            { return 30 * time.Minute }
func (testConfig) ChunkCharLimit() int                            { return 350000 }
func (testConfig) ChunkWindowPrompts() int                        { return 20 }
func (testConfig) ChunkLookaheadPrompts() int                     { return 6 }
func (testConfig) VocabularyVersion() int32                       { return 1 }
func (testConfig) VocabularyEffectiveFrom() time.Time             { return time.Time{} }
func (testConfig) SpineVersion() int32                            { return 2 }
func (testConfig) SpineEffectiveFrom() time.Time                  { return time.Time{} }
func (testConfig) ContractVersionRead() int32                     { return 1 }
func (testConfig) ContractVersionEvidence() int32                 { return 1 }
func (testConfig) ContractVersionDiscard() int32                  { return 1 }
func (testConfig) ContractEffectiveFrom() time.Time               { return time.Time{} }
func (testConfig) UseSystemPromptCache() bool                     { return true }
func (testConfig) ReadEnabled(string) bool                        { return true }
func (testConfig) EvidenceSecurityFlagPublishEnabled(string) bool { return true }

// E (and its PAIRS detector) is dark by default in the orchestration tests (its own fixture,
// evidenceOnConfig, arms it); the numeric knobs carry their default values.
func (testConfig) EvidenceEnabled(string) bool { return false }
func (testConfig) DetectorTopN() int           { return 50 }
func (testConfig) DetectorConcurrency() int    { return 8 }
func (testConfig) DetectorPairCeiling() int    { return 1000 }

// Compat write OFF by default — the orchestration tests run in the parallel-run posture,
// where the engine never writes raw_records.processing_status. The compat-on case uses
// compatOnConfig (compat_test.go).
func (testConfig) CompatWriteEnabled() bool { return false }

// CutoverDate zero keeps these tests on the no-floor behaviour: the LIVE no-state leg admits
// every in-window date and there is no pre-cutover leg, so no backfill expectation is needed.
func (testConfig) CutoverDate(string) time.Time { return time.Time{} }

type harness struct {
	proc            *sa.SessionAnalysisProcessor
	rawDAO          *daomocks.MockIRawRecordDAO
	analysis        *daomocks.MockISessionAnalysisDAO
	state           *daomocks.MockISessionAnalysisStateDAO
	turn            *daomocks.MockISessionTurnAnalysisDAO
	phase           *daomocks.MockISessionPhaseAnalysisDAO
	roundTripFault  *daomocks.MockISessionRoundTripFaultDAO
	secFlag         *daomocks.MockISecurityFlagDAO
	harnessArtifact *daomocks.MockIHarnessArtifactDAO
	classification  *daomocks.MockIHarnessArtifactClassificationDAO
	s3              *s3mocks.MockS3Client
	producer        *producermocks.MockLLMProducer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctrl := gomock.NewController(t)
	h := &harness{
		rawDAO:          daomocks.NewMockIRawRecordDAO(ctrl),
		analysis:        daomocks.NewMockISessionAnalysisDAO(ctrl),
		state:           daomocks.NewMockISessionAnalysisStateDAO(ctrl),
		turn:            daomocks.NewMockISessionTurnAnalysisDAO(ctrl),
		phase:           daomocks.NewMockISessionPhaseAnalysisDAO(ctrl),
		roundTripFault:  daomocks.NewMockISessionRoundTripFaultDAO(ctrl),
		secFlag:         daomocks.NewMockISecurityFlagDAO(ctrl),
		harnessArtifact: daomocks.NewMockIHarnessArtifactDAO(ctrl),
		classification:  daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl),
		s3:              s3mocks.NewMockS3Client(ctrl),
		producer:        producermocks.NewMockLLMProducer(ctrl),
	}
	h.proc = sa.NewSessionAnalysisProcessor(h.rawDAO, h.analysis, h.state, h.turn, h.phase, h.roundTripFault, h.secFlag, h.harnessArtifact, h.classification, h.s3, h.producer, nil, nil, testConfig{})
	h.roundTripFault.EXPECT().ReplaceSet(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	// The spine folds delegated child usage on every run; default to no children so
	// orchestration tests that do not exercise the fold need no per-test setup.
	h.rawDAO.EXPECT().AggregateSubagentUsageByParent(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
	// The commit re-reads the current content digest; the seeded record's is "sha1", so the
	// digest-revalidation matches and the commit proceeds.
	h.rawDAO.EXPECT().CurrentContentDigest(gomock.Any(), gomock.Any(), gomock.Any()).Return("sha1", nil).AnyTimes()
	// No default ReconcileOpenFlags stub here (unlike the read-only DAOs above): tests
	// whose commit reaches reconcileSecurityFlags for real (read != nil) declare their
	// own expectation — a shared AnyTimes() default would silently swallow every call
	// (gomock matches the FIRST registered expectation whose args match), making a
	// later, more specific per-test expectation unreachable and any "must not be called"
	// guard unenforceable.
	return h
}

// expectScan returns exactly the one candidate on the LIVE lane, empty BACKFILL, so
// ProcessFlat drives it through processCandidate.
func (h *harness) expectScan(candidate *dao.ActionableCandidate) {
	h.state.EXPECT().ScanLiveActionable(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]*dao.ActionableCandidate{candidate}, nil)
	h.state.EXPECT().ScanBackfillActionable(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil)
}

func sessionRawRecord(t *testing.T, orgID, personID, contentSha string) *model.RawRecordModel {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"s3_key": "s3://body", "content_sha256": contentSha})
	require.NoError(t, err)
	meta, err := json.Marshal(map[string]string{"person_id": personID})
	require.NoError(t, err)
	return &model.RawRecordModel{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		ConnectorID:    sa.SessionConnectorID,
		DataDate:       time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC),
		Payload:        payload,
		Metadata:       meta,
		FetchedAt:      time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
	}
}

// TestProcessFlat_PrefetchSkipGates is the skip-gate guardrail: the CHILD / OUT_OF_WINDOW / NO_SEAT pre-fetch gates mark the record
// SKIPPED with the right reason BEFORE any S3 read or LLM call — no paid work on a
// record that should not be analysed. Each case asserts MarkSkipped(reason) fires and
// the S3 client + every producer are Times(0).
func TestProcessFlat_PrefetchSkipGates(t *testing.T) {
	orgID := uuid.NewString()
	seatedPerson := uuid.NewString()

	tests := []struct {
		name        string
		ctx         func(context.Context) context.Context
		mutate      func(*model.RawRecordModel)
		wantReason  sessionintelligencepb.SessionAnalysisSkipReason
		childReadS3 bool           // a seated, in-window CHILD reads its body to write measured turns before the skip
		expectMore  func(*harness) // extra expectations (e.g. the CHILD parent lookup)
	}{
		{
			name: "child folds via its parent",
			ctx:  func(c context.Context) context.Context { return c },
			mutate: func(r *model.RawRecordModel) {
				r.Metadata = mustJSON(t, map[string]string{"person_id": seatedPerson, "relation_type": "SUBAGENT", "parent_record_key": "parent-key"})
			},
			wantReason:  sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD,
			childReadS3: true,
			expectMore: func(h *harness) {
				// Best-effort parent fence bump: parent not yet ingested → no bump.
				h.rawDAO.EXPECT().GetByRecordKey(gomock.Any(), orgID, sa.SessionConnectorID, "parent-key").
					Return(nil, dao.ErrNotFound)
			},
		},
		{
			name: "out of window",
			ctx: func(c context.Context) context.Context {
				return planscoped.WithConfigForTest(c, planscoped.Config{SeatsLimit: -1, DataWindowDays: 1, OrgCreatedAt: time.Now()})
			},
			mutate:     func(_ *model.RawRecordModel) {},
			wantReason: sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_OUT_OF_WINDOW,
		},
		{
			name: "not seated",
			ctx: func(c context.Context) context.Context {
				c = planscoped.WithConfigForTest(c, planscoped.Config{SeatsLimit: 1, DataWindowDays: -1})
				return planscoped.WithSeats(c, planscoped.SeatsContext{PersonIDs: []string{seatedPerson}})
			},
			mutate: func(r *model.RawRecordModel) {
				r.Metadata = mustJSON(t, map[string]string{"person_id": uuid.NewString()}) // an unseated person
			},
			wantReason: sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_NO_SEAT,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			record := sessionRawRecord(t, orgID, seatedPerson, "sha1")
			tt.mutate(record)

			h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
			h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)

			var gotReason sessionintelligencepb.SessionAnalysisSkipReason
			h.state.EXPECT().MarkSkipped(gomock.Any(), gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, _ *model.SessionAnalysisState, reason sessionintelligencepb.SessionAnalysisSkipReason) (bool, error) {
					gotReason = reason
					return true, nil
				})
			if tt.expectMore != nil {
				tt.expectMore(h)
			}
			// The guardrail: no producer call — the gate is pre-fetch. A seated, in-window CHILD
			// still reads its body once to write its measured turn grain before the skip (an empty
			// body cuts to no turns); the gate cases (out-of-window, unseated) read nothing at all.
			if tt.childReadS3 {
				h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(discardBodyJSON(t), nil).Times(1)
			} else {
				h.s3.EXPECT().ReadObject(gomock.Any(), gomock.Any()).Times(0)
			}
			h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Times(0)
			h.state.EXPECT().ClaimForProcessing(gomock.Any(), gomock.Any()).Times(0)

			processed, err := h.proc.ProcessFlat(tt.ctx(readyCtx()), orgID, 50)
			require.NoError(t, err)
			assert.Equal(t, 1, processed)
			assert.Equal(t, tt.wantReason, gotReason)
		})
	}
}

// budgetOneConfig caps the per-tick paid-work budget at one candidate.
type budgetOneConfig struct{ testConfig }

func (budgetOneConfig) GlobalBudgetPerOrgTick() int { return 1 }

// concurrency2Config bounds record parallelism to two.
type concurrency2Config struct{ testConfig }

func (concurrency2Config) RecordConcurrency() int { return 2 }

// TestProcessFlat_BoundedParallelism asserts the batch runs records concurrently
// (bounded by RecordConcurrency), not serially: with a limit of two, the first two
// records reach a barrier and only proceed once BOTH have arrived. A serial loop would
// stall the first record on the barrier forever, which the timeout detects.
func TestProcessFlat_BoundedParallelism(t *testing.T) {
	ctrl := gomock.NewController(t)
	h := &harness{
		rawDAO:          daomocks.NewMockIRawRecordDAO(ctrl),
		analysis:        daomocks.NewMockISessionAnalysisDAO(ctrl),
		state:           daomocks.NewMockISessionAnalysisStateDAO(ctrl),
		turn:            daomocks.NewMockISessionTurnAnalysisDAO(ctrl),
		phase:           daomocks.NewMockISessionPhaseAnalysisDAO(ctrl),
		roundTripFault:  daomocks.NewMockISessionRoundTripFaultDAO(ctrl),
		secFlag:         daomocks.NewMockISecurityFlagDAO(ctrl),
		harnessArtifact: daomocks.NewMockIHarnessArtifactDAO(ctrl),
		classification:  daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl),
		s3:              s3mocks.NewMockS3Client(ctrl),
		producer:        producermocks.NewMockLLMProducer(ctrl),
	}
	h.proc = sa.NewSessionAnalysisProcessor(h.rawDAO, h.analysis, h.state, h.turn, h.phase, h.roundTripFault, h.secFlag, h.harnessArtifact, h.classification, h.s3, h.producer, nil, nil, concurrency2Config{})
	h.roundTripFault.EXPECT().ReplaceSet(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	h.secFlag.EXPECT().ReconcileOpenFlags(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	orgID := uuid.NewString()
	childMeta := mustJSON(t, map[string]string{"person_id": uuid.NewString(), "relation_type": "SUBAGENT", "parent_record_key": "pk"})
	payload := mustJSON(t, map[string]string{"s3_key": "s3://body"})

	// Proof of ≥2 concurrency WITHOUT an outer goroutine (the goroutine linter forbids a
	// bare `go` here, and a hung serial call needs no external timeout this way): the first
	// arrival waits briefly; a second arrival that lands WHILE the first is still waiting
	// releases it and records concurrency. Under serial execution the second can't arrive
	// during the first's wait, so the first times out, clears firstWaiting, and the second
	// finds no one waiting — sawConcurrent stays false.
	var mu sync.Mutex
	arrived := 0
	firstWaiting := false
	sawConcurrent := false
	release := make(chan struct{})
	onArrive := func() {
		mu.Lock()
		arrived++
		n := arrived
		if n == 1 {
			firstWaiting = true
		}
		if n == 2 && firstWaiting {
			sawConcurrent = true
			close(release)
		}
		mu.Unlock()
		if n == 1 {
			select {
			case <-release:
			case <-time.After(3 * time.Second):
			}
			mu.Lock()
			firstWaiting = false
			mu.Unlock()
		}
	}

	candidates := make([]*dao.ActionableCandidate, 4)
	for i := range candidates {
		candidates[i] = &dao.ActionableCandidate{RawRecordID: uuid.NewString(), Category: dao.CategoryNoState}
	}
	h.state.EXPECT().ScanLiveActionable(gomock.Any(), gomock.Any(), gomock.Any()).Return(candidates, nil)
	h.state.EXPECT().ScanBackfillActionable(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)

	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), gomock.Any(), gomock.Any()).Times(4).
		DoAndReturn(func(_ context.Context, id string, _ time.Time) (*model.RawRecordModel, error) {
			onArrive()
			return &model.RawRecordModel{
				ID: id, OrganizationID: orgID, ConnectorID: sa.SessionConnectorID,
				DataDate: time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC),
				Payload:  payload, Metadata: childMeta, FetchedAt: time.Now(),
			}, nil
		})
	// Each record CHILD-skips (cheap), so the barrier isolates the concurrency, not the
	// full pipeline. A seated, in-window child reads its body once to write its measured turn
	// grain before the skip (an empty body cuts to no turns).
	h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(discardBodyJSON(t), nil).Times(4)
	h.state.EXPECT().MarkSkipped(gomock.Any(), gomock.Any(), sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD).Return(true, nil).Times(4)
	h.rawDAO.EXPECT().GetByRecordKey(gomock.Any(), orgID, sa.SessionConnectorID, "pk").Return(nil, dao.ErrNotFound).Times(4)

	processed, perr := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, perr)
	assert.Equal(t, 4, processed)
	// Read after ProcessFlat returns: errgroup.Wait() has joined every record goroutine,
	// so all onArrive writes happen-before this read.
	assert.True(t, sawConcurrent, "ProcessFlat ran serially — a second record never entered GetByIDForDate while the first was still waiting")
}

// TestProcessFlat_BudgetCaps asserts GlobalBudgetPerOrgTick is a live total-per-tick
// ceiling (not an inert knob): with two admitted candidates and a budget of one, only
// the first (LIVE) is processed.
func TestProcessFlat_BudgetCaps(t *testing.T) {
	ctrl := gomock.NewController(t)
	h := &harness{
		rawDAO:          daomocks.NewMockIRawRecordDAO(ctrl),
		analysis:        daomocks.NewMockISessionAnalysisDAO(ctrl),
		state:           daomocks.NewMockISessionAnalysisStateDAO(ctrl),
		turn:            daomocks.NewMockISessionTurnAnalysisDAO(ctrl),
		phase:           daomocks.NewMockISessionPhaseAnalysisDAO(ctrl),
		roundTripFault:  daomocks.NewMockISessionRoundTripFaultDAO(ctrl),
		secFlag:         daomocks.NewMockISecurityFlagDAO(ctrl),
		harnessArtifact: daomocks.NewMockIHarnessArtifactDAO(ctrl),
		classification:  daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl),
		s3:              s3mocks.NewMockS3Client(ctrl),
		producer:        producermocks.NewMockLLMProducer(ctrl),
	}
	h.proc = sa.NewSessionAnalysisProcessor(h.rawDAO, h.analysis, h.state, h.turn, h.phase, h.roundTripFault, h.secFlag, h.harnessArtifact, h.classification, h.s3, h.producer, nil, nil, budgetOneConfig{})
	h.roundTripFault.EXPECT().ReplaceSet(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	h.secFlag.EXPECT().ReconcileOpenFlags(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	orgID := uuid.NewString()
	kept := sessionRawRecord(t, orgID, uuid.NewString(), "sha1")
	kept.Metadata = mustJSON(t, map[string]string{"person_id": uuid.NewString(), "relation_type": "SUBAGENT", "parent_record_key": "pk"})

	// The caller passes limit=50 but the budget is 1, so the scan is clamped to
	// min(caller, budget)=1 — proving the upfront clamp against a larger caller limit.
	h.state.EXPECT().ScanLiveActionable(gomock.Any(), gomock.Any(), 1).Return([]*dao.ActionableCandidate{
		{RawRecordID: kept.ID, Category: dao.CategoryNoState},
		{RawRecordID: uuid.NewString(), Category: dao.CategoryNoState},
	}, nil)

	// Only the first candidate is loaded — the second is dropped by the post-dedup cap. The
	// loaded child reads its body once to write its measured turn grain before the skip.
	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), kept.ID, gomock.Any()).Return(kept, nil).Times(1)
	h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(discardBodyJSON(t), nil).Times(1)
	h.state.EXPECT().MarkSkipped(gomock.Any(), gomock.Any(), sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD).Return(true, nil).Times(1)
	h.rawDAO.EXPECT().GetByRecordKey(gomock.Any(), orgID, sa.SessionConnectorID, "pk").Return(nil, dao.ErrNotFound).Times(1)

	processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
}

// TestProcessFlat_CrossLaneDedup asserts end-to-end that a raw record returned by
// BOTH the LIVE and BACKFILL scans is processed exactly once: GetByID fires a single
// time. Without the cross-lane dedup the record would be handled twice — the second
// pass re-running it spine-only and wiping the first pass's security flags.
func TestProcessFlat_CrossLaneDedup(t *testing.T) {
	h := newHarness(t)
	orgID := uuid.NewString()
	record := sessionRawRecord(t, orgID, uuid.NewString(), "sha1")
	// A CHILD record so processCandidate short-circuits at the skip gate — the dedup,
	// not the full pipeline, is what this test isolates.
	record.Metadata = mustJSON(t, map[string]string{"person_id": uuid.NewString(), "relation_type": "SUBAGENT", "parent_record_key": "pk"})

	// The same raw record is admitted by LIVE (content-changed) and BACKFILL
	// (version-behind) in one tick.
	h.state.EXPECT().ScanLiveActionable(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]*dao.ActionableCandidate{{RawRecordID: record.ID, Category: dao.CategoryContentChanged}}, nil)
	h.state.EXPECT().ScanBackfillActionable(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]*dao.ActionableCandidate{{RawRecordID: record.ID, Category: dao.CategoryVersionBehind}}, nil)

	// The guard: loaded and skip-gated exactly once, not twice. The single load reads the child
	// body once to write its measured turn grain before the skip.
	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil).Times(1)
	h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(discardBodyJSON(t), nil).Times(1)
	h.state.EXPECT().MarkSkipped(gomock.Any(), gomock.Any(), sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD).Return(true, nil).Times(1)
	h.rawDAO.EXPECT().GetByRecordKey(gomock.Any(), orgID, sa.SessionConnectorID, "pk").Return(nil, dao.ErrNotFound).Times(1)

	processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
}

// readyCtx carries a resolved (unlimited) plan context so the processor's fail-closed
// preflight passes and the membership gates fail open — the base for tests not
// exercising a specific seat/window gate. Cases that DO exercise a gate override the
// config on top of this.
func readyCtx() context.Context {
	return planscoped.WithConfigForTest(context.Background(), planscoped.Config{SeatsLimit: -1, DataWindowDays: -1})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// TestProcessFlat_WritesTurnPhaseGrain asserts the commit threads R's turn/phase
// output through the FK-safe write: the phase set is replaced, each turn is upserted
// with its phase_id resolved from the fresh phase ids, and the trailing turns are
// retired.
func TestProcessFlat_WritesTurnPhaseGrain(t *testing.T) {
	h := newHarness(t)
	orgID, personID := uuid.NewString(), uuid.NewString()
	record := sessionRawRecord(t, orgID, personID, "sha1")
	claimToken := "tok-new"
	ord0, ord1 := int32(0), int32(1)

	h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
	h.state.EXPECT().ClaimForProcessing(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, s *model.SessionAnalysisState) (bool, error) {
			s.ClaimToken = &claimToken
			return true, nil
		})
	h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(twoHumanTurnDiscardBodyJSON(t), nil)
	h.state.EXPECT().TransitionToSpineDone(gomock.Any(), record.ID, claimToken, "sha1", gomock.Any(), int32(2), gomock.Any()).Return(true, nil)
	h.analysis.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Return(nil)
	// The measured grouper's own write, over the two real human turns twoHumanTurnDiscardBodyJSON
	// carries — runs before the judged overlay below (turn writes ride the master gate).
	h.turn.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Return(nil).Times(2)
	h.turn.EXPECT().SoftDeleteMeasuredBeyond(gomock.Any(), orgID, record.ID, int32(1)).Return(int64(0), nil)

	h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Return(&sa.ReadResult{
		Phases: []*sa.PhaseJudgment{{PhaseOrdinal: 0}, {PhaseOrdinal: 1}},
		Turns: []*sa.TurnJudgment{
			{PromptIndex: 0, PhaseOrdinal: &ord0},
			{PromptIndex: 1, PhaseOrdinal: &ord1},
		},
	}, nil)
	// D runs after R (R gates the tail); it finds nothing to discard here.
	h.producer.EXPECT().AnalyzeDiscard(gomock.Any(), gomock.Any()).Return(&sa.DiscardResult{}, nil)

	h.analysis.EXPECT().UpsertJudged(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, a *model.SessionAnalysis, _ bool) error {
			a.ID = "session-1"
			return nil
		})
	// The phase set is replaced; the returned map drives the turn re-point.
	h.phase.EXPECT().ReplacePhaseSet(gomock.Any(), orgID, "session-1", gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, phases []*model.SessionPhaseAnalysis) (map[int32]string, error) {
			require.Len(t, phases, 2)
			return map[int32]string{0: "phase-0", 1: "phase-1"}, nil
		})
	gotPhaseIDByPrompt := map[int32]string{}
	h.turn.EXPECT().UpdateJudged(gomock.Any(), gomock.Any()).Times(2).
		DoAndReturn(func(_ context.Context, turn *model.SessionTurnAnalysis) (int64, error) {
			if turn.PhaseID != nil {
				gotPhaseIDByPrompt[turn.PromptIndex] = *turn.PhaseID
			}
			return 1, nil // one live measured row matched — no requeue
		})
	h.state.EXPECT().TransitionToJudged(gomock.Any(), record.ID, claimToken, int32(0), gomock.Any()).Return(true, nil)
	h.secFlag.EXPECT().ReconcileOpenFlags(gomock.Any(), record.ID, gomock.Any()).Return(nil)

	processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	assert.Equal(t, "phase-0", gotPhaseIDByPrompt[0], "turn 0 is re-pointed to phase ordinal 0's fresh id")
	assert.Equal(t, "phase-1", gotPhaseIDByPrompt[1], "turn 1 is re-pointed to phase ordinal 1's fresh id")
}

// TestProcessFlat_ReadTurnCoverageGuard is the one-verdict-per-supplied-turn guardrail
// (the LLM output contract): R's verdicts must cover the supplied turn set exactly
// once. The supplied set is the machine-turn-FILTERED turn set (sp.turnIndexMap's values),
// never the raw prompt/group count. A gap, a duplicate, or an out-of-set index is a hard
// failure that routes to markFailed, not the commit's silent out-of-cut drop.
// - "rows match supplied": a 2-human-turn (4-prompt) body with verdicts for {0,1} commits —
// because the raw prompt count is 4, this alone proves the guard does NOT compare against
// sp.turnCount (which would false-fail at 2 != 4).
// - "machine-turn-filtered": a body with a trailing sub-agent turn (3 groups, 2 human) with
// verdicts for {0,1} commits — proving the guard uses the FILTERED set (2), not the raw
// group count (3); it kills the "use group count" mutant the first case cannot.
// - "under-emit": the same 2-human body with only a verdict for {0} fails — a supplied turn
// got no verdict.
// - "duplicate + missing": two verdicts for turn 0 and none for turn 1 — the COUNT matches
// (2 == 2) but coverage fails, so a bare count check would wrongly pass this. It fails.
// - "out-of-set index": a verdict whose rendered position rebases to a raw index outside the
// supplied set (-1 here) fails, even though the count matches.
func TestProcessFlat_ReadTurnCoverageGuard(t *testing.T) {
	ord0, ord1 := int32(0), int32(1)
	retry := sessionintelligencepb.WasRetry_WAS_RETRY_RETRY_SAME
	tests := []struct {
		name           string
		body           func(*testing.T) []byte
		measuredTurns  int   // rows the measured turn writer emits (one per group, unfiltered)
		maxPromptIndex int32 // SoftDeleteMeasuredBeyond's ceiling (last group ordinal)
		readTurns      []*sa.TurnJudgment
		readSecurity   []*sessionintelligencepb.PromptIssue // R's flattened security list, as parseReadResponse would produce
		wantFail       bool
		wantProcessed  int
		wantFlagCount  int // security flags the reconcile receives after the guard rebuilds the list
	}{
		{
			name:           "rows match supplied turns commits JUDGED",
			body:           twoHumanTurnDiscardBodyJSON,
			measuredTurns:  2,
			maxPromptIndex: 1,
			readTurns:      []*sa.TurnJudgment{{PromptIndex: 0, PhaseOrdinal: &ord0}, {PromptIndex: 1, PhaseOrdinal: &ord1}},
			wantProcessed:  1,
		},
		{
			name:           "machine-turn-filtered session: supplied is the filtered set, rows cover it",
			body:           humanTurnsWithSubAgentDiscardBodyJSON,
			measuredTurns:  3,
			maxPromptIndex: 2,
			readTurns:      []*sa.TurnJudgment{{PromptIndex: 0, PhaseOrdinal: &ord0}, {PromptIndex: 1, PhaseOrdinal: &ord1}},
			wantProcessed:  1,
		},
		{
			name:          "under-emit (a supplied turn got no verdict) fails to markFailed",
			body:          twoHumanTurnDiscardBodyJSON,
			readTurns:     []*sa.TurnJudgment{{PromptIndex: 0, PhaseOrdinal: &ord0}}, // 1 verdict, 2 supplied
			wantFail:      true,
			wantProcessed: 0,
		},
		{
			name: "duplicate verdict + a missing turn (count matches, coverage fails) fails to markFailed",
			body: twoHumanTurnDiscardBodyJSON,
			// Two verdicts for rendered position 0 (both rebase to raw 0) and none for turn 1:
			// len == 2 == supplied, but a bare count check would pass it — the coverage check fails.
			readTurns:     []*sa.TurnJudgment{{PromptIndex: 0, PhaseOrdinal: &ord0}, {PromptIndex: 0, PhaseOrdinal: &ord0}},
			wantFail:      true,
			wantProcessed: 0,
		},
		{
			name: "a phantom index does not rescue a missing turn: still fails to markFailed",
			body: twoHumanTurnDiscardBodyJSON,
			// Rendered position 99 has no entry in the 2-turn index map, so rebaseToRawIndex maps
			// it to -1 — dropped, not counted. Turn 1 then has no verdict, so coverage is short
			// (1 of 2): the phantom cannot pad the count to disguise the missing turn.
			readTurns:     []*sa.TurnJudgment{{PromptIndex: 0, PhaseOrdinal: &ord0}, {PromptIndex: 99, PhaseOrdinal: &ord1}},
			wantFail:      true,
			wantProcessed: 0,
		},
		{
			name:           "complete coverage plus a phantom (security + retry) commits JUDGED; phantom's flag and E-trigger dropped",
			body:           twoHumanTurnDiscardBodyJSON,
			measuredTurns:  2,
			maxPromptIndex: 1,
			// Both real turns covered exactly once, plus a hallucinated position 99 (rebases to
			// -1) that carries a security issue AND a retry tag. The phantom is dropped ENTIRELY
			// (not just from coverage): the session still commits JUDGED, but the guard rebuilds
			// read.SecurityIssues from the survivors so the phantom's pii-injection cannot open a
			// flag, while turn 1's api-key-leak survives (wantFlagCount 1), and its retry tag no
			// longer fires E (AnalyzeEvidence Times(0) below).
			readTurns: []*sa.TurnJudgment{
				{PromptIndex: 0, PhaseOrdinal: &ord0},
				{PromptIndex: 1, PhaseOrdinal: &ord1, PromptIssues: &sessionintelligencepb.PromptIssueList{Items: []*sessionintelligencepb.PromptIssue{
					{Slug: sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK, Severity: sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, Layer: "security"},
				}}},
				{PromptIndex: 99, PhaseOrdinal: &ord1, WasRetry: &retry, PromptIssues: &sessionintelligencepb.PromptIssueList{Items: []*sessionintelligencepb.PromptIssue{
					{Slug: sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_PII_INJECTION, Severity: sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, Layer: "security"},
				}}},
			},
			// The flattened list parseReadResponse would hand back carries BOTH issues; a guard
			// that dropped the phantom only from coverage would leave the pii here and open a
			// false flag. The rebuild overwrites this with the survivors' issues.
			readSecurity: []*sessionintelligencepb.PromptIssue{
				{Slug: sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK, Severity: sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, Layer: "security"},
				{Slug: sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_PII_INJECTION, Severity: sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, Layer: "security"},
			},
			wantProcessed: 1,
			wantFlagCount: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			orgID, personID := uuid.NewString(), uuid.NewString()
			record := sessionRawRecord(t, orgID, personID, "sha1")
			claimToken := "tok-guard"

			h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
			h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
			h.state.EXPECT().ClaimForProcessing(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, s *model.SessionAnalysisState) (bool, error) {
					s.ClaimToken = &claimToken
					return true, nil
				})
			h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(tt.body(t), nil)
			h.state.EXPECT().TransitionToSpineDone(gomock.Any(), record.ID, claimToken, "sha1", gomock.Any(), int32(2), gomock.Any()).Return(true, nil)
			// The session-grain measured row is written at spine_done, before the producers —
			// so it lands on the failure path too.
			h.analysis.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Return(nil)
			h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Return(&sa.ReadResult{
				Phases:         []*sa.PhaseJudgment{{PhaseOrdinal: 0}, {PhaseOrdinal: 1}},
				Turns:          tt.readTurns,
				SecurityIssues: tt.readSecurity,
			}, nil)

			if tt.wantFail {
				// The guard aborts runProducers before D runs and before the commit: no discard
				// call, no judged write — only markFailed's terminal transition.
				h.producer.EXPECT().AnalyzeDiscard(gomock.Any(), gomock.Any()).Times(0)
				h.analysis.EXPECT().UpsertJudged(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
				h.rawDAO.EXPECT().CurrentSourceUpdatedAt(gomock.Any(), record.ID, gomock.Any()).Return(record.UpdatedAt, nil)
				// errReadTurnCoverageMismatch is a record-class failure, so it increments the attempt
				// count (true); a non-terminal attempt count keeps it retryable, no JUDGE_FAILED.
				h.state.EXPECT().TransitionToFailed(gomock.Any(), record.ID, claimToken, gomock.Any(), true, gomock.Any(), gomock.Any()).Return(true, int32(1), nil)
			} else {
				h.analysis.EXPECT().UpsertJudged(gomock.Any(), gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, a *model.SessionAnalysis, _ bool) error {
						a.ID = "session-1"
						return nil
					})
				h.turn.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Return(nil).Times(tt.measuredTurns)
				h.turn.EXPECT().SoftDeleteMeasuredBeyond(gomock.Any(), orgID, record.ID, tt.maxPromptIndex).Return(int64(0), nil)
				h.producer.EXPECT().AnalyzeDiscard(gomock.Any(), gomock.Any()).Return(&sa.DiscardResult{}, nil)
				// E stays dark: the only retry tag belongs to the dropped phantom, so
				// confirmedRoundTrip sees no round-trip in the filtered read and E never fires.
				h.producer.EXPECT().AnalyzeEvidence(gomock.Any(), gomock.Any()).Times(0)
				h.phase.EXPECT().ReplacePhaseSet(gomock.Any(), orgID, "session-1", gomock.Any()).
					Return(map[int32]string{0: "phase-0", 1: "phase-1"}, nil)
				// The guard drops phantoms from read.Turns before commit, so only the two real
				// turns overlay a measured row.
				h.turn.EXPECT().UpdateJudged(gomock.Any(), gomock.Any()).Return(int64(1), nil).Times(2)
				h.state.EXPECT().TransitionToJudged(gomock.Any(), record.ID, claimToken, gomock.Any(), gomock.Any()).Return(true, nil)
				// The reconcile receives only the survivors' security issues — the guard rebuilt
				// read.SecurityIssues, so a phantom's flag never reaches here.
				h.secFlag.EXPECT().ReconcileOpenFlags(gomock.Any(), record.ID, gomock.Any()).
					DoAndReturn(func(_ context.Context, _ string, freshFlags []*model.SecurityFlag) error {
						assert.Len(t, freshFlags, tt.wantFlagCount)
						return nil
					})
			}

			processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
			require.NoError(t, err, "a per-record failure never fails the batch")
			assert.Equal(t, tt.wantProcessed, processed)
		})
	}
}

// detectorEnabledConfig arms E and its PAIRS detector (default-off testConfig keeps it dark).
type detectorEnabledConfig struct{ testConfig }

func (detectorEnabledConfig) EvidenceEnabled(string) bool { return true }

// TestProcessFlat_DetectorFailureDoesNotBillR pins the detector's before-R placement: the
// detector scores from the completed spine BEFORE the paid analyze_read call, so a detector
// failure fails the record without ever invoking R — a retry re-runs the cheap detector, never
// re-billing R. It also exercises the per-batch detector semaphore path (DetectorConcurrency > 0).
func TestProcessFlat_DetectorFailureDoesNotBillR(t *testing.T) {
	h := newHarness(t)
	detector := producermocks.NewMockPairDetector(gomock.NewController(t))
	h.proc = sa.NewSessionAnalysisProcessor(h.rawDAO, h.analysis, h.state, h.turn, h.phase, h.roundTripFault,
		h.secFlag, h.harnessArtifact, h.classification, h.s3, h.producer, detector, nil, detectorEnabledConfig{})

	orgID, personID := uuid.NewString(), uuid.NewString()
	record := sessionRawRecord(t, orgID, personID, "sha1")
	claimToken := "tok-detector"

	h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
	h.state.EXPECT().ClaimForProcessing(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, s *model.SessionAnalysisState) (bool, error) {
			s.ClaimToken = &claimToken
			return true, nil
		})
	h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(twoHumanTurnDiscardBodyJSON(t), nil)
	h.state.EXPECT().TransitionToSpineDone(gomock.Any(), record.ID, claimToken, "sha1", gomock.Any(), int32(2), gomock.Any()).Return(true, nil)
	h.analysis.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Return(nil)

	// The detector runs on the completed spine (topN/ceiling from config) and fails.
	detector.EXPECT().Detect(gomock.Any(), orgID, record.ID, gomock.Any(), 50, 1000).Return(nil, assert.AnError)

	// R is never billed: the record fails before runReadPass. No E/D, no judged write — only markFailed.
	h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Times(0)
	h.producer.EXPECT().AnalyzeDiscard(gomock.Any(), gomock.Any()).Times(0)
	h.analysis.EXPECT().UpsertJudged(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	h.rawDAO.EXPECT().CurrentSourceUpdatedAt(gomock.Any(), record.ID, gomock.Any()).Return(record.UpdatedAt, nil)
	h.state.EXPECT().TransitionToFailed(gomock.Any(), record.ID, claimToken, gomock.Any(), true, gomock.Any(), gomock.Any()).Return(true, int32(1), nil)

	processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, err, "a per-record failure never fails the batch")
	assert.Equal(t, 0, processed)
}

// TestProcessFlat_DetectorUnavailableDefersWithoutCounting pins that a detector AVAILABILITY
// failure (a 503/KindServer from the token-free detector service) is a non-counting deferred
// retry, NOT a failure: the record is left actionable (no markFailed, no attempt increment, no
// judged write) so a small pilot org cannot exhaust its records to JUDGE_FAILED while the
// detector is warming or down. A deterministic detector error still fails (the sibling test).
func TestProcessFlat_DetectorUnavailableDefersWithoutCounting(t *testing.T) {
	h := newHarness(t)
	detector := producermocks.NewMockPairDetector(gomock.NewController(t))
	h.proc = sa.NewSessionAnalysisProcessor(h.rawDAO, h.analysis, h.state, h.turn, h.phase, h.roundTripFault,
		h.secFlag, h.harnessArtifact, h.classification, h.s3, h.producer, detector, nil, detectorEnabledConfig{})

	orgID, personID := uuid.NewString(), uuid.NewString()
	record := sessionRawRecord(t, orgID, personID, "sha1")
	claimToken := "tok-detector-503"

	h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
	h.state.EXPECT().ClaimForProcessing(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, s *model.SessionAnalysisState) (bool, error) {
			s.ClaimToken = &claimToken
			return true, nil
		})
	h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(twoHumanTurnDiscardBodyJSON(t), nil)
	h.state.EXPECT().TransitionToSpineDone(gomock.Any(), record.ID, claimToken, "sha1", gomock.Any(), int32(2), gomock.Any()).Return(true, nil)
	h.analysis.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Return(nil)

	// The detector service is unavailable (a 503 → KindServer → isProviderTransient).
	detector.EXPECT().Detect(gomock.Any(), orgID, record.ID, gomock.Any(), 50, 1000).
		Return(nil, &refllmclient.Error{Kind: refllmclient.KindServer, Status: 503})

	// Deferred, not failed: R never runs, nothing is judged, and — crucially — markFailed does
	// NOT fire, so the attempt count never climbs toward the cap. The SPINE_DONE row is left
	// actionable for a later tick (via lease expiry) once the detector recovers.
	h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Times(0)
	h.producer.EXPECT().AnalyzeDiscard(gomock.Any(), gomock.Any()).Times(0)
	h.analysis.EXPECT().UpsertJudged(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	h.state.EXPECT().TransitionToFailed(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, err)
	assert.Equal(t, 0, processed)
}

// TestProcessFlat_BodyChangedDuringProducers_CommitsReadButSkipsStaleSecurity is the durable-staged-
// commit + stale-security regression: a body re-uploaded while
// R/E/D were in flight NEVER discards the paid verdict — R still lands JUDGED and serves — but that
// verdict is for a superseded body, so the commit must NOT publish its security flags (a verdict
// known to be stale never overwrites the security surface). Re-analysis of the current body is NOT
// forced here: it rides the normal CONTENT_CHANGED re-admit on the reprocess throttle, so an
// actively-edited session is not re-billed on every change. The record's claim-time digest is
// "sha2"; the harness's current-digest stub returns "sha1", so the commit's freshness re-read sees
// the mismatch. Assertions: JUDGED lands (durable) and ReconcileOpenFlags is NEVER called for the
// stale verdict.
func TestProcessFlat_BodyChangedDuringProducers_CommitsReadButSkipsStaleSecurity(t *testing.T) {
	h := newHarness(t)
	orgID, personID := uuid.NewString(), uuid.NewString()
	record := sessionRawRecord(t, orgID, personID, "sha2")
	claimToken := "tok-body-changed"

	h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
	h.state.EXPECT().ClaimForProcessing(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, s *model.SessionAnalysisState) (bool, error) {
			s.ClaimToken = &claimToken
			return true, nil
		})
	h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(discardBodyJSON(t), nil)
	h.state.EXPECT().TransitionToSpineDone(gomock.Any(), record.ID, claimToken, "sha2", gomock.Any(), int32(2), gomock.Any()).Return(true, nil)
	h.analysis.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Return(nil)
	h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Return(&sa.ReadResult{}, nil)
	h.producer.EXPECT().AnalyzeDiscard(gomock.Any(), gomock.Any()).Return(&sa.DiscardResult{}, nil)
	// The commit lands JUDGED despite the mid-flight body change — no reject, no requeue.
	// discardBodyJSON has zero prompts (supported ide_type), so the measured writer retires
	// the whole prior tail with maxPromptIndex -1 and writes no turn rows.
	h.analysis.EXPECT().UpsertJudged(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	h.turn.EXPECT().SoftDeleteMeasuredBeyond(gomock.Any(), orgID, record.ID, int32(-1)).Return(int64(0), nil)
	h.phase.EXPECT().ReplacePhaseSet(gomock.Any(), orgID, gomock.Any(), gomock.Any()).Return(map[int32]string{}, nil)
	// The freshness re-read saw claim "sha2" vs current "sha1" → the verdict is stale; R still lands
	// JUDGED (durable), and re-analysis of the current body rides the normal reprocess throttle.
	h.state.EXPECT().TransitionToJudged(gomock.Any(), record.ID, claimToken, int32(0), gomock.Any()).Return(true, nil)
	// The stale verdict must NOT publish its security flags — the throttled re-run reconciles against
	// the current body. secFlag has no default stub, so this Times(0) is the enforced skip guard.
	h.secFlag.EXPECT().ReconcileOpenFlags(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
}

// TestProcessFlat_ClaimStampsRepoName is the repo_name-on-claim regression: the claim path
// (ClaimForProcessing) must stamp repo_name onto the state row the same way it stamps person_id,
// so a processed→JUDGED session carries it — not only the SKIP path (applySkip). Before the fix a
// claimed row's session_analysis_state.repo_name stayed NULL while skipped (child) rows had it,
// leaving the repo-scoped scan legs blind to a claimed row.
func TestProcessFlat_ClaimStampsRepoName(t *testing.T) {
	h := newHarness(t)
	orgID, personID := uuid.NewString(), uuid.NewString()
	record := sessionRawRecord(t, orgID, personID, "sha1")
	record.Metadata = mustJSON(t, map[string]string{"person_id": personID, "repo_name": "example-org/example-repo"})
	claimToken := "tok-repo"

	var claimedRepo string
	h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
	h.state.EXPECT().ClaimForProcessing(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, s *model.SessionAnalysisState) (bool, error) {
			if s.RepoName != nil {
				claimedRepo = *s.RepoName
			}
			s.ClaimToken = &claimToken
			return true, nil
		})
	h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(discardBodyJSON(t), nil)
	h.state.EXPECT().TransitionToSpineDone(gomock.Any(), record.ID, claimToken, "sha1", gomock.Any(), int32(2), gomock.Any()).Return(true, nil)
	h.analysis.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Return(nil)
	h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Return(&sa.ReadResult{}, nil)
	h.producer.EXPECT().AnalyzeDiscard(gomock.Any(), gomock.Any()).Return(&sa.DiscardResult{}, nil)
	h.analysis.EXPECT().UpsertJudged(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	h.turn.EXPECT().SoftDeleteMeasuredBeyond(gomock.Any(), orgID, record.ID, int32(-1)).Return(int64(0), nil)
	h.phase.EXPECT().ReplacePhaseSet(gomock.Any(), orgID, gomock.Any(), gomock.Any()).Return(map[int32]string{}, nil)
	h.state.EXPECT().TransitionToJudged(gomock.Any(), record.ID, claimToken, int32(0), gomock.Any()).Return(true, nil)
	h.secFlag.EXPECT().ReconcileOpenFlags(gomock.Any(), record.ID, gomock.Any()).Return(nil)

	processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	assert.Equal(t, "example-org/example-repo", claimedRepo, "the claim path must stamp repo_name on the state row (like person_id)")
}

// TestProcessFlat_SpineOnlyReprocess_ZeroLLM is the spine-only-zero-LLM guardrail
// : a spine-version bump whose producer-read inputs are
// unchanged (same body content sha, same READ contract) skips every LLM call and
// re-commits the prior generation's call_state hashes unchanged.
func TestProcessFlat_SpineOnlyReprocess_ZeroLLM(t *testing.T) {
	h := newHarness(t)
	orgID, personID := uuid.NewString(), uuid.NewString()
	record := sessionRawRecord(t, orgID, personID, "sha1")
	priorReadHash := sa.ReadInputHash("sha1", testConfig{}.ContractVersionRead())
	// A real JUDGED prior always carries a settled call-state entry for every enabled producer
	// (E/D are stamped on every full run), so the spine-only reprocess decision keys off all
	// three; a fixture with only READ would spuriously read E/D as unsettled and force a re-run.
	priorCallState := &sessionintelligencepb.CallStateMap{Calls: map[string]*sessionintelligencepb.CallState{
		callKeyRead: {InputHash: priorReadHash, ContractVersion: 1, Status: sessionintelligencepb.CallStatus_CALL_STATUS_DONE},
		"EVIDENCE":  {ContractVersion: 1, Status: sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED},
		"DISCARD":   {ContractVersion: 1, Status: sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED},
	}}
	prior := &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: record.ID,
		Status:            sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED,
		VocabularyVersion: 1,
		CallState:         priorCallState,
	}
	reclaimedToken := "tok-reclaim"

	h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryVersionBehind})
	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
	// Reclaim path: prior (JUDGED), reclaim, reload with a fresh token.
	h.state.EXPECT().GetByRawRecordID(gomock.Any(), record.ID).Return(prior, nil)
	h.state.EXPECT().Reclaim(gomock.Any(), record.ID, gomock.Any(), true).Return(true, nil)
	h.state.EXPECT().GetByRawRecordID(gomock.Any(), record.ID).Return(&model.SessionAnalysisState{
		RawRecordID: record.ID, ClaimToken: &reclaimedToken,
	}, nil)
	h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(discardBodyJSON(t), nil)
	h.state.EXPECT().TransitionToSpineDone(gomock.Any(), record.ID, reclaimedToken, "sha1", gomock.Any(), int32(2), gomock.Any()).Return(true, nil)

	// The guardrail: NO producer is called.
	h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Times(0)
	h.producer.EXPECT().AnalyzeEvidence(gomock.Any(), gomock.Any()).Times(0)
	h.producer.EXPECT().AnalyzeDiscard(gomock.Any(), gomock.Any()).Times(0)

	// The spine-only re-affirm path: UpsertMeasuredReaffirmingJudged refreshes measured columns
	// AND restores judged_at (a cat-3 legacy reopen may have cleared it) without touching the
	// judged verdict; plain UpsertMeasured and UpsertJudged must NOT be called (the latter would
	// NULL the prior verdict via UpdateAll).
	h.analysis.EXPECT().UpsertMeasuredReaffirmingJudged(gomock.Any(), gomock.Any()).Return(nil)
	h.analysis.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Times(0)
	h.analysis.EXPECT().UpsertJudged(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	// R did not run, so no turn/phase re-derive either — the prior grain is carried.
	h.phase.EXPECT().ReplacePhaseSet(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	h.turn.EXPECT().UpdateJudged(gomock.Any(), gomock.Any()).Times(0)
	var committed dao.JudgedCommit
	h.state.EXPECT().TransitionToJudged(gomock.Any(), record.ID, reclaimedToken, int32(0), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ int32, c dao.JudgedCommit) (bool, error) {
			committed = c
			return true, nil
		})
	// The CRITICAL guard: a measured-only re-commit must NOT touch the security flags —
	// R did not run, so the prior generation's security readout is carried unchanged.
	// Clearing here would wipe a live api-key-leak flag with no replacement. newHarness
	// registers no default ReconcileOpenFlags expectation, so this Times(0) is the ONLY
	// one in play here — a shared permissive default would swallow any real call and
	// make this guard unenforceable (gomock matches the first registered call that fits).
	h.secFlag.EXPECT().ReconcileOpenFlags(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)

	// The committed call_state is the prior generation's, hashes unchanged.
	require.NotNil(t, committed.CallState)
	require.Contains(t, committed.CallState.Calls, callKeyRead)
	assert.Equal(t, priorReadHash, committed.CallState.Calls[callKeyRead].GetInputHash(),
		"a spine-only reprocess must not change the READ call input hash")
}

// outageMinOneConfig makes a single provider-side transient failure activate the outage
// signal, so the increment decision is testable with one record.
type outageMinOneConfig struct{ testConfig }

func (outageMinOneConfig) OutageSignalMinRecords() int { return 1 }

// TestMarkFailed_OutageGatesAttemptIncrement asserts the retry-cap protection: a provider-
// side transient failure under an active outage does NOT increment attempt_count, while a
// record-class failure always does.
func TestMarkFailed_OutageGatesAttemptIncrement(t *testing.T) {
	tests := []struct {
		name            string
		producerErr     error
		wantIncrement   bool
		attemptAfter    int32 // attempt_count after this transition (RetryCap is 3)
		wantJudgeFailed bool  // JUDGE_FAILED is stamped only once the failure is terminal (attemptAfter >= RetryCap)
	}{
		{name: "transient 5xx under an active outage does not increment; retryable, no JUDGE_FAILED", producerErr: &refllmclient.Error{Kind: refllmclient.KindServer}, wantIncrement: false, attemptAfter: 0, wantJudgeFailed: false},
		{name: "record-class 4xx increments; still retryable, no JUDGE_FAILED", producerErr: &refllmclient.Error{Kind: refllmclient.KindClient}, wantIncrement: true, attemptAfter: 1, wantJudgeFailed: false},
		{name: "record-class 4xx exhausting the retry budget is terminal; JUDGE_FAILED stamped", producerErr: &refllmclient.Error{Kind: refllmclient.KindClient}, wantIncrement: true, attemptAfter: 3, wantJudgeFailed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			h := &harness{
				rawDAO:          daomocks.NewMockIRawRecordDAO(ctrl),
				analysis:        daomocks.NewMockISessionAnalysisDAO(ctrl),
				state:           daomocks.NewMockISessionAnalysisStateDAO(ctrl),
				turn:            daomocks.NewMockISessionTurnAnalysisDAO(ctrl),
				phase:           daomocks.NewMockISessionPhaseAnalysisDAO(ctrl),
				roundTripFault:  daomocks.NewMockISessionRoundTripFaultDAO(ctrl),
				secFlag:         daomocks.NewMockISecurityFlagDAO(ctrl),
				harnessArtifact: daomocks.NewMockIHarnessArtifactDAO(ctrl),
				classification:  daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl),
				s3:              s3mocks.NewMockS3Client(ctrl),
				producer:        producermocks.NewMockLLMProducer(ctrl),
			}
			h.proc = sa.NewSessionAnalysisProcessor(h.rawDAO, h.analysis, h.state, h.turn, h.phase, h.roundTripFault, h.secFlag, h.harnessArtifact, h.classification, h.s3, h.producer, nil, nil, outageMinOneConfig{})
			h.roundTripFault.EXPECT().ReplaceSet(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			h.secFlag.EXPECT().ReconcileOpenFlags(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			h.rawDAO.EXPECT().AggregateSubagentUsageByParent(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

			orgID, personID := uuid.NewString(), uuid.NewString()
			record := sessionRawRecord(t, orgID, personID, "sha1")
			claimToken := "tok-fail"

			h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
			h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
			h.state.EXPECT().ClaimForProcessing(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, s *model.SessionAnalysisState) (bool, error) {
					s.ClaimToken = &claimToken
					return true, nil
				})
			h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(discardBodyJSON(t), nil)
			h.state.EXPECT().TransitionToSpineDone(gomock.Any(), record.ID, claimToken, "sha1", gomock.Any(), int32(2), gomock.Any()).Return(true, nil)
			h.analysis.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Return(nil)
			h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Return(nil, tt.producerErr)
			// markFailed re-reads the attempted body's digest + updated_at to stamp the anchor.
			h.rawDAO.EXPECT().CurrentContentDigest(gomock.Any(), record.ID, gomock.Any()).Return("sha1", nil)
			h.rawDAO.EXPECT().CurrentSourceUpdatedAt(gomock.Any(), record.ID, gomock.Any()).Return(record.UpdatedAt, nil)
			// The attempt-increment flag matches the outage classification, and the transition
			// reports the resulting attempt_count so markFailed can tell retryable from terminal.
			h.state.EXPECT().TransitionToFailed(gomock.Any(), record.ID, claimToken, gomock.Any(), tt.wantIncrement, gomock.Any(), gomock.Any()).Return(true, tt.attemptAfter, nil)
			// Static-on-failure serving: JUDGE_FAILED is
			// stamped ONLY once the failure is terminal (attempt_count >= RetryCap); a still-retryable
			// failure leaves the row measured-only so it does not serve as terminally failed mid-retry.
			wantJudgeFailedTimes := 0
			if tt.wantJudgeFailed {
				wantJudgeFailedTimes = 1
			}
			h.analysis.EXPECT().MarkJudgeFailed(gomock.Any(), record.ID).Return(true, nil).Times(wantJudgeFailedTimes)

			processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
			require.NoError(t, err, "a per-record failure never fails the batch")
			assert.Equal(t, 0, processed, "a failed record is not counted processed")
		})
	}
}

// vocab2Config bumps the current vocabulary version to 2.
type vocab2Config struct{ testConfig }

func (vocab2Config) VocabularyVersion() int32 { return 2 }

// TestProcessFlat_VocabBump_SupersedesAndReprocesses asserts that when the
// vocabulary version increased, a version-behind row must NOT be classified spine-only
// (which would restore the old vocab and loop forever) — it supersedes the old
// generation (state + content) and creates a FRESH generation at the new vocab, running
// the producers. Asserts the supersede calls fire, Reclaim does NOT, and R runs.
func TestProcessFlat_VocabBump_SupersedesAndReprocesses(t *testing.T) {
	ctrl := gomock.NewController(t)
	h := &harness{
		rawDAO:          daomocks.NewMockIRawRecordDAO(ctrl),
		analysis:        daomocks.NewMockISessionAnalysisDAO(ctrl),
		state:           daomocks.NewMockISessionAnalysisStateDAO(ctrl),
		turn:            daomocks.NewMockISessionTurnAnalysisDAO(ctrl),
		phase:           daomocks.NewMockISessionPhaseAnalysisDAO(ctrl),
		roundTripFault:  daomocks.NewMockISessionRoundTripFaultDAO(ctrl),
		secFlag:         daomocks.NewMockISecurityFlagDAO(ctrl),
		harnessArtifact: daomocks.NewMockIHarnessArtifactDAO(ctrl),
		classification:  daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl),
		s3:              s3mocks.NewMockS3Client(ctrl),
		producer:        producermocks.NewMockLLMProducer(ctrl),
	}
	h.proc = sa.NewSessionAnalysisProcessor(h.rawDAO, h.analysis, h.state, h.turn, h.phase, h.roundTripFault, h.secFlag, h.harnessArtifact, h.classification, h.s3, h.producer, nil, nil, vocab2Config{})
	h.roundTripFault.EXPECT().ReplaceSet(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	h.rawDAO.EXPECT().AggregateSubagentUsageByParent(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
	// The commit re-reads the current content digest; the seeded record's is "sha1", so the
	// digest-revalidation matches and the commit proceeds.
	h.rawDAO.EXPECT().CurrentContentDigest(gomock.Any(), gomock.Any(), gomock.Any()).Return("sha1", nil).AnyTimes()

	orgID, personID := uuid.NewString(), uuid.NewString()
	record := sessionRawRecord(t, orgID, personID, "sha1")
	record.Metadata = mustJSON(t, map[string]string{"person_id": personID, "repo_name": "example-org/example-repo"})
	freshToken := "tok-fresh"
	// The live prior generation is at the OLD vocab (1); current is 2 → a vocab bump.
	prior := &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: record.ID,
		Status:            sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED,
		VocabularyVersion: 1,
	}

	h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryVersionBehind})
	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
	h.state.EXPECT().GetByRawRecordID(gomock.Any(), record.ID).Return(prior, nil)
	// The supersede path: old state + old content superseded, a fresh generation claimed.
	h.state.EXPECT().Supersede(gomock.Any(), record.ID, int32(1)).Return(true, nil)
	h.analysis.EXPECT().SupersedeContent(gomock.Any(), record.ID).Return(true, nil)
	h.state.EXPECT().ClaimForProcessing(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, s *model.SessionAnalysisState) (bool, error) {
			assert.Equal(t, int32(2), s.VocabularyVersion, "the fresh generation is stamped the new vocab")
			require.NotNil(t, s.RepoName, "the fresh-generation claim stamps repo_name (like person_id)")
			assert.Equal(t, "example-org/example-repo", *s.RepoName)
			s.ClaimToken = &freshToken
			return true, nil
		})
	// The in-place Reclaim path must NOT be taken.
	h.state.EXPECT().Reclaim(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(discardBodyJSON(t), nil)
	h.state.EXPECT().TransitionToSpineDone(gomock.Any(), record.ID, freshToken, "sha1", gomock.Any(), int32(2), gomock.Any()).Return(true, nil)
	h.analysis.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Return(nil)
	// Producers RUN — a vocab bump is not spine-only.
	h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Return(&sa.ReadResult{}, nil)
	h.producer.EXPECT().AnalyzeDiscard(gomock.Any(), gomock.Any()).Return(&sa.DiscardResult{}, nil)
	h.analysis.EXPECT().UpsertJudged(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	// discardBodyJSON has zero prompts (but is a supported ide_type), so the measured writer
	// retires the whole prior tail with maxPromptIndex -1 and writes no turn rows.
	h.turn.EXPECT().SoftDeleteMeasuredBeyond(gomock.Any(), orgID, record.ID, int32(-1)).Return(int64(0), nil)
	h.phase.EXPECT().ReplacePhaseSet(gomock.Any(), orgID, gomock.Any(), gomock.Any()).Return(map[int32]string{}, nil)
	h.state.EXPECT().TransitionToJudged(gomock.Any(), record.ID, freshToken, int32(0), gomock.Any()).Return(true, nil)
	h.secFlag.EXPECT().ReconcileOpenFlags(gomock.Any(), record.ID, gomock.Any()).Return(nil)

	processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
}

// TestProcessFlat_SecurityReconcile is the security-flag reconcile guardrail
// : R's security-layer readout is reconciled into
// security_flags per raw_record in the commit transaction via the shared DAO reconcile
// (dao.SecurityFlagRepo.ReconcileOpenFlags) — the same method the session path calls,
// so the existing resolve/serving stays unchanged.
func TestProcessFlat_SecurityReconcile(t *testing.T) {
	h := newHarness(t)
	orgID, personID := uuid.NewString(), uuid.NewString()
	record := sessionRawRecord(t, orgID, personID, "sha1")
	claimToken := "tok-new"

	h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
	// Fresh claim (no-state): ClaimForProcessing stamps the token on the state.
	h.state.EXPECT().ClaimForProcessing(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, s *model.SessionAnalysisState) (bool, error) {
			s.ClaimToken = &claimToken
			return true, nil
		})
	h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(discardBodyJSON(t), nil)
	h.state.EXPECT().TransitionToSpineDone(gomock.Any(), record.ID, claimToken, "sha1", gomock.Any(), int32(2), gomock.Any()).Return(true, nil)
	h.analysis.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Return(nil)

	// R returns a security-layer PII injection at high severity.
	h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Return(&sa.ReadResult{
		SecurityIssues: []*sessionintelligencepb.PromptIssue{{
			Slug:     sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_PII_INJECTION,
			Severity: sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH,
			Layer:    "security",
			Note:     "email leaked",
		}},
	}, nil)
	h.producer.EXPECT().AnalyzeDiscard(gomock.Any(), gomock.Any()).Return(&sa.DiscardResult{}, nil)

	h.analysis.EXPECT().UpsertJudged(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	// discardBodyJSON has zero prompts (supported ide_type), so the measured writer retires the
	// whole prior tail with maxPromptIndex -1 and writes no turn rows.
	h.turn.EXPECT().SoftDeleteMeasuredBeyond(gomock.Any(), orgID, record.ID, int32(-1)).Return(int64(0), nil)
	// R returned no turns/phases here, so the FK-safe grain write is an empty phase
	// replace + the trailing-turn retire.
	h.phase.EXPECT().ReplacePhaseSet(gomock.Any(), orgID, gomock.Any(), gomock.Any()).Return(map[int32]string{}, nil)
	h.state.EXPECT().TransitionToJudged(gomock.Any(), record.ID, claimToken, int32(0), gomock.Any()).Return(true, nil)
	var created []*model.SecurityFlag
	h.secFlag.EXPECT().ReconcileOpenFlags(gomock.Any(), record.ID, gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, flags []*model.SecurityFlag) error {
			created = flags
			return nil
		})

	processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)

	require.Len(t, created, 1)
	assert.Equal(t, "pii-injection", created[0].FlagType)
	assert.Equal(t, "high", created[0].Severity)
	assert.Equal(t, record.ID, created[0].RawRecordID)
	assert.Equal(t, personID, created[0].PersonID)
	require.NotNil(t, created[0].FlagDetails)
	require.Len(t, created[0].FlagDetails.GetOccurrences(), 1)
	assert.Equal(t, "email leaked", created[0].FlagDetails.GetOccurrences()[0].GetSanitizedExcerpt())
}

// cutoverConfig arms a non-zero per-org cutover on top of testConfig (which returns zero).
type cutoverConfig struct {
	testConfig
	cutover time.Time
}

func (c cutoverConfig) CutoverDate(string) time.Time { return c.cutover }

// TestProcessFlat_ThreadsOrgCutoverIntoScan is the processor→scan seam for the hard floor: the
// processor resolves the org's configured cutover and threads it into the actionable scan's
// CutoverDate param, which is the SQL floor (data_date >= cutover, exercised at real-DB grain by
// TestSessionAnalysisStateRepo_CutoverHardFloor). Together they prove the config value actually
// reaches the predicate that picks records.
func TestProcessFlat_ThreadsOrgCutoverIntoScan(t *testing.T) {
	h := newHarness(t)
	orgID := uuid.NewString()
	cutover := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	h.proc = sa.NewSessionAnalysisProcessor(h.rawDAO, h.analysis, h.state, h.turn, h.phase, h.roundTripFault,
		h.secFlag, h.harnessArtifact, h.classification, h.s3, h.producer, nil, nil, cutoverConfig{cutover: cutover})

	var liveCutover, backfillCutover time.Time
	h.state.EXPECT().ScanLiveActionable(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, params dao.SessionAnalysisScanParams, _ int) ([]*dao.ActionableCandidate, error) {
			liveCutover = params.CutoverDate
			return nil, nil
		})
	h.state.EXPECT().ScanBackfillActionable(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, params dao.SessionAnalysisScanParams, _ int) ([]*dao.ActionableCandidate, error) {
			backfillCutover = params.CutoverDate
			return nil, nil
		})

	_, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, err)
	assert.Equal(t, cutover, liveCutover, "the org's configured cutover threads into the LIVE scan floor")
	assert.Equal(t, cutover, backfillCutover, "and into the re-admission scan params")
}
