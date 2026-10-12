// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package tenantdb is the single-tenant database seam. There is no row-level
// scoping and no per-tenant SET LOCAL — DB simply returns the connection (or the
// open transaction), and RunInScope runs its body in one transaction so a
// multi-statement unit of work stays atomic.
package tenantdb

import (
	"context"

	"gorm.io/gorm"
)

// Scope is accepted for call-site compatibility; single-tenant applies no
// row-level scoping for either value.
type Scope int

const (
	// ScopeEnforced is the default scope.
	ScopeEnforced Scope = iota
	// ScopeSystem marks system/maintenance work (purges).
	ScopeSystem
)

type txKey struct{}

// registeredDB is the process connection RunInScope opens its transaction on when
// the call site does not carry one. Register it once at startup.
var registeredDB *gorm.DB

// Register sets the process database used by RunInScope. Repos are constructed
// with the same *gorm.DB, so a transaction opened here is visible to their reads
// and writes through DB below. The handle is process-wide and last-writer-wins:
// this build constructs one processor, so a single Register is assumed. Registering
// a second, different DB would reroute the first — not a concern for the
// single-tenant library, which has no second-processor path.
func Register(db *gorm.DB) { registeredDB = db }

// txFromContext returns the transaction RunInScope placed on ctx, if any.
func txFromContext(ctx context.Context) (*gorm.DB, bool) {
	tx, ok := ctx.Value(txKey{}).(*gorm.DB)
	return tx, ok
}

// DB returns the query handle to use: the in-flight transaction when the call is
// inside a RunInScope body, otherwise defaultDB bound to ctx.
func DB(ctx context.Context, defaultDB *gorm.DB) *gorm.DB {
	if tx, ok := txFromContext(ctx); ok {
		return tx
	}
	if defaultDB != nil {
		return defaultDB.WithContext(ctx)
	}
	if registeredDB != nil {
		return registeredDB.WithContext(ctx)
	}
	return nil
}

// RunInScope runs fn in a single transaction. A nested call reuses the outer
// transaction. When no process DB is registered (pure in-memory unit tests), fn
// runs directly without a transaction.
func RunInScope(ctx context.Context, _ string, _ Scope, fn func(ctx context.Context) error) error {
	if _, ok := txFromContext(ctx); ok {
		return fn(ctx)
	}
	if registeredDB == nil {
		return fn(ctx)
	}
	return registeredDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
}

// RunInTx runs fn inside one transaction on db, threading that transaction through ctx
// so the DAO calls fn makes (via DB/WriteDB) enlist in it. If ctx already carries a
// transaction, fn runs in that one — the call nests without opening a second, so a repo
// method using RunInTx stays a single atomic unit whether it is called standalone or
// from inside a RunInScope body. Unlike RunInScope it needs no process-wide Register:
// the caller passes the connection, so a multi-statement write is atomic even when nothing
// registered a DB. db must be non-nil: a repo method that needs transaction-scoped
// atomicity (an advisory lock, a delete+insert) has a real connection, so a nil db is a
// construction error that panics here rather than silently running the body without a
// transaction and dropping those guarantees.
func RunInTx(ctx context.Context, db *gorm.DB, fn func(ctx context.Context) error) error {
	if _, ok := txFromContext(ctx); ok {
		return fn(ctx)
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
}

// RunInScopeValue is RunInScope for a body that returns a value.
func RunInScopeValue[T any](ctx context.Context, _ string, _ Scope, fn func(ctx context.Context) (T, error)) (T, error) {
	if _, ok := txFromContext(ctx); ok {
		return fn(ctx)
	}
	var zero T
	if registeredDB == nil {
		return fn(ctx)
	}
	var result T
	err := registeredDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var innerErr error
		result, innerErr = fn(context.WithValue(ctx, txKey{}, tx))
		return innerErr
	})
	if err != nil {
		return zero, err
	}
	return result, nil
}

// AssertNoOpenTx is a no-op in the single-tenant build; it existed to catch a
// scope opened without a surrounding transaction in the RLS build.
func AssertNoOpenTx(_ context.Context) {}
