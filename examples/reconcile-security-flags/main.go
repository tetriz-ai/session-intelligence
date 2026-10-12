// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Command reconcile-security-flags shows the security detector's reconcile half: it maps the
// security-layer issues a producer found (an exposed API key, a leaked DB credential) to open
// security flags for a record, collapsing repeats to one flag per type at the highest severity
// seen. It runs with no database — a tiny in-memory store captures what would be persisted.
package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/security"
)

// memStore is an in-memory security.FlagStore. Its single ReconcileOpenFlags method is the whole
// contract, so no DAO embedding is needed — it captures what would be persisted.
type memStore struct {
	flags []*model.SecurityFlag
}

func (m *memStore) ReconcileOpenFlags(_ context.Context, _ string, fresh []*model.SecurityFlag) error {
	m.flags = fresh
	return nil
}

func issue(slug sessionintelligencepb.PromptIssueSlug, sev sessionintelligencepb.IssueSeverity, note string) *sessionintelligencepb.PromptIssue {
	return &sessionintelligencepb.PromptIssue{Slug: slug, Severity: sev, Note: note}
}

func main() {
	store := &memStore{}
	reconciler := security.NewReconciler(store)

	// The read pass caught two issues on the same record: an API key twice (low, then high) and
	// a DB credential once. The evidence pass caught a PII issue; the engine gates evidence off
	// here, so it does not publish.
	readIssues := []*sessionintelligencepb.PromptIssue{
		issue(sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_LOW, "key in a comment"),
		issue(sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, "key in a config"),
		issue(sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_DB_CREDENTIAL_LEAK, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_MEDIUM, "dsn pasted"),
	}
	evidenceIssues := []*sessionintelligencepb.PromptIssue{
		issue(sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_PII_INJECTION, sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, "email in prompt"),
	}

	err := reconciler.ReconcileOpenFlags(
		context.Background(), "org-1", "person-1", "raw-1",
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		readIssues, evidenceIssues, false, // publishEvidence = false
	)
	if err != nil {
		panic(err)
	}

	sort.Slice(store.flags, func(i, j int) bool { return store.flags[i].FlagType < store.flags[j].FlagType })
	fmt.Printf("open flags: %d\n", len(store.flags))
	for _, f := range store.flags {
		fmt.Printf("- %s (%s): %d occurrence(s)\n", f.FlagType, f.Severity, len(f.FlagDetails.GetOccurrences()))
	}
}
