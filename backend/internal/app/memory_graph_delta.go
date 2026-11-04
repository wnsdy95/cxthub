package app

import (
	"bytes"
	"context"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryGraphAnchor struct {
	id    domain.ContentHash
	graph CapturedGraphSource
}

type memoryGraphDelta struct {
	images   map[[2]string]domain.RepositoryViewSourceEntry
	bytes    int
	overflow bool
}

func (delta *memoryGraphDelta) record(entry domain.RepositoryViewSourceEntry) {
	if delta.overflow {
		return
	}
	if delta.images == nil {
		delta.images = make(map[[2]string]domain.RepositoryViewSourceEntry)
	}
	key := [2]string{entry.Kind, entry.Key}
	if previous, exists := delta.images[key]; exists {
		delta.bytes -= graphSourceEntryBytes(previous) + len("graph_delta:")
	}
	delta.bytes += graphSourceEntryBytes(entry) + len("graph_delta:")
	delta.images[key] = entry
	if len(delta.images) > domain.MaxMemoryCaptureDeltaParts || delta.bytes > domain.MaxMemoryCaptureDeltaBytes {
		delta.images, delta.bytes, delta.overflow = nil, 0, true
	}
}

func (delta *memoryGraphDelta) entries() []domain.RepositoryViewSourceEntry {
	entries := make([]domain.RepositoryViewSourceEntry, 0, len(delta.images))
	for _, entry := range delta.images {
		entries = append(entries, entry)
	}
	sortGraphSourceEntries(entries)
	return entries
}

func sortGraphSourceEntries(entries []domain.RepositoryViewSourceEntry) {
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Kind != entries[right].Kind {
			return entries[left].Kind < entries[right].Kind
		}
		return entries[left].Key < entries[right].Key
	})
}

func restoreMemoryGraphDelta(ctx context.Context, bundle domain.MemoryCaptureBundle, base CapturedGraphSource) (CapturedGraphSource, error) {
	images := make(map[[2]string]domain.RepositoryViewSourceEntry, len(base.entries))
	for _, entry := range base.entries {
		if err := ctx.Err(); err != nil {
			return CapturedGraphSource{}, err
		}
		images[[2]string{entry.Kind, entry.Key}] = entry
	}
	for _, part := range bundle.Parts {
		if err := ctx.Err(); err != nil {
			return CapturedGraphSource{}, err
		}
		if !strings.HasPrefix(part.Kind, "graph_delta:") {
			continue
		}
		kind := strings.TrimPrefix(part.Kind, "graph_delta:")
		key := [2]string{kind, part.Key}
		if len(part.Value) == 0 {
			delete(images, key)
		} else {
			images[key] = domain.RepositoryViewSourceEntry{Kind: kind, Key: part.Key, Sequence: part.Sequence, Value: bytes.Clone(part.Value)}
		}
	}
	if len(images) > domain.MaxPendingProjectionInputRows {
		return CapturedGraphSource{}, domain.ErrRepositoryViewSourceLimit
	}
	entries := make([]domain.RepositoryViewSourceEntry, 0, len(images))
	retainedBytes := 0
	for _, entry := range images {
		if err := ctx.Err(); err != nil {
			return CapturedGraphSource{}, err
		}
		retainedBytes += graphSourceEntryBytes(entry)
		if retainedBytes > domain.MaxPendingProjectionInputBytes {
			return CapturedGraphSource{}, domain.ErrRepositoryViewSourceLimit
		}
		entries = append(entries, entry)
	}
	sortGraphSourceEntries(entries)
	return CapturedGraphSource{checkpoint: bundle.Header.Generation.Graph, entries: entries, decoded: base.decoded}, ctx.Err()
}

func RestoreCapturedEffectiveMemoryWithBase(ctx context.Context, bundle, baseBundle domain.MemoryCaptureBundle, factory outbound.GraphSourceReaderFactory) (*CapturedEffectiveMemory, error) {
	if err := domain.ValidateMemoryCapture(ctx, bundle); err != nil {
		return nil, err
	}
	base, err := domain.MemoryCaptureGraphBase(bundle)
	if err != nil {
		return nil, err
	}
	if base.ID == "" || baseBundle.Header.Version != domain.MemoryCaptureVersion || baseBundle.ID != base.ID ||
		baseBundle.Header.Generation.Graph != base.Checkpoint || baseBundle.Header.Rules != bundle.Header.Rules {
		return nil, domain.ErrIntegrity
	}
	restored, err := RestoreCapturedEffectiveMemory(ctx, baseBundle, factory)
	if err != nil {
		return nil, err
	}
	graph, err := restoreMemoryGraphDelta(ctx, bundle, restored.graph)
	if err != nil {
		return nil, err
	}
	return restoreMemoryWithGraph(ctx, bundle, graph, factory)
}

func BuildMemoryReadWithBase(ctx context.Context, bundle, baseBundle domain.MemoryCaptureBundle, factory outbound.GraphSourceReaderFactory) (domain.MemoryReadBundle, error) {
	captured, err := RestoreCapturedEffectiveMemoryWithBase(ctx, bundle, baseBundle, factory)
	if err != nil {
		return domain.MemoryReadBundle{}, err
	}
	return buildCapturedMemoryRead(ctx, captured, bundle.ID)
}
