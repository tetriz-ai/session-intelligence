// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import "github.com/prometheus/client_golang/prometheus"

// RecordsFailedTotalForTest exposes the private records-failed counter to the external
// orchestration tests, which drive ProcessFlat and assert the swallowed-failure signal.
var RecordsFailedTotalForTest *prometheus.CounterVec = recordsFailedTotal
