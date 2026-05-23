// Package nacha implements parsing and generation of NACHA-formatted ACH files.
// NACHA files use fixed-width 94-character records, each identified by a
// record type code in position 1.
package nacha

import (
	"fmt"
	"strings"
)

// Record type codes per NACHA specification.
const (
	RecordTypeFileHeader  = "1"
	RecordTypeBatchHeader = "5"
	RecordTypeEntry       = "6"
	RecordTypeAddenda     = "7"
	RecordTypeBatchCtrl   = "8"
	RecordTypeFileCtrl    = "9"
)

// NACHARecordLen is the fixed width of every NACHA record, including the
// newline-terminated 94-character body.
const NACHARecordLen = 94

// ReturnEntry represents a single ACH return extracted from a NACHA file.
// Every field maps directly to positions in the Entry Detail (type 6) and
// Addenda (type 7) records.
type ReturnEntry struct {
	TraceNumber   string // 15-digit trace number from Entry Detail positions 80-94
	RCode         string // e.g. "R01" from Addenda record positions 4-6
	Amount        int64  // in cents, from Entry Detail positions 30-39
	AccountNumber string // from Entry Detail positions 13-29 (trimmed)
	RoutingNumber string // receiving DFI + check digit, positions 4-12
}

// ParseFile parses a complete NACHA return file and returns all return entries.
// Each line must be exactly 94 characters. Blank/short lines are skipped.
// An Addenda record (type 7) is always associated with the immediately
// preceding Entry Detail record (type 6); this function enforces that pairing.
func ParseFile(content string) ([]ReturnEntry, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")

	var entries []ReturnEntry
	var pending *ReturnEntry // entry awaiting its Addenda record

	for lineNum, raw := range lines {
		// Trim trailing whitespace/CR; skip blank lines.
		line := strings.TrimRight(raw, "\r")
		if len(line) == 0 {
			continue
		}
		if len(line) < NACHARecordLen {
			// Partial lines are a format error, but we skip rather than abort
			// so that a file with a trailing newline doesn't fail.
			continue
		}

		recordType := string(line[0])

		switch recordType {
		case RecordTypeFileHeader:
			// Record type 1 — validate basic structure.
			if err := validateFileHeader(line); err != nil {
				return nil, fmt.Errorf("line %d file header: %w", lineNum+1, err)
			}

		case RecordTypeBatchHeader:
			// Record type 5 — batch begins; flush any dangling pending entry.
			if pending != nil {
				// Entry without addenda — store with empty RCode.
				entries = append(entries, *pending)
				pending = nil
			}

		case RecordTypeEntry:
			// Record type 6 — Entry Detail.
			// Flush any preceding entry that had no addenda.
			if pending != nil {
				entries = append(entries, *pending)
			}
			entry, err := parseEntryDetail(line)
			if err != nil {
				return nil, fmt.Errorf("line %d entry detail: %w", lineNum+1, err)
			}
			pending = &entry

		case RecordTypeAddenda:
			// Record type 7 — Addenda, must follow an Entry Detail.
			if pending == nil {
				return nil, fmt.Errorf("line %d: addenda record with no preceding entry detail", lineNum+1)
			}
			rcode, err := parseAddendaRCode(line)
			if err != nil {
				return nil, fmt.Errorf("line %d addenda: %w", lineNum+1, err)
			}
			pending.RCode = rcode
			entries = append(entries, *pending)
			pending = nil

		case RecordTypeBatchCtrl:
			// Record type 8 — Batch Control; flush any pending entry.
			if pending != nil {
				entries = append(entries, *pending)
				pending = nil
			}

		case RecordTypeFileCtrl:
			// Record type 9 — File Control; flush and finish.
			if pending != nil {
				entries = append(entries, *pending)
				pending = nil
			}

		default:
			return nil, fmt.Errorf("line %d: unknown record type %q", lineNum+1, recordType)
		}
	}

	// Handle any trailing entry if file ended without a File Control record.
	if pending != nil {
		entries = append(entries, *pending)
	}

	return entries, nil
}

// validateFileHeader checks the most critical fields on the File Header (type 1).
func validateFileHeader(line string) error {
	if string(line[0]) != RecordTypeFileHeader {
		return fmt.Errorf("expected record type 1, got %q", string(line[0]))
	}
	// Priority code must be "01".
	if line[1:3] != "01" {
		return fmt.Errorf("unexpected priority code %q (want 01)", line[1:3])
	}
	return nil
}

// parseEntryDetail extracts fields from a type-6 Entry Detail record.
//
// NACHA Entry Detail field positions (1-indexed):
//   1       Record Type Code        "6"
//   2-3     Transaction Code        (e.g. "22" = demand credit)
//   4-11    Receiving DFI ID        8-digit routing (no check digit)
//   12      Check Digit             1 digit
//   13-29   DFI Account Number      17 chars (left-justified, space padded)
//   30-39   Amount                  10 chars, in cents (no decimal)
//   40-54   Individual ID Number    15 chars
//   55-76   Individual Name         22 chars
//   77-78   Discretionary Data      2 chars
//   79      Addenda Record Ind      "1" if addenda follows, "0" otherwise
//   80-94   Trace Number            15 digits
func parseEntryDetail(line string) (ReturnEntry, error) {
	if len(line) < NACHARecordLen {
		return ReturnEntry{}, fmt.Errorf("entry detail too short: %d chars", len(line))
	}

	// Routing: positions 4-11 (DFI ID, 8 digits) + position 12 (check digit).
	routingNumber := strings.TrimSpace(line[3:12]) // 0-indexed: [3:12] = positions 4-12

	// Account number: positions 13-29, left-justified, space-padded.
	accountNumber := strings.TrimSpace(line[12:29])

	// Amount: positions 30-39, numeric string in cents.
	amountStr := strings.TrimSpace(line[29:39])
	var amount int64
	if _, err := fmt.Sscanf(amountStr, "%d", &amount); err != nil {
		return ReturnEntry{}, fmt.Errorf("invalid amount %q: %w", amountStr, err)
	}

	// Trace Number: positions 80-94 (0-indexed: [79:94]).
	traceNumber := strings.TrimSpace(line[79:94])

	return ReturnEntry{
		TraceNumber:   traceNumber,
		Amount:        amount,
		AccountNumber: accountNumber,
		RoutingNumber: routingNumber,
	}, nil
}

// parseAddendaRCode extracts the return reason code from a type-7 Addenda record.
//
// NACHA Addenda field positions (1-indexed):
//   1       Record Type Code        "7"
//   2-3     Addenda Type Code       "99" for return addenda
//   4-6     Return Reason Code      e.g. "R01"
//   7-9     Original Entry Trace (first 3 digits)
//   ... rest not needed for return processing
func parseAddendaRCode(line string) (string, error) {
	if len(line) < NACHARecordLen {
		return "", fmt.Errorf("addenda record too short: %d chars", len(line))
	}

	// Addenda Type Code at positions 2-3 (0-indexed: [1:3]).
	// For return entries this should be "99".
	addendaType := line[1:3]
	if addendaType != "99" {
		// Non-return addenda; no R-code to extract.
		return "", fmt.Errorf("addenda type %q is not a return addenda (want 99)", addendaType)
	}

	// Return Reason Code at positions 4-6 (0-indexed: [3:6]).
	rcode := strings.TrimSpace(line[3:6])
	if len(rcode) != 3 {
		return "", fmt.Errorf("invalid R-code length %d for %q", len(rcode), rcode)
	}

	return rcode, nil
}
