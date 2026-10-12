// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	coreaidlcv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/aidlc/v1"
	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	daomocks "github.com/tetriz-ai/session-intelligence/internal/dao/mocks"
)

// deploySkillInvocation is one SKILL invocation named "deploy" — the artifact-intent resolver's
// resolve key in these tests.
func deploySkillInvocation() invocationDetail {
	return invocationDetail{artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, slug: "deploy"}
}

// planSkillInvocation is a second SKILL invocation ("plan") sharing the (SKILL, source) inventory
// with deploySkillInvocation, so a resolve over both must fetch that inventory only once.
func planSkillInvocation() invocationDetail {
	return invocationDetail{artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, slug: "plan"}
}

// eRunSpine builds a fresh (no prior) spine that fires E on its artifact ledger and carries one
// resolvable "deploy" SKILL invocation.
func eRunSpine() *spineResult {
	return &spineResult{
		contentSha256: "sha1",
		ideType:       sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		ledgers:       spineLedgers{artifactInvocations: 1, invocations: []invocationDetail{deploySkillInvocation()}},
	}
}

// TestRunProducers_EvidenceRun_PendsOnUnclassifiedIntent: at E-run time an invoked artifact whose
// intent the sweep has not yet classified defers E — the call is persisted SKIPPED_PENDING_DATA
// with exactly that artifact id, and AnalyzeEvidence is NOT called.
func TestRunProducers_EvidenceRun_PendsOnUnclassifiedIntent(t *testing.T) {
	ctrl := gomock.NewController(t)
	orgID, rawRecordID := "org", "rr"
	prod := &countingProducer{readResult: &ReadResult{}}
	artifactDAO := daomocks.NewMockIHarnessArtifactDAO(ctrl)
	classificationDAO := daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl)

	artifactDAO.EXPECT().ListLiveForSource(gomock.Any(), orgID, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
		commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_VCS, gomock.Any()).
		Return([]*model.HarnessArtifact{{ID: "a1", OrganizationID: orgID, Name: "deploy"}}, nil)
	// intent_class is NULL → the artifact is pending.
	classificationDAO.EXPECT().GetByArtifact(gomock.Any(), orgID, intentClassificationTask, "a1").
		Return(&model.HarnessArtifactClassification{OrganizationID: orgID, HarnessArtifactID: "a1"}, nil)

	p := &SessionAnalysisProcessor{
		producer:           prod,
		cfg:                repickTestConfig{},
		harnessArtifactDAO: artifactDAO,
		classificationDAO:  classificationDAO,
	}
	sp := eRunSpine()

	read, _, err := p.runProducers(context.Background(), orgID,
		&model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, nil, sp, nil)

	require.NoError(t, err)
	require.NotNil(t, read, "R still ran and committed; only E defers")
	assert.Equal(t, 0, prod.evidenceCalls, "E must not run while an invoked artifact's intent is unresolved")
	evidence := sp.callState.GetCalls()[producerEvidence]
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA, evidence.GetStatus())
	assert.Equal(t, []string{"a1"}, evidence.GetPendingArtifactIds(), "exactly the unclassified artifact id is persisted")
	assert.Equal(t, "sha1", evidence.GetSourceContentSha256(), "the body digest is snapshotted for the re-pick")
	assert.Equal(t, bitEvidence, sp.pendingDataProducers)
}

// TestRunProducers_EvidenceRun_NoRowIsNotPending: an invocation that resolves to NO live artifact
// (neither VCS nor DESKTOP) is NOT pending — E proceeds with that intent unknown rather than
// stranding forever, the intent read is never issued, and the unresolved counter increments.
func TestRunProducers_EvidenceRun_NoRowIsNotPending(t *testing.T) {
	ctrl := gomock.NewController(t)
	orgID, rawRecordID := "org-norow", "rr"
	prod := &countingProducer{readResult: &ReadResult{}}
	artifactDAO := daomocks.NewMockIHarnessArtifactDAO(ctrl)
	classificationDAO := daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl)

	// VCS returns nothing, so the DESKTOP fallback is consulted; both are empty ⇒ no row.
	artifactDAO.EXPECT().ListLiveForSource(gomock.Any(), orgID, gomock.Any(), gomock.Any(),
		commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_VCS, gomock.Any()).Return(nil, nil)
	artifactDAO.EXPECT().ListLiveForSource(gomock.Any(), orgID, gomock.Any(), gomock.Any(),
		commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_DESKTOP, gomock.Any()).Return(nil, nil)
	// No resolved row ⇒ the intent read is never issued (gomock fails on any unexpected call).

	p := &SessionAnalysisProcessor{
		producer:           prod,
		cfg:                repickTestConfig{},
		harnessArtifactDAO: artifactDAO,
		classificationDAO:  classificationDAO,
	}
	sp := eRunSpine()
	before := testutil.ToFloat64(sessionEvidenceArtifactUnresolvedTotal.WithLabelValues(orgID))

	read, _, err := p.runProducers(context.Background(), orgID,
		&model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, nil, sp, nil)

	require.NoError(t, err)
	require.NotNil(t, read)
	assert.Equal(t, 1, prod.evidenceCalls, "a no-row invocation is not pending — E runs")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, sp.callState.GetCalls()[producerEvidence].GetStatus())
	assert.Equal(t, int32(0), sp.pendingDataProducers)
	assert.Equal(t, before+1, testutil.ToFloat64(sessionEvidenceArtifactUnresolvedTotal.WithLabelValues(orgID)),
		"a no-row invocation increments the unresolved counter so a systemic defect is visible")
}

// TestRunProducers_EvidenceRun_AllIntentPresentRuns: every resolved artifact already carries a
// classified intent, so E runs (no pending), and VCS winning means the DESKTOP fallback is never read.
func TestRunProducers_EvidenceRun_AllIntentPresentRuns(t *testing.T) {
	ctrl := gomock.NewController(t)
	orgID, rawRecordID := "org", "rr"
	prod := &countingProducer{readResult: &ReadResult{}}
	artifactDAO := daomocks.NewMockIHarnessArtifactDAO(ctrl)
	classificationDAO := daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl)

	artifactDAO.EXPECT().ListLiveForSource(gomock.Any(), orgID, gomock.Any(), gomock.Any(),
		commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_VCS, gomock.Any()).
		Return([]*model.HarnessArtifact{{ID: "a1", OrganizationID: orgID, Name: "deploy"}}, nil)
	// A DESKTOP read here would fail the test — VCS matched, so the fallback must not fire.
	reviewIntent := coreaidlcv1.ArtifactIntentClass_ARTIFACT_INTENT_CLASS_REVIEW
	classificationDAO.EXPECT().GetByArtifact(gomock.Any(), orgID, intentClassificationTask, "a1").
		Return(&model.HarnessArtifactClassification{OrganizationID: orgID, HarnessArtifactID: "a1", IntentClass: &reviewIntent}, nil)

	p := &SessionAnalysisProcessor{
		producer:           prod,
		cfg:                repickTestConfig{},
		harnessArtifactDAO: artifactDAO,
		classificationDAO:  classificationDAO,
	}
	sp := eRunSpine()

	_, _, err := p.runProducers(context.Background(), orgID,
		&model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, nil, sp, nil)

	require.NoError(t, err)
	assert.Equal(t, 1, prod.evidenceCalls, "all resolved artifacts are classified — E runs")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, sp.callState.GetCalls()[producerEvidence].GetStatus())
}

// TestRunProducers_EvidenceRun_IntentReadErrorDefers: a REAL intent-classification DB fault (not a
// missing row) on the intent resolve defers the record non-counting — because the resolve runs
// BEFORE the paid R call, runProducers returns errIntentResolverUnavailable (so the caller never
// markFails it) with R NEVER inferred (no re-bill on retry), and the classification-domain deferred
// counter climbs.
func TestRunProducers_EvidenceRun_IntentReadErrorDefers(t *testing.T) {
	ctrl := gomock.NewController(t)
	orgID, rawRecordID := "org-interr", "rr"
	prod := &countingProducer{readResult: &ReadResult{}}
	artifactDAO := daomocks.NewMockIHarnessArtifactDAO(ctrl)
	classificationDAO := daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl)

	artifactDAO.EXPECT().ListLiveForSource(gomock.Any(), orgID, gomock.Any(), gomock.Any(),
		commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_VCS, gomock.Any()).
		Return([]*model.HarnessArtifact{{ID: "a1", OrganizationID: orgID, Name: "deploy"}}, nil)
	classificationDAO.EXPECT().GetByArtifact(gomock.Any(), orgID, intentClassificationTask, "a1").
		Return(nil, errors.New("db down")) // a real DB fault, distinct from ErrNotFound

	p := &SessionAnalysisProcessor{
		producer:           prod,
		cfg:                repickTestConfig{},
		harnessArtifactDAO: artifactDAO,
		classificationDAO:  classificationDAO,
	}
	sp := eRunSpine()
	beforeIntent := testutil.ToFloat64(sessionEvidenceIntentDeferredTotal.WithLabelValues(orgID))
	beforeList := testutil.ToFloat64(sessionEvidenceArtifactListDeferredTotal.WithLabelValues(orgID))

	read, _, err := p.runProducers(context.Background(), orgID,
		&model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, nil, sp, nil)

	require.ErrorIs(t, err, errIntentResolverUnavailable, "a real intent-read DB fault defers non-counting, never markFails")
	assert.Nil(t, read)
	assert.Equal(t, 0, prod.readCalls, "R is NOT paid — the intent resolve defers before the paid R call, so a retry never re-bills R")
	assert.Equal(t, 0, prod.evidenceCalls, "E does not run when its intent read faulted")
	assert.Equal(t, beforeIntent+1, testutil.ToFloat64(sessionEvidenceIntentDeferredTotal.WithLabelValues(orgID)),
		"a classification-read fault increments the intent-deferred counter")
	assert.Equal(t, beforeList, testutil.ToFloat64(sessionEvidenceArtifactListDeferredTotal.WithLabelValues(orgID)),
		"the inventory-deferred counter is untouched by a classification-read fault")
}

// TestRunProducers_EvidenceRun_InventoryReadErrorDefers: a REAL harness_artifacts inventory DB fault
// on the intent resolve defers the record non-counting — like the classification-domain case it runs
// BEFORE the paid R call (R never inferred, no re-bill on retry), but it increments the DISTINCT
// inventory-domain counter, not the intent one.
func TestRunProducers_EvidenceRun_InventoryReadErrorDefers(t *testing.T) {
	ctrl := gomock.NewController(t)
	orgID, rawRecordID := "org-listerr", "rr"
	prod := &countingProducer{readResult: &ReadResult{}}
	artifactDAO := daomocks.NewMockIHarnessArtifactDAO(ctrl)
	classificationDAO := daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl)

	// The inventory fetch itself faults — the classification read is never reached.
	artifactDAO.EXPECT().ListLiveForSource(gomock.Any(), orgID, gomock.Any(), gomock.Any(),
		commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_VCS, gomock.Any()).
		Return(nil, errors.New("db down"))

	p := &SessionAnalysisProcessor{
		producer:           prod,
		cfg:                repickTestConfig{},
		harnessArtifactDAO: artifactDAO,
		classificationDAO:  classificationDAO,
	}
	sp := eRunSpine()
	beforeIntent := testutil.ToFloat64(sessionEvidenceIntentDeferredTotal.WithLabelValues(orgID))
	beforeList := testutil.ToFloat64(sessionEvidenceArtifactListDeferredTotal.WithLabelValues(orgID))

	read, _, err := p.runProducers(context.Background(), orgID,
		&model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, nil, sp, nil)

	require.ErrorIs(t, err, errIntentResolverUnavailable, "an inventory-read DB fault defers non-counting, never markFails")
	assert.Nil(t, read)
	assert.Equal(t, 0, prod.readCalls, "R is NOT paid — the inventory resolve defers before the paid R call")
	assert.Equal(t, 0, prod.evidenceCalls, "E does not run when its inventory read faulted")
	assert.Equal(t, beforeList+1, testutil.ToFloat64(sessionEvidenceArtifactListDeferredTotal.WithLabelValues(orgID)),
		"an inventory-read fault increments the inventory-deferred counter")
	assert.Equal(t, beforeIntent, testutil.ToFloat64(sessionEvidenceIntentDeferredTotal.WithLabelValues(orgID)),
		"the intent-deferred counter is untouched by an inventory-read fault")
}

// TestResolveEvidencePendingArtifactIDs_MemoizesInventoryFetch: a session invoking the same
// (SKILL, VCS) inventory under several slugs — including a repeated slug — fetches that inventory
// exactly ONCE (the per-(type, source, person) memo), and the per-resolved-row intent read is
// deduped so a repeated invocation contributes its id once.
func TestResolveEvidencePendingArtifactIDs_MemoizesInventoryFetch(t *testing.T) {
	ctrl := gomock.NewController(t)
	orgID := "org-memo"
	artifactDAO := daomocks.NewMockIHarnessArtifactDAO(ctrl)
	classificationDAO := daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl)

	// One VCS fetch of (SKILL, VCS) serves deploy, plan, AND the repeated deploy.
	artifactDAO.EXPECT().ListLiveForSource(gomock.Any(), orgID, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
		commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_VCS, gomock.Any()).
		Return([]*model.HarnessArtifact{{ID: "a1", Name: "deploy"}, {ID: "a2", Name: "plan"}}, nil).
		Times(1)
	reviewIntent := coreaidlcv1.ArtifactIntentClass_ARTIFACT_INTENT_CLASS_REVIEW
	classificationDAO.EXPECT().GetByArtifact(gomock.Any(), orgID, intentClassificationTask, "a1").
		Return(&model.HarnessArtifactClassification{IntentClass: &reviewIntent}, nil).Times(1)
	classificationDAO.EXPECT().GetByArtifact(gomock.Any(), orgID, intentClassificationTask, "a2").
		Return(&model.HarnessArtifactClassification{IntentClass: &reviewIntent}, nil).Times(1)

	p := &SessionAnalysisProcessor{harnessArtifactDAO: artifactDAO, classificationDAO: classificationDAO}
	sp := &spineResult{
		ideType: sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		ledgers: spineLedgers{invocations: []invocationDetail{
			deploySkillInvocation(), planSkillInvocation(), deploySkillInvocation(),
		}},
	}

	pending, resolved, err := p.resolveEvidencePendingArtifactIDs(context.Background(), orgID, "p1", sp)
	require.NoError(t, err)
	assert.Empty(t, pending, "every resolved artifact is classified — nothing pending")
	assert.NotEmpty(t, resolved, "a classified artifact's intent is returned for E's render")
}

// TestMatchesInvocationSlug pins the slug match the resolver runs in Go over the fetched inventory:
// a name-arm match (case-insensitive) AND a source_path-basename-slug match (a definition file's
// directory, or a bare filename).
func TestMatchesInvocationSlug(t *testing.T) {
	tests := []struct {
		name string
		row  *model.HarnessArtifact
		slug string
		want bool
	}{
		{name: "name arm, case-insensitive", row: &model.HarnessArtifact{Name: "Deploy"}, slug: "deploy", want: true},
		{name: "source_path basename slug (definition file → dir)", row: &model.HarnessArtifact{Name: "plan-v2", SourcePath: ".claude/skills/plan/SKILL.md"}, slug: "plan", want: true},
		{name: "source_path filename slug (non-definition file)", row: &model.HarnessArtifact{SourcePath: ".claude/commands/ship.md"}, slug: "ship", want: true},
		{name: "no match on either arm", row: &model.HarnessArtifact{Name: "other", SourcePath: ".claude/skills/x/SKILL.md"}, slug: "deploy", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, matchesInvocationSlug(tt.row, tt.slug))
		})
	}
}

// TestArtifactIntentResolved pins the wired resolver's per-id read: a non-UNSPECIFIED intent
// resolves; a NULL or UNSPECIFIED one does not; a missing row is unresolved (not an error); and a
// real DB fault is PROPAGATED (not swallowed) so the re-pick defers non-counting.
func TestArtifactIntentResolved(t *testing.T) {
	reviewIntent := coreaidlcv1.ArtifactIntentClass_ARTIFACT_INTENT_CLASS_REVIEW
	unspecified := coreaidlcv1.ArtifactIntentClass_ARTIFACT_INTENT_CLASS_UNSPECIFIED
	tests := []struct {
		name    string
		result  *model.HarnessArtifactClassification
		err     error
		want    bool
		wantErr bool
	}{
		{name: "classified intent resolves", result: &model.HarnessArtifactClassification{IntentClass: &reviewIntent}, want: true},
		{name: "null intent is unresolved", result: &model.HarnessArtifactClassification{}, want: false},
		{name: "UNSPECIFIED intent is unresolved", result: &model.HarnessArtifactClassification{IntentClass: &unspecified}, want: false},
		{name: "missing classification row is unresolved, not an error", err: dao.ErrNotFound, want: false},
		{name: "a real DB fault propagates (not swallowed)", err: errors.New("db down"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			classificationDAO := daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl)
			classificationDAO.EXPECT().GetByArtifact(gomock.Any(), "org", intentClassificationTask, "a1").
				Return(tt.result, tt.err)
			p := &SessionAnalysisProcessor{classificationDAO: classificationDAO}
			got, err := p.artifactIntentResolved(context.Background(), "org", "a1")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestRunProducers_EvidenceRePick_WiredResolverReSkips: with the REAL wired resolver, a persisted
// pending artifact whose intent is still NULL drives a re-skip — E stays pending and no producer runs.
func TestRunProducers_EvidenceRePick_WiredResolverReSkips(t *testing.T) {
	ctrl := gomock.NewController(t)
	orgID, rawRecordID := "org", "rr"
	prior := priorWithEvidencePending(orgID, rawRecordID, "sha1", []string{"a1"})
	prod := &countingProducer{}
	classificationDAO := daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl)
	classificationDAO.EXPECT().GetByArtifact(gomock.Any(), orgID, intentClassificationTask, "a1").
		Return(&model.HarnessArtifactClassification{OrganizationID: orgID, HarnessArtifactID: "a1"}, nil) // still NULL

	p := &SessionAnalysisProcessor{producer: prod, cfg: repickTestConfig{}, classificationDAO: classificationDAO}
	p.evidenceResolver = p.artifactIntentResolved // the constructor's wiring
	sp := &spineResult{contentSha256: "sha1"}

	read, _, err := p.runProducers(context.Background(), orgID,
		&model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, prior, sp, nil)

	require.ErrorIs(t, err, errProducersSkipped)
	assert.Nil(t, read)
	assert.Equal(t, 0, prod.evidenceCalls, "the still-unclassified intent re-skips E")
	assert.Equal(t, bitEvidence, sp.pendingDataProducers, "E stays pending")
}

// TestRunProducers_EvidenceRePick_WiredResolverIntentErrorDefers: with the REAL wired resolver, a
// real intent-classification DB fault on the re-pick defers the record — runProducers returns
// errIntentResolverUnavailable (no markFail) and the deferred counter climbs.
func TestRunProducers_EvidenceRePick_WiredResolverIntentErrorDefers(t *testing.T) {
	ctrl := gomock.NewController(t)
	orgID, rawRecordID := "org-repick-interr", "rr"
	prior := priorWithEvidencePending(orgID, rawRecordID, "sha1", []string{"a1"})
	prod := &countingProducer{}
	classificationDAO := daomocks.NewMockIHarnessArtifactClassificationDAO(ctrl)
	classificationDAO.EXPECT().GetByArtifact(gomock.Any(), orgID, intentClassificationTask, "a1").
		Return(nil, errors.New("db down"))

	p := &SessionAnalysisProcessor{producer: prod, cfg: repickTestConfig{}, classificationDAO: classificationDAO}
	p.evidenceResolver = p.artifactIntentResolved
	sp := &spineResult{contentSha256: "sha1"}
	before := testutil.ToFloat64(sessionEvidenceIntentDeferredTotal.WithLabelValues(orgID))

	read, _, err := p.runProducers(context.Background(), orgID,
		&model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID}, &rawRecordMetadata{PersonID: "p1"}, prior, sp, nil)

	require.ErrorIs(t, err, errIntentResolverUnavailable, "a re-pick intent-read fault defers non-counting")
	assert.Nil(t, read)
	assert.Equal(t, 0, prod.evidenceCalls)
	assert.Equal(t, before+1, testutil.ToFloat64(sessionEvidenceIntentDeferredTotal.WithLabelValues(orgID)))
}
