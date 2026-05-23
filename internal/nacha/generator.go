// Package nacha - NACHA file generator for the return simulator.
// Produces standards-compliant 94-character fixed-width records.
package nacha

import (
	"fmt"
	"strings"
	"time"
)

// GenerateReturnFile builds a minimal but spec-compliant NACHA return file
// for a single return entry.
func GenerateReturnFile(
	routingNumber string,
	accountNumber string,
	amount int64,
	traceNumber string,
	rcode string,
) string {
	now := time.Now().UTC()
	fileCreationDate := now.Format("060102")
	fileCreationTime := now.Format("1504")
	effectiveDate := now.Format("060102")

	var sb strings.Builder

	// ── Record 1: File Header ─────────────────────────────────────────────────
	fileHeader := "1" +
		"01" +
		padLeft(" "+routingNumber, 10) +
		padLeft(" "+routingNumber, 10) +
		fileCreationDate +
		fileCreationTime +
		"A" +
		"094" +
		"10" +
		"1" +
		padRight("ACH RETURN RDFI", 23) +
		padRight("ACH RETURN ODFI", 23) +
		padRight("", 8)
	sb.WriteString(padRight(fileHeader, NACHARecordLen))
	sb.WriteString("\n")

	// ── Record 5: Batch Header ────────────────────────────────────────────────
	batchHeader := "5" +
		"225" +
		padRight("ACH RETURN SIM", 16) +
		padRight("", 20) +
		padRight("ACHRETURN", 10) +
		"PPD" +
		padRight("RETURN", 10) +
		effectiveDate +
		effectiveDate +
		"   " +
		"1" +
		routingNumber[:8] +
		padLeft("1", 7, '0')
	sb.WriteString(padRight(batchHeader, NACHARecordLen))
	sb.WriteString("\n")

	// ── Record 6: Entry Detail ────────────────────────────────────────────────
	checkDigit := string(routingNumber[8])
	entryDetail := "6" +
		"21" +
		routingNumber[:8] +
		checkDigit +
		padRight(accountNumber, 17) +
		padLeft(fmt.Sprintf("%d", amount), 10, '0') +
		padRight(traceNumber, 15) +
		padRight("RETURN PAYMENT", 22) +
		"  " +
		"1" +
		padLeft(traceNumber, 15, '0')
	sb.WriteString(padRight(entryDetail, NACHARecordLen))
	sb.WriteString("\n")

	// ── Record 7: Addenda ─────────────────────────────────────────────────────
	addenda := "7" +
		"99" +
		padRight(rcode, 3) +
		padRight(traceNumber, 15) +
		"      " +
		routingNumber[:8] +
		padRight("", 44) +
		padLeft("1", 7, '0')
	sb.WriteString(padRight(addenda, NACHARecordLen))
	sb.WriteString("\n")

	// ── Record 8: Batch Control ───────────────────────────────────────────────
	entryHash := fmt.Sprintf("%010d", hashRouting(routingNumber[:8]))
	batchCtrl := "8" +
		"225" +
		padLeft("2", 6, '0') +
		entryHash +
		padLeft(fmt.Sprintf("%d", amount), 12, '0') +
		padLeft("0", 12, '0') +
		padRight("ACHRETURN", 10) +
		padRight("", 19) +
		"   " +
		routingNumber[:8] +
		padLeft("1", 7, '0')
	sb.WriteString(padRight(batchCtrl, NACHARecordLen))
	sb.WriteString("\n")

	// ── Record 9: File Control ────────────────────────────────────────────────
	fileCtrl := "9" +
		padLeft("1", 6, '0') +
		padLeft("1", 6, '0') +
		padLeft("2", 8, '0') +
		entryHash +
		padLeft(fmt.Sprintf("%d", amount), 12, '0') +
		padLeft("0", 12, '0') +
		padRight("", 39)
	sb.WriteString(padRight(fileCtrl, NACHARecordLen))
	sb.WriteString("\n")

	return sb.String()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func padRight(s string, width int) string {
	if len(s) >= width {
		return s[:width]
	}
	return s + strings.Repeat(" ", width-len(s))
}

func padLeft(s string, width int, pad ...rune) string {
	p := ' '
	if len(pad) > 0 {
		p = pad[0]
	}
	if len(s) >= width {
		return s[len(s)-width:]
	}
	return strings.Repeat(string(p), width-len(s)) + s
}

func hashRouting(routing8 string) int64 {
	var n int64
	for _, c := range routing8 {
		if c >= '0' && c <= '9' {
			n = n*10 + int64(c-'0')
		}
	}
	return n % 10_000_000_000
}