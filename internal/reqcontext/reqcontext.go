// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package reqcontext is a minimal shim for the request-context helpers the
// vendored client uses: a trace id carried on the context, and a person id.
package reqcontext

import "context"

// UnknownID is the sentinel returned when no identifier is present on the context.
const UnknownID = "UNKNOWN_ID"

type traceIDKey struct{}
type personIDKey struct{}

// CtxWithTraceID returns a context carrying the trace id, unless it is empty or
// the unknown sentinel.
func CtxWithTraceID(ctx context.Context, traceID string) context.Context {
	if traceID == "" || traceID == UnknownID {
		return ctx
	}
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

// TraceIDFromContext returns the trace id on the context, or UnknownID.
func TraceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey{}).(string); ok && v != "" {
		return v
	}
	return UnknownID
}

// WithPersonID returns a context carrying the person id.
func WithPersonID(ctx context.Context, personID string) context.Context {
	return context.WithValue(ctx, personIDKey{}, personID)
}
