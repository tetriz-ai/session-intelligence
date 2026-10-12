// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package sessionintelligence holds the conversation parsing, pricing and metrics helpers the
// engine reads to turn a raw session transcript into structured input.
package sessionintelligence

import (
	"context"
	"math"

	"go.uber.org/zap"

	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/logger"
	"github.com/tetriz-ai/session-intelligence/internal/metrictenant"
)

func commonIdeType(t sessionintelligencepb.IdeType) commonv1.IdeType {
	return commonv1.IdeType(t)
}

const fieldBillingCreditsUsed = "billing_credits_used"

const fieldReportedCostUSD = "reported_cost_usd"

const toolModeUnknown = "unknown"

func usableVendorFigure(ctx context.Context, orgID, field string, v float64) float64 {
	if v > 0 && v*1e6 < math.MaxInt64 {
		return v
	}
	if v != 0 {
		logger.WarnWithCtx(ctx, "vendor_figure_rejected",
			zap.String("field", field),
			zap.Float64("value", v),
		)
		vendorFigureRejectedTotal.WithLabelValues(metrictenant.OrgID(orgID), field).Inc()
	}
	return 0
}
