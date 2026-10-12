// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package tenantdb

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/tetriz-ai/session-intelligence/internal/testutil"
)

// TestRunInTx pins RunInTx's three contracts: a ctx already in a transaction nests (reuses it,
// opens no second tx, and does not touch db), a fresh call opens a tx and threads it through ctx,
// and a returned error rolls the transaction back.
func TestRunInTx(t *testing.T) {
	t.Run("reuses an existing ctx transaction without opening a new one", func(t *testing.T) {
		sentinel := &gorm.DB{}
		ctx := context.WithValue(context.Background(), txKey{}, sentinel)
		ran := false
		// db is nil: the reuse path must NOT dereference it.
		err := RunInTx(ctx, nil, func(innerCtx context.Context) error {
			ran = true
			got, ok := txFromContext(innerCtx)
			require.True(t, ok, "fn should still see the reused transaction")
			require.Same(t, sentinel, got, "the existing tx must be reused, not replaced")
			return nil
		})
		require.NoError(t, err)
		require.True(t, ran, "fn must run")
	})

	db, cleanup, ok := testutil.MustOpen()
	if !ok {
		return
	}
	defer cleanup()
	require.NoError(t, db.Exec("CREATE TABLE IF NOT EXISTS runintx_probe (v int)").Error)
	t.Cleanup(func() { _ = db.Exec("DROP TABLE IF EXISTS runintx_probe").Error })

	t.Run("opens a transaction and commits on success", func(t *testing.T) {
		require.NoError(t, db.Exec("DELETE FROM runintx_probe").Error)
		err := RunInTx(context.Background(), db, func(ctx context.Context) error {
			tx, ok := txFromContext(ctx)
			require.True(t, ok, "fn should run inside the opened transaction")
			return tx.Exec("INSERT INTO runintx_probe VALUES (1)").Error
		})
		require.NoError(t, err)
		var count int64
		require.NoError(t, db.Raw("SELECT count(*) FROM runintx_probe").Scan(&count).Error)
		require.Equal(t, int64(1), count, "a successful RunInTx commits its writes")
	})

	t.Run("rolls back the transaction on error", func(t *testing.T) {
		require.NoError(t, db.Exec("DELETE FROM runintx_probe").Error)
		sentinelErr := errors.New("boom")
		err := RunInTx(context.Background(), db, func(ctx context.Context) error {
			tx, _ := txFromContext(ctx)
			require.NoError(t, tx.Exec("INSERT INTO runintx_probe VALUES (2)").Error)
			return sentinelErr
		})
		require.ErrorIs(t, err, sentinelErr)
		var count int64
		require.NoError(t, db.Raw("SELECT count(*) FROM runintx_probe").Scan(&count).Error)
		require.Equal(t, int64(0), count, "a RunInTx that returns an error rolls its writes back")
	})
}

// TestRunInScope pins the B4 contract: once the processor registers a process DB, RunInScope opens a
// REAL transaction so a multi-statement unit of work (the commit: content + state + security
// reconcile) is atomic — a returned error rolls every write back. Without a registered DB (mock-only
// unit tests) the body runs directly, since mocks need no transaction. The audit's probe reproduced
// the pre-fix gap: an unregistered RunInScope left an inserted row after an error.
func TestRunInScope(t *testing.T) {
	t.Run("runs the body directly when no DB is registered", func(t *testing.T) {
		Register(nil)
		ran := false
		err := RunInScope(context.Background(), "org", ScopeEnforced, func(ctx context.Context) error {
			ran = true
			_, ok := txFromContext(ctx)
			require.False(t, ok, "no transaction should be threaded when no DB is registered")
			return nil
		})
		require.NoError(t, err)
		require.True(t, ran, "fn must run even without a registered DB")
	})

	db, cleanup, ok := testutil.MustOpen()
	if !ok {
		return
	}
	defer cleanup()
	require.NoError(t, db.Exec("CREATE TABLE IF NOT EXISTS runinscope_probe (v int)").Error)
	t.Cleanup(func() { _ = db.Exec("DROP TABLE IF EXISTS runinscope_probe").Error })

	Register(db)
	t.Cleanup(func() { Register(nil) }) // don't leak the registered DB into sibling tests

	t.Run("opens a transaction and commits on success", func(t *testing.T) {
		require.NoError(t, db.Exec("DELETE FROM runinscope_probe").Error)
		err := RunInScope(context.Background(), "org", ScopeEnforced, func(ctx context.Context) error {
			tx, ok := txFromContext(ctx)
			require.True(t, ok, "fn should run inside the opened transaction")
			return tx.Exec("INSERT INTO runinscope_probe VALUES (1)").Error
		})
		require.NoError(t, err)
		var count int64
		require.NoError(t, db.Raw("SELECT count(*) FROM runinscope_probe").Scan(&count).Error)
		require.Equal(t, int64(1), count, "a successful RunInScope commits its writes")
	})

	t.Run("rolls back every write on error (the B4 atomicity contract)", func(t *testing.T) {
		require.NoError(t, db.Exec("DELETE FROM runinscope_probe").Error)
		sentinelErr := errors.New("boom")
		err := RunInScope(context.Background(), "org", ScopeEnforced, func(ctx context.Context) error {
			tx, _ := txFromContext(ctx)
			require.NoError(t, tx.Exec("INSERT INTO runinscope_probe VALUES (2)").Error)
			return sentinelErr
		})
		require.ErrorIs(t, err, sentinelErr)
		var count int64
		require.NoError(t, db.Raw("SELECT count(*) FROM runinscope_probe").Scan(&count).Error)
		require.Equal(t, int64(0), count, "RunInScope rolls its writes back on error (B4)")
	})
}
