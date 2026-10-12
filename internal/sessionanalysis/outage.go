// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"errors"
	"sync"
	"time"

	"github.com/tetriz-ai/session-intelligence/internal/refllmclient"
)

// outageDetector is the best-effort provider-wide-outage signal. Gemini is called through
// reference-llm, and this process holds no shared circuit-breaker over that call, so the
// available signal is a short rolling window of provider-side (5xx /
// timeout / transport / 429) failures across the batch: the signal is active when at least
// minRecords such failures fell inside the window. It is process-local (one processor per
// scheduler process) and safe for the concurrent per-record fan-out.
type outageDetector struct {
	mu       sync.Mutex
	failures []time.Time // provider-side transient failure instants, pruned to the window
}

// recordTransient records a provider-side transient failure and prunes entries older than
// the window, so the slice stays bounded by the batch's failure rate over one window.
func (d *outageDetector) recordTransient(now time.Time, window time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failures = append(d.failures, now)
	d.pruneLocked(now, window)
}

// active reports whether at least minRecords provider-side transient failures fell inside
// the window ending at now — the provider-wide-outage signal.
func (d *outageDetector) active(now time.Time, window time.Duration, minRecords int) bool {
	if minRecords <= 0 {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked(now, window)
	return len(d.failures) >= minRecords
}

func (d *outageDetector) pruneLocked(now time.Time, window time.Duration) {
	cutoff := now.Add(-window)
	kept := d.failures[:0]
	for _, at := range d.failures {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	d.failures = kept
}

// isProviderTransient reports whether an error is a provider-side transient failure — the
// class that a provider-wide outage produces and that the outage signal counts. It is
// grounded in the refllmclient typed-error taxonomy: a timeout, a transport fault, an
// upstream 429, or a 5xx is the provider's; a deterministic client/input fault (4xx, a parse
// failure, an oversized or unbuildable request, a caller cancel, a model output-truncation
// that recurs identically) is the record's and must still exhaust into FAILED at the cap.
func isProviderTransient(err error) bool {
	var clientErr *refllmclient.Error
	if !errors.As(err, &clientErr) {
		return false
	}
	switch clientErr.Kind {
	case refllmclient.KindTimeout,
		refllmclient.KindTransport,
		refllmclient.KindTooManyRequests,
		refllmclient.KindServer:
		return true
	default:
		// KindClient / KindParse / KindResponseTooLarge / KindRequestBuild /
		// KindOutputTruncated / KindCanceled: the record's own fault, never an outage.
		return false
	}
}
