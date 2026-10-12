// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	coreaidlcv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/aidlc/v1"
	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
)

// intentClassificationTask is the classification-table task row whose intent_class column the E
// producer reads to tell whether an artifact's intent is resolved. The classification row is keyed
// (org, task, artifact) on harness_artifact_classifications; this const names the task row E reads.
// It is a single const so the task is one edit if it must change; the read is read-only.
const intentClassificationTask = coreaidlcv1.HarnessClassificationTask_HARNESS_CLASSIFICATION_TASK_STAGE

// artifactIntent reads one artifact's classified intent and returns the ArtifactIntentClass when
// the sweep has classified it (non-null, not UNSPECIFIED), else nil (unresolved). A missing
// classification row (the sweep has not reached the artifact) is unresolved, not an error. E
// renders the resolved class so it identifies reviewer invocations by classified intent, never by
// skill name. It opens its own tenant scope: it runs from the
// producer flow, which holds no open transaction.
func (p *SessionAnalysisProcessor) artifactIntent(ctx context.Context, orgID, artifactID string) (*coreaidlcv1.ArtifactIntentClass, error) {
	var intent *coreaidlcv1.ArtifactIntentClass
	if err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		classification, err := p.classificationDAO.GetByArtifact(sctx, orgID, intentClassificationTask, artifactID)
		if err != nil {
			if errors.Is(err, dao.ErrNotFound) {
				return nil // no classification row yet — intent unresolved, not an error
			}
			return fmt.Errorf("get artifact intent classification (artifact %s): %w", artifactID, err)
		}
		if classification.IntentClass != nil &&
			*classification.IntentClass != coreaidlcv1.ArtifactIntentClass_ARTIFACT_INTENT_CLASS_UNSPECIFIED {
			intent = classification.IntentClass
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("resolving artifact intent (org %s artifact %s): %w", orgID, artifactID, err)
	}
	return intent, nil
}

// artifactIntentResolved reports whether an artifact's intent is resolved — the boolean seam the
// re-pick decision consumes. It defers to artifactIntent so the resolved-vs-pending test and the
// rendered class can never disagree.
func (p *SessionAnalysisProcessor) artifactIntentResolved(ctx context.Context, orgID, artifactID string) (bool, error) {
	intent, err := p.artifactIntent(ctx, orgID, artifactID)
	if err != nil {
		return false, fmt.Errorf("resolving artifact intent (org %s artifact %s): %w", orgID, artifactID, err)
	}
	return intent != nil, nil
}

// artifactSetKey memoizes one live (artifact kind, source, person) inventory fetch within a single
// resolve pass. VCS rows are person-less, so their key carries an empty personID; DESKTOP rows are
// keyed by the session person. The (ide_type) is invariant across a session, so it is not part of
// the key.
type artifactSetKey struct {
	artifactType commonv1.HarnessArtifactType
	source       commonv1.HarnessArtifactSource
	personID     string
}

// resolveEvidencePendingArtifactIDs resolves the session's invoked artifacts to their live
// harness_artifacts rows (VCS-first, DESKTOP fallback) and returns the ids of the resolved rows
// whose intent is not yet classified — the artifact ids E must wait on before it can adjudicate
// them. An invocation that resolves to NO row is NOT pending: E
// proceeds with its intent unknown rather than stranding forever on an artifact the inventory does
// not carry, and each such invocation increments sessionEvidenceArtifactUnresolvedTotal so a
// systemic slug-match/inventory defect is a visible rate rather than a silent bypass.
//
// The inventory fetch is memoized per (artifact kind, source, person): a session that invokes the
// same (type, source) under many slugs fetches that set once, capping DB fetches at ~(#distinct
// kinds × 2 sources) regardless of invocation count. The per-resolved-row intent reads are deduped
// separately so a repeated invocation of one artifact contributes its id once. The reads run
// outside any open transaction.
func (p *SessionAnalysisProcessor) resolveEvidencePendingArtifactIDs(ctx context.Context, orgID, personID string, sp *spineResult) ([]string, map[string]coreaidlcv1.ArtifactIntentClass, error) {
	// The session and common IdeType enums are numerically identical, so the cast is exact.
	commonIdeType := commonv1.IdeType(sp.ideType)
	inventory := map[artifactSetKey][]*model.HarnessArtifact{}
	var pending []string
	// resolvedBySlug carries each invocation's classified intent to E's render (the LEDGER/
	// INVOCATIONS blocks), so E identifies a reviewer invocation by classified intent rather than
	// by slug name. Keyed by the invocation slug because that is
	// the render's identity; the first resolved row for a slug wins.
	resolvedBySlug := map[string]coreaidlcv1.ArtifactIntentClass{}
	seen := map[string]struct{}{}
	for _, invocation := range sp.ledgers.invocations {
		rows, err := p.resolveInvocationArtifacts(ctx, orgID, personID, commonIdeType, invocation, inventory)
		if err != nil {
			// Inventory-read failure domain: the harness_artifacts live-set fetch faulted.
			sessionEvidenceArtifactListDeferredTotal.WithLabelValues(orgID).Inc()
			return nil, nil, fmt.Errorf("resolving invocation %q (org %s): %w", invocation.slug, orgID, err)
		}
		if len(rows) == 0 {
			sessionEvidenceArtifactUnresolvedTotal.WithLabelValues(orgID).Inc()
			continue
		}
		for _, row := range rows {
			if _, ok := seen[row.ID]; ok {
				continue
			}
			seen[row.ID] = struct{}{}
			intent, err := p.artifactIntent(ctx, orgID, row.ID)
			if err != nil {
				// Intent-classification-read failure domain: the artifact_classifications read faulted.
				sessionEvidenceIntentDeferredTotal.WithLabelValues(orgID).Inc()
				return nil, nil, fmt.Errorf("checking intent for artifact %s (org %s): %w", row.ID, orgID, err)
			}
			if intent == nil {
				pending = append(pending, row.ID)
				continue
			}
			if _, already := resolvedBySlug[invocation.slug]; !already {
				resolvedBySlug[invocation.slug] = *intent
			}
		}
	}
	return pending, resolvedBySlug, nil
}

// resolveInvocationArtifacts resolves one invocation to its live harness_artifacts rows: it takes
// the org-canonical VCS inventory first (person-less) and slug-matches it, falling back to the
// session person's DESKTOP inventory ONLY when VCS yields no match — the two sources are never
// merged. A session with no person cannot have a DESKTOP fallback.
// Both inventory fetches go through the memo, so a repeated (type, source) is fetched once per pass.
func (p *SessionAnalysisProcessor) resolveInvocationArtifacts(ctx context.Context, orgID, personID string, ideType commonv1.IdeType, invocation invocationDetail, inventory map[artifactSetKey][]*model.HarnessArtifact) ([]*model.HarnessArtifact, error) {
	vcsRows, err := p.listArtifactsForSource(ctx, orgID, ideType, invocation.artifactType,
		commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_VCS, nil, inventory)
	if err != nil {
		return nil, fmt.Errorf("resolving VCS invocation %q: %w", invocation.slug, err)
	}
	if vcsMatches := matchingArtifacts(vcsRows, invocation.slug); len(vcsMatches) > 0 {
		return vcsMatches, nil
	}
	if personID == "" {
		return nil, nil // no person → no DESKTOP fallback
	}
	desktopRows, err := p.listArtifactsForSource(ctx, orgID, ideType, invocation.artifactType,
		commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_DESKTOP, &personID, inventory)
	if err != nil {
		return nil, fmt.Errorf("resolving DESKTOP invocation %q: %w", invocation.slug, err)
	}
	return matchingArtifacts(desktopRows, invocation.slug), nil
}

// listArtifactsForSource returns the live (artifact kind, source, person) inventory, fetching it
// through the DAO once per key and caching it on inventory for the rest of the resolve pass. The
// fetch opens its own tenant scope.
func (p *SessionAnalysisProcessor) listArtifactsForSource(ctx context.Context, orgID string, ideType commonv1.IdeType, artifactType commonv1.HarnessArtifactType, source commonv1.HarnessArtifactSource, personID *string, inventory map[artifactSetKey][]*model.HarnessArtifact) ([]*model.HarnessArtifact, error) {
	key := artifactSetKey{artifactType: artifactType, source: source}
	if personID != nil {
		key.personID = *personID
	}
	if cached, ok := inventory[key]; ok {
		return cached, nil
	}
	var rows []*model.HarnessArtifact
	if err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		fetched, err := p.harnessArtifactDAO.ListLiveForSource(sctx, orgID, ideType, artifactType, source, personID)
		if err != nil {
			return fmt.Errorf("list %s artifacts for source: %w", shortEnum(source.String(), "HARNESS_ARTIFACT_SOURCE_"), err)
		}
		rows = fetched
		return nil
	}); err != nil {
		return nil, fmt.Errorf("listing %s artifacts (org %s kind %s): %w",
			shortEnum(source.String(), "HARNESS_ARTIFACT_SOURCE_"), orgID, artifactType.String(), err)
	}
	inventory[key] = rows
	return rows, nil
}

// matchingArtifacts keeps the rows whose slug matches, over an already-fetched inventory set.
func matchingArtifacts(rows []*model.HarnessArtifact, slug string) []*model.HarnessArtifact {
	matches := make([]*model.HarnessArtifact, 0, len(rows))
	for _, row := range rows {
		if matchesInvocationSlug(row, slug) {
			matches = append(matches, row)
		}
	}
	return matches
}

// matchesInvocationSlug reports whether a live artifact row matches an invocation's already-lowercased
// slug — by its name OR by the slug derived from its install path (a SKILL.md / AGENT.md whose
// identity is its directory). Both arms are case-insensitive.
func matchesInvocationSlug(row *model.HarnessArtifact, slug string) bool {
	return strings.EqualFold(row.Name, slug) || strings.EqualFold(artifactSlugFromSourcePath(row.SourcePath), slug)
}

// artifactSlugFromSourcePath derives an artifact's bare slug from its install path: the containing
// directory's basename for a definition-file layout (a SKILL.md / AGENT.md whose own filename is
// the convention, not the identity), else the filename without its extension. Lowercased so the
// match against an already-lowercased invocation slug is case-insensitive.
func artifactSlugFromSourcePath(sourcePath string) string {
	base := path.Base(sourcePath)
	nameSansExt := strings.TrimSuffix(base, path.Ext(base))
	if isDefinitionFileName(nameSansExt) {
		return strings.ToLower(path.Base(path.Dir(sourcePath)))
	}
	return strings.ToLower(nameSansExt)
}

// isDefinitionFileName reports whether a filename (already stripped of its extension) is a
// convention-named definition file whose identity is its directory, not itself (SKILL.md,
// AGENT.md / AGENTS.md), rather than a file whose own name is the slug.
func isDefinitionFileName(nameSansExt string) bool {
	switch strings.ToLower(nameSansExt) {
	case "skill", "agent", "agents":
		return true
	default:
		return false
	}
}
