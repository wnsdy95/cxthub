//go:build postgres

package app

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGLegacyViewColdPairFinishesWithoutAnotherForegroundDemand(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	base := seedLegacyFindPGDocument(test, fixture, fixture.cir.Events[:3])
	document := seedLegacyFindPGDocument(test, fixture, fixture.cir.Events[:4])
	observed := newLegacyFindGenerationPGStore(fixture)
	fixture.service.blobs = observed
	page, err := fixture.service.ReadDocViewEvents(fixture.ctx, fixture.repo, document.Hash, base.Hash, -1, 20, "chat")
	if !errors.Is(err, domain.ErrDocRangeIndexPending) || page.Hash != "" || observed.cost != (legacyFindGenerationCosts{}) {
		test.Fatal("cold pair read bodies or returned unfinished data", page, observed.cost, err)
	}
	var waiting int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1 AND state='waiting'`, fixture.repo).Scan(&waiting); err != nil || waiting != 3 {
		test.Fatal("first demand did not persist source, base, and boundary work", waiting, err)
	}
	peer, err := store.NewPostgresStore(fixture.ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	peer.ConfigureLegacyByteRangePublication(true)
	restarted := NewService(peer, peer, nil, nil, peer)
	if err := restarted.ProcessDocRangeJobs(fixture.ctx, 3); err != nil {
		test.Fatal("background preparation required another foreground request", err)
	}
	var complete int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1 AND state='completed'`, fixture.repo).Scan(&complete); err != nil || complete != 3 {
		test.Fatal("pair did not complete before the next foreground read", complete, err)
	}
	page, err = fixture.service.ReadDocViewEvents(fixture.ctx, fixture.repo, document.Hash, base.Hash, -1, 20, "chat")
	if err != nil || page.Inherited != 3 || page.Offset != 3 || page.Total != 4 || len(page.Events) != 1 || page.Events[0].Index != 3 || !reflect.DeepEqual(page.Events[0].Event, document.CIR.Events[3]) {
		test.Fatal("prepared pair lost exact inherited-prefix selection", page, err)
	}
}

func readPreparedDocViewReadyPG(test *testing.T, fixture docViewPGFixture, hash, base domain.ContentHash) domain.DocViewPage {
	test.Helper()
	for range 12 {
		page, err := fixture.service.ReadDocViewEvents(fixture.ctx, fixture.repo, hash, base, -1, 20, "chat")
		if err == nil {
			return page
		}
		if !errors.Is(err, domain.ErrDocRangeIndexPending) {
			test.Fatal("cold view lost durable preparation state", err)
		}
		if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
			test.Fatal("cold view worker failed", err)
		}
	}
	test.Fatal("cold view did not finish bounded preparation")
	return domain.DocViewPage{}
}

func TestPGLegacyViewColdDemandSurvivesRestartWithoutRequestTimeBackfill(test *testing.T) {
	for _, fragment := range []bool{false, true} {
		test.Run(fmt.Sprintf("fragment=%t", fragment), func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			events := make([]domain.CIREvent, 2048)
			for index := range events {
				events[index] = historyMessage(domain.RoleUser, fmt.Sprintf("cold archival row %d", index))
			}
			document := seedLegacyFindPGDocument(test, fixture, events)
			observed := newLegacyFindGenerationPGStore(fixture)
			fixture.service.blobs = observed
			image := legacyFindPGImage(test, fixture)
			readOnly := outbound.WithDocReadOnly(fixture.ctx)
			if _, err := fixture.service.ReadDocViewEvents(readOnly, fixture.repo, document.Hash, "", 1024, 20, "chat"); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
				test.Fatal("cold read-only body performed compatibility work", err)
			}
			if _, err := fixture.service.ReadDocViewFragment(readOnly, fixture.repo, document.Hash, 1024, 0, 64); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
				test.Fatal("cold read-only fragment performed compatibility work", err)
			}
			if after := legacyFindPGImage(test, fixture); after != image {
				test.Fatal("cold read-only demand changed persisted state")
			}
			before, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo)
			if err != nil {
				test.Fatal(err)
			}
			for range 2 {
				if fragment {
					_, err = fixture.service.ReadDocViewFragment(fixture.ctx, fixture.repo, document.Hash, 1024, 0, 64)
				} else {
					_, err = fixture.service.ReadDocViewEvents(fixture.ctx, fixture.repo, document.Hash, "", 1024, 20, "chat")
				}
				if !errors.Is(err, domain.ErrDocRangeIndexPending) {
					test.Fatal("cold request did not retain durable pending state", err)
				}
			}
			if observed.cost != (legacyFindGenerationCosts{}) {
				test.Fatal("cold body or fragment read full metadata or original bytes", observed.cost)
			}
			var queued int
			if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='waiting'`, fixture.repo, document.Hash).Scan(&queued); err != nil || queued != 1 {
				test.Fatal("duplicate demand did not retain one waiting job", queued, err)
			}
			peer, err := store.NewPostgresStore(fixture.ctx, collaborationDSN(test))
			if err != nil {
				test.Fatal(err)
			}
			defer peer.Close()
			peer.ConfigureLegacyByteRangePublication(true)
			restarted := NewService(peer, peer, nil, nil, peer)
			if err := restarted.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
				test.Fatal("restarted worker did not finish legacy preparation", err)
			}
			image = legacyFindPGImage(test, fixture)
			page, err := fixture.service.ReadDocViewEvents(readOnly, fixture.repo, document.Hash, "", 1024, 20, "chat")
			if err != nil || page.Hash != document.Hash || page.DocIdentity != domain.DocumentIdentityLegacy || page.Total != len(events) || page.Next != 1044 || len(page.Events) != 20 || !reflect.DeepEqual(page.Envelope, document.CIR.Envelope) {
				test.Fatal("prepared cold body lost original identity or selection", page, err)
			}
			for index, event := range page.Events {
				if event.Index != 1024+index || !reflect.DeepEqual(event.Event, document.CIR.Events[1024+index]) {
					test.Fatal("backfill changed archival event order", index)
				}
			}
			if observed.cost.opens != 1 || observed.cost.pages != 1 || observed.cost.indexes != 0 || observed.cost.whole != 0 || observed.cost.chunks == 0 {
				test.Fatal("prepared cold body returned to full-index compatibility", observed.cost)
			}
			observed.cost = legacyFindGenerationCosts{}
			detail, err := fixture.service.ReadDocViewFragment(readOnly, fixture.repo, document.Hash, 1024, 0, 64)
			raw := docViewLegacyGenerationEventBytes(test, document, 1024)
			if err != nil || detail.JSON != string(raw[:min(len(raw), 64)]) || detail.Total != len(raw) || observed.cost.indexes != 0 || observed.cost.whole != 0 || observed.cost.pages != 1 {
				test.Fatal("prepared cold fragment changed raw bytes or read whole metadata", detail, observed.cost, err)
			}
			if after := legacyFindPGImage(test, fixture); after != image {
				test.Fatal("prepared read-only demand mutated source or derived state")
			}
			if after, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo); err != nil || after != before {
				test.Fatal("derived preparation emitted repository activity", before, after, err)
			}
		})
	}
}

func TestPGLegacyViewColdPairPreparesDurableBoundary(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	shared := historyMessage(domain.RoleUser, "shared archival prefix")
	base := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{shared})
	document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{shared, historyMessage(domain.RoleAssistant, "new answer")})
	observed := newLegacyFindGenerationPGStore(fixture)
	fixture.service.blobs = observed
	page := readPreparedDocViewReadyPG(test, fixture, document.Hash, base.Hash)
	if page.Hash != document.Hash || page.DocIdentity != domain.DocumentIdentityLegacy || page.Inherited != 1 || page.Offset != 1 || len(page.Events) != 1 || !reflect.DeepEqual(page.Events[0].Event, document.CIR.Events[1]) {
		test.Fatal("cold pair did not converge to its exact inherited boundary", page)
	}
	if observed.cost.indexes != 0 || observed.cost.whole != 0 {
		test.Fatal("cold pair rebuilt whole metadata inside a request", observed.cost)
	}
	var jobs int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1 AND state='completed'`, fixture.repo).Scan(&jobs); err != nil || jobs != 3 {
		test.Fatal("cold pair did not persist two generations and one boundary", jobs, err)
	}
}

func TestPGLegacyViewLateGenerationRepairKeepsBoundaryWork(test *testing.T) {
	for _, legacySource := range []bool{false, true} {
		for _, legacyBase := range []bool{false, true} {
			test.Run(fmt.Sprintf("legacy_source=%t/legacy_base=%t", legacySource, legacyBase), func(test *testing.T) {
				fixture := newDocViewPGFixture(test)
				hash := fixture.hash
				if legacySource {
					hash = seedLegacyFindPGDocument(test, fixture, fixture.cir.Events).Hash
					prepareLegacyFindGenerationPG(test, fixture, hash)
				} else if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, hash); err != nil {
					test.Fatal(err)
				}
				previous := fixture.cir
				previous.Events = previous.Events[:123]
				var base domain.ContentHash
				if legacyBase {
					base = seedLegacyFindPGDocument(test, fixture, previous.Events).Hash
					prepareLegacyFindGenerationPG(test, fixture, base)
				} else {
					base = seedDocViewBoundaryPG(test, fixture, previous)
				}
				storage := &boundaryReceiptRaceStore{PostgresStore: fixture.storage, fixture: fixture, test: test, mode: "late-generation"}
				fixture.service.blobs = storage
				image := legacyFindPGImage(test, fixture)
				if _, err := fixture.service.ReadDocViewEvents(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, base, -1, 20, "chat"); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || storage.calls != 0 {
					test.Fatal("read-only missing boundary created work", err, storage.calls)
				}
				if after := legacyFindPGImage(test, fixture); after != image {
					test.Fatal("read-only missing boundary changed persisted state")
				}
				page, err := fixture.service.ReadDocViewEvents(fixture.ctx, fixture.repo, hash, base, -1, 20, "chat")
				if !errors.Is(err, domain.ErrDocRangeIndexPending) || page.Hash != "" || !storage.invalidated || !storage.rebuilt {
					test.Fatal("late-generation race did not preserve pending state", page, err, storage.invalidated, storage.rebuilt)
				}
				var active int
				if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3 AND state IN ('waiting','running','retrying')`, fixture.repo, hash, base).Scan(&active); err != nil || active != 1 || storage.calls != 2 {
					test.Fatal("late generation repair left no durable boundary work", active, storage.calls, err)
				}
				page = readPreparedDocViewReadyPG(test, fixture, hash, base)
				if page.Inherited != 123 || page.Offset != 123 || len(page.Events) != 20 || !reflect.DeepEqual(page.Events[0].Event, fixture.cir.Events[123]) {
					test.Fatal("late boundary repair changed original source selection", page)
				}
			})
		}
	}
}
