// Package agentid defines the single canonical Agent identity contract.
package agentid

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

var ErrInvalid = errors.New("invalid Agent ID")

// New returns a canonical lowercase RFC 4122 UUIDv4 using crypto/rand.
func New() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("agentid: random UUIDv4: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return format(raw), nil
}

// Parse validates and returns the canonical UUIDv4 string. Uppercase, braces,
// non-v4 UUIDs, and non-RFC-4122 variants are rejected rather than normalized.
func Parse(value string) (string, error) {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return "", ErrInvalid
	}
	var compact [32]byte
	index := 0
	for offset := 0; offset < len(value); offset++ {
		if offset == 8 || offset == 13 || offset == 18 || offset == 23 {
			continue
		}
		character := value[offset]
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return "", ErrInvalid
		}
		compact[index] = character
		index++
	}
	var raw [16]byte
	if _, err := hex.Decode(raw[:], compact[:]); err != nil {
		return "", ErrInvalid
	}
	if raw[6]>>4 != 4 || raw[8]&0xc0 != 0x80 {
		return "", ErrInvalid
	}
	canonical := format(raw)
	if canonical != value {
		return "", ErrInvalid
	}
	return canonical, nil
}

func Valid(value string) bool {
	_, err := Parse(value)
	return err == nil
}

func format(raw [16]byte) string {
	var compact [32]byte
	hex.Encode(compact[:], raw[:])
	return string(compact[0:8]) + "-" + string(compact[8:12]) + "-" + string(compact[12:16]) + "-" +
		string(compact[16:20]) + "-" + string(compact[20:32])
}
