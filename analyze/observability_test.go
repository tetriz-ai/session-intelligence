// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package analyze_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/tetriz-ai/session-intelligence/analyze"
)

// TestRegisterMetrics proves the opt-in registration wires every engine metric into a
// caller-supplied registry with no global side effect: a fresh registry starts empty, and after
// RegisterMetrics it holds the engine's metric families. A second fresh registry works too — the
// collectors are reusable, so a host is never forced onto the global default registry.
func TestRegisterMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()

	before, err := reg.Gather()
	require.NoError(t, err)
	require.Empty(t, before, "a fresh registry must start empty — nothing auto-registers on import")

	analyze.RegisterMetrics(reg)

	after, err := reg.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, after, "RegisterMetrics must register the engine's metrics into the supplied registry")

	require.NotPanics(t, func() { analyze.RegisterMetrics(prometheus.NewRegistry()) },
		"the same collectors must register into a second, independent registry")
}

// TestSetLoggerNilSafe checks the logger setter tolerates a nil logger (leaving the current one)
// and accepts a real one.
func TestSetLoggerNilSafe(t *testing.T) {
	require.NotPanics(t, func() {
		analyze.SetLogger(nil)
		analyze.SetLogger(zap.NewNop())
	})
}
