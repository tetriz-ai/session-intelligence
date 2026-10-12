// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

// reconcileSecurityFlags builds the fresh security-flag set from THIS pass's own R and E
// readouts and hands it to the shared DAO reconcile (dao.SecurityFlagRepo.ReconcileOpenFlags),
// which replaces the raw record's open flags outright — last-writer-wins, with no cross-writer
// preservation. These tests pin the boundary: the freshFlags this call builds, that E's slugs
// are gated by EvidenceSecurityFlagPublishEnabled while R's are not, and that the reconcile is
// always invoked (including with an empty set, so a stale flag is cleared).

import (
	"context"
	"testing"
	"time"

	"github.com/tetriz-ai/session-intelligence/security"

	"go.uber.org/mock/gomock"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao/mocks"
)

// secFlagGateConfig is a minimal Config double for reconcileSecurityFlags, which reads only
// EvidenceSecurityFlagPublishEnabled off cfg. It embeds the interface (nil) so any other cfg
// call would panic loudly rather than pass silently — this test must exercise only the gate.
type secFlagGateConfig struct {
	Config
	publish bool
}

func (c secFlagGateConfig) EvidenceSecurityFlagPublishEnabled(string) bool { return c.publish }

// TestReconcileSecurityFlags_BuildsFlagFromR pins that a security-layer issue R caught this
// pass is built into one fresh flag and handed to the reconcile.
func TestReconcileSecurityFlags_BuildsFlagFromR(t *testing.T) {
	ctrl := gomock.NewController(t)
	securityFlagDAO := mocks.NewMockISecurityFlagDAO(ctrl)

	var captured []*model.SecurityFlag
	securityFlagDAO.EXPECT().ReconcileOpenFlags(gomock.Any(), "raw-1", gomock.Any()).DoAndReturn(
		func(_ context.Context, _ string, flags []*model.SecurityFlag) error {
			captured = flags
			return nil
		})

	// Gate off: R's slugs are unaffected by EvidenceSecurityFlagPublishEnabled.
	p := &SessionAnalysisProcessor{securityFlagDAO: securityFlagDAO, securityReconciler: security.NewReconciler(securityFlagDAO), cfg: secFlagGateConfig{publish: false}}
	record := &model.RawRecordModel{ID: "raw-1", OrganizationID: "org-1", FetchedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	read := &ReadResult{
		SecurityIssues: []*sessionintelligencepb.PromptIssue{
			{Slug: sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK, Severity: sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, Note: "sk-*** in prompt"},
		},
	}

	if err := p.reconcileSecurityFlags(context.Background(), record, "person-1", read); err != nil {
		t.Fatalf("reconcileSecurityFlags returned an error: %v", err)
	}

	if len(captured) != 1 {
		t.Fatalf("expected 1 fresh flag built from R's readout, got %d", len(captured))
	}
	if got := captured[0]; got.FlagType != "api-key-leak" || got.PersonID != "person-1" || got.OrganizationID != "org-1" {
		t.Errorf("unexpected flag built: %+v", got)
	}
}

// TestReconcileSecurityFlags_BuildsFlagFromEvidence pins that a security-layer slug E alone
// caught on a round trip (EvidenceSecurityIssues) is built into a fresh flag on the same path.
func TestReconcileSecurityFlags_BuildsFlagFromEvidence(t *testing.T) {
	ctrl := gomock.NewController(t)
	securityFlagDAO := mocks.NewMockISecurityFlagDAO(ctrl)

	var captured []*model.SecurityFlag
	securityFlagDAO.EXPECT().ReconcileOpenFlags(gomock.Any(), "raw-1", gomock.Any()).DoAndReturn(
		func(_ context.Context, _ string, flags []*model.SecurityFlag) error {
			captured = flags
			return nil
		})

	// Gate on: E's slug is persisted.
	p := &SessionAnalysisProcessor{securityFlagDAO: securityFlagDAO, securityReconciler: security.NewReconciler(securityFlagDAO), cfg: secFlagGateConfig{publish: true}}
	record := &model.RawRecordModel{ID: "raw-1", OrganizationID: "org-1", FetchedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	read := &ReadResult{
		EvidenceRan: true,
		EvidenceSecurityIssues: []*sessionintelligencepb.PromptIssue{
			{Slug: sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK, Severity: sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, Note: "sk-*** on round trip"},
		},
	}

	if err := p.reconcileSecurityFlags(context.Background(), record, "person-1", read); err != nil {
		t.Fatalf("reconcileSecurityFlags returned an error: %v", err)
	}

	if len(captured) != 1 {
		t.Fatalf("expected 1 fresh flag built from E's readout, got %d", len(captured))
	}
	if got := captured[0]; got.FlagType != "api-key-leak" {
		t.Errorf("unexpected flag built: %+v", got)
	}
}

// TestReconcileSecurityFlags_EvidenceGatedOff pins the EvidenceSecurityFlagPublishEnabled
// write gate: with it off, E's security slug is computed but NOT persisted, while R's slug on
// the same pass still is — so only R's flag reaches the reconcile.
func TestReconcileSecurityFlags_EvidenceGatedOff(t *testing.T) {
	ctrl := gomock.NewController(t)
	securityFlagDAO := mocks.NewMockISecurityFlagDAO(ctrl)

	var captured []*model.SecurityFlag
	securityFlagDAO.EXPECT().ReconcileOpenFlags(gomock.Any(), "raw-1", gomock.Any()).DoAndReturn(
		func(_ context.Context, _ string, flags []*model.SecurityFlag) error {
			captured = flags
			return nil
		})

	p := &SessionAnalysisProcessor{securityFlagDAO: securityFlagDAO, securityReconciler: security.NewReconciler(securityFlagDAO), cfg: secFlagGateConfig{publish: false}}
	record := &model.RawRecordModel{ID: "raw-1", OrganizationID: "org-1", FetchedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	read := &ReadResult{
		EvidenceRan: true,
		SecurityIssues: []*sessionintelligencepb.PromptIssue{
			{Slug: sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK, Severity: sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, Note: "R caught sk-***"},
		},
		EvidenceSecurityIssues: []*sessionintelligencepb.PromptIssue{
			{Slug: sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_PII_INJECTION, Severity: sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, Note: "E caught pii on round trip"},
		},
	}

	if err := p.reconcileSecurityFlags(context.Background(), record, "person-1", read); err != nil {
		t.Fatalf("reconcileSecurityFlags returned an error: %v", err)
	}

	if len(captured) != 1 {
		t.Fatalf("expected only R's flag (E gated off), got %d flags", len(captured))
	}
	if got := captured[0]; got.FlagType != "api-key-leak" {
		t.Errorf("expected R's api-key-leak flag, got %+v", got)
	}
}

// TestReconcileSecurityFlags_NothingCaught_StillReconciles pins that a pass catching nothing
// (R ran, no issues) still calls the reconcile with an empty set, so a stale prior open flag
// is cleared rather than silently surviving.
func TestReconcileSecurityFlags_NothingCaught_StillReconciles(t *testing.T) {
	ctrl := gomock.NewController(t)
	securityFlagDAO := mocks.NewMockISecurityFlagDAO(ctrl)

	var captured []*model.SecurityFlag
	securityFlagDAO.EXPECT().ReconcileOpenFlags(gomock.Any(), "raw-1", gomock.Any()).DoAndReturn(
		func(_ context.Context, _ string, flags []*model.SecurityFlag) error {
			captured = flags
			return nil
		})

	p := &SessionAnalysisProcessor{securityFlagDAO: securityFlagDAO, securityReconciler: security.NewReconciler(securityFlagDAO), cfg: secFlagGateConfig{publish: true}}
	record := &model.RawRecordModel{ID: "raw-1", OrganizationID: "org-1", FetchedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	read := &ReadResult{EvidenceRan: true}

	if err := p.reconcileSecurityFlags(context.Background(), record, "person-1", read); err != nil {
		t.Fatalf("reconcileSecurityFlags returned an error: %v", err)
	}

	if len(captured) != 0 {
		t.Fatalf("expected an empty fresh set, got %d flags", len(captured))
	}
}
