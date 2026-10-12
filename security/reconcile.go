// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package security is the session security detector's flag-reconcile half. The
// session-intelligence engine produces security-layer prompt issues (exposed
// credentials, private keys, sensitive files); this package maps them to open
// security flags for a record and persists them last-writer-wins. It owns the
// slug→flag-type vocabulary and the severity ranking; the engine calls into it.
package security

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/logredact"
)

// slugFlagType maps the security-layer prompt-issue slugs to the kebab-case
// flag_type stored on security_flags. Only these slugs cross the seam; any other
// slug is a non-security issue and is ignored.
var slugFlagType = map[sessionintelligencepb.PromptIssueSlug]string{
	sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK:       "api-key-leak",
	sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_DB_CREDENTIAL_LEAK: "db-credential-leak",
	sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_PII_INJECTION:      "pii-injection",
}

var severityString = map[sessionintelligencepb.IssueSeverity]string{
	sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_LOW:      "low",
	sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_MEDIUM:   "medium",
	sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH:     "high",
	sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_CRITICAL: "critical",
}

var severityRank = map[string]int{"low": 1, "medium": 2, "high": 3, "critical": 4}

// FlagStore persists a record's reconciled open flags; the bundled DAO implements it, or supply your own.
type FlagStore interface {
	ReconcileOpenFlags(ctx context.Context, rawRecordID string, freshFlags []*model.SecurityFlag) error
}

// Reconciler persists a record's open security flags from detected prompt issues.
type Reconciler struct {
	store FlagStore
}

// NewReconciler binds a Reconciler to the security-flag store.
func NewReconciler(store FlagStore) *Reconciler {
	return &Reconciler{store: store}
}

// ReconcileOpenFlags maps the security-layer issues in readIssues (and, when
// publishEvidence is true, evidenceIssues) to open security flags for the record
// and atomically replaces the record's open flags with them.
//
// readIssues are the read pass's readout, always published. evidenceIssues are the
// evidence pass's net-new round-trip findings, gated by publishEvidence (the
// engine decides the gate from its per-org config). Issues carrying a non-security
// slug are ignored. Multiple issues mapping to one flag_type collapse to a single
// flag, promoted to the highest severity seen.
func (r *Reconciler) ReconcileOpenFlags(
	ctx context.Context,
	orgID, personID, rawRecordID string,
	flagDate time.Time,
	readIssues, evidenceIssues []*sessionintelligencepb.PromptIssue,
	publishEvidence bool,
) error {
	flags := BuildOpenFlags(orgID, personID, rawRecordID, flagDate, readIssues, evidenceIssues, publishEvidence)
	if err := r.store.ReconcileOpenFlags(ctx, rawRecordID, flags); err != nil {
		return fmt.Errorf("reconcile open security flags for raw record %s: %w", rawRecordID, err)
	}
	return nil
}

// BuildOpenFlags maps the security-layer issues in readIssues (and, when publishEvidence is true,
// evidenceIssues) to a record's reconciled open security flags WITHOUT persisting them. It is the
// shared build step: ReconcileOpenFlags (the batch persisting path) and the single-session public
// path both call it, so the two cannot drift on masking, dedup, the severity rule or the slug filter.
//
// It applies the defensive secret masking (logredact.Redact over each note — a producer miss never
// persists a raw secret, though only known shapes are caught), collapses multiple issues of one
// flag_type to a single flag promoted to the highest severity seen (an unspecified or unrecognized
// severity defaults to "medium" before ranking, so a member never ranks below its documented
// default), and drops any non-security slug (only api-key-leak / db-credential-leak / pii-injection
// cross the seam).
func BuildOpenFlags(
	orgID, personID, rawRecordID string,
	flagDate time.Time,
	readIssues, evidenceIssues []*sessionintelligencepb.PromptIssue,
	publishEvidence bool,
) []*model.SecurityFlag {
	type group struct {
		severity    string
		occurrences []*sessionintelligencepb.SecurityFlagOccurrence
	}
	groups := make(map[string]*group)
	addIssues := func(issues []*sessionintelligencepb.PromptIssue) {
		for _, issue := range issues {
			flagType, ok := slugFlagType[issue.GetSlug()]
			if !ok {
				continue // not a security-layer slug
			}
			sev, known := severityString[issue.GetSeverity()]
			if !known {
				// An unspecified or unrecognized severity defaults to "medium" BEFORE ranking,
				// so a member never ranks below its documented default: an UNSPECIFIED issue
				// co-occurring with a LOW one on the same slug yields "medium", never "low".
				// Pinned by TestReconcileSeverityCooccurrenceNeverBelowMedium.
				sev = "medium"
			}
			// Defensive backstop: sanitizing a finding's excerpt is the producer's
			// responsibility, but mask well-known secret shapes (tokens, emails, home
			// paths) here too so a producer miss does not persist a raw secret. This
			// catches only known shapes — a novel secret can still slip through.
			occ := &sessionintelligencepb.SecurityFlagOccurrence{SanitizedExcerpt: logredact.Redact(issue.GetNote())}
			if g, ok := groups[flagType]; ok {
				g.occurrences = append(g.occurrences, occ)
				if severityRank[sev] > severityRank[g.severity] {
					g.severity = sev
				}
			} else {
				groups[flagType] = &group{severity: sev, occurrences: []*sessionintelligencepb.SecurityFlagOccurrence{occ}}
			}
		}
	}
	addIssues(readIssues)
	if publishEvidence {
		addIssues(evidenceIssues)
	}

	flags := make([]*model.SecurityFlag, 0, len(groups))
	for flagType, g := range groups {
		// Every issue contributes a ranked severity (an unspecified one defaults to
		// "medium" above), so a group always has a non-empty severity here.
		sev := g.severity
		flags = append(flags, &model.SecurityFlag{
			OrganizationID: orgID,
			PersonID:       personID,
			RawRecordID:    rawRecordID,
			FlagType:       flagType,
			Severity:       sev,
			FlagDetails:    &sessionintelligencepb.SecurityFlagDetails{Occurrences: g.occurrences},
			FlagDate:       flagDate,
		})
	}
	// flag_type is unique per group, so sorting by it gives a stable order independent of
	// Go's randomized map iteration — the reconcile's delete+insert and any caller that
	// renders the set then see the same order every run.
	sort.Slice(flags, func(i, j int) bool { return flags[i].FlagType < flags[j].FlagType })
	return flags
}
