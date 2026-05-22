package domain_test

import (
	"testing"

	"github.com/tanisha/ach-retry-orchestrator/internal/domain"
)

// TestRouteRCode_Retryable verifies that all retryable R-codes route correctly.
func TestRouteRCode_Retryable(t *testing.T) {
	retryableCodes := []string{"R01", "R08", "R09"}

	for _, code := range retryableCodes {
		t.Run(code, func(t *testing.T) {
			cat, desc, known := domain.RouteRCode(code)

			if !known {
				t.Errorf("%s: expected known=true, got false", code)
			}
			if cat != domain.CategoryRetryable {
				t.Errorf("%s: expected CategoryRetryable, got %v (desc: %s)", code, cat, desc)
			}
		})
	}
}

// TestRouteRCode_NonRetryable verifies that all non-retryable R-codes route correctly.
// Pay special attention to R07 — it must be NonRetryable, NOT ComplianceEscalation.
func TestRouteRCode_NonRetryable(t *testing.T) {
	testCases := []struct {
		code string
		note string
	}{
		{"R02", "Account Closed"},
		{"R03", "No Account"},
		{"R04", "Invalid Account Number"},
		{"R06", "Returned per ODFI Request"},
		{"R07", "Authorization Revoked — must be NonRetryable, NOT ComplianceEscalation"},
		{"R10", "Customer Advises Not Authorized"},
		{"R11", "Not in Accordance with Terms"},
		{"R12", "Branch Sold to Another DFI"},
		{"R13", "RDFI Not Qualified"},
		{"R17", "File Record Edit Criteria"},
		{"R18", "Improper Effective Entry Date"},
		{"R19", "Amount Field Error"},
		{"R20", "Non-Transaction Account"},
		{"R21", "Invalid Company Identification"},
		{"R22", "Invalid Individual ID Number"},
		{"R23", "Credit Entry Refused by Receiver"},
		{"R24", "Duplicate Entry"},
		{"R25", "Addenda Error"},
		{"R26", "Mandatory Field Error"},
		{"R27", "Trace Number Error"},
		{"R28", "Routing Number Check Digit Error"},
		{"R29", "Corporate Customer Advises Not Authorized"},
		{"R30", "RDFI Not Participant in Check Truncation"},
		{"R31", "Permissible Return Entry"},
		{"R32", "RDFI Non-Settlement"},
		{"R33", "Return of XCK Entry"},
	}

	for _, tc := range testCases {
		tc := tc // capture range variable
		t.Run(tc.code, func(t *testing.T) {
			cat, desc, known := domain.RouteRCode(tc.code)

			if !known {
				t.Errorf("%s (%s): expected known=true, got false", tc.code, tc.note)
			}
			if cat != domain.CategoryNonRetryable {
				t.Errorf(
					"%s (%s): expected CategoryNonRetryable, got %v (desc: %s). Note: %s",
					tc.code, tc.note, cat, desc, tc.note,
				)
			}
		})
	}
}

// TestRouteRCode_R07_NotComplianceEscalation is a dedicated, explicit test for
// R07. This exists as a standalone test (in addition to the table above) because
// R07 misclassification is a known pitfall. If this test fails, it's a hard
// NACHA compliance violation — the automated system would be escalating
// payments that should simply be stopped and have the customer notified.
func TestRouteRCode_R07_NotComplianceEscalation(t *testing.T) {
	cat, desc, known := domain.RouteRCode("R07")

	if !known {
		t.Fatal("R07: expected known=true, got false — R07 must be in the routing table")
	}
	if cat == domain.CategoryComplianceEscalation {
		t.Fatalf(
			"R07 was incorrectly classified as ComplianceEscalation (desc: %s). "+
				"R07 (Authorization Revoked by Customer) is NonRetryable only. "+
				"Correct action: stop retrying, notify the customer.",
			desc,
		)
	}
	if cat != domain.CategoryNonRetryable {
		t.Errorf("R07: expected CategoryNonRetryable, got %v", cat)
	}
}

// TestRouteRCode_ComplianceEscalation verifies that all compliance-escalation
// R-codes route correctly.
func TestRouteRCode_ComplianceEscalation(t *testing.T) {
	escalationCodes := []struct {
		code string
		note string
	}{
		{"R05", "Unauthorized Debit — NACHA rules violation, needs compliance review"},
		{"R14", "Representative Payee Deceased"},
		{"R15", "Beneficiary or Account Holder Deceased"},
		{"R16", "Account Frozen / OFAC — federal violation risk"},
	}

	for _, tc := range escalationCodes {
		tc := tc
		t.Run(tc.code, func(t *testing.T) {
			cat, desc, known := domain.RouteRCode(tc.code)

			if !known {
				t.Errorf("%s: expected known=true, got false", tc.code)
			}
			if cat != domain.CategoryComplianceEscalation {
				t.Errorf(
					"%s: expected CategoryComplianceEscalation, got %v (desc: %s)",
					tc.code, cat, desc,
				)
			}
		})
	}
}

// TestRouteRCode_Unknown verifies that an unrecognized R-code defaults to
// NonRetryable and marks known=false. This is the safe-default behavior:
// never retry something we don't understand.
func TestRouteRCode_Unknown(t *testing.T) {
	unknownCodes := []string{"R99", "X01", "", "INVALID", "r01"} // lowercase should also miss

	for _, code := range unknownCodes {
		t.Run("unknown_"+code, func(t *testing.T) {
			cat, _, known := domain.RouteRCode(code)

			if known {
				t.Errorf("%q: expected known=false for unknown R-code, got true", code)
			}
			if cat != domain.CategoryNonRetryable {
				t.Errorf("%q: expected CategoryNonRetryable safe default, got %v", code, cat)
			}
		})
	}
}

// TestRouteRCode_R05_IsComplianceNotNonRetryable specifically validates R05
// to ensure it didn't accidentally land in the NonRetryable bucket. R05 is
// the unauthorized debit code and must always go to compliance.
func TestRouteRCode_R05_IsComplianceNotNonRetryable(t *testing.T) {
	cat, desc, known := domain.RouteRCode("R05")

	if !known {
		t.Fatal("R05 must be a known R-code")
	}
	if cat != domain.CategoryComplianceEscalation {
		t.Fatalf(
			"R05 (Unauthorized Debit) must be ComplianceEscalation, got %v (desc: %s). "+
				"Automated handling of R05 is a NACHA rules violation.",
			cat, desc,
		)
	}
}

// TestAllRCodeDefinitions_NoDuplicates verifies that AllRCodeDefinitions does
// not return duplicate codes. A duplicate in the routing table is a logic bug.
func TestAllRCodeDefinitions_NoDuplicates(t *testing.T) {
	defs := domain.AllRCodeDefinitions()
	seen := make(map[string]bool, len(defs))

	for _, d := range defs {
		if seen[d.Code] {
			t.Errorf("duplicate R-code in definitions: %s", d.Code)
		}
		seen[d.Code] = true
	}
}

// TestAllRCodeDefinitions_MinimumCoverage ensures we have at least R01–R33
// covered (minus the R05 placeholder which is documented but not double-stored).
func TestAllRCodeDefinitions_MinimumCoverage(t *testing.T) {
	defs := domain.AllRCodeDefinitions()
	// R01–R33 = 33 codes, all defined in the routing table.
	const minExpected = 33
	if len(defs) < minExpected {
		t.Errorf("expected at least %d R-code definitions, got %d", minExpected, len(defs))
	}
}

// TestIsTransitionAllowed validates the FSM transition table.
func TestIsTransitionAllowed(t *testing.T) {
	allowed := []struct{ from, to domain.PaymentState }{
		{domain.StateInitiated, domain.StatePending},
		{domain.StatePending, domain.StateSubmitted},
		{domain.StateSubmitted, domain.StateSettled},
		{domain.StateSubmitted, domain.StateReturned},
		{domain.StateReturned, domain.StatePending},
		{domain.StateReturned, domain.StateFailedNonRetryable},
		{domain.StateReturned, domain.StateFailedRetryableExhausted},
		{domain.StateReturned, domain.StateComplianceEscalation},
	}
	for _, tc := range allowed {
		if !domain.IsTransitionAllowed(tc.from, tc.to) {
			t.Errorf("expected %s → %s to be allowed", tc.from, tc.to)
		}
	}

	forbidden := []struct{ from, to domain.PaymentState }{
		// Terminal states cannot transition anywhere.
		{domain.StateSettled, domain.StateInitiated},
		{domain.StateFailedNonRetryable, domain.StatePending},
		{domain.StateComplianceEscalation, domain.StateReturned},
		// Skipping states is not allowed.
		{domain.StateInitiated, domain.StateSettled},
		{domain.StateInitiated, domain.StateSubmitted},
	}
	for _, tc := range forbidden {
		if domain.IsTransitionAllowed(tc.from, tc.to) {
			t.Errorf("expected %s → %s to be forbidden", tc.from, tc.to)
		}
	}
}

// TestIsTerminalState ensures terminal states are correctly identified.
func TestIsTerminalState(t *testing.T) {
	terminals := []domain.PaymentState{
		domain.StateSettled,
		domain.StateFailedNonRetryable,
		domain.StateFailedRetryableExhausted,
		domain.StateComplianceEscalation,
	}
	for _, s := range terminals {
		if !domain.IsTerminalState(s) {
			t.Errorf("expected %s to be terminal", s)
		}
	}

	nonTerminals := []domain.PaymentState{
		domain.StateInitiated,
		domain.StatePending,
		domain.StateSubmitted,
		domain.StateReturned,
	}
	for _, s := range nonTerminals {
		if domain.IsTerminalState(s) {
			t.Errorf("expected %s to be non-terminal", s)
		}
	}
}
