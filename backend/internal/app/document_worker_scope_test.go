package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type documentWorkerScopeRequiredMetadata struct {
	outbound.MetadataStore
}

func (documentWorkerScopeRequiredMetadata) GetRepo(_ context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return domain.Repo{ID: repo, RequiredDocIdentity: domain.DocumentIdentityRootV1}, nil
}

func TestDocumentWorkerScopeUsesProcessAdmissionAndOwnedYieldOnly(test *testing.T) {
	scope := newDocumentWorkerScope(yieldingPreparationMetadata{}, nil, documentquery.LegacyCompatibility)
	ctx := scope.WorkerContext(inbound.WithRepositoryActor(context.Background(), "original actor"))
	if _, system := inbound.RepositoryActor(ctx); !system || !reflect.DeepEqual(inbound.DocumentIdentities(ctx), scope.documents.supported) {
		test.Fatal("worker context lost system actor or supported identities")
	}
	if err := outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityLegacy); err != nil {
		test.Fatal(err)
	}
	if scope.CanYield(ctx) {
		test.Fatal("unadmitted worker acquired yield authority")
	}
	var captured *resourcePermit
	err := scope.WithinWork(ctx, func(bound context.Context) error {
		captured, _ = bound.Value(resourcePermitKey{}).(*resourcePermit)
		if captured == nil || captured.owner != processResources || !captured.live.Load() || captured.class != ResourceBulk || !scope.CanYield(bound) {
			test.Fatal("worker work did not reuse original process admission")
		}
		if err := scope.WithinWork(bound, func(borrowed context.Context) error {
			if borrowed.Value(resourcePermitKey{}) != captured || scope.CanYield(borrowed) {
				test.Fatal("borrowed permit became a new yield owner")
			}
			return nil
		}); err != nil {
			return err
		}
		transactional := newDocumentWorkerScope(yieldingPreparationMetadata{nested: true}, nil, documentquery.LegacyCompatibility)
		unknown := newDocumentWorkerScope(preparationMetadata{}, nil, documentquery.LegacyCompatibility)
		for _, restricted := range []*documentWorkerScope{transactional, unknown} {
			if restricted.CanYield(bound) {
				test.Fatal("unknown or borrowed transaction permitted yielding")
			}
			if err := restricted.Yield(bound, outbound.RootPublicationResources{HeapBytes: 1}); err != nil {
				return err
			}
		}
		return scope.Yield(bound, outbound.RootPublicationResources{HeapBytes: maxParkedRootHeapBytes + 1})
	})
	if err != nil || captured == nil || captured.live.Load() {
		test.Fatal("work did not release the original permit", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := scope.WithinWork(canceled, func(context.Context) error {
		test.Fatal("canceled work reached its body")
		return nil
	}); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
}

func TestDocumentWorkerScopeExpiresSourceAtPinnedReadReturn(test *testing.T) {
	config, storage := documentWorkersConfig(test)
	metadata := &graphReadTransactions{FSStore: storage}
	config.Metadata = metadata
	ctx, _ := admissionTestContext(time.Second)
	ctx = inbound.WithSystemActor(ctx)
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	document := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{
		{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "pinned source lifetime"}}},
	})
	scope := newDocumentWorkerScope(metadata, storage, config.Profile)
	sources, err := newDocumentWorkerSources(config, scope)
	if err != nil {
		test.Fatal(err)
	}
	var source documentquery.Source
	var readContext context.Context
	err = scope.WithinWork(scope.WorkerContext(ctx), func(work context.Context) error {
		if err := scope.WithinRepositoryRead(work, repo, func(read context.Context) error {
			readContext = read
			if !metadata.InTransaction(read) || metadata.active.Load() != 1 || scope.CanYield(read) || read.Value(resourcePermitKey{}) != work.Value(resourcePermitKey{}) {
				test.Fatal("worker source escaped its pinned snapshot or admission")
			}
			var err error
			source, err = sources.OpenSource(read, repo, document.Hash)
			if err != nil {
				return err
			}
			entry, err := source.Entry(0)
			if err != nil {
				return err
			}
			if _, err := source.Location(0); err != nil {
				return err
			}
			raw, err := source.Read(entry.Offset, entry.Length)
			if err != nil || domain.HashContent(raw) != entry.Hash {
				test.Fatal("source did not read the selected original event", err)
			}
			return nil
		}); err != nil {
			return err
		}
		if work.Err() != nil || ctx.Err() != nil || readContext.Err() != context.Canceled || metadata.active.Load() != 0 {
			test.Fatal("read lifetime canceled its caller or escaped snapshot cleanup")
		}
		for _, operation := range []func() error{
			func() error { _, err := source.Entry(0); return err },
			func() error { _, err := source.Location(0); return err },
			func() error { _, err := source.Read(0, 1); return err },
		} {
			if err := operation(); !errors.Is(err, context.Canceled) {
				test.Fatal("cached source callback outlived the pinned read", err)
			}
		}
		return nil
	})
	if err != nil || ctx.Err() != nil || metadata.reads.Load() != 1 {
		test.Fatal("worker source reopened a transaction or canceled the caller", err)
	}
}

func TestDocumentWorkerScopePreservesSnapshotFailureAndIdentityFence(test *testing.T) {
	config, storage := documentWorkersConfig(test)
	repo := domain.HashContent([]byte(test.Name()))
	failure := errors.New("read snapshot completion failed")
	scope := newDocumentWorkerScope(streamingPreparationMetadata{afterRead: failure}, storage, config.Profile)
	var returned context.Context
	if err := scope.WithinRepositoryRead(context.Background(), repo, func(read context.Context) error {
		returned = read
		return nil
	}); !errors.Is(err, failure) || returned == nil || returned.Err() != context.Canceled {
		test.Fatal("scope lost snapshot completion failure or source cleanup", err)
	}
	scope = newDocumentWorkerScope(documentWorkerScopeRequiredMetadata{}, storage, config.Profile)
	if err := scope.WithinRepositoryRead(context.Background(), repo, func(context.Context) error {
		test.Fatal("worker bypassed the repository identity fence")
		return nil
	}); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		test.Fatal(err)
	}
}

func TestDocumentWorkerScopeBorrowedSnapshotStaysAlive(test *testing.T) {
	config, storage := documentWorkersConfig(test)
	metadata := &graphReadTransactions{FSStore: storage}
	scope := newDocumentWorkerScope(metadata, storage, config.Profile)
	repo := domain.HashContent([]byte(test.Name()))
	failure := errors.New("borrowed read body failed")
	ctx := scope.WorkerContext(context.Background())
	if err := scope.WithinWork(ctx, func(work context.Context) error {
		if err := metadata.WithinReadSnapshot(work, func(borrowed context.Context) error {
			var captured context.Context
			err := scope.WithinRepositoryRead(borrowed, repo, func(read context.Context) error {
				captured = read
				if !metadata.InTransaction(read) || scope.CanYield(read) {
					test.Fatal("borrowed transaction was not retained by worker read")
				}
				return failure
			})
			if !errors.Is(err, failure) || captured == nil || captured.Err() != context.Canceled || borrowed.Err() != nil ||
				!metadata.InTransaction(borrowed) || metadata.active.Load() != 1 || scope.CanYield(borrowed) {
				test.Fatal("worker read canceled or released the borrowed transaction", err)
			}
			return nil
		}); err != nil {
			return err
		}
		if work.Err() != nil || metadata.InTransaction(work) || !scope.CanYield(work) {
			test.Fatal("borrowed read changed outer work ownership")
		}
		return nil
	}); err != nil || ctx.Err() != nil || metadata.reads.Load() != 1 || metadata.active.Load() != 0 {
		test.Fatal("borrowed read reopened or retained a snapshot", err)
	}
}
