package domain

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

const MaxSettingsBundleBytes = 2 << 20

func ValidSettingsKind(kind string) bool {
	return kind == "claude" || kind == "agents" || kind == "codex"
}

func SettingsObjectHash(bundle SettingsBundle) (ContentHash, error) {
	canonical, err := json.Marshal(struct {
		Kind  string         `json:"kind"`
		Files []SettingsFile `json:"files"`
	}{bundle.Kind, bundle.Files})
	if err != nil {
		return "", err
	}
	return HashContent(canonical), nil
}

// ValidateSettingsBundle is a contract shared by the latest team settings and content-addressed settings objects.
func ValidateSettingsBundle(expectedKind string, expectedHash ContentHash, bundle SettingsBundle) error {
	if !ValidSettingsKind(expectedKind) || bundle.Kind != expectedKind {
		return fmt.Errorf("%w: settings kind mismatch: got %q want %q", ErrIntegrity, bundle.Kind, expectedKind)
	}
	if expectedHash != "" {
		if err := ValidateContentHash(expectedHash); err != nil {
			return err
		}
	}
	seen := make(map[string]bool, len(bundle.Files))
	total := 0
	for _, file := range bundle.Files {
		if !portableSettingsPath(file.Path) {
			return fmt.Errorf("%w: unsafe settings path %q", ErrIntegrity, file.Path)
		}
		clean := path.Clean(file.Path)
		if seen[clean] {
			return fmt.Errorf("%w: duplicate settings path %q", ErrIntegrity, file.Path)
		}
		seen[clean] = true
		remaining := MaxSettingsBundleBytes - total
		if len(file.ContentB64) > base64.StdEncoding.EncodedLen(remaining) {
			return fmt.Errorf("%w: settings bundle exceeds %d bytes", ErrIntegrity, MaxSettingsBundleBytes)
		}
		decoded, err := base64.StdEncoding.DecodeString(file.ContentB64)
		if err != nil {
			return fmt.Errorf("%w: invalid settings content for %q", ErrIntegrity, file.Path)
		}
		total += len(decoded)
		if total > MaxSettingsBundleBytes {
			return fmt.Errorf("%w: settings bundle exceeds %d bytes", ErrIntegrity, MaxSettingsBundleBytes)
		}
	}
	if expectedHash != "" {
		got, err := SettingsObjectHash(bundle)
		if err != nil {
			return err
		}
		if got != expectedHash {
			return fmt.Errorf("%w: settings hash mismatch: got %s want %s", ErrIntegrity, got, expectedHash)
		}
	}
	return nil
}

func portableSettingsPath(value string) bool {
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\\:") {
		return false
	}
	for _, r := range value {
		if r <= 0x1f || r == 0x7f {
			return false
		}
	}
	clean := path.Clean(value)
	if clean != value || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return false
	}
	for _, segment := range strings.Split(clean, "/") {
		if segment == "" || strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") {
			return false
		}
		base := strings.ToUpper(strings.SplitN(segment, ".", 2)[0])
		switch base {
		case "CON", "PRN", "AUX", "NUL",
			"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
			"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
			return false
		}
	}
	return true
}
