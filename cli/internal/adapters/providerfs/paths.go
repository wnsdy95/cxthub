// Package providerfs provides filesystem path helpers for provider (claude/codex) paths.
//
// Package providerfs provides side-effect-free path utilities for provider
// session and memory files.
package providerfs

import (
	"github.com/wnsdy95/cxthub/cli/internal/domain"
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

// NewSessionID preserves the adapter API; identifier rules live in domain.
func NewSessionID() string             { return domain.NewSessionID() }
func ValidSessionID(value string) bool { return domain.ValidSessionID(value) }

// SessionIDFromPath extracts the canonical provider session UUID from Claude's
// `<uuid>.jsonl` and Codex's `rollout-...-<uuid>.jsonl` filenames. It never
// trusts directory components and returns an empty string for malformed names.
func SessionIDFromPath(value string) string {
	base := filepath.Base(value)
	base = strings.TrimSuffix(base, ".superseded")
	base = strings.TrimSuffix(base, ".jsonl")
	if len(base) < 36 {
		return ""
	}
	candidate := base[len(base)-36:]
	if !ValidSessionID(candidate) {
		return ""
	}
	return candidate
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
