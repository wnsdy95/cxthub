package memory

import (
	"context"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// ClaudeMemorySource implements MemorySource by reading Claude Auto Memory (MEMORY.md) (_RECONCILIATION §B).
type ClaudeMemorySource struct{}

// NewClaudeMemorySource creates a ClaudeMemorySource.
func NewClaudeMemorySource() *ClaudeMemorySource { return &ClaudeMemorySource{} }

// Provider returns claude.
func (s *ClaudeMemorySource) Provider() domain.ProviderKind { return domain.ProviderClaude }

// ReadNative reads ~/.claude/projects/<cwd-encoded>/memory/MEMORY.md.
// A missing or inaccessible file returns found=false so the caller can fall back to CIR distillation.
func (s *ClaudeMemorySource) ReadNative(_ context.Context, cwd string) (domain.NativeMemory, bool, error) {
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
	return domain.NativeMemory{Provider: domain.ProviderClaude, Source: "claude:MEMORY.md", Text: string(data)}, true, nil
}

// ClaudeMemorySink implements MemorySink by writing a MemoryDigest to the Claude project instructions file (CLAUDE.md) (_RECONCILIATION §B).
type ClaudeMemorySink struct{}

// NewClaudeMemorySink creates a ClaudeMemorySink.
func NewClaudeMemorySink() *ClaudeMemorySink { return &ClaudeMemorySink{} }

// Provider returns claude.
func (s *ClaudeMemorySink) Provider() domain.ProviderKind { return domain.ProviderClaude }

// Inject writes the digest to cwd/CLAUDE.md and returns the path (including cxt-managed marker).
func (s *ClaudeMemorySink) Inject(_ context.Context, digest domain.MemoryDigest, cwd string) (string, error) {
	path := filepath.Join(cwd, "CLAUDE.md")
	if err := providerfs.WriteRegularFileAtomic(path, []byte(renderMemoryMarkdown(digest)), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Ensure Claude adapters implement the memory ports.
var (
	_ outbound.MemorySource = (*ClaudeMemorySource)(nil)
	_ outbound.MemorySink   = (*ClaudeMemorySink)(nil)
)
