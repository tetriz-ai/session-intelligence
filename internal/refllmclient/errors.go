// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package refllmclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// ErrorKind classifies why a reference-llm call failed (the client's error taxonomy).
type ErrorKind int

// Error kinds returned by the client.
const (
	// KindTimeout is the error kind for a request that exceeded its deadline.
	KindTimeout ErrorKind = iota
	KindCanceled
	KindTransport
	KindTooManyRequests
	KindServer
	KindClient
	KindParse
	KindResponseTooLarge
	KindRequestBuild
	// KindOutputTruncated is a 5xx whose body identifies itself as a model output-budget
	// truncation. It is separated from KindServer because it is DETERMINISTIC: the same
	// request truncates again, at the same billed cost, so a caller that retries a 5xx by
	// default must stop on this one. The distinction comes from the response body's `code`
	// field rather than the status, which is 502 for both.
	KindOutputTruncated
)

// String renders the kind as a stable, readable token for operator logs (the
// callers %w-wrap this Error, so a bare integer would force a manual lookup).
func (k ErrorKind) String() string {
	switch k {
	case KindTimeout:
		return "timeout"
	case KindCanceled:
		return "canceled"
	case KindTransport:
		return "transport"
	case KindTooManyRequests:
		return "too_many_requests"
	case KindServer:
		return "server"
	case KindClient:
		return "client"
	case KindParse:
		return "parse"
	case KindResponseTooLarge:
		return "response_too_large"
	case KindRequestBuild:
		return "request_build"
	case KindOutputTruncated:
		return "output_truncated"
	default:
		return fmt.Sprintf("unknown(%d)", int(k))
	}
}

// Error is the single typed error the client returns. Status is the HTTP status, or 0
// when no response arrived. The response body is deliberately NOT retained: it can carry
// transcript prose, and a pattern redactor cannot mask free text, so the wrapped err
// carries only the status plus the machine-identifier summary that the constructor
// extracts with logredact.VendorErrorDetail — never the raw body.
type Error struct {
	Kind   ErrorKind
	Status int
	err    error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("refllmclient: kind=%s status=%d: %v", e.Kind, e.Status, e.err)
	}
	return fmt.Sprintf("refllmclient: kind=%s: %v", e.Kind, e.err)
}

func (e *Error) Unwrap() error { return e.err }

func classifyTransport(err error) ErrorKind {
	switch {
	case errors.Is(err, context.Canceled):
		return KindCanceled
	case isTimeout(err):
		return KindTimeout
	default:
		return KindTransport
	}
}

func classifyStatus(code int) ErrorKind {
	switch {
	case code >= http.StatusInternalServerError:
		return KindServer
	case code == http.StatusTooManyRequests:
		return KindTooManyRequests
	default:
		return KindClient
	}
}

// errorCodeOutputTruncated is the `code` reference-llm sets on a model output-budget
// truncation (LLMOutputTruncatedError). It is a wire contract shared with
// reference-llm/app/llm/exceptions.py — the two must not drift.
const errorCodeOutputTruncated = "OUTPUT_TRUNCATED"

// classifyBody refines a status-derived kind using the response body's own `code`.
//
// Needed because reference-llm answers 502 for every LLM failure, transient and permanent
// alike, so the status alone cannot tell a restarted pod from an output that will truncate
// identically on every retry. The body says which. An unparseable or code-less body leaves
// the status-derived kind untouched, so an older peer that predates the field still behaves
// exactly as before.
func classifyBody(kind ErrorKind, body []byte) ErrorKind {
	if len(body) == 0 {
		return kind
	}
	// FastAPI wraps a raised HTTPException's detail under "detail".
	var envelope struct {
		Detail struct {
			Code string `json:"code"`
		} `json:"detail"`
		Code string `json:"code"`
	}
	// A body that does not parse is not a truncation, so the status-derived kind already
	// stands and the parse error changes nothing.
	if err := json.Unmarshal(body, &envelope); err != nil {
		return kind
	}
	code := envelope.Detail.Code
	if code == "" {
		code = envelope.Code
	}
	if code == errorCodeOutputTruncated {
		return KindOutputTruncated
	}
	return kind
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
