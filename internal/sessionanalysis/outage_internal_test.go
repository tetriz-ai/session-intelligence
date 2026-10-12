// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tetriz-ai/session-intelligence/internal/refllmclient"
)

// TestIsProviderTransient covers the error taxonomy: a provider-side transient kind counts
// toward the outage signal; a record-class fault never does.
func TestIsProviderTransient(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "server 5xx is transient", err: &refllmclient.Error{Kind: refllmclient.KindServer}, want: true},
		{name: "timeout is transient", err: &refllmclient.Error{Kind: refllmclient.KindTimeout}, want: true},
		{name: "transport is transient", err: &refllmclient.Error{Kind: refllmclient.KindTransport}, want: true},
		{name: "429 is transient", err: &refllmclient.Error{Kind: refllmclient.KindTooManyRequests}, want: true},
		{name: "4xx client fault is not transient", err: &refllmclient.Error{Kind: refllmclient.KindClient}, want: false},
		{name: "parse fault is not transient", err: &refllmclient.Error{Kind: refllmclient.KindParse}, want: false},
		{name: "output truncation is not transient", err: &refllmclient.Error{Kind: refllmclient.KindOutputTruncated}, want: false},
		{name: "response too large is not transient", err: &refllmclient.Error{Kind: refllmclient.KindResponseTooLarge}, want: false},
		{name: "caller cancel is not transient", err: &refllmclient.Error{Kind: refllmclient.KindCanceled}, want: false},
		{name: "wrapped server error unwraps to transient", err: fmt.Errorf("analyze_read: %w", &refllmclient.Error{Kind: refllmclient.KindServer}), want: true},
		{name: "a non-client error is not transient", err: errors.New("plain error"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isProviderTransient(tt.err))
		})
	}
}

// TestOutageDetector covers the rolling-window signal: it activates at minRecords failures
// inside the window and deactivates as entries age out.
func TestOutageDetector(t *testing.T) {
	window := 10 * time.Minute
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	var detector outageDetector

	assert.False(t, detector.active(now, window, 3), "no failures: inactive")

	detector.recordTransient(now, window)
	detector.recordTransient(now, window)
	assert.False(t, detector.active(now, window, 3), "two failures below the threshold: inactive")

	detector.recordTransient(now, window)
	assert.True(t, detector.active(now, window, 3), "three failures reach the threshold: active")

	// The failures age out of the window.
	later := now.Add(11 * time.Minute)
	assert.False(t, detector.active(later, window, 3), "the window has passed: inactive")

	// minRecords <= 0 never activates (defensive).
	detector.recordTransient(later, window)
	assert.False(t, detector.active(later, window, 0))
}
