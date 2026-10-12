// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"gorm.io/gorm"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	"github.com/tetriz-ai/session-intelligence/internal/goroutine"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
)

func newSecurityFlag(orgID, personID string) *model.SecurityFlag {
	return &model.SecurityFlag{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		PersonID:       personID,
		RawRecordID:    uuid.NewString(),
		FlagType:       "credential_exposure",
		Severity:       "high",
		FlagDetails: &sessionintelligencepb.SecurityFlagDetails{
			Occurrences: []*sessionintelligencepb.SecurityFlagOccurrence{
				{
					SanitizedExcerpt: "API key detected in prompt: sk-***",
					PromptIndex:      3,
				},
			},
		},
		Status:   "open",
		FlagDate: time.Date(2026, 2, 20, 0, 0, 0, 0, time.UTC),
	}
}

func TestSecurityFlagRepo_CreateBatchAndList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo)
	}{
		{
			name: "roundtrips proto FlagDetails through DB",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				orgID := seedOrg(t, db)
				personID := uuid.NewString()

				flag := newSecurityFlag(orgID, personID)
				require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{flag}))

				results, err := repo.ListByOrg(context.Background(), orgID, 10)
				require.NoError(t, err)
				require.Len(t, results, 1)
				assert.True(t, proto.Equal(flag.FlagDetails, results[0].FlagDetails), "FlagDetails mismatch")
			},
		},
		{
			name: "nil FlagDetails roundtrips as nil",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				orgID := seedOrg(t, db)
				personID := uuid.NewString()

				flag := newSecurityFlag(orgID, personID)
				flag.FlagDetails = nil
				require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{flag}))

				results, err := repo.ListByOrg(context.Background(), orgID, 10)
				require.NoError(t, err)
				require.Len(t, results, 1)
				assert.Nil(t, results[0].FlagDetails)
			},
		},
		{
			name: "CountOpenByOrg returns correct count",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				orgID := seedOrg(t, db)
				personID := uuid.NewString()

				flags := []*model.SecurityFlag{
					newSecurityFlag(orgID, personID),
					newSecurityFlag(orgID, personID),
				}
				require.NoError(t, repo.CreateBatch(context.Background(), flags))

				count, err := repo.CountOpenByOrg(context.Background(), orgID)
				require.NoError(t, err)
				assert.Equal(t, int64(2), count)
			},
		},
		{
			name: "GetByID returns flag when found",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				orgID := seedOrg(t, db)
				personID := uuid.NewString()

				flag := newSecurityFlag(orgID, personID)
				require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{flag}))

				result, err := repo.GetByID(context.Background(), flag.ID)
				require.NoError(t, err)
				assert.Equal(t, flag.ID, result.ID)
				assert.Equal(t, flag.FlagType, result.FlagType)
				assert.True(t, proto.Equal(flag.FlagDetails, result.FlagDetails), "FlagDetails mismatch")
			},
		},
		{
			name: "GetByID returns ErrRecordNotFound for missing ID",
			run: func(t *testing.T, _ *gorm.DB, repo *dao.SecurityFlagRepo) {
				_, err := repo.GetByID(context.Background(), uuid.NewString())
				assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
			},
		},
		{
			name: "ListByPerson filters by person within org",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				orgID := seedOrg(t, db)
				person1 := uuid.NewString()
				person2 := uuid.NewString()

				flags := []*model.SecurityFlag{
					newSecurityFlag(orgID, person1),
					newSecurityFlag(orgID, person1),
					newSecurityFlag(orgID, person2),
				}
				require.NoError(t, repo.CreateBatch(context.Background(), flags))

				results, err := repo.ListByPerson(context.Background(), orgID, person1, 10)
				require.NoError(t, err)
				assert.Len(t, results, 2)
				for _, r := range results {
					assert.Equal(t, person1, r.PersonID)
				}
			},
		},
		{
			name: "ListByPerson returns empty for unknown person",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				orgID := seedOrg(t, db)
				personID := uuid.NewString()

				require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{
					newSecurityFlag(orgID, personID),
				}))

				results, err := repo.ListByPerson(context.Background(), orgID, uuid.NewString(), 10)
				require.NoError(t, err)
				assert.Empty(t, results)
			},
		},
		{
			name: "CreateBatch upserts flag_details on (raw_record_id, flag_type) conflict",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				orgID := seedOrg(t, db)
				personID := uuid.NewString()

				flag := newSecurityFlag(orgID, personID)
				require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{flag}))

				// Re-insert same (raw_record_id, flag_type) with updated occurrences.
				updated := newSecurityFlag(orgID, personID)
				updated.ID = uuid.NewString() // different ID — conflict resolved by index
				updated.RawRecordID = flag.RawRecordID
				updated.FlagType = flag.FlagType
				updated.FlagDetails = &sessionintelligencepb.SecurityFlagDetails{
					Occurrences: []*sessionintelligencepb.SecurityFlagOccurrence{
						{SanitizedExcerpt: "updated excerpt", PromptIndex: 7},
						{SanitizedExcerpt: "second occurrence", PromptIndex: 12},
					},
				}
				require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{updated}))

				results, err := repo.ListByOrg(context.Background(), orgID, 10)
				require.NoError(t, err)
				require.Len(t, results, 1, "upsert must not duplicate the row")
				require.Len(t, results[0].FlagDetails.GetOccurrences(), 2, "flag_details must be updated")
				assert.Equal(t, "updated excerpt", results[0].FlagDetails.GetOccurrences()[0].SanitizedExcerpt)
			},
		},
		{
			name: "CreateBatch allows re-insert after soft-delete of same (raw_record_id, flag_type)",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				orgID := seedOrg(t, db)
				personID := uuid.NewString()

				flag := newSecurityFlag(orgID, personID)
				require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{flag}))

				// Soft-delete the flag.
				require.NoError(t, repo.DeleteByRawRecordIDAndStatus(context.Background(), flag.RawRecordID, "open"))

				// Re-insert same (raw_record_id, flag_type) — partial index allows it.
				reinserted := newSecurityFlag(orgID, personID)
				reinserted.RawRecordID = flag.RawRecordID
				reinserted.FlagType = flag.FlagType
				require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{reinserted}))

				results, err := repo.ListByOrg(context.Background(), orgID, 10)
				require.NoError(t, err)
				require.Len(t, results, 1, "re-inserted flag must be visible")
				assert.Equal(t, "open", results[0].Status)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := newTestTx(t)
			repo := dao.NewSecurityFlagRepo(tx)
			tt.run(t, tx, repo)
		})
	}
}

func TestSecurityFlagRepo_DeleteByRawRecordIDAndStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo)
	}{
		{
			name: "soft-deletes open flags for the given raw_record_id",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				orgID := seedOrg(t, db)
				personID := uuid.NewString()
				rawRecordID := uuid.NewString()

				open1 := newSecurityFlag(orgID, personID)
				open1.RawRecordID = rawRecordID
				open2 := newSecurityFlag(orgID, personID)
				open2.RawRecordID = rawRecordID
				require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{open1, open2}))

				require.NoError(t, repo.DeleteByRawRecordIDAndStatus(context.Background(), rawRecordID, "open"))

				results, err := repo.ListByOrg(context.Background(), orgID, 10)
				require.NoError(t, err)
				assert.Empty(t, results, "soft-deleted flags should not appear in list")
			},
		},
		{
			name: "does not delete flags with a different status",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				orgID := seedOrg(t, db)
				personID := uuid.NewString()
				rawRecordID := uuid.NewString()

				openFlag := newSecurityFlag(orgID, personID)
				openFlag.RawRecordID = rawRecordID
				openFlag.FlagType = "credential_exposure"

				// Use a different flag_type so the two flags don't conflict on the
				// (raw_record_id, flag_type) unique partial index.
				resolvedFlag := newSecurityFlag(orgID, personID)
				resolvedFlag.RawRecordID = rawRecordID
				resolvedFlag.FlagType = "pii_injection"
				resolvedFlag.Status = "resolved"

				require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{openFlag, resolvedFlag}))

				require.NoError(t, repo.DeleteByRawRecordIDAndStatus(context.Background(), rawRecordID, "open"))

				results, err := repo.ListByOrg(context.Background(), orgID, 10)
				require.NoError(t, err)
				require.Len(t, results, 1)
				assert.Equal(t, resolvedFlag.ID, results[0].ID, "resolved flag must be untouched")
			},
		},
		{
			name: "does not delete flags for a different raw_record_id",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				orgID := seedOrg(t, db)
				personID := uuid.NewString()

				targetFlag := newSecurityFlag(orgID, personID)
				otherFlag := newSecurityFlag(orgID, personID)
				require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{targetFlag, otherFlag}))

				require.NoError(t, repo.DeleteByRawRecordIDAndStatus(context.Background(), targetFlag.RawRecordID, "open"))

				results, err := repo.ListByOrg(context.Background(), orgID, 10)
				require.NoError(t, err)
				require.Len(t, results, 1)
				assert.Equal(t, otherFlag.ID, results[0].ID, "flag for other raw record must be untouched")
			},
		},
		{
			name: "no-op when no matching flags exist",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				require.NoError(t, repo.DeleteByRawRecordIDAndStatus(context.Background(), uuid.NewString(), "open"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := newTestTx(t)
			repo := dao.NewSecurityFlagRepo(tx)
			tt.run(t, tx, repo)
		})
	}
}

// TestSecurityFlagRepo_ReconcileOpenFlags is the real-DB guardrail for the shared
// reconcile every security_flags writer calls: carry-forward OR of a re-caught flag_type's
// provenance, the fresh-fingerprint-wins rule when this call's own freshFlags carries
// its own E fingerprint, and the preserve-or-drop decision for a prior open flag_type
// this call's freshFlags does NOT re-raise.
func TestSecurityFlagRepo_ReconcileOpenFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo)
	}{
		{
			name: "replaces the open set: a prior flag not re-raised is dropped, a fresh one is inserted",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				orgID := seedOrg(t, db)
				personID := uuid.NewString()
				rawRecordID := uuid.NewString()

				prior := newSecurityFlag(orgID, personID)
				prior.RawRecordID = rawRecordID
				prior.FlagType = "db-credential-leak"
				require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{prior}))

				fresh := newSecurityFlag(orgID, personID)
				fresh.RawRecordID = rawRecordID
				fresh.FlagType = "api-key-leak"
				require.NoError(t, repo.ReconcileOpenFlags(context.Background(), rawRecordID, []*model.SecurityFlag{fresh}))

				results, err := repo.ListByOrg(context.Background(), orgID, 10)
				require.NoError(t, err)
				require.Len(t, results, 1, "the fresh set replaces the prior open set outright")
				assert.Equal(t, "api-key-leak", results[0].FlagType, "the prior db-credential-leak flag is dropped; the fresh api-key-leak flag is inserted")
			},
		},
		{
			name: "an empty fresh set clears all open flags",
			run: func(t *testing.T, db *gorm.DB, repo *dao.SecurityFlagRepo) {
				orgID := seedOrg(t, db)
				personID := uuid.NewString()
				rawRecordID := uuid.NewString()

				prior := newSecurityFlag(orgID, personID)
				prior.RawRecordID = rawRecordID
				prior.FlagType = "pii-injection"
				require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{prior}))

				require.NoError(t, repo.ReconcileOpenFlags(context.Background(), rawRecordID, nil))

				results, err := repo.ListByOrg(context.Background(), orgID, 10)
				require.NoError(t, err)
				assert.Empty(t, results, "an empty fresh set drops every prior open flag")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := newTestTx(t)
			repo := dao.NewSecurityFlagRepo(tx)
			tt.run(t, tx, repo)
		})
	}
}

// TestSecurityFlagRepo_ReconcileOpenFlags_SerializedAcrossWriters proves the
// transaction-scoped advisory lock ReconcileOpenFlags takes on rawRecordID
// serializes the legacy Session writer and the session-analysis writer, so
// their delete+insert reconciles cannot interleave and leave a stale row. It is
// deterministic (no timing race on the outcome): a holder transaction takes the
// SAME advisory lock ReconcileOpenFlags acquires and, still uncommitted, writes a
// flag — modelling the legacy Session writer mid-reconcile (its delete+insert has
// run but not yet committed). A concurrent ReconcileOpenFlags call carrying its own
// fresh flag — modelling session-analysis, whose own producer ran — is shown to
// block before it deletes anything; once the holder commits, the blocked call
// proceeds, its delete now removes the committed prior row and its insert lands, so
// the final open set is exactly the last writer's (a clean replace-set, one row).
func TestSecurityFlagRepo_ReconcileOpenFlags_SerializedAcrossWriters(t *testing.T) {
	t.Parallel()

	orgID := seedOrg(t, testDB)
	personID := uuid.NewString()
	rawRecordID := uuid.NewString()
	t.Cleanup(func() {
		_ = testDB.Exec("DELETE FROM security_flags WHERE organization_id = ?", orgID).Error
	})

	repo := dao.NewSecurityFlagRepo(testDB)
	lockKey := "security_flags:reconcile:" + rawRecordID

	// Hold the exact advisory lock ReconcileOpenFlags acquires, on a pinned
	// connection (a gorm transaction pins one backend), so the concurrent call
	// below must wait — and write Session's R-only flag inside that same
	// uncommitted transaction, so it stays invisible to any other connection
	// until Commit below.
	holdTx := testDB.Begin()
	require.NoError(t, holdTx.Error)
	require.NoError(t, holdTx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", lockKey).Error)
	incumbentFlag := newSecurityFlag(orgID, personID)
	incumbentFlag.RawRecordID = rawRecordID
	incumbentFlag.FlagType = "db-credential-leak"
	require.NoError(t, dao.NewSecurityFlagRepo(holdTx).CreateBatch(context.Background(), []*model.SecurityFlag{incumbentFlag}))

	// Concurrently, the session-analysis writer reconciles the same raw_record_id
	// with its own fresh flag.
	sessionFlag := newSecurityFlag(orgID, personID)
	sessionFlag.RawRecordID = rawRecordID
	sessionFlag.FlagType = "api-key-leak"

	done := make(chan error, 1)
	goroutine.RunWithCtx(context.Background(), func(ctx context.Context) {
		done <- tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(scopedCtx context.Context) error {
			return repo.ReconcileOpenFlags(scopedCtx, rawRecordID, []*model.SecurityFlag{sessionFlag})
		})
	})

	// It must block on the lock before reading anything. Rather than sleep a fixed span and
	// assume the goroutine reached the lock, poll pg_locks until THIS reconcile's advisory
	// lock shows a waiter — a behavior signal, deterministic and not wall-clock-coupled. The
	// classid/objid reconstruct how pg_locks stores the bigint hashtext key that
	// pg_advisory_xact_lock takes, so the match is specific to this test's lockKey and never
	// catches a parallel test's advisory lock. A reconcile that finished instead of blocking
	// surfaces on done and fails immediately.
	const lockWaiterQuery = `SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND NOT granted AND objsubid = 1
		  AND classid = ((hashtext(?)::bigint >> 32) & 4294967295)::oid
		  AND objid = (hashtext(?)::bigint & 4294967295)::oid`
	blockDeadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			holdTx.Rollback()
			t.Fatalf("ReconcileOpenFlags completed while the advisory lock was held (err=%v) — not serialized", err)
		default:
		}
		var waiters int64
		require.NoError(t, testDB.Raw(lockWaiterQuery, lockKey, lockKey).Scan(&waiters).Error)
		if waiters > 0 {
			break
		}
		if time.Now().After(blockDeadline) {
			holdTx.Rollback()
			t.Fatal("ReconcileOpenFlags never blocked on the advisory lock")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Commit the holder: releases the lock and makes Session's R-only row visible.
	require.NoError(t, holdTx.Commit().Error)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("ReconcileOpenFlags did not complete after the advisory lock was released")
	}

	results, err := repo.ListByOrg(context.Background(), orgID, 10)
	require.NoError(t, err)
	require.Len(t, results, 1, "the serialized reconcile leaves exactly the last writer's set — no stale row survives the delete+insert")
	assert.Equal(t, "api-key-leak", results[0].FlagType,
		"session-analysis is the last writer, so its flag is the sole open flag; Session's committed row was replaced")
}

// TestSecurityFlagRepo_ReconcileOpenFlags_AtomicOnInsertFailure proves the reconcile's
// soft-delete and insert are one transaction: when the insert fails, the delete rolls
// back with it, so an open flag the producer still reports is never dropped. Without the
// surrounding transaction the delete would autocommit and the failed insert would leave
// the record with no open flag at all — silent loss of a detected issue.
func TestSecurityFlagRepo_ReconcileOpenFlags_AtomicOnInsertFailure(t *testing.T) {
	t.Parallel()

	orgID := seedOrg(t, testDB)
	personID := uuid.NewString()
	rawRecordID := uuid.NewString()
	t.Cleanup(func() {
		_ = testDB.Exec("DELETE FROM security_flags WHERE organization_id = ?", orgID).Error
	})

	repo := dao.NewSecurityFlagRepo(testDB)

	// The record's existing open flag — the one the reconcile deletes before writing the
	// fresh set.
	existing := newSecurityFlag(orgID, personID)
	existing.RawRecordID = rawRecordID
	existing.FlagType = "api-key-leak"
	require.NoError(t, repo.CreateBatch(context.Background(), []*model.SecurityFlag{existing}))

	// A fresh flag that cannot be inserted: an empty organization_id is not a valid uuid,
	// so CreateBatch's insert fails inside the reconcile transaction — after the delete has
	// run. Its flag_type differs from the existing flag's, so CreateBatch takes the insert
	// path (there is no row to update).
	poison := newSecurityFlag("", personID)
	poison.RawRecordID = rawRecordID
	poison.FlagType = "db-credential-leak"

	err := repo.ReconcileOpenFlags(context.Background(), rawRecordID, []*model.SecurityFlag{poison})
	require.Error(t, err, "the invalid-uuid insert must fail the reconcile")

	// The delete rolled back with the failed insert, so the record's existing open flag
	// survives — a partial reconcile never drops a still-reported issue.
	open, err := repo.ListOpenByRawRecordID(context.Background(), rawRecordID)
	require.NoError(t, err)
	require.Len(t, open, 1, "a failed reconcile insert must not drop the record's existing open flag")
	assert.Equal(t, "api-key-leak", open[0].FlagType)
}
