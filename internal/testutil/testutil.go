// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package testutil provides test-only helpers: a Prometheus gauge/counter reader
// and a Postgres test-database opener. The opener reads POSTGRES_TEST_DSN and
// skips (returns ok=false) when no database is reachable, so the unit suite runs
// with no Postgres while the DAO suite runs against a real one.
package testutil

import (
	"fmt"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	prom_testutil "github.com/prometheus/client_golang/prometheus/testutil"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// ToFloat64 returns the current value of a single-series counter or gauge.
func ToFloat64(c prometheus.Collector) float64 { return prom_testutil.ToFloat64(c) }

// dsn returns the configured test DSN, or "" when unset.
func dsn() string { return os.Getenv("POSTGRES_TEST_DSN") }

// openTestDB opens the configured test database. The bool reports whether a DSN was
// configured at all; a non-nil error is a configured-but-unreachable database. The two are
// kept distinct because they deserve opposite treatment: no DSN is a legitimate skip, a set
// DSN that cannot be reached is a fault.
func openTestDB() (*gorm.DB, bool, error) {
	if dsn() == "" {
		return nil, false, nil
	}
	db, err := gorm.Open(postgres.Open(dsn()), &gorm.Config{})
	if err != nil {
		return nil, true, err
	}
	return db, true, nil
}

// MustOpen opens the test database from POSTGRES_TEST_DSN. It returns
// (db, cleanup, true) when a database is reachable, and (nil, noop, false) when
// POSTGRES_TEST_DSN is unset or the server is unreachable, so a mixed package can
// still run its database-free tests.
func MustOpen() (*gorm.DB, func(), bool) {
	db, configured, err := openTestDB()
	if !configured {
		fmt.Fprintln(os.Stderr, "skipping DAO tests: POSTGRES_TEST_DSN not set")
		return nil, func() {}, false
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipping DAO tests: cannot reach test database: %v\n", err)
		return nil, func() {}, false
	}
	return db, func() {}, true
}

// MustOpenOrFail is MustOpen for a suite in which every test needs the database. With no DSN
// configured it exits cleanly (a legitimate skip where no Postgres is provisioned). But a DSN
// that is SET and unreachable is a failure, not a skip: the suite was asked to run against a
// database and could not, so it exits non-zero rather than let a broken CI database turn the
// whole suite green. suite names the suite in the message.
func MustOpenOrFail(suite string) (*gorm.DB, func()) {
	db, configured, err := openTestDB()
	if !configured {
		fmt.Fprintf(os.Stderr, "skipping %s: POSTGRES_TEST_DSN not set\n", suite)
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL %s: POSTGRES_TEST_DSN is set but the database is unreachable: %v\n", suite, err)
		os.Exit(1)
	}
	return db, func() {}
}
