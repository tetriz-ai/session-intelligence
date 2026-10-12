// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package logredact

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text", "hello world", "hello world"},
		{"bearer token", "auth: Bearer abc.def", "auth: [REDACTED]"},
		{"github personal", "token=ghp_xyz123abc", "token=[REDACTED]"}, // fake token, test fixture. gitleaks:allow
		{"github oauth", "gho_xxx", "[REDACTED]"},
		{"github server", "ghs_yyy", "[REDACTED]"},
		{"github user token", "ghu_abc123", "[REDACTED]"},
		{"github refresh token", "ghr_abc123", "[REDACTED]"},
		{"github fine-grained pat", "github_pat_11ABC_defGHI", "[REDACTED]"}, // fake token, test fixture. gitleaks:allow
		{"gitlab pat", "glpat-xy_z-123", "[REDACTED]"},                       // fake token, test fixture. gitleaks:allow
		{"npm token", "npm_" + strings.Repeat("a", 36), "[REDACTED]"},        // real npm tokens are npm_ + 36 base62 chars
		{"google access", "ya29.token-here", "[REDACTED]"},
		{"openai", "sk-abcdefghijklmnopqrstuvwxyz012345", "[REDACTED]"}, // realistic-length fake; min-length anchor
		{"stripe live", "sk_live_abc123DEF456", "[REDACTED]"},           // fake token, test fixture. gitleaks:allow
		{"slack bot", "xoxb-12345", "[REDACTED]"},
		{"slack user", "xoxp-67890", "[REDACTED]"},
		{"slack admin", "xoxa-abcdef", "[REDACTED]"},
		{"slack app", "xapp-1-ABC123", "[REDACTED]"},
		{"aws access key", "AKIAIOSFODNN7EXAMPLE", "[REDACTED]"}, // AWS docs example id
		{"google api key", "AIza" + strings.Repeat("b", 35), "[REDACTED]"},
		{"slack refresh token", "xoxr-2420083914-abcdef0123456789", "[REDACTED]"},
		{
			"pem private key",
			"-----BEGIN RSA PRIVATE KEY-----\nMIIabc123\n-----END RSA PRIVATE KEY-----",
			"[REDACTED]",
		},
		{
			// PGP armored keys put "BLOCK" between KEY and the dashes — the header must still match.
			"pgp private key block",
			"-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQOYBF123keymaterial\n-----END PGP PRIVATE KEY BLOCK-----",
			"[REDACTED]",
		},
		{"jwt", "eyJabc.eyJdef.signature123", "[REDACTED]"},
		{"connection url password", "postgres://admin:s3cr3t@db.host:5432/app", "postgres://admin:[REDACTED]@db.host:5432/app"},
		{"connection url no username", "redis://:s3cr3t@cache:6379", "redis://:[REDACTED]@cache:6379"},
		{"connection url slash in password", "redis://default:p/w@cache:6379", "redis://default:[REDACTED]@cache:6379"},
		{"env secret assignment", "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG", "AWS_SECRET_ACCESS_KEY=[REDACTED]"},
		{"json quoted secret value", `{"password": "hunter2"}`, `{"password": [REDACTED]}`},
		{"quoted multi-word secret", `DB_PASSWORD="hunter two"`, "DB_PASSWORD=[REDACTED]"},
		{"email", "user@example.com", "u***@e***.com"},
		{"path with username", "/Users/alice/Documents/file.txt", "~/Documents/file.txt"},
		{"linux home", "/home/bob/code/main.go", "~/code/main.go"},
		{"multiple patterns", "Bearer xyz and email a@b.co", "[REDACTED] and email a***@b***.co"},
		// False positives the broadened patterns must NOT redact: "sk-" inside a word (no word
		// boundary, too short for the anchor); a non-secret colon label; npm_config_* env vars
		// (now length-anchored); and ordinary fields merely ending in "key" or a word like "pwd".
		{"not an openai key", "disk-usage report: 80%", "disk-usage report: 80%"},
		{"not a secret kv", "status: running", "status: running"},
		{"npm config not a token", "npm_config_registry=https://registry.npmjs.org", "npm_config_registry=https://registry.npmjs.org"},
		{"routing_key not a secret", "routing_key=orders.created", "routing_key=orders.created"},
		{"word ending in key", "monkey: banana", "monkey: banana"},
		{"pwd is working dir", "pwd=/var/app", "pwd=/var/app"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Redact(tt.in)
			if got != tt.want {
				t.Errorf("Redact(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRedactStruct(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]any
		want map[string]any
	}{
		{
			name: "redacts secret in PII-named field",
			in:   map[string]any{"password": "supersecret"},
			want: map[string]any{"password": "s***"},
		},
		{
			name: "redacts email value under email key",
			in:   map[string]any{"email": "alice@example.com"},
			want: map[string]any{"email": "a***@e***.com"},
		},
		{
			name: "redacts bearer in non-PII field",
			in:   map[string]any{"msg": "got Bearer xyz123"},
			want: map[string]any{"msg": "got [REDACTED]"},
		},
		{
			name: "preserves number and bool leaves",
			in:   map[string]any{"count": 42.0, "ok": true, "msg": "ok"},
			want: map[string]any{"count": 42.0, "ok": true, "msg": "ok"},
		},
		{
			name: "recurses into nested object",
			in: map[string]any{
				"outer": map[string]any{
					"token": "abc123",
					"deep":  map[string]any{"api_key": "k-secret"},
				},
			},
			want: map[string]any{
				"outer": map[string]any{
					"token": "a***",
					"deep":  map[string]any{"api_key": "k***"},
				},
			},
		},
		{
			name: "redacts strings inside list",
			in:   map[string]any{"lines": []any{"Bearer token-x", "ok", "ghp_y"}},
			want: map[string]any{"lines": []any{"[REDACTED]", "ok", "[REDACTED]"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := structpb.NewStruct(tt.in)
			if err != nil {
				t.Fatalf("NewStruct: %v", err)
			}
			RedactStruct(s)
			got := s.AsMap()
			if !mapDeepEqual(got, tt.want) {
				t.Errorf("RedactStruct produced %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRedactStruct_Nil(_ *testing.T) {
	// Should not panic.
	RedactStruct(nil)
}

// mapDeepEqual compares two maps with cross-type number tolerance — structpb
// always materializes numbers as float64 so the test maps can use either.
func mapDeepEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		if !deepEqual(av, bv) {
			return false
		}
	}
	return true
}

func deepEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return false
		}
		return mapDeepEqual(av, bv)
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
