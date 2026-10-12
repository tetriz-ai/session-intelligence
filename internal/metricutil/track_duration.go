// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package metricutil holds the shared DAO-duration histogram and its opt-in registration.
package metricutil

import (
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	daoFunctionRunDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "dao_function_run_duration_seconds",
			Help: "time taken by each dao functions",
			// The top bound sits above the 2m statement_timeout every prod DSN sets, so a
			// query that exhausts it still lands in a finite bucket rather than +Inf, and a
			// DAO issuing many statements stays measurable well past a single one's cap.
			// The sub-10s bounds are load-bearing outside this file: monitoring's DAO
			// dashboard derives Apdex from le=0.1/0.5 and its slow-query panels from
			// le=1/4, and a query pinned to a bound that moves returns nothing at all.
			// Bounds may be appended; existing ones may not be moved.
			Buckets: []float64{0.01, 0.02, 0.04, 0.06, 0.08, 0.1, 0.15, 0.2, 0.3, 0.5, 0.75, 1, 2, 4, 6, 10, 15, 30, 60, 120, 180},
		},
		[]string{"service_name", "pkg_path", "struct_name", "func_name"})
)

// Register registers the DAO duration histogram with reg. It is created unregistered — see
// analyze.RegisterMetrics for the public wire-once contract.
func Register(reg prometheus.Registerer) {
	reg.MustRegister(daoFunctionRunDuration)
}

// TrackDuration records the elapsed duration of an operation to a metric.
func TrackDuration(pkgPath, daoImplName, funcName string, st time.Time) {
	runDuration := time.Since(st)
	serviceName := getServiceNameFromPkgPath(pkgPath)
	daoFunctionRunDuration.WithLabelValues(serviceName, pkgPath, daoImplName, funcName).Observe(runDuration.Seconds())
}

func getServiceNameFromPkgPath(pkgPath string) string {
	if i := strings.Index(pkgPath, "/"); i >= 0 {
		return pkgPath[:i]
	}
	return pkgPath
}
