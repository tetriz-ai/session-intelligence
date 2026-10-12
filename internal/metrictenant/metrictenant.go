// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package metrictenant provides the tenant-label helper used in metric labels.
// Single-tenant: every org id is passed through unchanged.
package metrictenant

// OrgID returns the label to use for an organization in metrics. Single-tenant,
// so the org id is passed through unchanged.
func OrgID(orgID string) string { return orgID }
