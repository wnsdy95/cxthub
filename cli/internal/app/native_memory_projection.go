package app

import (
	"context"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// readTargetNativeMemory reads provider-owned memory for projection decisions.
// Failure is deliberately fail-open: keeping a possibly duplicated baseline is
// safer than deleting the portable copy when its provider ownership cannot be
// proven.
func readTargetNativeMemory(
	ctx context.Context,
	sources map[domain.ProviderKind]outbound.MemorySource,
	target domain.ProviderKind,
	cwd, sessionID string,
) (*domain.NativeMemory, bool) {
	source, ok := sources[target]
	if !ok {
		return nil, false
	}
	native, found, err := source.ReadNative(ctx, cwd, sessionID)
	if err != nil || !found || strings.TrimSpace(native.Text) == "" {
		return nil, false
	}
	if native.Provider != "" && native.Provider != target {
		return nil, false
	}
	return &native, true
}

// projectAutoLoadedNative removes only a working-tree-scoped baseline from a
// provider-visible copy. Session-scoped memory (notably Codex stage1 memory)
// must cross the materialization boundary because the new session gets a new
// provider ID.
func projectAutoLoadedNative(digest domain.MemoryDigest, native *domain.NativeMemory) domain.MemoryDigest {
	if native == nil || native.Scope != domain.NativeMemoryScopeWorkingTree ||
		strings.TrimSpace(native.AutoLoadedPrefix) == "" {
		return digest
	}
	return domain.WithoutAutoLoadedSummaryPrefix(digest, native.Text, native.AutoLoadedPrefix)
}
