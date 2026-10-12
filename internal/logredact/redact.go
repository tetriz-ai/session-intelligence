// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package logredact applies regex-based PII / secret redaction to free-form
// strings and structured payloads. Used as a defense-in-depth pass before
// re-emitting client-supplied log lines: even if an upstream redactor has a
// bug, secrets caught here never reach the logs.
//
// Patterns cover the common secret / PII shapes a client log line can carry
// (API keys, tokens, connection-URL credentials, PEM blocks, KEY=VALUE pairs).
package logredact

import (
	"regexp"
	"sort"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

// piiKeys are field names whose string values are masked entirely (not just
// pattern-matched). Compared lower-case.
var piiKeys = map[string]struct{}{
	"email":         {},
	"name":          {},
	"token":         {},
	"secret":        {},
	"password":      {},
	"authorization": {},
	"session_token": {},
	"access_token":  {},
	"refresh_token": {},
	"api_key":       {},
}

// redactedPlaceholder is the replacement string written wherever a value is
// fully masked (token regex match, malformed email/path, etc.).
const redactedPlaceholder = "[REDACTED]"

// tokenPatterns match well-known secret formats anywhere inside a string.
// Each match is replaced wholesale with redactedPlaceholder.
var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)Bearer\s+\S+`),
	regexp.MustCompile(`ya29\.\S+`),                                            // Google OAuth access token
	regexp.MustCompile(`gh[opsur]_\S+`),                                        // GitHub OAuth/PAT/server/user/refresh tokens
	regexp.MustCompile(`github_pat_[0-9A-Za-z_]+`),                             // GitHub fine-grained PAT
	regexp.MustCompile(`glpat-[0-9A-Za-z_\-]+`),                                // GitLab PAT
	regexp.MustCompile(`\bnpm_[0-9A-Za-z]{36,}\b`),                             // npm automation token (36 base62 chars); anchored so npm_config_*/npm_package_* env vars are NOT matches
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`),                               // OpenAI-style key; \b + min length so "disk-usage" / "sk-ab" are NOT matches
	regexp.MustCompile(`\b[sr]k_(?:live|test)_[0-9A-Za-z]+`),                   // Stripe secret/restricted keys
	regexp.MustCompile(`xox[bpasor]-\S+`),                                      // Slack bot/user/admin/workspace/legacy/refresh tokens
	regexp.MustCompile(`xapp-\S+`),                                             // Slack app-level token
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                                 // AWS access key id
	regexp.MustCompile(`\bASIA[0-9A-Z]{16}\b`),                                 // AWS temporary access key id
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`),                           // Google API key
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), // JWT
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z0-9 ]*-----.*?-----END [A-Z0-9 ]*PRIVATE KEY[A-Z0-9 ]*-----`), // PEM/PGP private-key block; trailing header words (e.g. PGP "... PRIVATE KEY BLOCK-----") are matched
}

// connectionURLCredRegex masks the password in a scheme://[user]:password@host URL,
// keeping the scheme, user and host so the log still says which service failed. The user
// is optional (a username-less DSN like redis://:pw@host still masks), and the password
// may contain any non-@/non-space byte (so an unencoded "/" in the password is still masked).
var connectionURLCredRegex = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://[^:@\s]*):[^@\s]+@`)

// keyValueSecretRegex masks the VALUE of a KEY=VALUE or KEY: VALUE pair whose key is (or
// ends in) a recognized secret term, keeping the key name so the log still says which setting
// was redacted. Catches env-dump lines like AWS_SECRET_ACCESS_KEY=... that no single-token
// pattern recognises. The key list is a specific allowlist (not a bare "ends in key") so
// ordinary fields like routing_key / cache_key / a word like "monkey" are NOT redacted; an
// optional quote is tolerated on either side so a JSON value ("token": "x") is caught, and a
// fully-quoted value ("a b") is masked whole rather than only its first token.
var keyValueSecretRegex = regexp.MustCompile(`(?i)\b([a-z0-9_.\-]*(?:secret|passwd|password|api[_.\-]?key|access[_.\-]?key|secret[_.\-]?key|private[_.\-]?key|auth[_.\-]?token|token))(["']?\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s]+)`)

var emailRegex = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// pathUsernameRegex masks "/Users/<name>/" or "/home/<name>/" → "~/".
// It does not know the actual username, so it masks any single segment after
// /Users or /home.
var pathUsernameRegex = regexp.MustCompile(`/(Users|home)/[^/]+/`)

// Redact returns s with all known token patterns, emails, and home-dir paths
// replaced. Used for free-form fields like log messages and event names.
func Redact(s string) string {
	for _, p := range tokenPatterns {
		s = p.ReplaceAllString(s, redactedPlaceholder)
	}
	s = connectionURLCredRegex.ReplaceAllString(s, "${1}:"+redactedPlaceholder+"@")
	s = keyValueSecretRegex.ReplaceAllString(s, "${1}${2}"+redactedPlaceholder)
	s = emailRegex.ReplaceAllStringFunc(s, maskEmail)
	s = pathUsernameRegex.ReplaceAllString(s, "~/")
	return s
}

// RedactStruct walks a structpb.Struct in place. String leaves are passed
// through Redact; if the leaf's KEY is a PII name, the entire value is
// replaced with a single-char-prefixed mask (or [REDACTED] for short values).
// No-op on nil.
func RedactStruct(s *structpb.Struct) {
	if s == nil {
		return
	}
	redactFields(s.GetFields())
}

func redactFields(fields map[string]*structpb.Value) {
	for k, v := range fields {
		redactValue(k, v)
	}
}

func redactValue(parentKey string, v *structpb.Value) {
	if v == nil {
		return
	}
	switch kind := v.GetKind().(type) {
	case *structpb.Value_StringValue:
		if isPIIKey(parentKey) {
			kind.StringValue = maskPIIValue(kind.StringValue)
		} else {
			kind.StringValue = Redact(kind.StringValue)
		}
	case *structpb.Value_StructValue:
		if kind.StructValue != nil {
			redactFields(kind.StructValue.GetFields())
		}
	case *structpb.Value_ListValue:
		if kind.ListValue != nil {
			for _, item := range kind.ListValue.GetValues() {
				// List items have no key context — treat as free-form strings.
				redactValue("", item)
			}
		}
	}
}

func isPIIKey(key string) bool {
	if key == "" {
		return false
	}
	_, ok := piiKeys[strings.ToLower(key)]
	return ok
}

// maskPIIValue masks a value whose key suggests it carries PII. If the value
// looks like an email, mask it as an email; otherwise show only the first
// character followed by ***.
func maskPIIValue(v string) string {
	if v == "" {
		return v
	}
	if emailRegex.MatchString(v) {
		return emailRegex.ReplaceAllStringFunc(v, maskEmail)
	}
	if len(v) <= 1 {
		return v
	}
	return string(v[0]) + "***"
}

func maskEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return redactedPlaceholder
	}
	local := email[:at]
	domain := email[at+1:]
	dot := strings.IndexByte(domain, '.')
	if dot < 0 {
		return redactedPlaceholder
	}
	domainName := domain[:dot]
	rest := domain[dot:]
	if local == "" || domainName == "" {
		return redactedPlaceholder
	}
	return string(local[0]) + "***@" + string(domainName[0]) + "***" + rest
}

// vendorDiagnosticKeys are response-body fields whose values are machine-issued
// identifiers rather than prose: safe to carry into an error or a log line. The
// list is deliberately short. Anything absent from it — including a vendor's
// human-readable message — is dropped, because a validation message routinely
// quotes the value that failed validation, and that value is our own submitted
// payload.
var vendorDiagnosticKeys = map[string]struct{}{
	"category":       {},
	"code":           {},
	"correlationId":  {},
	"correlation_id": {},
	"errorType":      {},
	"error_code":     {},
	"requestId":      {},
	"status":         {},
}

// maxVendorDiagnosticValue bounds a value admitted as an identifier. A machine id
// is short and has no spaces; anything longer or containing whitespace is prose
// wearing an allowlisted key, and is dropped.
const maxVendorDiagnosticValue = 64

// VendorErrorDetail summarises an external API's error body for an error message
// or a log line, admitting ONLY machine-issued identifiers.
//
// This is an allowlist, not a redaction pass, and that is the point: Redact masks
// what it recognises (emails, tokens, home paths) and cannot mask what it does
// not — a name, a job title, a company, a URL. A vendor rejecting a contact or a
// deal echoes exactly those fields back, so passing its body through Redact
// leaves most of a person's identity intact. Naming what may leave is the only
// version of this that holds when the vendor changes its error shape.
//
// It scans for the allowlisted keys rather than parsing the body. Parsing would
// buy nothing here — nesting and types are irrelevant to a fixed set of scalar
// fields — while a scan cannot fail, so there is no parse outcome to interpret
// and no branch where a malformed body could fall through to its own raw text.
//
// Returns "" when no admitted key is present. The caller should pair it with the
// HTTP status, which is always safe.
func VendorErrorDetail(body []byte) string {
	s := string(body)
	var kept []string
	for _, key := range sortedVendorDiagnosticKeys() {
		m := vendorDiagnosticPattern(key).FindStringSubmatch(s)
		if m == nil {
			continue
		}
		v := m[1]
		// Re-validated after matching, not trusted because the key was allowlisted:
		// a machine id is short and has no whitespace, so prose wearing an
		// allowlisted key is dropped here.
		if v == "" || len(v) > maxVendorDiagnosticValue || strings.ContainsAny(v, " \t\n\r") {
			continue
		}
		kept = append(kept, key+"="+v)
	}
	// Redact over the allowlist's own output: two independent layers, so an
	// email-shaped value that ever reached an allowlisted key is still masked.
	// The allowlist is the control; this is the seatbelt on it.
	return Redact(strings.Join(kept, " "))
}

func sortedVendorDiagnosticKeys() []string {
	keys := make([]string, 0, len(vendorDiagnosticKeys))
	for k := range vendorDiagnosticKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// vendorDiagnosticPattern matches one allowlisted key's string value. Case
// -insensitive on the key because vendors differ (correlationId, correlation_id),
// and the value class is deliberately narrow: no escapes, no quotes, so a value
// containing either simply does not match and is dropped rather than half-read.
func vendorDiagnosticPattern(key string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)"` + regexp.QuoteMeta(key) + `"\s*:\s*"([^"\\]*)"`)
}
