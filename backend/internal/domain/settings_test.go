package domain

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestValidateSettingsBundle(t *testing.T) {
	valid := SettingsBundle{
		Kind: "claude",
		Files: []SettingsFile{{
			Path:       "commands/review.md",
			ContentB64: base64.StdEncoding.EncodeToString([]byte("review carefully")),
		}},
	}
	hash, err := SettingsObjectHash(valid)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		expectedKind string
		expectedHash ContentHash
		bundle       SettingsBundle
		wantErr      bool
	}{
		{name: "valid latest", expectedKind: "claude", bundle: valid},
		{name: "valid object", expectedKind: "claude", expectedHash: hash, bundle: valid},
		{name: "kind mismatch", expectedKind: "agents", bundle: valid, wantErr: true},
		{name: "unsafe path", expectedKind: "claude", bundle: SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "../secret", ContentB64: ""}}}, wantErr: true},
		{name: "windows drive path", expectedKind: "claude", bundle: SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "C:/secret", ContentB64: ""}}}, wantErr: true},
		{name: "windows device path", expectedKind: "claude", bundle: SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "commands/CON.md", ContentB64: ""}}}, wantErr: true},
		{name: "trailing dot path", expectedKind: "claude", bundle: SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "commands/review.", ContentB64: ""}}}, wantErr: true},
		{name: "duplicate path", expectedKind: "claude", bundle: SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "a", ContentB64: ""}, {Path: "a", ContentB64: ""}}}, wantErr: true},
		{name: "invalid base64", expectedKind: "claude", bundle: SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "a", ContentB64: "!"}}}, wantErr: true},
		{name: "oversized encoded content", expectedKind: "claude", bundle: SettingsBundle{Kind: "claude", Files: []SettingsFile{{Path: "a", ContentB64: strings.Repeat("A", base64.StdEncoding.EncodedLen(MaxSettingsBundleBytes)+4)}}}, wantErr: true},
		{name: "hash mismatch", expectedKind: "claude", expectedHash: HashContent([]byte("other")), bundle: valid, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSettingsBundle(tt.expectedKind, tt.expectedHash, tt.bundle)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateSettingsBundle() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrIntegrity) {
				t.Fatalf("error = %v, want ErrIntegrity", err)
			}
		})
	}
}
