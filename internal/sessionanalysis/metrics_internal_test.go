// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"

	"github.com/tetriz-ai/session-intelligence/internal/dao"
)

// TestObserveScanStarvation pins the two-sided starvation signals: the NO_STATE-present starvation
// gate (a non-empty scan with zero never-analysed rows increments the starved counter) and the
// pending-age gauge, including the freeze-regression guard — a drained tick must reset the gauge to
// 0 rather than leave it frozen at its last (large) value. Each sub-case uses a UNIQUE orgID so the
// per-label counter/gauge state does not bleed across sub-cases.
func TestObserveScanStarvation(t *testing.T) {
	now := time.Now()
	past := now.Add(-2 * time.Hour)
	noState := func() *dao.ActionableCandidate {
		return &dao.ActionableCandidate{Category: dao.CategoryNoState, DataDate: past}
	}
	pending := func() *dao.ActionableCandidate {
		return &dao.ActionableCandidate{Category: dao.CategoryPendingData, DataDate: past}
	}

	tests := []struct {
		name string
		// orgID must be unique per sub-case so counter/gauge state does not bleed.
		orgID string
		// ticks are observed in order; the gauge/counter are read back after the last one.
		ticks         [][]*dao.ActionableCandidate
		wantGauge     float64 // asserted when gaugePositive is false
		gaugePositive bool    // assert gauge > 0 instead of == wantGauge
		wantStarved   float64 // absolute counter value after the last tick
	}{
		{
			name:        "idle tick resets gauge and counts no starvation",
			orgID:       "org-idle",
			ticks:       [][]*dao.ActionableCandidate{nil},
			wantGauge:   0,
			wantStarved: 0,
		},
		{
			name:        "only no-state candidates: present ⇒ not starved, no pending gauge",
			orgID:       "org-nostate",
			ticks:       [][]*dao.ActionableCandidate{{noState()}},
			wantGauge:   0,
			wantStarved: 0,
		},
		{
			name:          "candidates but none no-state: starved, pending gauge set",
			orgID:         "org-pending",
			ticks:         [][]*dao.ActionableCandidate{{pending()}},
			gaugePositive: true,
			wantStarved:   1,
		},
		{
			name:        "pending then drained on the same org: gauge resets to 0",
			orgID:       "org-drain",
			ticks:       [][]*dao.ActionableCandidate{{pending()}, nil},
			wantGauge:   0,
			wantStarved: 1, // only the first (non-empty, no-no-state) tick is starvation
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, candidates := range tt.ticks {
				observeScanStarvation(tt.orgID, candidates, now)
			}
			gauge := testutil.ToFloat64(pendingDataOldestAgeSeconds.WithLabelValues(tt.orgID))
			if tt.gaugePositive {
				assert.Greater(t, gauge, 0.0, "the pending gauge is now − the oldest pending candidate's age")
			} else {
				assert.Equal(t, tt.wantGauge, gauge)
			}
			assert.Equal(t, tt.wantStarved, testutil.ToFloat64(scanStarvedRunsTotal.WithLabelValues(tt.orgID)))
		})
	}
}
