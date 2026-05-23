package nacha_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/nacha"
)

// buildTestFile constructs a minimal valid NACHA file string for testing.
// Each record is padded to exactly 94 characters.
func buildTestFile(routingNumber, accountNumber string, amountCents int64, traceNumber, rcode string) string {
	return nacha.GenerateReturnFile(routingNumber, accountNumber, amountCents, traceNumber, rcode)
}

func TestParseFile_R01SingleEntry(t *testing.T) {
	content := buildTestFile("021000021", "123456789", 10050, "021000020000001", "R01")
	entries, err := nacha.ParseFile(content)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	e := entries[0]
	assert.Equal(t, "R01", e.RCode)
	assert.Equal(t, int64(10050), e.Amount)
	assert.Equal(t, "123456789", e.AccountNumber)
	// Trace number is right-padded/zero-padded to 15 chars in the file.
	assert.Equal(t, "021000020000001", strings.TrimSpace(e.TraceNumber))
}

func TestParseFile_R02(t *testing.T) {
	content := buildTestFile("021000021", "987654321", 5000, "021000020000002", "R02")
	entries, err := nacha.ParseFile(content)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "R02", entries[0].RCode)
}

func TestParseFile_R09(t *testing.T) {
	content := buildTestFile("021000021", "111111111", 250000, "021000020000009", "R09")
	entries, err := nacha.ParseFile(content)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "R09", entries[0].RCode)
	assert.Equal(t, int64(250000), entries[0].Amount)
}

func TestParseFile_R05Compliance(t *testing.T) {
	content := buildTestFile("021000021", "555555555", 1, "021000020000005", "R05")
	entries, err := nacha.ParseFile(content)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "R05", entries[0].RCode)
}

func TestParseFile_RoundTrip(t *testing.T) {
	// Generate → parse → verify field fidelity.
	cases := []struct {
		routing string
		account string
		amount  int64
		trace   string
		rcode   string
	}{
		{"021000021", "12345678", 100, "021000020000001", "R01"},
		{"021000021", "99887766", 999999, "021000020000099", "R09"},
		{"021000021", "11223344", 1, "021000020000005", "R05"},
	}

	for _, tc := range cases {
		t.Run(tc.rcode, func(t *testing.T) {
			file := nacha.GenerateReturnFile(tc.routing, tc.account, tc.amount, tc.trace, tc.rcode)
			entries, err := nacha.ParseFile(file)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			assert.Equal(t, tc.rcode, entries[0].RCode)
			assert.Equal(t, tc.amount, entries[0].Amount)
		})
	}
}

func TestParseFile_EmptyContent(t *testing.T) {
	entries, err := nacha.ParseFile("")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestParseFile_UnknownRecordType(t *testing.T) {
	// Insert a record with type "X".
	badLine := strings.Repeat("X", 94)
	content := buildTestFile("021000021", "123456789", 100, "021000020000001", "R01")
	content = content + badLine + "\n"
	_, err := nacha.ParseFile(content)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown record type")
}

func TestParseFile_AddendaWithoutEntry(t *testing.T) {
	// A type-7 addenda record with no preceding type-6 entry must error.
	addendaLine := "7" + "99" + "R01" + strings.Repeat(" ", 94-6)
	_, err := nacha.ParseFile(addendaLine[:94] + "\n")
	// The file header check will catch this first (type "7" != "1"),
	// which is still an error — just verify we get one.
	assert.Error(t, err)
}

func TestGenerateReturnFile_RecordLengths(t *testing.T) {
	content := nacha.GenerateReturnFile("021000021", "123456789", 5000, "021000020000001", "R01")
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	for i, line := range lines {
		assert.Equalf(t, nacha.NACHARecordLen, len(line),
			"record %d has wrong length: %q", i+1, line)
	}
}

func TestGenerateReturnFile_ExpectedRecordTypes(t *testing.T) {
	content := nacha.GenerateReturnFile("021000021", "123456789", 5000, "021000020000001", "R01")
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")

	require.Len(t, lines, 6, "expected 6 records: 1 file header + 1 batch header + 1 entry + 1 addenda + 1 batch ctrl + 1 file ctrl")
	assert.Equal(t, "1", string(lines[0][0]), "record 0 should be file header")
	assert.Equal(t, "5", string(lines[1][0]), "record 1 should be batch header")
	assert.Equal(t, "6", string(lines[2][0]), "record 2 should be entry detail")
	assert.Equal(t, "7", string(lines[3][0]), "record 3 should be addenda")
	assert.Equal(t, "8", string(lines[4][0]), "record 4 should be batch control")
	assert.Equal(t, "9", string(lines[5][0]), "record 5 should be file control")
}
