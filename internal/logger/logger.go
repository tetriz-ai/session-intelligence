// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package logger is a minimal structured-logging shim over zap. It provides the
// small API surface the engine and DAO use, with a process-global logger so call
// sites need no logger injection.
package logger

import (
	"context"

	"go.uber.org/zap"
)

// Structured field-key constants used across log call sites.
const (
	OrgID     = "org_id"
	PersonID  = "person_id"
	SessionID = "session_id"
	RecordID  = "record_id"
)

var global = zap.NewNop()

// SetLogger replaces the process-global logger. Call once at startup; the default
// is a no-op logger so libraries stay quiet unless the host opts in.
func SetLogger(l *zap.Logger) {
	if l != nil {
		global = l
	}
}

// Info logs at info level. The context is accepted for call-site compatibility;
// this shim does not extract fields from it.
func Info(_ context.Context, msg string, fields ...zap.Field) { global.Info(msg, fields...) }

// Error logs at error level.
func Error(_ context.Context, msg string, fields ...zap.Field) { global.Error(msg, fields...) }

// WarnWithCtx logs at warn level.
func WarnWithCtx(_ context.Context, msg string, fields ...zap.Field) {
	global.Warn(msg, fields...)
}

// ErrorWithStackTrace logs at error level with a stack trace.
func ErrorWithStackTrace(_ context.Context, msg string, fields ...zap.Field) {
	global.Error(msg, fields...)
}

// InfoNoCtx logs at info level without a context; the no-context variants mirror the source API.
func InfoNoCtx(msg string, fields ...zap.Field) { global.Info(msg, fields...) }

// ErrorNoCtx logs at error level without a context.
func ErrorNoCtx(msg string, fields ...zap.Field) { global.Error(msg, fields...) }

// Warn logs at warn level.
func Warn(msg string, fields ...zap.Field) { global.Warn(msg, fields...) }

// RecoverPanicAndError recovers a panic in a deferred call and logs it, so a
// background goroutine cannot crash the process.
func RecoverPanicAndError(_ context.Context) {
	if r := recover(); r != nil {
		global.Error("recovered panic", zap.Any("panic", r))
	}
}
