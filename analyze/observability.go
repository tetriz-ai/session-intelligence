// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package analyze

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/tetriz-ai/session-intelligence/internal/logger"
	"github.com/tetriz-ai/session-intelligence/internal/metricutil"
	"github.com/tetriz-ai/session-intelligence/internal/sessionanalysis"
	"github.com/tetriz-ai/session-intelligence/internal/sessionintelligence"
)

// SetLogger makes the engine log through l. The engine is silent by default (a no-op logger),
// so a library user who wants its logs calls this once at startup with their own *zap.Logger.
// A nil logger is ignored, leaving the current one in place.
func SetLogger(l *zap.Logger) {
	logger.SetLogger(l)
}

// RegisterMetrics registers every engine metric with reg: the batch pipeline's counters and
// gauges, the parser and plan-tier counters, and the DAO duration histogram. The engine
// registers nothing on its own, so importing it has no global Prometheus side effect; a host
// that scrapes metrics calls this once at startup with its own registry. Call it once — a
// second call with the same registry panics (prometheus.MustRegister), which is the intended
// wire-once contract. The single-session AnalyzeSession path emits none of these: they cover
// the batch pipeline and its DAOs.
func RegisterMetrics(reg prometheus.Registerer) {
	sessionanalysis.RegisterMetrics(reg)
	sessionintelligence.RegisterMetrics(reg)
	metricutil.Register(reg)
}
