// Package tests — unit tests that require no live infrastructure.
// No build tag: these run with plain `go test ./tests/...`
package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/nacha"
)

// ── NACHA parser unit tests ────────────────────────────────────────────────────

func TestNACHA_RoundTrip_R01(t *testing.T) {
	assertRoundTrip(t, "R01", 10050)
}

func TestNACHA_RoundTrip_R02(t *testing.T) {
	assertRoundTrip(t, "R02", 5000)
}

func TestNACHA_RoundTrip_R05(t *testing.T) {
	assertRoundTrip(t, "R05", 1)
}

func TestNACHA_RoundTrip_R09(t *testing.T) {
	assertRoundTrip(t, "R09", 250000)
}

func assertRoundTrip(t *testing.T, rcode string, amountCents int64) {
	t.Helper()
	file := nacha.GenerateReturnFile("021000021", "123456789", amountCents, "021000020000001", rcode)
	entries, err := nacha.ParseFile(file)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, rcode, entries[0].RCode)
	assert.Equal(t, amountCents, entries[0].Amount)
}

func TestNACHA_RecordLengths(t *testing.T) {
	file := nacha.GenerateReturnFile("021000021", "123456789", 100, "021000020000001", "R01")
	lines := splitNACHALines(file)
	for i, line := range lines {
		assert.Equalf(t, nacha.NACHARecordLen, len(line),
			"record %d has wrong length", i+1)
	}
}

func TestNACHA_RecordTypeSequence(t *testing.T) {
	file := nacha.GenerateReturnFile("021000021", "123456789", 100, "021000020000001", "R01")
	lines := splitNACHALines(file)
	require.Len(t, lines, 6, "expected exactly 6 records")
	want := []string{"1", "5", "6", "7", "8", "9"}
	for i, rt := range want {
		assert.Equalf(t, rt, string(lines[i][0]), "record %d type wrong", i+1)
	}
}

func TestNACHA_EmptyContent(t *testing.T) {
	entries, err := nacha.ParseFile("")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestNACHA_UnknownRecordType(t *testing.T) {
	// Append a line with an unknown record type after a valid file.
	file := nacha.GenerateReturnFile("021000021", "123456789", 100, "021000020000001", "R01")
	badLine := "X" + strings.Repeat(" ", nacha.NACHARecordLen-1)
	_, err := nacha.ParseFile(file + badLine + "\n")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown record type")
}

// ── Input validation unit tests ────────────────────────────────────────────────
//
// These test the validation logic directly via a minimal httptest server that
// mirrors the handler's validation block — no Temporal/PG/Redis involved.

func TestValidation_Amount(t *testing.T) {
	srv := newValidationServer(t)
	defer srv.Close()

	cases := []struct {
		desc   string
		body   map[string]string
		expect int
	}{
		{"valid positive", body("10.00", "123456789", "021000021"), http.StatusCreated},
		{"valid decimal", body("0.01", "123456789", "021000021"), http.StatusCreated},
		{"missing amount", map[string]string{"account_number": "123456789", "routing_number": "021000021"}, http.StatusBadRequest},
		{"negative", body("-1.00", "123456789", "021000021"), http.StatusBadRequest},
		{"zero", body("0", "123456789", "021000021"), http.StatusBadRequest},
		{"zero decimal", body("0.00", "123456789", "021000021"), http.StatusBadRequest},
		{"non-numeric", body("abc", "123456789", "021000021"), http.StatusBadRequest},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.desc, func(t *testing.T) {
			resp := postJSON(t, srv.URL+"/payments", tc.body)
			assert.Equal(t, tc.expect, resp.StatusCode, tc.desc)
		})
	}
}

func TestValidation_AccountNumber(t *testing.T) {
	srv := newValidationServer(t)
	defer srv.Close()

	cases := []struct {
		acct   string
		expect int
	}{
		{"1234", http.StatusCreated},              // 4 digits — minimum valid
		{"12345678901234567", http.StatusCreated}, // 17 digits — maximum valid
		{"123", http.StatusBadRequest},            // 3 digits — too short
		{"123456789012345678", http.StatusBadRequest}, // 18 digits — too long
		{"12345abc", http.StatusBadRequest},       // contains letters
		{"", http.StatusBadRequest},              // empty
	}

	for _, tc := range cases {
		tc := tc
		t.Run("acct="+tc.acct, func(t *testing.T) {
			resp := postJSON(t, srv.URL+"/payments", body("10.00", tc.acct, "021000021"))
			assert.Equal(t, tc.expect, resp.StatusCode)
		})
	}
}

func TestValidation_RoutingNumber(t *testing.T) {
	srv := newValidationServer(t)
	defer srv.Close()

	cases := []struct {
		routing string
		expect  int
	}{
		{"021000021", http.StatusCreated},     // 9 digits — valid
		{"02100002", http.StatusBadRequest},   // 8 digits — too short
		{"0210000210", http.StatusBadRequest}, // 10 digits — too long
		{"02100002X", http.StatusBadRequest},  // contains letter
		{"", http.StatusBadRequest},           // missing
	}

	for _, tc := range cases {
		tc := tc
		t.Run("routing="+tc.routing, func(t *testing.T) {
			resp := postJSON(t, srv.URL+"/payments", body("10.00", "123456789", tc.routing))
			assert.Equal(t, tc.expect, resp.StatusCode)
		})
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// body is a convenience constructor for the three required POST /payments fields.
func body(amount, account, routing string) map[string]string {
	return map[string]string{
		"amount":         amount,
		"account_number": account,
		"routing_number": routing,
	}
}

// postJSON posts v as JSON to url and returns the response.
func postJSON(t *testing.T, url string, v any) *http.Response {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	return resp
}

// newValidationServer returns an httptest.Server running only the validation
// logic from api.Handler.createPayment — no Temporal/PG/Redis calls.
// Returns 201 on valid input, 400 on invalid.
func newValidationServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(validationHandler))
}

// validationHandler mirrors the validation block in api.(*Handler).createPayment.
// It must be kept in sync with internal/api/handlers.go.
func validationHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/payments" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	var req struct {
		Amount        string `json:"amount"`
		AccountNumber string `json:"account_number"`
		RoutingNumber string `json:"routing_number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !isPositiveDecimal(req.Amount) {
		http.Error(w, "invalid amount", http.StatusBadRequest)
		return
	}
	if !isAccountNumber(req.AccountNumber) {
		http.Error(w, "invalid account_number", http.StatusBadRequest)
		return
	}
	if !isRoutingNumber(req.RoutingNumber) {
		http.Error(w, "invalid routing_number", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

// isPositiveDecimal returns true if s is a non-zero positive decimal number.
func isPositiveDecimal(s string) bool {
	if s == "" {
		return false
	}
	dotSeen := false
	hasNonZeroDigit := false
	for i, c := range s {
		switch {
		case c == '.' && !dotSeen && i > 0:
			dotSeen = true
		case c >= '0' && c <= '9':
			if c > '0' {
				hasNonZeroDigit = true
			}
		default:
			return false // negative sign or letters → invalid
		}
	}
	return hasNonZeroDigit
}

// isAccountNumber returns true if s is 4–17 ASCII digits.
func isAccountNumber(s string) bool {
	if len(s) < 4 || len(s) > 17 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// isRoutingNumber returns true if s is exactly 9 ASCII digits.
func isRoutingNumber(s string) bool {
	if len(s) != 9 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// splitNACHALines splits a NACHA file string on newlines, dropping empty lines.
func splitNACHALines(s string) []string {
	var result []string
	for _, line := range strings.Split(s, "\n") {
		if len(line) > 0 {
			result = append(result, line)
		}
	}
	return result
}
