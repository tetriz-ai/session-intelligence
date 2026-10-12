// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package security_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	daomocks "github.com/tetriz-ai/session-intelligence/internal/dao/mocks"
	"github.com/tetriz-ai/session-intelligence/security"
)

func issue(slug sessionintelligencepb.PromptIssueSlug, sev sessionintelligencepb.IssueSeverity, note string) *sessionintelligencepb.PromptIssue {
	return &sessionintelligencepb.PromptIssue{Slug: slug, Severity: sev, Note: note}
}

// captureReconcile wires a mock that records the flags passed to the DAO's
// ReconcileOpenFlags and returns them for assertion.
func captureReconcile(t *testing.T) (*security.Reconciler, *[]*model.SecurityFlag) {
	t.Helper()
	ctrl := gomock.NewController(t)
	daoMock := daomocks.NewMockISecurityFlagDAO(ctrl)
	var captured []*model.SecurityFlag
	daoMock.EXPECT().ReconcileOpenFlags(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, flags []*model.SecurityFlag) error {
			captured = flags
			return nil
		}).AnyTimes()
	return security.NewReconciler(daoMock), &captured
}

func TestReconciler_MapsSlugToFlagType(t *testing.T) {
	cases := []struct {
		slug     sessionintelligencepb.PromptIssueSlug
		flagType string
	}{
		{sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK, "api-key-leak"},
		{sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_DB_CREDENTIAL_LEAK, "db-credential-leak"},
		{sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_PII_INJECTION, "pii-injection"},
	}
	for _, tc := range cases {
		t.Run(tc.flagType, func(t *testing.T) {
			r, captured := captureReconcile(t)
			err := r.ReconcileOpenFlags(context.Background(), "org", "person", "rr", time.Now(),
				[]*sessionintelligencepb.PromptIssue{issue(tc.slug, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, "n")},
				nil, false)
			require.NoError(t, err)
			require.Len(t, *captured, 1)
			assert.Equal(t, tc.flagType, (*captured)[0].FlagType)
			assert.Equal(t, "org", (*captured)[0].OrganizationID)
			assert.Equal(t, "person", (*captured)[0].PersonID)
			assert.Equal(t, "rr", (*captured)[0].RawRecordID)
		})
	}
}

func TestReconciler_IgnoresNonSecuritySlug(t *testing.T) {
	r, captured := captureReconcile(t)
	err := r.ReconcileOpenFlags(context.Background(), "org", "person", "rr", time.Now(),
		[]*sessionintelligencepb.PromptIssue{issue(sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_UNSPECIFIED, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, "n")},
		nil, false)
	require.NoError(t, err)
	assert.Empty(t, *captured)
}

// TestReconcileSeverityCooccurrenceNeverBelowMedium pins that an UNSPECIFIED-severity issue
// co-occurring with a LOW one on the same slug yields "medium", not "low": the unspecified
// member defaults to "medium" before ranking, so it never drags the flag below its default.
func TestReconcileSeverityCooccurrenceNeverBelowMedium(t *testing.T) {
	r, captured := captureReconcile(t)
	slug := sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK
	err := r.ReconcileOpenFlags(context.Background(), "org", "person", "rr", time.Now(),
		[]*sessionintelligencepb.PromptIssue{
			issue(slug, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_UNSPECIFIED, "unspecified"),
			issue(slug, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_LOW, "low"),
		}, nil, false)
	require.NoError(t, err)
	require.Len(t, *captured, 1)
	assert.Equal(t, "medium", (*captured)[0].Severity, "UNSPECIFIED+LOW ranks at medium, never low")
}

func TestReconciler_PromotesToHighestSeverity(t *testing.T) {
	r, captured := captureReconcile(t)
	slug := sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK
	err := r.ReconcileOpenFlags(context.Background(), "org", "person", "rr", time.Now(),
		[]*sessionintelligencepb.PromptIssue{
			issue(slug, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_LOW, "a"),
			issue(slug, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_CRITICAL, "b"),
			issue(slug, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_MEDIUM, "c"),
		}, nil, false)
	require.NoError(t, err)
	require.Len(t, *captured, 1, "same flag_type collapses to one flag")
	assert.Equal(t, "critical", (*captured)[0].Severity, "promoted to highest severity seen")
	assert.Len(t, (*captured)[0].FlagDetails.GetOccurrences(), 3, "all occurrences retained")
}

// TestReconcileUnspecifiedSeverityDefaultsToMedium pins the plausible default: a
// security-layer issue whose severity is UNSPECIFIED still raises a flag, at "medium",
// rather than being dropped. See the default site in reconcile.go.
func TestReconcileUnspecifiedSeverityDefaultsToMedium(t *testing.T) {
	r, captured := captureReconcile(t)
	err := r.ReconcileOpenFlags(context.Background(), "org", "person", "rr", time.Now(),
		[]*sessionintelligencepb.PromptIssue{
			issue(sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK,
				sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_UNSPECIFIED, "unspecified severity"),
		}, nil, false)
	require.NoError(t, err)
	require.Len(t, *captured, 1, "an unspecified-severity finding still raises a flag")
	assert.Equal(t, "medium", (*captured)[0].Severity, "unspecified severity defaults to medium, not dropped")
}

// TestReconcileMasksKnownSecretShapesInExcerpt pins the defensive backstop: a well-known
// secret shape left in a finding's note is masked before it is persisted. This is best-effort
// (known shapes only) — sanitizing findings remains the producer's responsibility.
func TestReconcileMasksKnownSecretShapesInExcerpt(t *testing.T) {
	r, captured := captureReconcile(t)
	err := r.ReconcileOpenFlags(context.Background(), "org", "person", "rr", time.Now(),
		[]*sessionintelligencepb.PromptIssue{
			issue(sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_PII_INJECTION,
				sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, "leaked address jane@example.com in a prompt"),
		}, nil, false)
	require.NoError(t, err)
	require.Len(t, *captured, 1)
	excerpt := (*captured)[0].FlagDetails.GetOccurrences()[0].GetSanitizedExcerpt()
	assert.NotContains(t, excerpt, "jane@example.com", "a known secret shape is masked before persistence")
}

func TestReconciler_EvidenceGate(t *testing.T) {
	slug := sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_DB_CREDENTIAL_LEAK
	readIssues := []*sessionintelligencepb.PromptIssue{issue(sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, "r")}
	evidenceIssues := []*sessionintelligencepb.PromptIssue{issue(slug, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, "e")}

	t.Run("gate off: evidence issues do not publish", func(t *testing.T) {
		r, captured := captureReconcile(t)
		require.NoError(t, r.ReconcileOpenFlags(context.Background(), "org", "p", "rr", time.Now(), readIssues, evidenceIssues, false))
		require.Len(t, *captured, 1)
		assert.Equal(t, "api-key-leak", (*captured)[0].FlagType, "only the read pass's flag published")
	})
	t.Run("gate on: evidence issues publish", func(t *testing.T) {
		r, captured := captureReconcile(t)
		require.NoError(t, r.ReconcileOpenFlags(context.Background(), "org", "p", "rr", time.Now(), readIssues, evidenceIssues, true))
		types := map[string]bool{}
		for _, f := range *captured {
			types[f.FlagType] = true
		}
		assert.True(t, types["api-key-leak"] && types["db-credential-leak"], "both read and evidence flags published")
	})
}
