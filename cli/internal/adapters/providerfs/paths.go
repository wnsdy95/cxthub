// Package providerfs provides filesystem path helpers for provider (claude/codex) paths.
//
// Package providerfs provides side-effect-free path utilities for provider
// session and memory files.
package providerfs

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// EncodeCwd encodes the absolute cwd to the Claude session directory name rule.
// Rule (empirically verified): All non-alphanumeric characters ('/' '.', '_' spaces, unicode) are replaced with '-'.
// Example: /Users/work/my_proj → -Users-work-my-proj
func EncodeCwd(absPath string) string {
	out := make([]rune, 0, len(absPath))
	for _, r := range absPath {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}

// ClaudeProjectsDir returns the ~/.claude/projects path.
func ClaudeProjectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// CodexSessionsDir returns the ~/.codex/sessions path.
func CodexSessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "sessions"), nil
}

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

// IsProviderSessionPath reports whether value stays inside a resolved Claude
// or Codex session root and names a regular session JSONL file. A missing leaf
// is accepted because a superseded session can disappear after the boundary
// was recorded; existing symlinks and special files are always rejected.
func IsProviderSessionPath(value string) bool {
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return false
	}
	if !strings.HasSuffix(value, ".jsonl") && !strings.HasSuffix(value, ".jsonl.superseded") {
		return false
	}
	if info, err := os.Lstat(value); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false
		}
	} else if !os.IsNotExist(err) {
		return false
	}
	realDir, err := filepath.EvalSymlinks(filepath.Dir(value))
	if err != nil {
		return false
	}
	for _, rootFn := range []func() (string, error){ClaudeProjectsDir, CodexSessionsDir} {
		root, err := rootFn()
		if err != nil {
			continue
		}
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		candidate := filepath.Join(realDir, filepath.Base(value))
		rel, err := filepath.Rel(realRoot, candidate)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}
