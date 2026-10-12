// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package planscoped is the single-tenant seat/plan seam. DB just returns the
// connection and the plan config defaults to unlimited (all seats, no data
// window) when the host installs none. A host that wants to exercise the gating
// paths can still install a Config and seat set on the context (see
// WithConfigForTest / WithSeats).
package planscoped

import (
	"context"
	"time"

	"gorm.io/gorm"

	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
)

// Config is the resolved plan for the tenant.
type Config struct {
	Plan           commonv1.Plan
	State          commonv1.PlanState
	SeatsLimit     int // -1 unlimited, or a positive cap
	DataWindowDays int // -1 unlimited, or a positive day count
	OrgCreatedAt   time.Time
}

// IsFrozen reports whether the plan is frozen.
func (c Config) IsFrozen() bool { return c.State == commonv1.PlanState_PLAN_STATE_FROZEN }

// SeatsUnlimited reports whether the seat cap is unlimited (-1).
func (c Config) SeatsUnlimited() bool { return c.SeatsLimit < 0 }

// unlimited is the default config for a single-tenant host that installs none:
// every seat allowed, no data window.
var unlimited = Config{SeatsLimit: -1, DataWindowDays: -1}

type configKey struct{}
type seatsKey struct{}

// SeatsContext is the set of person ids to admit when seat gating is exercised.
type SeatsContext struct {
	PersonIDs []string
}

// WithConfigForTest installs cfg on ctx so DB and the gating helpers read it.
func WithConfigForTest(ctx context.Context, cfg Config) context.Context {
	return context.WithValue(ctx, configKey{}, cfg)
}

// WithSeats installs the admitted seat set on ctx.
func WithSeats(ctx context.Context, sc SeatsContext) context.Context {
	return context.WithValue(ctx, seatsKey{}, sc)
}

// ConfigFromContext returns the installed Config, or the unlimited default with
// ok=true. Single-tenant is always "resolved", so callers that defer when no plan
// is present proceed here rather than stalling.
func ConfigFromContext(ctx context.Context) (Config, bool) {
	if cfg, ok := ctx.Value(configKey{}).(Config); ok {
		return cfg, true
	}
	return unlimited, true
}

func seatsFromContext(ctx context.Context) (SeatsContext, bool) {
	sc, ok := ctx.Value(seatsKey{}).(SeatsContext)
	return sc, ok
}

// DataCutoff returns the data-window cutoff for cfg. An unlimited window
// (DataWindowDays < 0) has no cutoff; a capped window anchors on OrgCreatedAt.
func DataCutoff(cfg Config) (time.Time, bool) {
	if cfg.DataWindowDays < 0 {
		return time.Time{}, false
	}
	return cfg.OrgCreatedAt.Truncate(24*time.Hour).AddDate(0, 0, -cfg.DataWindowDays), true
}

// AllowedPersonIDs returns the admitted seat set, or nil when unrestricted.
func AllowedPersonIDs(ctx context.Context) []string {
	if sc, ok := seatsFromContext(ctx); ok {
		return sc.PersonIDs
	}
	return nil
}

// HasSeatPersonID reports whether personID is admitted. With no seat set installed
// (the single-tenant default) every person is admitted.
func HasSeatPersonID(ctx context.Context, personID string) bool {
	sc, ok := seatsFromContext(ctx)
	if !ok {
		return true
	}
	for _, id := range sc.PersonIDs {
		if id == personID {
			return true
		}
	}
	return false
}

// DB returns the query handle. Single-tenant applies no seat IN-clause and no
// data-window cutoff (there is no RLS), so it is the transaction-aware handle.
func DB(ctx context.Context, db *gorm.DB) *gorm.DB { return tenantdb.DB(ctx, db) }

// WriteDB mirrors DB for write paths.
func WriteDB(ctx context.Context, db *gorm.DB) *gorm.DB { return tenantdb.DB(ctx, db) }

// DBDataWindowOnly mirrors DB; single-tenant applies no data-window cutoff.
func DBDataWindowOnly(ctx context.Context, db *gorm.DB) *gorm.DB { return tenantdb.DB(ctx, db) }
