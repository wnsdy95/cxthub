package documentquery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type sourceReadSnapshots struct {
	reader   SnapshotReader
	calls    int
	failure  error
	snapshot *domain.Snapshot
}

func (reader *sourceReadSnapshots) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	reader.calls++
	if reader.failure != nil {
		return domain.Snapshot{}, reader.failure
	}
	if reader.snapshot != nil {
		return *reader.snapshot, nil
	}
	return reader.reader.GetSnapshot(ctx, repo, hash)
}

type sourceReadScope struct {
	Scope
	test *testing.T
}

func (scope sourceReadScope) WithinDocumentRead(context.Context, domain.ContentHash, func(context.Context) error) error {
	scope.test.Fatal("source opened another admitted read")
	return domain.ErrIntegrity
}

func (scope sourceReadScope) WithinDocumentSnapshot(context.Context, domain.ContentHash, func(context.Context) error) error {
	scope.test.Fatal("source opened another snapshot")
	return domain.ErrIntegrity
}

type sourceReadDerivedChunks struct {
	fixture *conversationFindFixture
	calls   int
}

func (reader *sourceReadDerivedChunks) ReadLegacyDerivedByteChunk(ctx context.Context, repo, hash domain.ContentHash, binding domain.DocReadGenerationBinding, ordinal int, descriptor domain.ConversationManifestChunk) ([]byte, error) {
	reader.fixture.requireRead(ctx)
	reader.calls++
	if repo != reader.fixture.repo || hash != reader.fixture.hash || binding != reader.fixture.binding || ordinal != 0 || !outbound.DocReadOnly(ctx) {
		reader.fixture.test.Fatal("derived source lost its pinned identity, binding or read-only context")
	}
	return reader.fixture.bodies[descriptor.Hash], nil
}

func withinSourceRead(test *testing.T, queries *Queries, fixture *conversationFindFixture, work func(context.Context)) {
	test.Helper()
	bound, release, err := fixture.AcquireInteractive(queries.readContext(inbound.WithSystemActor(context.Background())))
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	err = fixture.WithinDocumentSnapshot(bound, fixture.repo, func(pinned context.Context) error {
		ctx, cancel := context.WithCancel(pinned)
		defer cancel()
		work(ctx)
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
}

func assertSourceReadFailure(test *testing.T, source Source, failure error) {
	test.Helper()
	if entry, err := source.Entry(0); !errors.Is(err, failure) || entry != (domain.DocRangeEntry{}) {
		test.Fatal("failed source exposed entry metadata", entry, err)
	}
	if location, err := source.Location(0); !errors.Is(err, failure) || location != (domain.DocEventIndex{}) {
		test.Fatal("failed source exposed location metadata", location, err)
	}
	if raw, err := source.Read(0, 1); !errors.Is(err, failure) || raw != nil {
		test.Fatal("failed source exposed bytes", raw, err)
	}
}

func TestSourceReadFacadePreservesExactExistingSource(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		test.Run(fmt.Sprintf("legacy=%t", legacy), func(test *testing.T) {
			queries, fixture := newReadOnlyQueries(test, legacy, []domain.CIREvent{
				legacyFindGenerationMessage(domain.RoleUser, "first"),
				legacyFindGenerationMessage(domain.RoleAssistant, "selected 한글🙂"),
			})
			assertReadOnlyDependencies(test, queries)
			metadata := &sourceReadSnapshots{reader: queries.metadata}
			queries.metadata = metadata
			queries.scope = sourceReadScope{Scope: queries.scope, test: test}
			withinSourceRead(test, queries, fixture, func(ctx context.Context) {
				snapshot := domain.Snapshot{ID: fixture.hash, RepoID: fixture.repo, DocHash: fixture.hash, DocIdentity: fixture.identity}
				original, err := queries.docViewSnapshotSource(ctx, fixture.repo, fixture.hash, &snapshot, false)
				if err != nil {
					test.Fatal(err)
				}
				entry, err := original.entry(1)
				if err != nil {
					test.Fatal(err)
				}
				want, err := original.read(entry.Offset, entry.Length)
				if err != nil || domain.HashContent(want) != entry.Hash {
					test.Fatal("original source lost canonical event identity", err)
				}
				for _, open := range []func(context.Context, domain.ContentHash, domain.ContentHash) (Source, error){queries.OpenSource, queries.OpenViewSource} {
					metadata.calls, fixture.pages, fixture.chunks = 0, 0, 0
					source, err := open(ctx, fixture.repo, fixture.hash)
					if err != nil || source.ctx != ctx || source.Binding() != original.binding || source.Total() != original.total ||
						source.Root() != original.root || source.StreamBytes() != original.streamBytes || !reflect.DeepEqual(source.Envelope(), original.envelope) {
						test.Fatal("facade changed original fields or retained a different context", err)
					}
					if source.Root() == legacy || metadata.calls != 1 || fixture.pages != 0 || fixture.chunks != 0 {
						test.Fatal("source inferred identity, reread metadata or eagerly read the document", metadata.calls, fixture.pages, fixture.chunks)
					}
					actual, err := source.Entry(1)
					if err != nil || actual != entry {
						test.Fatal("facade changed entry metadata", actual, err)
					}
					location, err := source.Location(1)
					if err != nil || location != entry.DocEventIndex || fixture.pages != 1 || fixture.chunks != 0 {
						test.Fatal("location bypassed the selected page cache or loaded bytes", location, err)
					}
					for attempt := 0; attempt < 2; attempt++ {
						raw, err := source.Read(entry.Offset, entry.Length)
						if err != nil || !bytes.Equal(raw, want) || fixture.chunks != 1 || fixture.pages != 1 || metadata.calls != 1 {
							test.Fatal("facade changed canonical bytes or repeated selected source work", fixture.pages, fixture.chunks, err)
						}
					}
				}
				if fixture.snapshots != 1 || fixture.admissions != 1 || fixture.preparations != 0 || fixture.enqueued != 0 {
					test.Fatal("source changed the caller's snapshot, admission or preparation boundaries")
				}
			})
		})
	}
}

func TestSourceReadDisplayAndViewKeepDistinctLegacyReadiness(test *testing.T) {
	queries, fixture := newReadOnlyQueries(test, true, []domain.CIREvent{{Kind: domain.EventToolResult, Output: "legacy display bytes"}})
	readiness := &readOnlyByteReadiness{fixture: fixture, enabled: true}
	queries.view.ByteReadiness = readiness
	metadata := &sourceReadSnapshots{reader: queries.metadata}
	queries.metadata = metadata
	withinSourceRead(test, queries, fixture, func(ctx context.Context) {
		source, err := queries.OpenSource(ctx, fixture.repo, fixture.hash)
		if err != nil || source.Root() || source.Binding() != fixture.binding || source.StreamBytes() != fixture.generation.StreamBytes || metadata.calls != 1 {
			test.Fatal("display no longer prefers the existing legacy metadata generation", err)
		}
		entry, err := source.Entry(0)
		if err != nil {
			test.Fatal(err)
		}
		if raw, err := source.Read(entry.Offset, entry.Length); err != nil || domain.HashContent(raw) != entry.Hash {
			test.Fatal("metadata-only legacy display lost its original bytes", err)
		}
		metadata.calls = 0
		view, err := queries.OpenViewSource(ctx, fixture.repo, fixture.hash)
		if !errors.Is(err, errDocViewGenerationNeedsPreparation) || !reflect.DeepEqual(view, Source{}) || metadata.calls != 1 || readiness.checks != 0 || fixture.enqueued != 0 || fixture.preparations != 0 {
			test.Fatal("view source bypassed legacy byte-readiness selection or prepared work", err)
		}
	})
}

func TestSourceReadCancellationPrecedesCachedAccessAndClosedScope(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		test.Run(fmt.Sprintf("legacy=%t", legacy), func(test *testing.T) {
			queries, fixture := newReadOnlyQueries(test, legacy, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "cached bytes")})
			var escaped Source
			withinSourceRead(test, queries, fixture, func(ctx context.Context) {
				bound, cancel := context.WithCancel(ctx)
				defer cancel()
				var err error
				escaped, err = queries.OpenSource(bound, fixture.repo, fixture.hash)
				if err != nil || escaped.ctx != bound {
					test.Fatal("source replaced the caller context", err)
				}
				entry, err := escaped.Entry(0)
				if err != nil {
					test.Fatal(err)
				}
				if _, err := escaped.Location(0); err != nil {
					test.Fatal(err)
				}
				if _, err := escaped.Read(entry.Offset, entry.Length); err != nil {
					test.Fatal(err)
				}
				pages, chunks, admissions := fixture.pages, fixture.chunks, fixture.admissions
				cancel()
				assertSourceReadFailure(test, escaped, context.Canceled)
				if fixture.pages != pages || fixture.chunks != chunks || fixture.admissions != admissions {
					test.Fatal("canceled cached source reached a port or reacquired admission")
				}
			})
			assertSourceReadFailure(test, escaped, context.Canceled)
		})
	}
}

func TestSourceReadZeroInvalidAndCanceledOpenFailBeforePorts(test *testing.T) {
	assertSourceReadFailure(test, Source{}, domain.ErrValidation)
	if source := (Source{}); source.Binding() != (domain.DocReadGenerationBinding{}) || source.Total() != 0 || source.Root() || source.StreamBytes() != 0 || !reflect.DeepEqual(source.Envelope(), domain.CIREnvelope{}) {
		test.Fatal("zero source exposed metadata")
	}
	queries, err := New(constructorPrefixConfig(test, LegacyCompatibility))
	if err != nil {
		test.Fatal(err)
	}
	repo, hash := domain.HashContent([]byte("source repo")), domain.HashContent([]byte("source hash"))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, open := range []func(context.Context, domain.ContentHash, domain.ContentHash) (Source, error){queries.OpenSource, queries.OpenViewSource} {
		for _, request := range []struct {
			ctx        context.Context
			repo, hash domain.ContentHash
			failure    error
		}{
			{ctx: canceled, repo: repo, hash: hash, failure: context.Canceled},
			{ctx: nil, repo: repo, hash: hash, failure: domain.ErrValidation},
			{ctx: context.Background(), repo: "invalid", hash: hash, failure: domain.ErrIntegrity},
			{ctx: context.Background(), repo: repo, hash: "", failure: domain.ErrIntegrity},
		} {
			source, err := open(request.ctx, request.repo, request.hash)
			if !errors.Is(err, request.failure) || !reflect.DeepEqual(source, Source{}) {
				test.Fatal("invalid or canceled source open reached storage or returned content", err)
			}
		}
	}
	var missing *Queries
	for _, queries := range []*Queries{missing, {}} {
		if source, err := queries.OpenSource(context.Background(), repo, hash); !errors.Is(err, domain.ErrValidation) || !reflect.DeepEqual(source, Source{}) {
			test.Fatal("unconfigured source owner did not fail closed", err)
		}
	}
}

func TestSourceReadAuthorizesBeforeMetadataWithoutAnotherSnapshot(test *testing.T) {
	queries, fixture := newReadOnlyQueries(test, false, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "authorized source")})
	metadata := &sourceReadSnapshots{reader: queries.metadata}
	queries.metadata = metadata
	queries.scope = sourceReadScope{Scope: queries.scope, test: test}
	withinSourceRead(test, queries, fixture, func(ctx context.Context) {
		for _, open := range []func(context.Context, domain.ContentHash, domain.ContentHash) (Source, error){queries.OpenSource, queries.OpenViewSource} {
			fixture.readFailure = domain.ErrForbidden
			if source, err := open(ctx, fixture.repo, fixture.hash); !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(source, Source{}) || metadata.calls != 0 {
				test.Fatal("new source entrypoint bypassed current authorization", err)
			}
			fixture.readFailure = nil
			snapshotCalls := 0
			bound := inbound.WithDocumentReadAuthorizer(ctx, func(context.Context, domain.ContentHash) (context.Context, error) {
				test.Fatal("source invoked a general authorizer inside the pinned snapshot")
				return nil, domain.ErrIntegrity
			})
			bound = inbound.WithDocumentSnapshotReadAuthorizer(bound, func(current context.Context, repo domain.ContentHash) (context.Context, error) {
				fixture.requireRead(current)
				snapshotCalls++
				if repo != fixture.repo {
					test.Fatal("snapshot authorization changed repository")
				}
				return current, domain.ErrForbidden
			})
			if source, err := open(bound, fixture.repo, fixture.hash); !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(source, Source{}) || metadata.calls != 0 || snapshotCalls != 1 {
				test.Fatal("source bypassed current snapshot authorization", err)
			}
			bound = inbound.WithDocumentSnapshotReadAuthorizer(bound, func(current context.Context, _ domain.ContentHash) (context.Context, error) {
				fixture.requireRead(current)
				return current, nil
			})
			if source, err := open(bound, fixture.repo, fixture.hash); err != nil || source.ctx != bound || metadata.calls != 1 {
				test.Fatal("authorized source changed caller scope", err)
			}
			metadata.calls = 0
		}
		if fixture.snapshots != 1 || fixture.admissions != 1 || fixture.chunks != 0 || fixture.preparations != 0 {
			test.Fatal("source authorization changed snapshot, admission or body-read boundaries")
		}
	})
}

func TestSourceReadPreservesMetadataAndGenerationErrors(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, stage := range []string{"metadata", "generation"} {
			for _, failure := range []error{domain.ErrIntegrity, domain.ErrForbidden, domain.ErrConflict, domain.ErrUnsupportedDocumentIdentity, context.Canceled, context.DeadlineExceeded} {
				test.Run(fmt.Sprintf("legacy=%t/%s/%v", legacy, stage, failure), func(test *testing.T) {
					queries, fixture := newReadOnlyQueries(test, legacy, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "source error")})
					metadata := &sourceReadSnapshots{reader: queries.metadata}
					queries.metadata = metadata
					if stage == "metadata" {
						metadata.failure = fmt.Errorf("snapshot: %w", failure)
					} else {
						fixture.generationFailure = fmt.Errorf("generation: %w", failure)
					}
					withinSourceRead(test, queries, fixture, func(ctx context.Context) {
						for _, open := range []func(context.Context, domain.ContentHash, domain.ContentHash) (Source, error){queries.OpenSource, queries.OpenViewSource} {
							source, err := open(ctx, fixture.repo, fixture.hash)
							if !errors.Is(err, failure) || !reflect.DeepEqual(source, Source{}) || fixture.pages != 0 || fixture.chunks != 0 || fixture.enqueued != 0 {
								test.Fatal("source error fell back, leaked content or prepared work", err)
							}
						}
					})
				})
			}
		}
	}
}

func TestSourceReadMissingSnapshotAndColdLegacyReadMetadataOnce(test *testing.T) {
	for _, prepared := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			test.Run(fmt.Sprintf("prepared=%t/missing=%t", prepared, missing), func(test *testing.T) {
				storage := newLegacyFindGenerationStore(test)
				doc := storage.seed(test, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "legacy source bytes")})
				metadata := &sourceReadSnapshots{reader: storage.FSStore}
				if missing {
					metadata.failure = domain.ErrNotFound
				}
				var generations outbound.DocRangeGenerationStore
				if prepared {
					generations = storage
				}
				queries := newLegacyFindGenerationQueries(test, metadata, storage, generations)
				queries.readOnly = true
				ctx := outbound.WithDocReadOnly(legacyFindGenerationContext())
				for _, open := range []func(context.Context, domain.ContentHash, domain.ContentHash) (Source, error){queries.OpenSource, queries.OpenViewSource} {
					metadata.calls, storage.cost = 0, legacyFindGenerationCosts{}
					source, err := open(ctx, storage.repo, doc.Hash)
					if err != nil || source.Root() || source.Total() != 1 || !reflect.DeepEqual(source.Envelope(), doc.CIR.Envelope) || metadata.calls != 1 {
						test.Fatal("legacy/missing-snapshot fallback changed or reread metadata", metadata.calls, err)
					}
					entry, err := source.Entry(0)
					if err != nil {
						test.Fatal(err)
					}
					if raw, err := source.Read(entry.Offset, entry.Length); err != nil || domain.HashContent(raw) != entry.Hash || metadata.calls != 1 {
						test.Fatal("legacy fallback changed canonical bytes or reread snapshot", metadata.calls, err)
					}
					if prepared && (source.Binding() != storage.fixtures[doc.Hash].binding || storage.cost.opens != 1 || storage.cost.pages != 1 || storage.cost.indexes != 0 || storage.cost.whole != 0) {
						test.Fatal("ready legacy source bypassed its selected generation", storage.cost)
					}
					if !prepared && (source.Binding() != (domain.DocReadGenerationBinding{}) || source.StreamBytes() != 0 || storage.cost.indexes != 1) {
						test.Fatal("cold legacy source changed existing fallback fields or repeated its index", storage.cost)
					}
					if storage.cost.writes != 0 || storage.cost.manifests != 1 || storage.cost.chunks != 1 {
						test.Fatal("legacy facade added preparation or request-wide byte scans", storage.cost)
					}
				}
			})
		}
	}
}

func TestSourceReadSnapshotMatchingAndCurrentBindingStayAuthoritative(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		test.Run(fmt.Sprintf("legacy=%t", legacy), func(test *testing.T) {
			queries, fixture := newReadOnlyQueries(test, legacy, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "bound source")})
			metadata := &sourceReadSnapshots{reader: queries.metadata}
			queries.metadata = metadata
			withinSourceRead(test, queries, fixture, func(ctx context.Context) {
				for _, damage := range []string{"snapshot id", "document hash", "identity"} {
					snapshot := domain.Snapshot{ID: fixture.hash, RepoID: fixture.repo, DocHash: fixture.hash, DocIdentity: fixture.identity}
					failure := domain.ErrIntegrity
					switch damage {
					case "snapshot id":
						snapshot.ID = domain.HashContent([]byte("wrong snapshot"))
					case "document hash":
						snapshot.DocHash = domain.HashContent([]byte("wrong document"))
					case "identity":
						snapshot.DocIdentity = "unsupported"
						failure = domain.ErrUnsupportedDocumentIdentity
					}
					metadata.snapshot = &snapshot
					for _, open := range []func(context.Context, domain.ContentHash, domain.ContentHash) (Source, error){queries.OpenSource, queries.OpenViewSource} {
						if source, err := open(ctx, fixture.repo, fixture.hash); !errors.Is(err, failure) || !reflect.DeepEqual(source, Source{}) || fixture.pages != 0 || fixture.chunks != 0 {
							test.Fatal("source bypassed snapshot matching", damage, err)
						}
					}
				}
				metadata.snapshot = nil
				first, err := queries.OpenSource(ctx, fixture.repo, fixture.hash)
				if err != nil {
					test.Fatal(err)
				}
				fixture.binding.SourceVersion++
				second, err := queries.OpenSource(ctx, fixture.repo, fixture.hash)
				if err != nil || first.Binding() == second.Binding() || second.Binding() != fixture.binding {
					test.Fatal("source reused a previous generation binding", err)
				}
				fixture.generationFailure = domain.ErrNotFound
				if source, err := queries.OpenSource(ctx, fixture.repo, fixture.hash); !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(source, Source{}) {
					test.Fatal("source bypassed current ownership after a warm read", err)
				}
			})
		})
	}
}

func TestSourceReadDerivedLegacyBytesKeepPinnedBinding(test *testing.T) {
	queries, fixture := newReadOnlyQueries(test, true, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleAssistant, "derived legacy bytes")})
	generation, objects, err := domain.BuildLegacyDerivedDocByteRangeGeneration(context.Background(), queries.index.(*readOnlyCoreReads).document)
	if err != nil {
		test.Fatal(err)
	}
	fixture.generation = generation
	for _, object := range objects {
		fixture.objects[object.Hash] = object.Bytes
	}
	raw, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture.binding.RangeGeneration = domain.HashContent(raw)
	derived := &sourceReadDerivedChunks{fixture: fixture}
	queries.view.DerivedChunks = derived
	withinSourceRead(test, queries, fixture, func(ctx context.Context) {
		source, err := queries.OpenSource(ctx, fixture.repo, fixture.hash)
		if err != nil || source.Root() || source.Binding() != fixture.binding || source.StreamBytes() != generation.StreamBytes {
			test.Fatal("derived legacy source changed binding or identity", err)
		}
		entry, err := source.Entry(0)
		if err != nil {
			test.Fatal(err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			raw, err := source.Read(entry.Offset, entry.Length)
			if err != nil || domain.HashContent(raw) != entry.Hash || derived.calls != 1 || fixture.chunks != 0 {
				test.Fatal("derived source changed bytes, reread a chunk or used original storage", err)
			}
		}
	})
}

func TestSourceReadInvalidAccessAndReaderErrorsFailClosed(test *testing.T) {
	calls := 0
	failure := fmt.Errorf("source read: %w", domain.ErrConflict)
	source := Source{ctx: context.Background(), source: docViewSource{total: 1,
		entry: func(int) (domain.DocRangeEntry, error) {
			calls++
			return domain.DocRangeEntry{}, failure
		},
		location: func(int) (domain.DocEventIndex, error) {
			calls++
			return domain.DocEventIndex{}, failure
		},
		read: func(int, int) ([]byte, error) {
			calls++
			return nil, failure
		},
	}}
	for _, position := range []int{-1, 1, int(^uint(0) >> 1)} {
		if entry, err := source.Entry(position); !errors.Is(err, domain.ErrValidation) || entry != (domain.DocRangeEntry{}) {
			test.Fatal("invalid source entry was accepted", position, err)
		}
		if location, err := source.Location(position); !errors.Is(err, domain.ErrValidation) || location != (domain.DocEventIndex{}) {
			test.Fatal("invalid source location was accepted", position, err)
		}
	}
	for _, request := range [][2]int{{-1, 1}, {0, 0}, {0, -1}} {
		if raw, err := source.Read(request[0], request[1]); !errors.Is(err, domain.ErrValidation) || raw != nil {
			test.Fatal("invalid source range was accepted", request, err)
		}
	}
	if calls != 0 {
		test.Fatal("invalid facade access reached a reader", calls)
	}
	assertSourceReadFailure(test, source, domain.ErrConflict)
	if calls != 3 {
		test.Fatal("valid access did not delegate exact existing reader errors", calls)
	}
	assertSourceReadFailure(test, Source{ctx: context.Background(), source: docViewSource{total: 1}}, domain.ErrValidation)
}

func TestSourceReadMalformedPagesAreNotCachedAndBodyErrorsDoNotFallback(test *testing.T) {
	for _, mutation := range []string{"short page", "empty page", "wrong position", "read failure"} {
		test.Run(mutation, func(test *testing.T) {
			storage := &legacyFindGenerationMalformedStore{legacyFindGenerationStore: newLegacyFindGenerationStore(test), mutation: mutation}
			doc := storage.seed(test, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "first"), legacyFindGenerationMessage(domain.RoleAssistant, "second")})
			queries := newLegacyFindGenerationQueries(test, storage.FSStore, storage.legacyFindGenerationStore, storage)
			ctx := outbound.WithDocReadOnly(legacyFindGenerationContext())
			source, err := queries.OpenSource(ctx, storage.repo, doc.Hash)
			if err != nil {
				test.Fatal(err)
			}
			if entry, err := source.Entry(0); !errors.Is(err, domain.ErrIntegrity) || entry != (domain.DocRangeEntry{}) {
				test.Fatal("source accepted a malformed metadata page", entry, err)
			}
			pages := storage.cost.pages
			storage.mutation = ""
			if location, err := source.Location(0); err != nil || location.Index != 0 || storage.cost.pages != pages+1 || storage.cost.manifests != 0 || storage.cost.chunks != 0 {
				test.Fatal("source cached a failed page or loaded original bytes for metadata", location, err)
			}
		})
	}
	for _, legacy := range []bool{false, true} {
		test.Run(fmt.Sprintf("corrupt-body/legacy=%t", legacy), func(test *testing.T) {
			queries, fixture := newReadOnlyQueries(test, legacy, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "original")})
			withinSourceRead(test, queries, fixture, func(ctx context.Context) {
				source, err := queries.OpenSource(ctx, fixture.repo, fixture.hash)
				if err != nil {
					test.Fatal(err)
				}
				entry, err := source.Entry(0)
				if err != nil {
					test.Fatal(err)
				}
				fixture.bodies[fixture.manifest.Chunks[0].Hash] = []byte("corrupt")
				if raw, err := source.Read(entry.Offset, entry.Length); !errors.Is(err, domain.ErrIntegrity) || raw != nil || fixture.preparations != 0 || fixture.enqueued != 0 {
					test.Fatal("source exposed corrupt bytes or changed the original integrity/fallback boundary", err)
				}
			})
		})
	}
}

func TestSourceReadSelectedMetadataCostDoesNotScanTheArchive(test *testing.T) {
	for _, count := range []int{256, 4096} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			storage := newLegacyFindGenerationStore(test)
			events := make([]domain.CIREvent, count)
			for position := range events {
				events[position] = legacyFindGenerationMessage(domain.RoleUser, fmt.Sprintf("source event %d", position))
			}
			doc := storage.seed(test, events)
			metadata := &sourceReadSnapshots{reader: storage.FSStore}
			queries := newLegacyFindGenerationQueries(test, metadata, storage, storage)
			ctx := outbound.WithDocReadOnly(legacyFindGenerationContext())
			source, err := queries.OpenSource(ctx, storage.repo, doc.Hash)
			if err != nil || metadata.calls != 1 || source.Total() != count {
				test.Fatal("prepared source could not open selected metadata", err)
			}
			entry, err := source.Entry(count - 1)
			if err != nil {
				test.Fatal(err)
			}
			if location, err := source.Location(count - 1); err != nil || location != entry.DocEventIndex {
				test.Fatal("selected source location changed", err)
			}
			cost := storage.cost
			if cost.opens != 1 || cost.pages != 1 || cost.directories > 2 || cost.metadataBytes > domain.MaxDocRangeGenerationBytes+domain.MaxDocRangePageBytes+2*domain.MaxDocRangeDirectoryBytes ||
				cost.manifests != 0 || cost.chunks != 0 || cost.indexes != 0 || cost.whole != 0 || cost.writes != 0 {
				test.Fatal("selected facade metadata scaled with full document bodies", cost)
			}
			if raw, err := source.Read(entry.Offset, entry.Length); err != nil || domain.HashContent(raw) != entry.Hash || metadata.calls != 1 ||
				storage.cost.manifests != 1 || storage.cost.chunks != 1 || storage.cost.indexes != 0 || storage.cost.whole != 0 || storage.cost.writes != 0 {
				test.Fatal("selected source bytes added request-wide scans", storage.cost, err)
			}
		})
	}
}
