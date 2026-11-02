package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (captured *CapturedEffectiveMemory) Export(ctx context.Context) (domain.MemoryCaptureBundle, error) {
	return captured.exportWithGraphBase(ctx, nil, nil)
}

func (captured *CapturedEffectiveMemory) exportWithGraphBase(ctx context.Context, base *memoryGraphAnchor, delta *memoryGraphDelta) (domain.MemoryCaptureBundle, error) {
	if err := ctx.Err(); err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	if captured == nil || captured.reader == nil || captured.generation.Validate() != nil || captured.graph.Checkpoint() != captured.generation.Graph || captured.reader.repo != captured.generation.Graph.RepoID {
		return domain.MemoryCaptureBundle{}, domain.ErrIntegrity
	}
	if err := validateCapturedMemoryPins(ctx, captured.reader); err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	header := domain.MemoryCaptureHeader{Version: domain.MemoryCaptureVersion, Rules: domain.MemoryCaptureRules, Generation: captured.generation, Selection: captured.selection, Content: captured.content, PinsCaptured: captured.reader.pinsCaptured}
	var parts []domain.MemoryCapturePart
	graphEntries, prefix := captured.graph.entries, "graph:"
	if base != nil && delta != nil && !delta.overflow {
		header.Version = domain.MemoryCaptureDeltaVersion
		raw, err := json.Marshal(base.graph.checkpoint)
		if err != nil {
			return domain.MemoryCaptureBundle{}, err
		}
		parts = append(parts, domain.MemoryCapturePart{Kind: "graph_ref", Key: string(base.id), Sequence: base.graph.checkpoint.Sequence, Value: raw})
		graphEntries, prefix = delta.entries(), "graph_delta:"
	}
	for _, entry := range graphEntries {
		if err := ctx.Err(); err != nil {
			return domain.MemoryCaptureBundle{}, err
		}
		if entry.Deleted && header.Version == domain.MemoryCaptureVersion {
			return domain.MemoryCaptureBundle{}, domain.ErrIntegrity
		}
		parts = append(parts, domain.MemoryCapturePart{Kind: prefix + entry.Kind, Key: entry.Key, Sequence: entry.Sequence, Value: entry.Value})
	}
	for hash, raw := range captured.reader.objects {
		if err := ctx.Err(); err != nil {
			return domain.MemoryCaptureBundle{}, err
		}
		if raw == nil {
			parts = append(parts, domain.MemoryCapturePart{Kind: "absent", Key: string(hash)})
			continue
		}
		if err := validateCapturedMemoryObject(ctx, hash, raw); err != nil {
			return domain.MemoryCaptureBundle{}, err
		}
		for offset := 0; offset < len(raw); offset += domain.MemoryCaptureChunkBytes {
			parts = append(parts, domain.MemoryCapturePart{Kind: "memory", Key: fmt.Sprintf("%s:%08d", hash, offset/domain.MemoryCaptureChunkBytes), Value: raw[offset:min(offset+domain.MemoryCaptureChunkBytes, len(raw))]})
		}
	}
	for _, pin := range captured.reader.prPins {
		raw, err := json.Marshal(pin)
		if err != nil {
			return domain.MemoryCaptureBundle{}, err
		}
		parts = append(parts, domain.MemoryCapturePart{Kind: "pr_pin", Key: pin.ID, Value: raw})
	}
	for _, pin := range captured.reader.gitPins {
		raw, err := json.Marshal(pin)
		if err != nil {
			return domain.MemoryCaptureBundle{}, err
		}
		parts = append(parts, domain.MemoryCapturePart{Kind: "git_pin", Key: pin.ID, Value: raw})
	}
	return domain.SealMemoryCapture(ctx, header, parts)
}

func RestoreCapturedEffectiveMemory(ctx context.Context, bundle domain.MemoryCaptureBundle, factory outbound.GraphSourceReaderFactory) (*CapturedEffectiveMemory, error) {
	if err := domain.ValidateMemoryCapture(ctx, bundle); err != nil {
		return nil, err
	}
	if factory == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	if bundle.Header.Version != domain.MemoryCaptureVersion {
		return nil, domain.ErrCatalogUnsupported
	}
	return restoreMemoryWithGraph(ctx, bundle, CapturedGraphSource{checkpoint: bundle.Header.Generation.Graph}, factory)
}

func restoreMemoryWithGraph(ctx context.Context, bundle domain.MemoryCaptureBundle, graph CapturedGraphSource, factory outbound.GraphSourceReaderFactory) (*CapturedEffectiveMemory, error) {
	header := bundle.Header
	reader := &capturedMemoryReader{repo: header.Generation.Graph.RepoID, pinsCaptured: header.PinsCaptured, objects: make(map[domain.ContentHash]json.RawMessage)}
	for _, part := range bundle.Parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch {
		case strings.HasPrefix(part.Kind, "graph:"):
			graph.entries = append(graph.entries, domain.RepositoryViewSourceEntry{Kind: strings.TrimPrefix(part.Kind, "graph:"), Key: part.Key, Sequence: part.Sequence, Value: bytes.Clone(part.Value)})
		case part.Kind == "memory":
			separator := strings.LastIndexByte(part.Key, ':')
			identity := domain.ContentHash(part.Key[:separator])
			reader.objects[identity] = append(reader.objects[identity], part.Value...)
		case part.Kind == "absent":
			reader.objects[domain.ContentHash(part.Key)] = nil
		case part.Kind == "pr_pin":
			var pin domain.PRContributionMemory
			if err := domain.DecodeRepositorySourceJSON(ctx, part.Value, &pin); err != nil {
				return nil, err
			}
			if pin.ID != part.Key {
				return nil, domain.ErrIntegrity
			}
			reader.prPins = append(reader.prPins, pin)
		case part.Kind == "git_pin":
			var pin domain.GitContributionMemory
			if err := domain.DecodeRepositorySourceJSON(ctx, part.Value, &pin); err != nil {
				return nil, err
			}
			if pin.ID != part.Key {
				return nil, domain.ErrIntegrity
			}
			reader.gitPins = append(reader.gitPins, pin)
		}
	}
	if err := validateCapturedMemoryPins(ctx, reader); err != nil {
		return nil, err
	}
	for hash, raw := range reader.objects {
		if raw == nil {
			continue
		}
		if err := validateCapturedMemoryObject(ctx, hash, raw); err != nil {
			return nil, err
		}
	}
	metadata, view, entries, err := graph.prepareInputs(ctx)
	if err != nil {
		return nil, err
	}
	evidence, err := factory.BuildGraphSourceReader(ctx, graph.Checkpoint(), metadata, entries)
	if err != nil {
		return nil, err
	}
	if evidence == nil {
		return nil, domain.ErrIntegrity
	}
	reader.view = domain.RepositoryView{Snapshots: view.Snapshots, Refs: view.Refs, History: view.History, Revision: view.Revision}
	queries, err := newFrozenMemoryQueries(reader, evidence, &header.Generation, false)
	if err != nil {
		return nil, err
	}
	if _, err := queries.SelectMemory(ctx, reader.repo, header.Selection); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &CapturedEffectiveMemory{generation: header.Generation, selection: header.Selection, content: header.Content, graph: graph, reader: reader, evidence: evidence}, nil
}

func validateCapturedMemoryObject(ctx context.Context, hash domain.ContentHash, raw []byte) error {
	digest, err := domain.DecodeMemoryCaptureObject(ctx, raw)
	if err != nil {
		return err
	}
	actual, err := domain.MemoryDigestHashBounded(digest, domain.MaxMemoryCaptureObjectBytes)
	if err != nil || actual != hash || domain.HashContent(raw) != hash {
		return domain.ErrIntegrity
	}
	return ctx.Err()
}
