package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"

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

// ReadNative reads Claude's repository-scoped auto-memory entrypoint.
// A missing or inaccessible file returns found=false so the caller can fall back to CIR distillation.
func (s *ClaudeMemorySource) ReadNative(ctx context.Context, cwd, _ string) (domain.NativeMemory, bool, error) {
	resolution := resolveClaudeAutoMemory(ctx, cwd)
	if !resolution.proven || !resolution.enabled || resolution.memoryDir == "" {
		return domain.NativeMemory{}, false, nil
	}
	p := filepath.Join(resolution.memoryDir, "MEMORY.md")
	data, err := providerfs.ReadRegularFile(p)
	if err != nil {
		return domain.NativeMemory{}, false, nil
	}
	if strings.TrimSpace(os.Getenv("CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT")) != ClaudeMemoryConfigFingerprint(ctx, cwd) {
		return domain.NativeMemory{}, false, nil
	}
	text := string(data)
	return domain.NativeMemory{
		Provider: domain.ProviderClaude,
		Source:   "claude:MEMORY.md",
		Scope:    domain.NativeMemoryScopeWorkingTree,
		// Resolving the correct file does not prove that the target runtime
		// loaded these exact bytes. Claude can disable auto memory after settings
		// resolution and the file can change before startup, while its transcript
		// exposes no exact-content acknowledgement. Keep the baseline portable
		// until the provider offers such an attestation.
		Text: text,
	}, true, nil
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
