// Package domain_test contains pure unit tests for the R-code router.
// No infrastructure or Temporal test suite required.
package tests

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/domain"
)

// ────────────────────────────────────────────────────────────────────────────
// Table-driven R-code routing tests
// ────────────────────────────────────────────────────────────────────────────

func TestRouteRCode(t *testing.T) {
	cases := []struct {
		code             string
		wantCategory     domain.ReturnCategory
		wantKnown        bool
		description      string // partial description check (non-empty = check it)
	}{
		// ── Retryable ────────────────────────────────────────────────────────
		{"R01", domain.CategoryRetryable, true, ""},
		{"R08", domain.CategoryRetryable, true, ""},
		{"R09", domain.CategoryRetryable, true, ""},

		// ── NonRetryable ─────────────────────────────────────────────────────
		{"R02", domain.CategoryNonRetryable, true, ""},
		{"R03", domain.CategoryNonRetryable, true, ""},
		{"R04", domain.CategoryNonRetryable, true, ""},
		{"R06", domain.CategoryNonRetryable, true, ""},
		// R07 is explicitly NonRetryable — NOT compliance escalation.
		{"R07", domain.CategoryNonRetryable, true, ""},
		{"R10", domain.CategoryNonRetryable, true, ""},
		{"R11", domain.CategoryNonRetryable, true, ""},
		{"R17", domain.CategoryNonRetryable, true, ""},
		{"R29", domain.CategoryNonRetryable, true, ""},
		{"R33", domain.CategoryNonRetryable, true, ""},

		// ── ComplianceEscalation ─────────────────────────────────────────────
		{"R05", domain.CategoryComplianceEscalation, true, ""},
		{"R14", domain.CategoryComplianceEscalation, true, ""},
		{"R15", domain.CategoryComplianceEscalation, true, ""},
		{"R16", domain.CategoryComplianceEscalation, true, ""},

		// ── Unknown → NonRetryable safe default ──────────────────────────────
		{"R99", domain.CategoryNonRetryable, false, ""},
		{"XXXX", domain.CategoryNonRetryable, false, ""},
		{"", domain.CategoryNonRetryable, false, ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.code, func(t *testing.T) {
			gotCategory, gotDescription, gotKnown := domain.RouteRCode(tc.code)
			assert.Equal(t, tc.wantCategory, gotCategory,
				"code=%q: wrong category", tc.code)
			assert.Equal(t, tc.wantKnown, gotKnown,
				"code=%q: wrong known flag", tc.code)
			if tc.description != "" {
				assert.Contains(t, gotDescription, tc.description,
					"code=%q: description mismatch", tc.code)
			}
		})
	}
}

// ── Explicit R07 compliance guard ─────────────────────────────────────────────
// This test exists solely to guard against the common mistake of routing R07
// to compliance escalation.  It is separate from the table above so a future
// developer sees a clear failure message if this invariant is violated.
func TestR07_IsNonRetryable_NotCompliance(t *testing.T) {
	category, _, _ := domain.RouteRCode("R07")
	assert.Equal(t, domain.CategoryNonRetryable, category,
		"R07 (Authorization Revoked by Customer) MUST be NonRetryable — "+
			"retrying R07 is a NACHA rules violation")
	assert.NotEqual(t, domain.CategoryComplianceEscalation, category,
		"R07 must NOT be routed to ComplianceEscalation")
}

// ── Retry delay sanity checks ──────────────────────────────────────────────────
func TestRetryDelayFor(t *testing.T) {
	assert.Equal(t, 24*60*60, int(domain.RetryDelayFor("R01").Seconds()),
		"R01 retry delay must be 24h")
	assert.Equal(t, 24*60*60, int(domain.RetryDelayFor("R08").Seconds()),
		"R08 retry delay must be 24h")
	assert.Equal(t, 48*60*60, int(domain.RetryDelayFor("R09").Seconds()),
		"R09 retry delay must be 48h")
}

// ── MaxRepresentments constant ─────────────────────────────────────────────────
func TestMaxRepresentments_IsTwo(t *testing.T) {
	assert.Equal(t, 2, domain.MaxRepresentments,
		"MaxRepresentments must be 2 per NACHA rules")
}
