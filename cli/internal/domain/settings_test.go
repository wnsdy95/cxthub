package domain

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestValidateSettingsBundle(t *testing.T) {
	valid := SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "commands/review.md", ContentB64: base64.StdEncoding.EncodeToString([]byte("review"))}}}
	hash, err := SettingsObjectHash(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSettingsBundle("claude", hash, valid); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		kind   string
		hash   ContentHash
		bundle SettingsBundle
	}{
		{"kind mismatch", "agents", hash, valid},
		{"hash mismatch", "claude", HashContent([]byte("other")), valid},
		{"parent path", "claude", "", SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "../outside", ContentB64: ""}}}},
		{"backslash path", "claude", "", SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: `..\\outside`, ContentB64: ""}}}},
		{"windows drive path", "claude", "", SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "C:/outside", ContentB64: ""}}}},
		{"windows device path", "claude", "", SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "commands/NUL.txt", ContentB64: ""}}}},
		{"trailing space path", "claude", "", SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "commands/review ", ContentB64: ""}}}},
		{"duplicate path", "claude", "", SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "a", ContentB64: ""}, {Path: "a", ContentB64: ""}}}},
		{"bad base64", "claude", "", SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "a", ContentB64: "%%%"}}}},
		{"oversized encoded content", "claude", "", SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "a", ContentB64: strings.Repeat("A", base64.StdEncoding.EncodedLen(MaxSettingsBundleBytes)+4)}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSettingsBundle(tc.kind, tc.hash, tc.bundle); !errors.Is(err, ErrHashMismatch) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
