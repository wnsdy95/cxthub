package documentquery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type externalEnvelopeGenerationStore struct {
	*legacyFindGenerationStore
	generation       domain.DocRangeGeneration
	binding          domain.DocReadGenerationBinding
	externalEnvelope json.RawMessage
	envelopeError    error
	onEnvelope       func()
	envelopeReads    int
}

type externalEnvelopeGenerationRead struct {
	outbound.DocRangeGenerationRead
	storage *externalEnvelopeGenerationStore
}

func (storage *externalEnvelopeGenerationStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if repo != storage.repo || hash != storage.generation.Hash {
		return nil, domain.ErrNotFound
	}
	if err := storage.binding.Validate(); err != nil {
		return nil, err
	}
	return &externalEnvelopeGenerationRead{storage: storage}, nil
}

func (read *externalEnvelopeGenerationRead) Descriptor() domain.DocRangeGeneration {
	return read.storage.generation
}

func (read *externalEnvelopeGenerationRead) Binding() domain.DocReadGenerationBinding {
	return read.storage.binding
}

func (read *externalEnvelopeGenerationRead) Envelope(ctx context.Context) (json.RawMessage, error) {
	read.storage.envelopeReads++
	if read.storage.onEnvelope != nil {
		read.storage.onEnvelope()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := read.Binding().Validate(); err != nil {
		return nil, err
	}
	return bytes.Clone(read.storage.externalEnvelope), read.storage.envelopeError
}

func newExternalEnvelopeGenerationStore(test *testing.T) (*externalEnvelopeGenerationStore, domain.CIREnvelope) {
	test.Helper()
	envelope := domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex, SourceModel: "current-model",
		Cwd: strings.Repeat("x", 257<<10), GitBranch: "feature/external-envelope", SessionOriginID: "external-envelope-session",
		SourceModels: []string{"previous-model", "current-model"}, ContextTokens: 800000, OutputTokens: 1234, CompactionCount: 3}
	raw, err := domain.CanonicalBytes(domain.CIRDocument{Envelope: envelope, Events: []domain.CIREvent{}})
	if err != nil {
		test.Fatal(err)
	}
	var document struct {
		Envelope json.RawMessage `json:"envelope"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		test.Fatal(err)
	}
	identity := domain.DocumentIdentityLegacy
	generation := domain.DocRangeGeneration{Version: 6, SourceIdentity: &identity, Algorithm: 1, Hash: domain.HashContent(raw),
		EnvelopeRef: &domain.DocRangeEnvelopeReference{Hash: domain.HashContent(document.Envelope), Bytes: len(document.Envelope)}}
	encoded, err := generation.Encode()
	if err != nil || generation.Envelope != nil {
		test.Fatal("invalid external-envelope descriptor", err)
	}
	storage := &externalEnvelopeGenerationStore{legacyFindGenerationStore: newLegacyFindGenerationStore(test), generation: generation,
		externalEnvelope: document.Envelope,
		binding:          domain.DocReadGenerationBinding{SourceID: "11111111-1111-4111-8111-111111111111", RangeGeneration: domain.HashContent(encoded)}}
	if err := storage.PutSnapshot(legacyFindGenerationContext(), domain.Snapshot{ID: generation.Hash, DocHash: generation.Hash, RepoID: storage.repo}); err != nil {
		test.Fatal(err)
	}
	return storage, envelope
}

func TestDocViewAndVisibleBaseReadCompleteExternalEnvelope(test *testing.T) {
	for _, visible := range []bool{false, true} {
		test.Run(map[bool]string{false: "view base", true: "visible base"}[visible], func(test *testing.T) {
			storage, expected := newExternalEnvelopeGenerationStore(test)
			queries := newLegacyFindGenerationQueries(test, storage.FSStore, storage.legacyFindGenerationStore, storage)
			open := queries.openDocViewBase
			if visible {
				open = queries.openDocVisibleBase
			}
			source, err := open(outbound.WithDocReadOnly(legacyFindGenerationContext()), storage.repo, storage.generation.Hash)
			if err != nil || !reflect.DeepEqual(source.envelope, expected) || source.binding != storage.binding || source.total != 0 ||
				source.byteStorage != domain.DocRangeByteStorageDerived || storage.envelopeReads != 1 {
				test.Fatalf("base lost complete external envelope or binding: reads=%d error=%v", storage.envelopeReads, err)
			}
			if storage.cost.whole != 0 || storage.cost.manifests != 0 || storage.cost.indexes != 0 {
				test.Fatal("external envelope selected a compatibility document read", storage.cost)
			}
		})
	}
}

func TestDocVisibleBasePropagatesExternalEnvelopeFailures(test *testing.T) {
	for _, failure := range []error{domain.ErrIntegrity, domain.ErrForbidden, domain.ErrConflict, context.Canceled, context.DeadlineExceeded} {
		test.Run(failure.Error(), func(test *testing.T) {
			storage, _ := newExternalEnvelopeGenerationStore(test)
			storage.envelopeError = failure
			queries := newLegacyFindGenerationQueries(test, storage.FSStore, storage.legacyFindGenerationStore, storage)
			source, err := queries.openDocVisibleBase(outbound.WithDocReadOnly(legacyFindGenerationContext()), storage.repo, storage.generation.Hash)
			if !errors.Is(err, failure) || !reflect.DeepEqual(source, docViewSource{}) || storage.envelopeReads != 1 {
				test.Fatal("external envelope failure was hidden or returned partial source", err)
			}
		})
	}
	for _, cancelDuringRead := range []bool{false, true} {
		test.Run(map[bool]string{false: "malformed envelope", true: "canceled envelope"}[cancelDuringRead], func(test *testing.T) {
			storage, _ := newExternalEnvelopeGenerationStore(test)
			ctx, cancel := context.WithCancel(outbound.WithDocReadOnly(legacyFindGenerationContext()))
			defer cancel()
			want := domain.ErrIntegrity
			storage.externalEnvelope = json.RawMessage(`{"cwd":`)
			if cancelDuringRead {
				storage.onEnvelope, want = cancel, context.Canceled
			}
			queries := newLegacyFindGenerationQueries(test, storage.FSStore, storage.legacyFindGenerationStore, storage)
			source, err := queries.openDocVisibleBase(ctx, storage.repo, storage.generation.Hash)
			if !errors.Is(err, want) || !reflect.DeepEqual(source, docViewSource{}) || storage.envelopeReads != 1 {
				test.Fatal("invalid or canceled external envelope was accepted", err)
			}
		})
	}
}

func TestDocViewRangeUsesEnvelopeAccessorForInlineGeneration(test *testing.T) {
	for _, failure := range []error{nil, domain.ErrForbidden, context.Canceled} {
		storage, _ := newExternalEnvelopeGenerationStore(test)
		manifest, _, err := domain.ConversationManifestForCIR(domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex}, Events: []domain.CIREvent{}})
		if err != nil {
			test.Fatal(err)
		}
		storage.generation = domain.DocRangeGeneration{Version: 2, Algorithm: 1, Hash: storage.generation.Hash, Envelope: manifest.Envelope}
		encoded, err := storage.generation.Encode()
		if err != nil {
			test.Fatal(err)
		}
		storage.binding.RangeGeneration = domain.HashContent(encoded)
		storage.externalEnvelope, storage.envelopeError = manifest.Envelope, failure
		source, err := openDocViewRange(outbound.WithDocReadOnly(legacyFindGenerationContext()), storage, nil, storage, storage.repo, storage.generation.Hash)
		if storage.envelopeReads != 1 {
			test.Fatal("inline generation bypassed envelope accessor", storage.envelopeReads)
		}
		if failure != nil {
			if !errors.Is(err, failure) || !reflect.DeepEqual(source, docViewRange{}) {
				test.Fatal("inline envelope failure fell back or returned partial range", err)
			}
		} else if err != nil || !bytes.Equal(source.envelope, manifest.Envelope) || source.binding != storage.binding {
			test.Fatal("inline generation changed envelope or binding", err)
		}
	}
}
