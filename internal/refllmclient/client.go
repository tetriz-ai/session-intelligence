// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package refllmclient is the single Go HTTP client to the reference-llm
// service. It is a thin, pure-transport layer over net/http — deliberately not
// routed through a shared vendor HTTP gateway with its own retries/circuit-breaker —
// and exposes a SuccessHook seam for post-success emission plus a typed error taxonomy.
package refllmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/tetriz-ai/session-intelligence/internal/logger"
	"github.com/tetriz-ai/session-intelligence/internal/logredact"
	"github.com/tetriz-ai/session-intelligence/internal/reqcontext"
)

// Client is the reference-llm HTTP client; construct it with New.
type Client struct {
	baseURL    string
	httpClient *http.Client
	hook       SuccessHook   // nil = no emission; the post-success emit seam
	timeout    time.Duration // unconditional per-call cap via context.WithTimeout; 0 = no cap beyond the http.Client backstop (see New)
}

// Option configures a Client at construction.
type Option func(*Client)

// WithSuccessHook attaches the post-success emit seam (nil disables emission).
func WithSuccessHook(h SuccessHook) Option { return func(c *Client) { c.hook = h } }

// WithTimeout caps every call at d via a context deadline.
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

// WithHTTPClient overrides the underlying http.Client (test seam).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// defaultHTTPTimeout is a last-resort backstop on the underlying http.Client so a
// caller that sets neither WithTimeout nor a context deadline fails bounded rather
// than hanging forever. It is deliberately generous — every current caller imposes a
// tighter per-call deadline, so this never fires for them; it only guards a future
// caller. Override the whole client (and this) via WithHTTPClient.
const defaultHTTPTimeout = 10 * time.Minute

// New builds a client pointing at the reference-llm base URL.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{baseURL: baseURL, httpClient: &http.Client{Timeout: defaultHTTPTimeout}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// postJSON is the transport primitive every endpoint method calls: marshal, POST,
// read under the size ceiling, status-check, and (on 2xx) decode into responseTarget.
// It returns the verbatim body and a typed *Error.
func (c *Client) postJSON(ctx context.Context, path string, req, responseTarget any, maxBody int64) ([]byte, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, &Error{Kind: KindRequestBuild, err: fmt.Errorf("marshal request: %w", err)}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(reqBytes))
	if err != nil {
		return nil, &Error{Kind: KindRequestBuild, err: fmt.Errorf("build request: %w", err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if traceID := reqcontext.TraceIDFromContext(ctx); traceID != "" && traceID != reqcontext.UnknownID {
		httpReq.Header.Set("X-Trace-ID", traceID)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, &Error{Kind: classifyTransport(err), err: fmt.Errorf("do request: %w", err)}
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			logger.WarnWithCtx(ctx, "refllmclient: closing response body failed", zap.Error(cerr))
		}
	}()

	// +1 so a body that hits the cap surfaces as an explicit KindResponseTooLarge rather
	// than a silent truncation.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		// A mid-stream read failure is a transport fault, not a parse failure.
		return nil, &Error{Kind: KindTransport, Status: resp.StatusCode, err: fmt.Errorf("read response body: %w", err)}
	}
	if int64(len(body)) > maxBody {
		return body, &Error{
			Kind: KindResponseTooLarge, Status: resp.StatusCode,
			err: fmt.Errorf("response body exceeds %d bytes", maxBody),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The body can hold transcript prose, which a pattern redactor cannot mask, so it is
		// never echoed into the message. classifyBody has already read the body for its `code`;
		// VendorErrorDetail then admits only machine identifiers (code, correlationId, status)
		// for the message, and the raw body is dropped on return.
		httpErr := fmt.Errorf("reference-llm returned HTTP %d", resp.StatusCode)
		if detail := logredact.VendorErrorDetail(body); detail != "" {
			httpErr = fmt.Errorf("reference-llm returned HTTP %d: %s", resp.StatusCode, detail)
		}
		return body, &Error{
			Kind: classifyBody(classifyStatus(resp.StatusCode), body), Status: resp.StatusCode,
			err: httpErr,
		}
	}
	if responseTarget != nil {
		if err := json.Unmarshal(body, responseTarget); err != nil {
			return body, &Error{Kind: KindParse, Status: resp.StatusCode, err: fmt.Errorf("unmarshal response: %w", err)}
		}
	}
	return body, nil
}
