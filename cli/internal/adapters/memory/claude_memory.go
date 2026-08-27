package memory

import (
	"context"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// ClaudeMemorySource implements MemorySource by reading Claude Auto Memory (MEMORY.md) (compatibility rules).
type ClaudeMemorySource struct{}

// NewClaudeMemorySource creates a ClaudeMemorySource.
func NewClaudeMemorySource() *ClaudeMemorySource { return &ClaudeMemorySource{} }

// Provider returns claude.
func (s *ClaudeMemorySource) Provider() domain.ProviderKind { return domain.ProviderClaude }

// ReadNative reads ~/.claude/projects/<cwd-encoded>/memory/MEMORY.md.
// A missing or inaccessible file returns found=false so the caller can fall back to CIR distillation.
func (s *ClaudeMemorySource) ReadNative(_ context.Context, cwd, _ string) (domain.NativeMemory, bool, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	root, err := providerfs.ClaudeProjectsDir()
	if err != nil {
		return domain.NativeMemory{}, false, nil
	}
	p := filepath.Join(root, providerfs.EncodeCwd(abs), "memory", "MEMORY.md")
	data, err := providerfs.ReadRegularFile(p)
	if err != nil {
		return domain.NativeMemory{}, false, nil
	}
	text := string(data)
	return domain.NativeMemory{
		Provider: domain.ProviderClaude,
		Source:   "claude:MEMORY.md",
		Scope:    domain.NativeMemoryScopeWorkingTree,
		// Claude currently loads the first 200 lines or 25KB of auto memory
		// (https://code.claude.com/docs/en/memory).
		// Use lower bounds and complete lines only so a provider-visible suffix
		// is never removed on an ambiguous boundary.
		AutoLoadedPrefix: claudeAutoLoadedPrefix(text),
		Text:             text,
	}, true, nil
}

const (
	claudeAutoMemorySafeLines = 199
	claudeAutoMemorySafeBytes = 24 << 10
)

func claudeAutoLoadedPrefix(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if len(text) <= claudeAutoMemorySafeBytes && strings.Count(text, "\n")+1 <= claudeAutoMemorySafeLines {
		return text
	}
	end := len(text)
	if end > claudeAutoMemorySafeBytes {
		end = claudeAutoMemorySafeBytes
	}
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	lines := 0
	lastCompleteLine := 0
	for i := 0; i < end; i++ {
		if text[i] != '\n' {
			continue
		}
		lines++
		lastCompleteLine = i + 1
		if lines >= claudeAutoMemorySafeLines {
			break
		}
	}
	return strings.TrimSpace(text[:lastCompleteLine])
}

// ClaudeMemorySink implements MemorySink by writing a MemoryDigest to the Claude project instructions file (CLAUDE.md) (compatibility rules).
type ClaudeMemorySink struct{}

// NewClaudeMemorySink creates a ClaudeMemorySink.
func NewClaudeMemorySink() *ClaudeMemorySink { return &ClaudeMemorySink{} }

// Provider returns claude.
func (s *ClaudeMemorySink) Provider() domain.ProviderKind { return domain.ProviderClaude }

// Inject refreshes the bounded cxt-managed region in cwd/CLAUDE.md while
// preserving user-authored content outside the markers.
func (s *ClaudeMemorySink) Inject(_ context.Context, digest domain.MemoryDigest, cwd string) (string, error) {
	path := filepath.Join(cwd, "CLAUDE.md")
	if err := writeManagedMemory(path, digest); err != nil {
		return "", err
	}
	return path, nil
}

// Ensure Claude adapters implement the memory ports.
var (
	_ outbound.MemorySource = (*ClaudeMemorySource)(nil)
	_ outbound.MemorySink   = (*ClaudeMemorySink)(nil)
)
