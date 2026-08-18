package agentid

import (
	"errors"
	"testing"
)

func TestNewReturnsUniqueCanonicalUUIDv4(t *testing.T) {
	seen := make(map[string]struct{})
	for index := 0; index < 128; index++ {
		id, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if !Valid(id) {
			t.Fatalf("New returned invalid ID %q", id)
		}
		if parsed, err := Parse(id); err != nil || parsed != id {
			t.Fatalf("Parse(%q) = %q, %v", id, parsed, err)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate ID %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestParseRejectsNonCanonicalNonV4AndWrongVariant(t *testing.T) {
	for _, value := range []string{
		"", "agt_0123456789abcdef0123456789abcdef",
		"550e8400-e29b-41d4-a716-446655440000 ",
		"550E8400-E29B-41D4-A716-446655440000",
		"{550e8400-e29b-41d4-a716-446655440000}",
		"550e8400-e29b-11d4-a716-446655440000",
		"550e8400-e29b-41d4-7716-446655440000",
		"550e8400e29b41d4a716446655440000",
	} {
		if parsed, err := Parse(value); !errors.Is(err, ErrInvalid) || parsed != "" {
			t.Fatalf("Parse(%q) = %q, %v", value, parsed, err)
		}
		if Valid(value) {
			t.Fatalf("Valid(%q) = true", value)
		}
	}
}
