// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
	"github.com/tetriz-ai/session-intelligence/internal/testutil"
)

// testDB is the shared connection initialised once in TestMain from
// POSTGRES_TEST_DSN. The schema is loaded from migrations/latest.sql by the
// docker-compose Postgres (see the repo README).
var testDB *gorm.DB

// TestMain opens the test database for the whole package. Every test here needs
// it, so an unreachable server skips the suite (MustOpenOrFail).
func TestMain(m *testing.M) {
	db, cleanup := testutil.MustOpenOrFail("session-intelligence dao")
	testDB = db
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// newTestTx begins a transaction rolled back at cleanup, isolating each test.
func newTestTx(t *testing.T) *gorm.DB {
	t.Helper()
	tx := testDB.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx
}

// seedOrg returns a fresh organization UUID for use as a tenant scope. It does
// not insert a row: the engine is single-tenant and no table has an FK to an
// organizations table, so the UUID is used directly as organization_id.
func seedOrg(t *testing.T, _ *gorm.DB) string {
	t.Helper()
	return uuid.NewString()
}

// loopTestCtx builds a context with an unlimited single-tenant plan config and the
// given seats, mirroring the config a host installs in production.
func loopTestCtx(personIDs ...string) context.Context {
	ctx := planscoped.WithConfigForTest(context.Background(), planscoped.Config{
		Plan:           commonv1.Plan_PLAN_TRIAL,
		State:          commonv1.PlanState_PLAN_STATE_ACTIVE,
		SeatsLimit:     100,
		DataWindowDays: 3650,
		OrgCreatedAt:   time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	return planscoped.WithSeats(ctx, planscoped.SeatsContext{PersonIDs: personIDs})
}

func strPtr(s string) *string { return &s }

func ptr[T any](v T) *T { return &v }

// cleanupSessionRows deletes the session/security rows for the given orgs from the
// shipped tables, in FK-safe child-first order.
func cleanupSessionRows(orgIDs ...string) {
	tables := []string{
		"session_turn_analyses",
		"session_phase_analyses",
		"session_round_trip_faults",
		"session_analysis_state",
		"session_analyses",
		"security_flags",
	}
	for _, orgID := range orgIDs {
		for _, table := range tables {
			testDB.Exec("DELETE FROM "+table+" WHERE organization_id = ?", orgID)
		}
	}
}
