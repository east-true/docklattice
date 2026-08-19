package operation

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A bounded tail keeps the newest bytes, so its head lands wherever the limit
// falls. When that is inside a multi-byte rune the record used to travel to the
// Server as invalid UTF-8, which the Server API classifies as corrupt data and
// answers with 500 for every subsequent read of the operation - including a
// repeated idempotent cancel, which reads the same terminal record again.
func TestBoundedOutputTailNeverStartsMidRune(t *testing.T) {
	for _, limit := range []int{4, 5, 7, 10, 11, 16, 32} {
		for _, chunk := range []int{1, 2, 3, 5, 24} {
			payload := []byte(strings.Repeat("한글", 8) + "abc" + strings.Repeat("é", 6))
			operation := &Operation{outputLimit: limit}
			for start := 0; start < len(payload); start += chunk {
				end := min(start+chunk, len(payload))
				if _, err := operation.WriteOutput(payload[start:end]); err != nil {
					t.Fatalf("limit=%d chunk=%d: %v", limit, chunk, err)
				}
			}
			tail := operation.record.OutputTail
			if !utf8.Valid(tail) {
				t.Fatalf("limit=%d chunk=%d: tail is not valid UTF-8: %q", limit, chunk, tail)
			}
			if len(tail) > limit {
				t.Fatalf("limit=%d chunk=%d: tail exceeds its bound: %d bytes", limit, chunk, len(tail))
			}
			// The retained text must remain a genuine suffix of the output.
			if !strings.HasSuffix(string(payload), string(tail)) {
				t.Fatalf("limit=%d chunk=%d: tail is not a suffix of the output: %q", limit, chunk, tail)
			}
			if !operation.record.OutputTruncated {
				t.Fatalf("limit=%d chunk=%d: a bounded tail must report truncation", limit, chunk)
			}
		}
	}
}

func TestBoundedOutputTailKeepsWholeOutputThatFits(t *testing.T) {
	operation := &Operation{outputLimit: 64}
	if _, err := operation.WriteOutput([]byte("한글 output")); err != nil {
		t.Fatal(err)
	}
	if string(operation.record.OutputTail) != "한글 output" {
		t.Fatalf("tail = %q", operation.record.OutputTail)
	}
	if operation.record.OutputTruncated {
		t.Fatal("an output that fits must not be reported as truncated")
	}
}

func TestTrimPartialLeadingRuneRemovesAtMostOneSplitSequence(t *testing.T) {
	full := []byte("한")
	for cut := 1; cut < len(full); cut++ {
		trimmed := TrimPartialLeadingRune(append(append([]byte(nil), full[cut:]...), []byte("ok")...))
		if string(trimmed) != "ok" {
			t.Fatalf("cut=%d trimmed = %q", cut, trimmed)
		}
	}
	if string(TrimPartialLeadingRune([]byte("한글"))) != "한글" {
		t.Fatal("a tail already starting on a rune boundary must be returned unchanged")
	}
	if string(TrimPartialLeadingRune(nil)) != "" {
		t.Fatal("an empty tail must stay empty")
	}
	// Input with no rune start at all is not truncation damage. It is returned
	// unchanged so the Server's validity check still reports it.
	nonText := []byte{0x80, 0x80, 0x80, 0x80, 0x80}
	if len(TrimPartialLeadingRune(nonText)) != len(nonText) {
		t.Fatal("input carrying no rune start must be returned unchanged")
	}
}
