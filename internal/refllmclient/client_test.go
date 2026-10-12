// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package refllmclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	wpc "github.com/tetriz-ai/session-intelligence/internal/refllmclient"
	wpcmocks "github.com/tetriz-ai/session-intelligence/internal/refllmclient/mocks"
	"github.com/tetriz-ai/session-intelligence/internal/reqcontext"
)

func kindOf(t *testing.T, err error) wpc.ErrorKind {
	t.Helper()
	var we *wpc.Error
	if !errors.As(err, &we) {
		t.Fatalf("expected *refllmclient.Error, got %T: %v", err, err)
	}
	return we.Kind
}

// serverReturning is the default handler body for a case: write status + body.
func serverReturning(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

const okGenerateBody = `{
	"result": "{\"score\":9}",
	"task_type": "analyze_prompt",
	"model": "gemini-2.5",
	"provider": "gemini",
	"usage": {"prompt_tokens": 10, "completion_tokens": 20, "thoughts_tokens": 5, "total_tokens": 35},
	"cost_usd": 0.0012,
	"metadata": {"has_blocking": true}
}`

func TestGenerateContent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		status    int
		body      string
		wantKind  *wpc.ErrorKind
		checkResp func(t *testing.T, resp *wpc.GenerateContentResponse)
	}{
		{
			name:   "happy path parses envelope incl usage cost thinking tokens and raw",
			status: http.StatusOK,
			body:   okGenerateBody,
			checkResp: func(t *testing.T, resp *wpc.GenerateContentResponse) {
				if resp.Result != `{"score":9}` || resp.Model != "gemini-2.5" || resp.Provider != "gemini" {
					t.Fatalf("envelope fields wrong: %+v", resp)
				}
				if resp.Usage == nil || resp.Usage.ThinkingTokens != 5 || resp.Usage.TotalTokens != 35 {
					t.Fatalf("usage wrong: %+v", resp.Usage)
				}
				if resp.CostUSD == nil || *resp.CostUSD != 0.0012 {
					t.Fatalf("cost wrong: %v", resp.CostUSD)
				}
				if string(resp.Metadata) == "" || !strings.Contains(string(resp.Metadata), "has_blocking") {
					t.Fatalf("metadata wrong: %s", resp.Metadata)
				}
				if len(resp.Raw) == 0 {
					t.Fatalf("Raw not set to verbatim body")
				}
			},
		},
		{name: "5xx is KindServer", status: http.StatusBadGateway, body: "upstream down", wantKind: kindPtr(wpc.KindServer)},
		{name: "429 is KindTooManyRequests", status: http.StatusTooManyRequests, body: "slow down", wantKind: kindPtr(wpc.KindTooManyRequests)},
		{name: "4xx is KindClient", status: http.StatusBadRequest, body: "bad input", wantKind: kindPtr(wpc.KindClient)},
		{name: "2xx non-JSON body is KindParse", status: http.StatusOK, body: "not json", wantKind: kindPtr(wpc.KindParse)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := serverReturning(tt.status, tt.body)
			defer srv.Close()
			c := wpc.New(srv.URL)
			resp, err := c.GenerateContent(context.Background(), wpc.GenerateContentRequest{TaskType: "analyze_prompt", OrgID: "o1"})
			if tt.wantKind != nil {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if got := kindOf(t, err); got != *tt.wantKind {
					t.Fatalf("kind = %d, want %d", got, *tt.wantKind)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tt.checkResp(t, resp)
		})
	}
}

func TestGenerateContent_RequestShapeAndTraceHeader(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath, gotCT, gotTrace, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotCT, gotTrace = r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("X-Trace-ID")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, okGenerateBody)
	}))
	defer srv.Close()

	ctx := reqcontext.CtxWithTraceID(context.Background(), "trace-xyz")
	c := wpc.New(srv.URL)
	if _, err := c.GenerateContent(ctx, wpc.GenerateContentRequest{Content: "hi", TaskType: "analyze_prompt", OrgID: "o1", SourceID: "rr1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/generate-content" {
		t.Fatalf("method/path wrong: %s %s", gotMethod, gotPath)
	}
	if gotCT != "application/json" || gotTrace != "trace-xyz" {
		t.Fatalf("headers wrong: ct=%q trace=%q", gotCT, gotTrace)
	}
	if !strings.Contains(gotBody, `"task_type":"analyze_prompt"`) || !strings.Contains(gotBody, `"source_id":"rr1"`) {
		t.Fatalf("request body wrong: %s", gotBody)
	}
}

func TestGenerateContent_HookFiresOnSuccessOnly(t *testing.T) {
	t.Parallel()
	// Fires on a 2xx, with the request and decoded response.
	okSrv := serverReturning(http.StatusOK, okGenerateBody)
	defer okSrv.Close()
	ctrl := gomock.NewController(t)
	hook := wpcmocks.NewMockSuccessHook(ctrl)
	var gotReq wpc.GenerateContentRequest
	var gotResp *wpc.GenerateContentResponse
	hook.EXPECT().OnGenerateContent(gomock.Any(), gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, req wpc.GenerateContentRequest, resp *wpc.GenerateContentResponse) {
			gotReq, gotResp = req, resp
		}).Times(1)
	c := wpc.New(okSrv.URL, wpc.WithSuccessHook(hook))
	if _, err := c.GenerateContent(context.Background(), wpc.GenerateContentRequest{TaskType: "analyze_prompt", OrgID: "o1", SourceID: "rr1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotReq.SourceID != "rr1" || gotResp.Model != "gemini-2.5" {
		t.Fatalf("hook fired with wrong args: req=%+v resp.Model=%q", gotReq, gotResp.Model)
	}

	// Does NOT fire on a non-2xx: a hook with zero expectations fails the test if
	// any of its methods is called.
	errSrv := serverReturning(http.StatusInternalServerError, "boom")
	defer errSrv.Close()
	hook2 := wpcmocks.NewMockSuccessHook(gomock.NewController(t))
	c2 := wpc.New(errSrv.URL, wpc.WithSuccessHook(hook2))
	_, err := c2.GenerateContent(context.Background(), wpc.GenerateContentRequest{TaskType: "analyze_prompt", OrgID: "o1"})
	if got := kindOf(t, err); got != wpc.KindServer {
		t.Fatalf("expected a KindServer error on a 5xx, got kind=%d", got)
	}
}

func TestGenerateContent_TransportAndBuildErrors(t *testing.T) {
	t.Parallel()
	// Unreachable server → KindTransport (connection refused, not a timeout).
	c := wpc.New("http://127.0.0.1:1")
	_, err := c.GenerateContent(context.Background(), wpc.GenerateContentRequest{TaskType: "t", OrgID: "o1"})
	if got := kindOf(t, err); got != wpc.KindTransport {
		t.Fatalf("unreachable: kind=%d want KindTransport", got)
	}
	// Malformed base URL → KindRequestBuild (NewRequest fails).
	c2 := wpc.New("://bad-url")
	_, err = c2.GenerateContent(context.Background(), wpc.GenerateContentRequest{TaskType: "t", OrgID: "o1"})
	if got := kindOf(t, err); got != wpc.KindRequestBuild {
		t.Fatalf("bad url: kind=%d want KindRequestBuild", got)
	}
}

func TestGenerateContent_TimeoutAndCancel(t *testing.T) {
	t.Parallel()
	// WithTimeout caps the call → KindTimeout.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		_, _ = io.WriteString(w, okGenerateBody)
	}))
	defer slow.Close()
	c := wpc.New(slow.URL, wpc.WithTimeout(20*time.Millisecond))
	_, err := c.GenerateContent(context.Background(), wpc.GenerateContentRequest{TaskType: "t", OrgID: "o1"})
	if got := kindOf(t, err); got != wpc.KindTimeout {
		t.Fatalf("timeout: kind=%d want KindTimeout", got)
	}

	// Pre-cancelled context → KindCanceled.
	ok := serverReturning(http.StatusOK, okGenerateBody)
	defer ok.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c2 := wpc.New(ok.URL)
	_, err = c2.GenerateContent(ctx, wpc.GenerateContentRequest{TaskType: "t", OrgID: "o1"})
	if got := kindOf(t, err); got != wpc.KindCanceled {
		t.Fatalf("cancel: kind=%d want KindCanceled", got)
	}
}

func TestDetectRoundTrips(t *testing.T) {
	t.Parallel()
	const okBody = `{"threshold":0.4,"encoder":"all-MiniLM-L6-v2","nli_model":"cross-encoder/nli-deberta-v3-small","results":[{"prompt_index":2,"round_trip_score":0.71,"is_round_trip":true,"features":{"nli_contradiction":0.8}}]}`

	// Happy path: the request body carries the pair batch verbatim, and the response decodes
	// with the threshold + model provenance + per-pair scores.
	var gotBody wpc.DetectRoundTripsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()

	client := wpc.New(srv.URL)
	req := wpc.DetectRoundTripsRequest{Pairs: []wpc.RoundTripPair{
		{PromptIndex: 2, P: "add retry", AgentResponse: "done", Q: "no, revert", Prior: ""},
	}}
	resp, err := client.DetectRoundTrips(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotBody.Pairs) != 1 || gotBody.Pairs[0].PromptIndex != 2 || gotBody.Pairs[0].Q != "no, revert" {
		t.Fatalf("server got wrong request body: %+v", gotBody)
	}
	if resp.Threshold != 0.4 || resp.Encoder != "all-MiniLM-L6-v2" {
		t.Fatalf("response provenance wrong: %+v", resp)
	}
	if len(resp.Results) != 1 || !resp.Results[0].IsRoundTrip || resp.Results[0].RoundTripScore != 0.71 {
		t.Fatalf("response results wrong: %+v", resp.Results)
	}
	if resp.Results[0].Features["nli_contradiction"] != 0.8 {
		t.Fatalf("response features wrong: %+v", resp.Results[0].Features)
	}

	// A 5xx surfaces as a typed KindServer error (no hook — the detector is token-free).
	errSrv := serverReturning(http.StatusInternalServerError, "boom")
	defer errSrv.Close()
	_, err = wpc.New(errSrv.URL).DetectRoundTrips(context.Background(), req)
	var serverErr *wpc.Error
	if !errors.As(err, &serverErr) || serverErr.Kind != wpc.KindServer {
		t.Fatalf("expected a KindServer error on 500, got %v", err)
	}
}

func kindPtr(k wpc.ErrorKind) *wpc.ErrorKind { return &k }

func TestWithHTTPClient(t *testing.T) {
	t.Parallel()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the client gives up rather than sleeping a fixed span, so the test is
		// fast and not wall-clock-coupled; the injected 1ns timeout fires long before this.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		_, _ = io.WriteString(w, okGenerateBody)
	}))
	defer slow.Close()

	// A custom client with a 1ns timeout must be the one actually used — proving
	// WithHTTPClient replaces the default client (and its generous backstop).
	c := wpc.New(slow.URL, wpc.WithHTTPClient(&http.Client{Timeout: time.Nanosecond}))
	_, err := c.GenerateContent(context.Background(), wpc.GenerateContentRequest{TaskType: "t", OrgID: "o1"})
	if err == nil {
		t.Fatalf("expected an error from the injected 1ns-timeout client")
	}
	// A KindTimeout proves the injected 1ns-timeout client was used: the default client's
	// generous backstop would instead have let the server response succeed (no error).
	// Asserting the error kind is deterministic — no wall-clock bound to flake under load.
	if got := kindOf(t, err); got != wpc.KindTimeout {
		t.Fatalf("kind=%d want KindTimeout (http.Client.Timeout surfaces as a timeout)", got)
	}
}

func TestErrorKindString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind wpc.ErrorKind
		want string
	}{
		{wpc.KindTimeout, "timeout"},
		{wpc.KindCanceled, "canceled"},
		{wpc.KindTransport, "transport"},
		{wpc.KindTooManyRequests, "too_many_requests"},
		{wpc.KindServer, "server"},
		{wpc.KindClient, "client"},
		{wpc.KindParse, "parse"},
		{wpc.KindResponseTooLarge, "response_too_large"},
		{wpc.KindRequestBuild, "request_build"},
		{wpc.ErrorKind(99), "unknown(99)"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if got := tt.kind.String(); got != tt.want {
				t.Fatalf("ErrorKind(%d).String() = %q, want %q", int(tt.kind), got, tt.want)
			}
		})
	}
}

// TestGenerateContent_ErrorMessageOmitsBodyProse is the S9 leak guard: a non-2xx body
// can carry transcript prose, so the error message must surface only the machine
// identifiers the body's allowlisted keys hold (code, correlationId) and never the prose.
func TestGenerateContent_ErrorMessageOmitsBodyProse(t *testing.T) {
	t.Parallel()
	const prose = "arbitrary transcript prose that must never reach an error string"
	body := `{"code":"SERVER_BUSY","correlationId":"abc123","message":"` + prose + `"}`
	srv := serverReturning(http.StatusBadGateway, body)
	defer srv.Close()

	c := wpc.New(srv.URL)
	_, err := c.GenerateContent(context.Background(), wpc.GenerateContentRequest{TaskType: "analyze_prompt", OrgID: "o1"})
	if err == nil {
		t.Fatal("expected an error on a 5xx")
	}
	msg := err.Error()
	if strings.Contains(msg, prose) {
		t.Fatalf("error message leaked body prose: %q", msg)
	}
	if !strings.Contains(msg, "SERVER_BUSY") || !strings.Contains(msg, "abc123") {
		t.Fatalf("error message dropped the machine identifiers: %q", msg)
	}
}
