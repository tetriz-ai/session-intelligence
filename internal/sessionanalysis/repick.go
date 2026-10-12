// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// cloneCallState deep-copies a CallStateMap so a re-pick can carry the prior generation's
// per-producer state forward without mutating the loaded state row's proto. A nil source
// yields an empty (non-nil) map.
func cloneCallState(src *sessionintelligencepb.CallStateMap) *sessionintelligencepb.CallStateMap {
	empty := &sessionintelligencepb.CallStateMap{Calls: map[string]*sessionintelligencepb.CallState{}}
	if src == nil {
		return empty
	}
	cloned, ok := proto.Clone(src).(*sessionintelligencepb.CallStateMap)
	if !ok {
		return empty
	}
	return cloned
}

// priorCallState is prior's CallState, nil-safe for a first-time row (prior == nil).
func priorCallState(prior *model.SessionAnalysisState) *sessionintelligencepb.CallStateMap {
	if prior == nil {
		return nil
	}
	return prior.CallState
}

// rePickDecision is how a SKIPPED_PENDING_DATA record is re-picked from its PERSISTED
// dependency alone — no S3.
type rePickDecision int

const (
	// rePickResync: the body digest changed since the producer went pending, so the pending
	// snapshot is stale (the conversation was re-uploaded) — abandon it and re-run in full.
	rePickResync rePickDecision = iota
	// rePickReSkip: at least one persisted pending artifact is still unresolved — re-skip
	// cheaply (one intent read per id, no detector/LLM), leaving the pending bit set.
	rePickReSkip
	// rePickResolved: every persisted pending artifact resolved — carry R by rehydration and
	// re-run the pending producer half.
	rePickResolved
)

// evidenceRePickDecision decides how to re-pick a record whose EVIDENCE producer is
// SKIPPED_PENDING_DATA, from the state row alone — no S3.
// If currentSha differs from the digest stamped when E went pending, the
// snapshot is stale and E re-runs in full (rePickResync). Otherwise it consults resolved for
// each persisted pending artifact id — any still unresolved → rePickReSkip; all resolved →
// rePickResolved.
//
// resolved is the caller's cheap intent read (GetByArtifact over exactly the persisted ids, no
// S3). It is supplied by the E-assembly slice, which owns the artifact-intent classification
// task; this function owns the decision over the persisted dependency snapshot so the cost
// claim (one read per waiting id, never the S3 body) is enforced here. A record with no EVIDENCE
// pending state resolves as rePickResolved (there is nothing to wait on). A resolved() error
// (a real DB fault, distinct from an unresolved-but-present read) is propagated so the caller
// defers the re-pick non-counting rather than silently re-skipping it forever.
func evidenceRePickDecision(prior *model.SessionAnalysisState, currentSha string, resolved func(artifactID string) (bool, error)) (rePickDecision, error) {
	call := pendingEvidenceCall(prior)
	if call == nil {
		return rePickResolved, nil
	}
	if call.GetSourceContentSha256() != currentSha {
		return rePickResync, nil
	}
	for _, artifactID := range call.GetPendingArtifactIds() {
		isResolved, err := resolved(artifactID)
		if err != nil {
			return 0, fmt.Errorf("resolving pending artifact %q intent: %w", artifactID, err)
		}
		if !isResolved {
			return rePickReSkip, nil
		}
	}
	return rePickResolved, nil
}

// pendingEvidenceCall returns the EVIDENCE CallState iff it is SKIPPED_PENDING_DATA, else nil.
func pendingEvidenceCall(prior *model.SessionAnalysisState) *sessionintelligencepb.CallState {
	if prior == nil || prior.CallState == nil {
		return nil
	}
	call, ok := prior.CallState.GetCalls()[producerEvidence]
	if !ok || call.GetStatus() != sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA {
		return nil
	}
	return call
}
