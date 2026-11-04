//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docDisplaySnapshotFactsPG struct {
	*store.PostgresStore
	reads   int
	failure error
}

func (storage *docDisplaySnapshotFactsPG) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	storage.reads++
	if storage.failure != nil {
		return domain.Snapshot{}, storage.failure
	}
	return storage.PostgresStore.GetSnapshot(ctx, repo, hash)
}

func TestPGDocDisplaySourceUsesOneSnapshotFact(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		test.Run(fmt.Sprintf("legacy=%t", legacy), func(test *testing.T) {
			test.Setenv("CXT_TEST_DSN", a11IsolatedPGDSN(test, context.Background()))
			fixture := newDocDisplayPGFixture(test)
			event := domain.CIREvent{Kind: domain.EventToolResult, CallID: "single-metadata-fact", Output: "typed source🙂"}
			var hash domain.ContentHash
			if legacy {
				hash = seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{event}).Hash
				prepareLegacyFindGenerationPG(test, fixture, hash)
			} else {
				hash = seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}})
			}
			target := docDisplayTargetPG(test, fixture, hash, 0)
			metadata := &docDisplaySnapshotFactsPG{PostgresStore: fixture.storage}
			fixture.service.meta = metadata
			for _, change := range []string{"valid", "binding", "event", "metadata_failure"} {
				_, err := repositoryReadForRepo(fixture.ctx, fixture.service, fixture.repo, func(read context.Context) (bool, error) {
					current := target
					var want error
					switch change {
					case "binding":
						current.Binding.SourceVersion++
						want = domain.ErrConflict
					case "event":
						current.Event.Hash = domain.HashContent([]byte("another event"))
						want = domain.ErrIntegrity
					case "metadata_failure":
						metadata.failure = errors.New("snapshot lookup interrupted")
						want = metadata.failure
					}
					metadata.reads = 0
					source, err := fixture.service.docDisplaySource(read, current)
					if !errors.Is(err, want) || metadata.reads != 1 {
						test.Fatal("source reread/ignored the captured snapshot fact", change, metadata.reads, err, want)
					}
					metadata.failure = nil
					if want == nil {
						if source.binding != target.Binding {
							test.Fatal("source changed its code-generation binding")
						}
						raw, err := source.read(target.Event.Offset, target.Event.Length)
						if err != nil || domain.HashContent(raw) != target.Event.Hash {
							test.Fatal("metadata reuse changed the exact event bytes", err)
						}
					}
					return true, nil
				})
				if err != nil {
					test.Fatal(err)
				}
			}
			if legacy {
				if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`, fixture.repo, hash); err != nil {
					test.Fatal(err)
				}
				_, err := repositoryReadForRepo(fixture.ctx, fixture.service, fixture.repo, func(read context.Context) (bool, error) {
					metadata.reads = 0
					source, err := fixture.service.docDisplaySource(read, target)
					if err != nil || metadata.reads != 1 || source.binding != target.Binding {
						test.Fatal("registered legacy range lost its supported missing-metadata path", metadata.reads, err)
					}
					return true, nil
				})
				if err != nil {
					test.Fatal(err)
				}
			}
		})
	}
}
