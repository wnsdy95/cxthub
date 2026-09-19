package domain

import (
	"crypto/rand"
	"encoding/hex"
)

// NewSessionID generates a UUIDv4-like identifier for session filenames (without external dependencies on crypto/rand).
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand failed while generating session ID: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// ValidSessionID accepts the canonical UUID-shaped identifiers used in
// provider session filenames. Repository-controlled boundary state must not
// turn a session ID into a path or glob fragment.
func ValidSessionID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
				return false
			}
		}
	}
	return true
}
