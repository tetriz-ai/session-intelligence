// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"errors"
	"time"
)

// RawRecordKey identifies a raw record by its natural key.
type RawRecordKey struct {
	OrganizationID string
	ConnectorID    string
	OperationID    string
	RecordKey      string
	DataDate       time.Time
}

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("dao: record not found")
