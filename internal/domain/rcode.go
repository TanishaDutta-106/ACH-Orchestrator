// Package domain — rcode.go
//
// This file implements NACHA ACH return code (R-code) routing. Every R-code
// maps to exactly one ReturnCategory, which the retry orchestrator uses to
// decide the next action for a returned payment.
//
// Reference: NACHA Operating Rules, Article Two, Return Entries.
package domain

// ReturnCategory is the orchestrator's classification of an R-code.
// Three possible outcomes exist for any returned payment.
type ReturnCategory int

const (
	// CategoryRetryable means the payment can be re-presented to the RDFI,
	// subject to the representment limit in rules.go. Typical cause: a
	// temporary account condition (e.g. insufficient funds).
	CategoryRetryable ReturnCategory = iota

	// CategoryNonRetryable means NACHA rules or the nature of the return
	// prohibit re-submission. The payment must be marked failed and the
	// originator notified. Retrying would expose the originator to fines.
	CategoryNonRetryable

	// CategoryComplianceEscalation means the return indicates a potential
	// legal, regulatory, or fraud issue. A compliance officer must review
	// before any further action is taken. Automated retrying is prohibited.
	CategoryComplianceEscalation
)

// RCodeDefinition pairs a human-readable description with its routing category.
// Storing the description here means log messages and audit entries can include
// plain-English context without a separate lookup table.
type RCodeDefinition struct {
	Code        string
	Description string
	Category    ReturnCategory
}

// rCodeTable is the authoritative mapping of every standard NACHA R-code
// (R01–R33) to its routing category.
//
// Design note: a slice is used here (not a map) so that the definitions serve
// as documentation you can read top-to-bottom. The exported lookup function
// builds an O(1) map at init time. See init() below.
//
// IMPORTANT — R07 categorization:
// R07 (Authorization Revoked by Customer) is NonRetryable. Some teams
// mistakenly escalate R07 to compliance, but the correct NACHA treatment is
// to halt retrying and notify the customer. Authorization disputes belong in
// the originator's customer-service workflow, not a compliance queue.
var rCodeTable = []RCodeDefinition{
	// ── Retryable ────────────────────────────────────────────────────────────
	// These codes reflect transient account conditions. NACHA permits
	// re-presentation up to 2 times within a defined window.

	{
		Code:        "R01",
		Description: "Insufficient Funds",
		Category:    CategoryRetryable,
	},
	{
		Code:        "R08",
		Description: "Payment Stopped",
		// R08 can be retried if the stop-payment order is removed. In practice
		// most originators attempt one representment and then contact the customer.
		Category: CategoryRetryable,
	},
	{
		Code:        "R09",
		Description: "Uncollected Funds",
		// Funds exist but have not yet cleared (e.g. a deposited check hold).
		// Re-presenting after a short delay usually resolves this.
		Category: CategoryRetryable,
	},

	// ── NonRetryable ─────────────────────────────────────────────────────────
	// These codes indicate permanent account or authorization problems.
	// Retrying would violate NACHA rules or be commercially pointless.

	{
		Code:        "R02",
		Description: "Account Closed",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R03",
		Description: "No Account / Unable to Locate Account",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R04",
		Description: "Invalid Account Number",
		Category:    CategoryNonRetryable,
	},
	// R05 is defined in the ComplianceEscalation block below. It is intentionally
	// absent from the NonRetryable block — unauthorized debits require human review.
	{
		Code:        "R06",
		Description: "Returned per ODFI's Request",
		// ODFI-initiated return; originator should not re-present without
		// coordinating with their bank.
		Category: CategoryNonRetryable,
	},
	{
		Code:        "R07",
		Description: "Authorization Revoked by Customer",
		// The customer has explicitly revoked the debit authorization.
		// Retrying would constitute an unauthorized debit — a NACHA violation.
		// Do NOT escalate to compliance; notify the customer and stop.
		Category: CategoryNonRetryable,
	},
	{
		Code:        "R10",
		Description: "Customer Advises Not Authorized",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R11",
		Description: "Customer Advises Entry Not in Accordance with the Terms of the Authorization",
		// Similar to R10 but specifically about terms mismatch. Stop and notify.
		Category: CategoryNonRetryable,
	},
	{
		Code:        "R12",
		Description: "Branch Sold to Another DFI",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R13",
		Description: "RDFI Not Qualified to Participate",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R17",
		Description: "File Record Edit Criteria",
		// Formatting or data error in the ACH entry. Fix the entry before
		// re-submission; automated retry without a fix will also fail.
		Category: CategoryNonRetryable,
	},
	{
		Code:        "R18",
		Description: "Improper Effective Entry Date",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R19",
		Description: "Amount Field Error",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R20",
		Description: "Non-Transaction Account",
		// The receiving account does not permit ACH debits (e.g. savings-only).
		Category: CategoryNonRetryable,
	},
	{
		Code:        "R21",
		Description: "Invalid Company Identification",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R22",
		Description: "Invalid Individual ID Number",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R23",
		Description: "Credit Entry Refused by Receiver",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R24",
		Description: "Duplicate Entry",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R25",
		Description: "Addenda Error",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R26",
		Description: "Mandatory Field Error",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R27",
		Description: "Trace Number Error",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R28",
		Description: "Routing Number Check Digit Error",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R29",
		Description: "Corporate Customer Advises Not Authorized",
		// Corporate equivalent of R10. Stop and notify originator.
		Category: CategoryNonRetryable,
	},
	{
		Code:        "R30",
		Description: "RDFI Not Participant in Check Truncation Program",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R31",
		Description: "Permissible Return Entry (CCD and CTX only)",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R32",
		Description: "RDFI Non-Settlement",
		Category:    CategoryNonRetryable,
	},
	{
		Code:        "R33",
		Description: "Return of XCK Entry",
		Category:    CategoryNonRetryable,
	},

	// ── ComplianceEscalation ─────────────────────────────────────────────────
	// These codes indicate potential unauthorized transactions, fraud, or
	// regulatory exposure. Human review is mandatory before any further action.

	{
		Code:        "R05",
		Description: "Unauthorized Debit to Consumer Account Using Corporate SEC Code",
		// Misuse of a corporate SEC code (CCD/CTX) against a consumer account.
		// This is a NACHA rules violation and must go to compliance.
		Category: CategoryComplianceEscalation,
	},
	{
		Code:        "R14",
		Description: "Representative Payee Deceased or Unable to Continue in That Capacity",
		// Involves a legally designated representative payee — compliance and
		// potentially legal counsel must determine next steps.
		Category: CategoryComplianceEscalation,
	},
	{
		Code:        "R15",
		Description: "Beneficiary or Account Holder Deceased",
		// Debiting a deceased person's account creates legal exposure.
		// Escalate immediately; do not retry.
		Category: CategoryComplianceEscalation,
	},
	{
		Code:        "R16",
		Description: "Account Frozen / Entry Returned per OFAC Instruction",
		// OFAC (Office of Foreign Assets Control) sanctions freeze. Retrying
		// would be a federal violation. Immediate compliance escalation required.
		Category: CategoryComplianceEscalation,
	},
}

// rCodeMap is the O(1) lookup map built from rCodeTable at package init time.
var rCodeMap map[string]RCodeDefinition

func init() {
	rCodeMap = make(map[string]RCodeDefinition, len(rCodeTable))
	for _, def := range rCodeTable {
		rCodeMap[def.Code] = def
	}
}

// RouteRCode maps an R-code string to its ReturnCategory.
//
// Unknown codes default to CategoryNonRetryable. This is the safest default:
// if we encounter an R-code we don't recognize, retrying blindly could violate
// NACHA rules. Alerting the on-call team to add the new code is preferable to
// silent misbehavior.
//
// Usage:
//
//	cat, desc, ok := RouteRCode("R01")
//	// cat == CategoryRetryable, ok == true
func RouteRCode(code string) (category ReturnCategory, description string, known bool) {
	def, ok := rCodeMap[code]
	if !ok {
		return CategoryNonRetryable, "Unknown R-code — defaulting to NonRetryable", false
	}
	return def.Category, def.Description, true
}

// AllRCodeDefinitions returns a copy of every known R-code definition.
// Useful for administrative endpoints and test assertions.
func AllRCodeDefinitions() []RCodeDefinition {
	defs := make([]RCodeDefinition, 0, len(rCodeMap))
	for _, d := range rCodeMap {
		defs = append(defs, d)
	}
	return defs
}
