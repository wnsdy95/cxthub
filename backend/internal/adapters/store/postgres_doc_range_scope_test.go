//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docRangeScopeTracePG struct {
	docRangeGenerationTracer
	isolation  atomic.Int64
	identities atomic.Int64
}

func (trace *docRangeScopeTracePG) TraceQueryStart(ctx context.Context, connection *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(query.SQL, "current_setting('transaction_isolation')") {
		trace.isolation.Add(1)
	}
	if query.SQL == docRangeJobIdentitySQL {
		trace.identities.Add(1)
	}
	return trace.docRangeGenerationTracer.TraceQueryStart(ctx, connection, query)
}

func (trace *docRangeScopeTracePG) facts() [2]int64 {
	return [2]int64{trace.descriptors.Load(), trace.objects.Load()}
}

func newDocRangeScopeFixturePG(test *testing.T) (docDisplayPGFixture, *PostgresStore, *docRangeScopeTracePG) {
	test.Helper()
	fixture := docDisplayPG(test)
	trace := &docRangeScopeTracePG{}
	reader := docRangeBoundaryPGTracedStore(test, fixture.storage, fixture.ctx, trace)
	return fixture, reader, trace
}

func TestDocRangeScopePGRepeatedOpenSharesVerifiedFacts(test *testing.T) {
	fixture, storage, trace := newDocRangeScopeFixturePG(test)
	if err := storage.WithinReadSnapshot(fixture.ctx, func(bound context.Context) error {
		opened, err := storage.OpenDocRangeGeneration(bound, fixture.repo, fixture.doc.hash)
		if err != nil {
			return err
		}
		descriptor, binding := opened.Descriptor(), opened.Binding()
		entries, err := opened.ReadPage(bound, 0)
		if err != nil {
			return err
		}
		expected := slices.Clone(entries)
		cold := trace.facts()
		if cold[0] != 1 || cold[1] < 1 || cold[1] > docRangeGenerationCacheObjects {
			test.Fatal("cold scope did not fetch one bounded generation/page proof", cold)
		}
		for range 16 {
			changed := opened.Descriptor()
			changed.Envelope[0] = '!'
			changed.EventPages.Hash = domain.HashContent([]byte("caller mutation"))
			entries[0].Hash, entries[0].Kind, entries[0].ToolName = "changed", domain.EventReasoning, "changed"
			again, err := storage.OpenDocRangeGeneration(bound, fixture.repo, fixture.doc.hash)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(again.Descriptor(), descriptor) || again.Binding() != binding {
				test.Fatal("shared scope exposed mutable descriptor or binding values")
			}
			entries, err = again.ReadPage(bound, 0)
			if err != nil || !reflect.DeepEqual(entries, expected) {
				test.Fatal("shared reader reused caller-mutated page entries", entries, err)
			}
			entry, err := storage.proveDocDisplayEntry(bound, fixture.targets[0], false)
			if err != nil || entry != expected[0] {
				test.Fatal("display target did not share the verified range fact", entry, err)
			}
		}
		for _, change := range []string{"binding", "event", "kind"} {
			target := fixture.targets[0]
			switch change {
			case "binding":
				target.Binding.SourceVersion++
			case "event":
				target.Event.Hash = domain.HashContent([]byte("different event"))
			case "kind":
				target = fixture.targets[2]
			}
			if _, err := storage.proveDocDisplayEntry(bound, target, false); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("warm range bypassed display target validation", change, err)
			}
		}
		if trace.facts() != cold || trace.forbidden.Load() != 0 || trace.identities.Load() < 16 || trace.isolation.Load() != 1 {
			test.Fatal("warm opens/proofs repeated facts or omitted SQL identity/liveness checks", cold, trace.facts(), trace.forbidden.Load(), trace.identities.Load(), trace.isolation.Load())
		}
		if cached := bound.Value(repositoryTxKey{}).(*repositoryTx).docRange; cached == nil || len(cached.objects) > docRangeGenerationCacheObjects {
			test.Fatal("snapshot range sharing was absent or exceeded its object bound")
		}
		test.Logf("replays=16 descriptor_queries=%d object_queries=%d isolation_queries=%d identity_queries=%d", cold[0], cold[1], trace.isolation.Load(), trace.identities.Load())
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestDocRangeScopeDescriptorIdentityCopies(test *testing.T) {
	identity := domain.DocumentIdentityLegacy
	reader := &postgresDocRangeGenerationRead{generation: domain.DocRangeGeneration{SourceIdentity: &identity, Envelope: []byte(`{"source":"original"}`)}}
	expected := reader.Descriptor()
	changed := reader.Descriptor()
	*changed.SourceIdentity = domain.DocumentIdentityRootV1
	changed.Envelope[0] = '!'
	if !reflect.DeepEqual(reader.Descriptor(), expected) || identity != domain.DocumentIdentityLegacy {
		test.Fatal("shared descriptor leaked its identity pointer or envelope bytes")
	}
}

func TestDocRangeScopePGRepoHashStoreSeparation(test *testing.T) {
	fixture, storage, trace := newDocRangeScopeFixturePG(test)
	otherRepo, _ := rootPGRepo(test, fixture.ctx, fixture.storage)
	unowned, _ := rootPGRepo(test, fixture.ctx, fixture.storage)
	seedRootPG(test, fixture.ctx, fixture.storage, otherRepo, fixture.doc)
	otherDoc := rangeFixture(test, 129)
	seedRootPG(test, fixture.ctx, fixture.storage, fixture.repo, otherDoc)
	if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, otherDoc.hash); err != nil {
		test.Fatal(err)
	}
	peer := &PostgresStore{pool: storage.pool}
	if err := storage.WithinReadSnapshot(fixture.ctx, func(bound context.Context) error {
		original, err := storage.OpenDocRangeGeneration(bound, fixture.repo, fixture.doc.hash)
		if err != nil {
			return err
		}
		want, err := original.ReadPage(bound, 0)
		if err != nil {
			return err
		}
		if read, err := peer.OpenDocRangeGeneration(bound, fixture.repo, fixture.doc.hash); !errors.Is(err, domain.ErrConflict) || read != nil {
			test.Fatal("same-pool foreign store reused an owned reader", read, err)
		}
		beforeMissing := trace.descriptors.Load()
		for range 2 {
			if read, err := storage.OpenDocRangeGeneration(bound, unowned, fixture.doc.hash); !errors.Is(err, domain.ErrNotFound) || read != nil {
				test.Fatal("warm hash bypassed repository membership", read, err)
			}
		}
		if trace.descriptors.Load() != beforeMissing+2 {
			test.Fatal("missing-membership errors were cached instead of rechecked")
		}
		for _, request := range []struct {
			repo, hash domain.ContentHash
			events     int64
		}{{otherRepo, fixture.doc.hash, 3}, {fixture.repo, otherDoc.hash, 129}, {fixture.repo, fixture.doc.hash, 3}} {
			before := trace.facts()
			read, err := storage.OpenDocRangeGeneration(bound, request.repo, request.hash)
			if err != nil {
				return err
			}
			if read.Descriptor().Hash != request.hash || read.Descriptor().EventCount != request.events {
				test.Fatal("scope reused a different repository/document descriptor", request, read.Descriptor())
			}
			if _, err := read.ReadPage(bound, 0); err != nil {
				return err
			}
			after := trace.facts()
			if after[0] != before[0]+1 || after[1] <= before[1] {
				test.Fatal("repo/hash transition reused another scope or retained multiple current readers", request, before, after)
			}
		}
		before := trace.facts()
		actual, err := original.ReadPage(bound, 0)
		if err != nil || !reflect.DeepEqual(actual, want) || trace.facts() != before {
			test.Fatal("switching the current slot poisoned an existing scoped reader", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestDocRangeScopePGFreshSnapshotsRevalidate(test *testing.T) {
	for _, change := range []string{"membership", "source", "generation", "missing_generation", "page", "identity"} {
		test.Run(change, func(test *testing.T) {
			fixture, storage, trace := newDocRangeScopeFixturePG(test)
			if err := storage.WithinReadSnapshot(fixture.ctx, func(bound context.Context) error {
				opened, err := storage.OpenDocRangeGeneration(bound, fixture.repo, fixture.doc.hash)
				if err != nil {
					return err
				}
				entries, err := opened.ReadPage(bound, 0)
				if err != nil {
					return err
				}
				descriptor, binding := opened.Descriptor(), opened.Binding()
				page, err := opened.PageHash(bound, 0)
				if err != nil {
					return err
				}
				var wantOpen error
				switch change {
				case "membership":
					_, err = fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, fixture.doc.hash)
					wantOpen = domain.ErrNotFound
				case "source":
					_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.doc.hash)
					wantOpen = domain.ErrDocRangeIndexUnavailable
				case "generation":
					_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_range_generations_v2 SET descriptor=$2 WHERE doc_hash=$1`, fixture.doc.hash, []byte(`{}`))
					wantOpen = domain.ErrIntegrity
				case "missing_generation":
					_, err = fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.doc.hash)
					wantOpen = domain.ErrDocRangeIndexUnavailable
				case "page":
					_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_range_objects_v2 SET bytes=$3 WHERE doc_hash=$1 AND hash=$2`, fixture.doc.hash, page, []byte(`[]`))
				case "identity":
					docRangeJobPGDeclareLegacy(test, fixture.storage, fixture.ctx, fixture.repo, fixture.doc.hash)
				}
				if err != nil {
					return err
				}
				before := trace.facts()
				again, err := storage.OpenDocRangeGeneration(bound, fixture.repo, fixture.doc.hash)
				if err != nil || again.Binding() != binding || !reflect.DeepEqual(again.Descriptor(), descriptor) {
					test.Fatal("concurrent mutation changed a pinned range binding", change, err)
				}
				actual, err := again.ReadPage(bound, 0)
				if err != nil || !reflect.DeepEqual(actual, entries) || trace.facts() != before {
					test.Fatal("pinned reopen lost its verified object fact", change, err, before, trace.facts())
				}
				return storage.WithinReadSnapshot(fixture.ctx, func(fresh context.Context) error {
					before := trace.facts()
					var priorObjects int64
					for attempt := range 2 {
						read, err := storage.OpenDocRangeGeneration(fresh, fixture.repo, fixture.doc.hash)
						if wantOpen != nil {
							if !errors.Is(err, wantOpen) || read != nil {
								test.Fatal("fresh scope reused revoked/corrupt generation evidence", change, read, err, wantOpen)
							}
							continue
						}
						if err != nil {
							return err
						}
						if change == "identity" {
							_, err = storage.proveDocDisplayEntry(fresh, fixture.targets[0], false)
						} else {
							_, err = read.ReadPage(fresh, 0)
						}
						if !errors.Is(err, domain.ErrIntegrity) {
							test.Fatal("fresh scope skipped identity or selected-object proof", change, err)
						}
						if change == "page" && attempt > 0 && trace.objects.Load() <= priorObjects {
							test.Fatal("invalid selected object was cached")
						}
						priorObjects = trace.objects.Load()
					}
					wantDescriptors := int64(1)
					if wantOpen != nil {
						wantDescriptors = 2
					}
					if trace.descriptors.Load() != before[0]+wantDescriptors {
						test.Fatal("fresh scope skipped generation validation or cached an error", change, before, trace.facts())
					}
					return nil
				})
			}); err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestDocRangeScopePGTransactionGuards(test *testing.T) {
	fixture, storage, _ := newDocRangeScopeFixturePG(test)
	if read, err := storage.OpenDocRangeGeneration(fixture.ctx, fixture.repo, fixture.doc.hash); !errors.Is(err, domain.ErrConflict) || read != nil {
		test.Fatal("unbound reader opened a range scope", read, err)
	}
	if err := storage.WithinRepository(fixture.ctx, fixture.repo, func(write context.Context) error {
		if read, err := storage.OpenDocRangeGeneration(write, fixture.repo, fixture.doc.hash); !errors.Is(err, domain.ErrConflict) || read != nil {
			test.Fatal("write transaction opened a shared range scope", read, err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	var retained outbound.DocRangeGenerationRead
	var retainedContext context.Context
	if err := storage.WithinReadSnapshot(fixture.ctx, func(bound context.Context) error {
		var err error
		retained, err = storage.OpenDocRangeGeneration(bound, fixture.repo, fixture.doc.hash)
		if err != nil {
			return err
		}
		if _, err := retained.ReadPage(bound, 0); err != nil {
			return err
		}
		retainedContext = bound
		canceled, cancel := context.WithCancel(bound)
		cancel()
		if read, err := storage.OpenDocRangeGeneration(canceled, fixture.repo, fixture.doc.hash); !errors.Is(err, context.Canceled) || read != nil {
			test.Fatal("warm Open ignored cancellation", read, err)
		}
		if _, err := retained.ReadPage(canceled, 0); !errors.Is(err, context.Canceled) {
			test.Fatal("warm page ignored cancellation", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if retainedContext.Value(repositoryTxKey{}).(*repositoryTx).docRange != nil {
		test.Fatal("closed snapshot retained its current range cache")
	}
	if read, err := storage.OpenDocRangeGeneration(retainedContext, fixture.repo, fixture.doc.hash); !errors.Is(err, pgx.ErrTxClosed) || read != nil {
		test.Fatal("closed snapshot reopened a cached reader", read, err)
	}
	if _, err := retained.ReadPage(retainedContext, 0); !errors.Is(err, pgx.ErrTxClosed) {
		test.Fatal("closed snapshot served a cached page", err)
	}
	if err := storage.WithinReadSnapshot(fixture.ctx, func(fresh context.Context) error {
		if _, err := retained.ReadPage(fresh, 0); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("retained reader crossed independent snapshots", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	for _, options := range []pgx.TxOptions{
		{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly},
		{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadWrite},
	} {
		transaction, err := storage.pool.BeginTx(fixture.ctx, options)
		if err != nil {
			test.Fatal(err)
		}
		bound := &repositoryTx{Tx: transaction, owner: storage, readOnly: true}
		bound.docRange = &postgresDocRangeGenerationRead{transaction: bound, repo: fixture.repo, generation: retained.Descriptor(), binding: retained.Binding()}
		fake := context.WithValue(fixture.ctx, repositoryTxKey{}, bound)
		read, err := storage.OpenDocRangeGeneration(fake, fixture.repo, fixture.doc.hash)
		rollbackPG(transaction)
		if !errors.Is(err, domain.ErrConflict) || read != nil {
			test.Fatal("warm fake scope bypassed actual isolation/read-only SQL", options, read, err)
		}
	}
}

func TestDocRangeScopePGAbortedOpenFailsClosed(test *testing.T) {
	fixture, storage, trace := newDocRangeScopeFixturePG(test)
	aborted := errors.New("expected aborted read scope")
	err := storage.WithinReadSnapshot(fixture.ctx, func(bound context.Context) error {
		opened, err := storage.OpenDocRangeGeneration(bound, fixture.repo, fixture.doc.hash)
		if err != nil {
			return err
		}
		if _, err := opened.ReadPage(bound, 0); err != nil {
			return err
		}
		before, checks := trace.facts(), trace.isolation.Load()
		_, err = storage.db(bound).Exec(bound, `SELECT 1/0`)
		var division *pgconn.PgError
		if !errors.As(err, &division) || division.Code != "22012" {
			test.Fatal("scope was not deliberately aborted", err)
		}
		if entries, err := opened.ReadPage(bound, 0); err == nil || entries != nil {
			test.Fatal("aborted transaction served a cached range page", entries, err)
		}
		read, err := storage.OpenDocRangeGeneration(bound, fixture.repo, fixture.doc.hash)
		var rejected *pgconn.PgError
		if !errors.As(err, &rejected) || rejected.Code != "25P02" || read != nil {
			test.Fatal("aborted transaction returned its cached range reader", read, err)
		}
		if trace.facts() != before || trace.isolation.Load() != checks+1 {
			test.Fatal("aborted warm Open omitted liveness SQL or fetched fresh facts", before, trace.facts())
		}
		return aborted
	})
	if !errors.Is(err, aborted) {
		test.Fatal(err)
	}
}

func TestDocRangeScopePGBorrowedAbortedPageFailsClosed(test *testing.T) {
	for _, scope := range []string{"direct", "nested_snapshot"} {
		test.Run(scope, func(test *testing.T) {
			fixture, storage, _ := newDocRangeScopeFixturePG(test)
			transaction, err := storage.pool.BeginTx(fixture.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(transaction)
			bound := context.WithValue(fixture.ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, readOnly: true})
			verify := func(read context.Context) error {
				opened, err := storage.OpenDocRangeGeneration(read, fixture.repo, fixture.doc.hash)
				if err != nil {
					return err
				}
				if _, err := opened.ReadPage(read, 0); err != nil {
					return err
				}
				_, err = storage.db(read).Exec(read, `SELECT 1/0`)
				var division *pgconn.PgError
				if !errors.As(err, &division) || division.Code != "22012" {
					test.Fatal("borrowed scope was not deliberately aborted", err)
				}
				entries, err := opened.ReadPage(read, 0)
				var rejected *pgconn.PgError
				if !errors.As(err, &rejected) || rejected.Code != "25P02" || entries != nil {
					test.Fatal("borrowed aborted transaction served a cached page", entries, err)
				}
				return nil
			}
			if scope == "nested_snapshot" {
				err = storage.WithinReadSnapshot(bound, verify)
			} else {
				err = verify(bound)
			}
			if err != nil {
				test.Fatal(err)
			}
		})
	}
}
