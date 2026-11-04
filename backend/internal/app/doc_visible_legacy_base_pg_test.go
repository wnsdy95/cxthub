//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func seedDocVisibleLegacyBasePG(test *testing.T, fixture docViewPGFixture, hidden int) (domain.ContentHash, domain.SessionDoc, domain.CIRDocument) {
	test.Helper()
	document := visiblePGDocument(fixture, hidden, 3)
	document.Events[3].Output = strings.Repeat("needle ", 45)
	base := seedLegacyFindPGDocument(test, fixture, document.Events[:2])
	return seedDocViewBoundaryPG(test, fixture, document), base, document
}

func readDocVisibleLegacyBaseReadyPG(test *testing.T, fixture docViewPGFixture, hash domain.ContentHash, request domain.DocVisiblePageRequest) domain.DocVisiblePage {
	test.Helper()
	for attempt := 0; attempt < 12; attempt++ {
		page, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, request)
		if err == nil {
			return page
		}
		if !errors.Is(err, domain.ErrDocRangeIndexPending) || !reflect.DeepEqual(page, domain.DocVisiblePage{}) {
			test.Fatal("legacy-base preparation exposed a partial selection", page, err)
		}
		if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
			test.Fatal("legacy-base preparation worker failed", err)
		}
	}
	test.Fatal("legacy-base selection did not become ready")
	return domain.DocVisiblePage{}
}

func docVisibleLegacyBaseJobImagePG(test *testing.T, fixture docViewPGFixture) string {
	test.Helper()
	var image string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT coalesce(jsonb_agg(jsonb_build_array(to_jsonb(job),job.xmin::text)
 ORDER BY job.doc_hash,job.base_hash),'[]'::jsonb)::text FROM doc_range_jobs_v1 job WHERE job.repo_id=$1`, fixture.repo).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return image
}

func TestPGDocVisibleLegacyBaseReplaysFoldsFindAndDeclaredIdentity(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	hash, base, document := seedDocVisibleLegacyBasePG(test, fixture, 139)
	request := domain.DocVisiblePageRequest{View: "chat", Base: base.Hash, Offset: -1, Limit: 2}
	page := readVisibleReplayReadyPG(test, fixture, hash, request)
	if page.Projection != domain.DocVisibleProjection || page.Hash != hash || page.Base != base.Hash ||
		page.Start != 2 || page.From != 2 || page.Inherited != 2 || page.Total != len(document.Events) || page.NextCursor == nil {
		test.Fatal("real Root selection lost its original legacy base", page)
	}
	before := legacyFindPGImage(test, fixture)
	assertVisibleReplayPG(test, fixture, page)
	continued := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{Cursor: *page.NextCursor})
	if continued.Base != base.Hash || continued.Generation != page.Generation || continued.Inherited != 2 || continued.From != page.To {
		test.Fatal("visible continuation changed the legacy-base selection", continued)
	}
	assertVisibleReplayPG(test, fixture, continued)
	fold := readVisibleReplayFoldPG(test, fixture, page)
	if fold.Base != base.Hash || fold.Generation != page.Generation || fold.Start != 2 || fold.Inherited != 2 || fold.NextCursor == nil {
		test.Fatal("fold lost the legacy-base binding or bounded continuation", fold)
	}
	assertVisibleReplayPG(test, fixture, fold)
	for _, event := range fold.Events {
		if event.Index < fold.From || event.Index >= fold.To || event.Event.Kind != document.Events[event.Index].Kind || event.Event.Seq != document.Events[event.Index].Seq ||
			!event.Deferred && !reflect.DeepEqual(event.Event, document.Events[event.Index]) {
			test.Fatal("fold changed the original Root event", event.Index)
		}
	}
	foldNext := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{Expand: *fold.NextCursor})
	if foldNext.From != fold.To || foldNext.Base != base.Hash || foldNext.Generation != page.Generation {
		test.Fatal("fold continuation escaped the selected base", foldNext)
	}
	assertVisibleReplayPG(test, fixture, foldNext)
	findRequest := domain.DocFindPageRequest{Selection: page.ReplayCursor, Query: "needle", From: page.Start, To: page.Total}
	var matches []domain.DocFindMatch
	for attempt := 0; attempt < 6; attempt++ {
		found, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, findRequest)
		if err != nil || found.Projection != domain.DocFindProjection || found.Hash != hash || found.Generation != page.Generation {
			test.Fatal("Root find rejected a genuine legacy base", found, err)
		}
		matches = append(matches, found.Matches...)
		for _, match := range found.Matches {
			if match.Index != 3 || match.RevealCursor == "" {
				test.Fatal("find changed original event coordinates", match)
			}
		}
		if len(found.Matches) > 0 {
			match := found.Matches[0]
			revealed, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{Cursor: match.RevealCursor})
			if err != nil || revealed.Base != base.Hash || revealed.Generation != page.Generation || revealed.Inherited != 2 ||
				revealed.View != "all" || revealed.From != match.Index || len(revealed.Units) != 1 || revealed.Units[0].Event == nil ||
				revealed.Units[0].Event.Index != match.Index || revealed.Units[0].Event.Event.Kind != document.Events[match.Index].Kind ||
				revealed.Units[0].Event.Event.Seq != document.Events[match.Index].Seq {
				test.Fatal("find reveal escaped its actual Root and legacy generations", revealed, err)
			}
			assertVisibleReplayPG(test, fixture, revealed)
		}
		if found.NextCursor == nil {
			break
		}
		if *found.NextCursor == findRequest.Cursor {
			test.Fatal("legacy-base find continuation did not advance")
		}
		findRequest = domain.DocFindPageRequest{Query: "needle", Cursor: *found.NextCursor}
	}
	if len(matches) != 45 {
		test.Fatal("find lost bounded matches with a legacy base", len(matches))
	}
	for position, match := range matches {
		if match.Offset != position*len("needle ") || match.Length != len("needle") || match.Raw {
			test.Fatal("find continuation changed decoded field coordinates", match)
		}
	}
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		for _, selected := range []struct {
			hash    domain.ContentHash
			version int
		}{{hash, 2}, {base.Hash, 4}} {
			generation, err := fixture.storage.OpenDocRangeGeneration(read, fixture.repo, selected.hash)
			if err != nil {
				return err
			}
			if generation.Descriptor().Version != selected.version || generation.Descriptor().Hash != selected.hash || generation.Binding().Validate() != nil {
				test.Fatal("selection used a relabeled or unbound range generation", generation.Descriptor(), generation.Binding())
			}
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	current, err := fixture.storage.GetDoc(fixture.ctx, fixture.repo, base.Hash)
	if err != nil || current.DocumentRef() != base.DocumentRef() {
		test.Fatal("legacy base was rewritten as a Root document", current.DocumentRef(), err)
	}
	canonical, err := domain.CanonicalBytes(current.CIR)
	if err != nil || domain.HashContent(canonical) != base.Hash {
		test.Fatal("legacy base lost its declared canonical hash", err)
	}
	snapshot, err := fixture.storage.GetSnapshot(fixture.ctx, fixture.repo, base.Hash)
	if err != nil || snapshot.DocIdentity != domain.DocumentIdentityLegacy || snapshot.DocHash != base.Hash || snapshot.ID != base.Hash {
		test.Fatal("legacy snapshot identity changed", snapshot, err)
	}
	if after := legacyFindPGImage(test, fixture); before != after {
		test.Fatal("warm legacy-base replay or find rewrote persisted source or projections")
	}
}

type docVisibleLegacyBasePGStore struct {
	*docViewPGStore
	test        *testing.T
	base        domain.ContentHash
	parentOpens int
	parentPages int
}

type docVisibleLegacyBasePGRead struct {
	outbound.DocRangeGenerationRead
	storage *docVisibleLegacyBasePGStore
	hash    domain.ContentHash
}

func (storage *docVisibleLegacyBasePGStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	read, err := storage.docViewPGStore.OpenDocRangeGeneration(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	if hash == storage.base {
		storage.parentOpens++
	}
	return &docVisibleLegacyBasePGRead{DocRangeGenerationRead: read, storage: storage, hash: hash}, nil
}

func (read *docVisibleLegacyBasePGRead) ReadPage(ctx context.Context, position int) ([]domain.DocRangeEntry, error) {
	if read.hash == read.storage.base {
		read.storage.parentPages++
	}
	return read.DocRangeGenerationRead.ReadPage(ctx, position)
}

func (storage *docVisibleLegacyBasePGStore) GetDoc(_ context.Context, _, hash domain.ContentHash) (domain.SessionDoc, error) {
	storage.test.Error("foreground materialized a complete document", hash)
	return domain.SessionDoc{}, domain.ErrIntegrity
}

func (storage *docVisibleLegacyBasePGStore) GetDocManifest(_ context.Context, _, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.test.Error("foreground loaded a complete archival manifest", hash)
	return domain.DocChunkManifest{}, domain.ErrIntegrity
}

func (storage *docVisibleLegacyBasePGStore) DocReadIndex(_ context.Context, _, hash domain.ContentHash) (domain.DocReadIndex, error) {
	storage.test.Error("foreground loaded a full legacy event index", hash)
	return domain.DocReadIndex{}, domain.ErrIntegrity
}

func (storage *docVisibleLegacyBasePGStore) DocRangeIndex(_ context.Context, _, hash domain.ContentHash) (domain.DocRangeIndex, error) {
	storage.test.Error("foreground loaded a full range index", hash)
	return domain.DocRangeIndex{}, domain.ErrIntegrity
}

func (storage *docVisibleLegacyBasePGStore) EnsureDocRangeIndex(_ context.Context, _, hash domain.ContentHash) error {
	storage.test.Error("foreground built a document range index", hash)
	return domain.ErrIntegrity
}

func TestPGDocVisibleLegacyBaseColdPreparationIsDurableBoundedAndWarmReadOnly(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	shared := domain.DocRangePrefixBatchPages*domain.DocRangePageEvents + 37
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope}
	for position := 0; position < shared+40; position++ {
		event := historyMessage(domain.RoleUser, "resumable legacy-base prefix "+string(fixture.repo))
		event.Seq = position
		document.Events = append(document.Events, event)
	}
	base := seedLegacyFindPGDocument(test, fixture, document.Events[:shared])
	hash := seedDocViewBoundaryPG(test, fixture, document)
	observed := &docVisibleLegacyBasePGStore{docViewPGStore: &docViewPGStore{PostgresStore: fixture.storage}, test: test, base: base.Hash}
	fixture.service.blobs = observed
	request := domain.DocVisiblePageRequest{View: "chat", Base: base.Hash, Offset: -1, Limit: 2}
	before, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	jobs := docVisibleLegacyBaseJobImagePG(test, fixture)
	if page, err := fixture.service.ReadDocVisiblePage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || !reflect.DeepEqual(page, domain.DocVisiblePage{}) {
		test.Fatal("read-only cold legacy base was not unavailable", page, err)
	}
	if jobs != docVisibleLegacyBaseJobImagePG(test, fixture) {
		test.Fatal("read-only cold base enqueued work")
	}
	if page, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocRangeIndexPending) || !reflect.DeepEqual(page, domain.DocVisiblePage{}) || observed.pages != 0 {
		test.Fatal("cold request scanned or exposed legacy-base history", page, err, observed.pages)
	}
	var state string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, fixture.repo, base.Hash).Scan(&state); err != nil || state != "waiting" {
		test.Fatal("cold legacy range request was not durable", state, err)
	}
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("missing legacy-base prefix was computed in the foreground", err)
	}
	if observed.pages != 0 || observed.parentPages != 0 {
		test.Fatal("foreground request replayed the parent prefix", observed.pages, observed.parentPages)
	}
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	var prefix int
	var complete bool
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT events,complete FROM doc_range_boundaries_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, fixture.repo, hash, base.Hash).Scan(&prefix, &complete); err != nil || prefix != domain.DocRangePrefixBatchPages*domain.DocRangePageEvents || complete {
		test.Fatal("legacy prefix exceeded its durable batch budget", prefix, complete, err)
	}
	if page, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocRangeIndexPending) || !reflect.DeepEqual(page, domain.DocVisiblePage{}) {
		test.Fatal("partial legacy prefix escaped as completed inheritance", page, err)
	}
	fixture.service = NewService(fixture.storage, observed, nil, nil, fixture.storage)
	page := readDocVisibleLegacyBaseReadyPG(test, fixture, hash, request)
	if page.Base != base.Hash || page.Start != shared || page.Inherited != shared || page.From != shared || len(page.Units) != 2 {
		test.Fatal("restarted service lost the durable legacy prefix", page)
	}
	jobs = docVisibleLegacyBaseJobImagePG(test, fixture)
	for attempt := 0; attempt < 3; attempt++ {
		warm, err := fixture.service.ReadDocVisiblePage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
		if err != nil || !reflect.DeepEqual(warm, page) {
			test.Fatal("warm read-only legacy-base selection changed", warm, err)
		}
		assertVisibleReplayPG(test, fixture, warm)
		if warm.Units[0].Event == nil || warm.Units[0].Event.Index != shared || !reflect.DeepEqual(warm.Units[0].Event.Event, document.Events[shared]) {
			test.Fatal("first visible event did not follow the prepared legacy prefix", warm)
		}
	}
	if observed.parentOpens == 0 || observed.parentPages != 0 || observed.fallbackPages != 0 || observed.pages > 12 {
		test.Fatal("warm work replayed full parent history or escaped pinned ranges", observed.parentOpens, observed.parentPages, observed.pages, observed.fallbackPages)
	}
	if jobs != docVisibleLegacyBaseJobImagePG(test, fixture) {
		test.Fatal("warm reads rewrote preparation receipts")
	}
	after, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo)
	if err != nil || before != after {
		test.Fatal("derived legacy prefix changed repository facts", before, after, err)
	}
}

func TestPGDocVisibleLegacyBaseWarmCostDoesNotScaleWithParentHistory(test *testing.T) {
	for _, count := range []int{1000, 10000} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: make([]domain.CIREvent, count+20)}
			for position := range document.Events {
				document.Events[position] = historyMessage(domain.RoleUser, "archived parent and selected recent source")
				document.Events[position].Seq = position
			}
			base := seedLegacyFindPGDocument(test, fixture, document.Events[:count])
			hash := seedDocViewBoundaryPG(test, fixture, document)
			observed := &docVisibleLegacyBasePGStore{docViewPGStore: &docViewPGStore{PostgresStore: fixture.storage}, test: test, base: base.Hash}
			fixture.service.blobs = observed
			request := domain.DocVisiblePageRequest{View: "chat", Base: base.Hash, Offset: -1, Limit: 2}
			page := readDocVisibleLegacyBaseReadyPG(test, fixture, hash, request)
			observed.parentOpens, observed.parentPages, observed.pages, observed.fallbackPages = 0, 0, 0, 0
			for range 3 {
				warm, err := fixture.service.ReadDocVisiblePage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
				if err != nil || !reflect.DeepEqual(warm, page) || warm.From != count || warm.Inherited != count {
					test.Fatal("warm range changed after legacy prefix preparation", warm, err)
				}
				found, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, domain.DocFindPageRequest{
					Selection: page.ReplayCursor, Query: "absent needle", From: page.From, To: page.To})
				if err != nil || found.Generation != page.Generation || found.NextCursor != nil || len(found.Matches) != 0 {
					test.Fatal("warm find did not retain the exact legacy-base range", found, err)
				}
			}
			if observed.parentOpens != 6 || observed.parentPages != 0 || observed.fallbackPages != 0 || observed.pages > 12 {
				test.Fatal("warm work grew with parent history", count, observed.parentOpens, observed.parentPages, observed.pages, observed.fallbackPages)
			}
			test.Logf("parent_events=%d reads=3 finds=3 parent_opens=%d parent_pages=%d source_pages=%d full_index_fallbacks=%d",
				count, observed.parentOpens, observed.parentPages, observed.pages, observed.fallbackPages)
		})
	}
}

func TestPGDocVisibleLegacyBaseWorkerProtocolFence(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	hash, base, _ := seedDocVisibleLegacyBasePG(test, fixture, 4)
	request := domain.DocVisiblePageRequest{View: "chat", Base: base.Hash, Offset: -1, Limit: 2}
	assertOldWorkerClaim := func(doc, parent domain.ContentHash, allowed bool) {
		test.Helper()
		transaction, err := fixture.pool.Begin(fixture.ctx)
		if err != nil {
			test.Fatal(err)
		}
		defer transaction.Rollback(fixture.ctx)
		tag, err := transaction.Exec(fixture.ctx, `UPDATE doc_range_jobs_v1 SET state='running',version=version+1,
 lease_until=clock_timestamp()+interval '1 minute' WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, fixture.repo, doc, parent)
		var postgresError *pgconn.PgError
		if allowed && (err != nil || tag.RowsAffected() != 1) || !allowed && (!errors.As(err, &postgresError) || postgresError.Code != "0A000") {
			test.Fatal("old worker crossed its supported job contract", allowed, err)
		}
	}
	if _, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal(err)
	}
	assertOldWorkerClaim(base.Hash, "", false)
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		test.Fatal("current worker could not prepare legacy metadata", err)
	}
	if _, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal(err)
	}
	assertOldWorkerClaim(hash, base.Hash, false)
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		test.Fatal("current worker could not prepare a mixed prefix", err)
	}
	if _, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, request); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.EnqueueDocRangeJob(fixture.ctx, fixture.repo, fixture.hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	assertOldWorkerClaim(fixture.hash, "", true)
}

func TestPGDocVisibleLegacyBaseEmptyAndFullyInheritedSelections(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		source    int
		inherited int
	}{{"empty base", 2, 0}, {"fully inherited", 2, 2}, {"empty pair", 0, 0}} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: fixture.cir.Events[:scenario.source]}
			base := seedLegacyFindPGDocument(test, fixture, document.Events[:scenario.inherited])
			hash := seedDocViewBoundaryPG(test, fixture, document)
			request := domain.DocVisiblePageRequest{View: "chat", Base: base.Hash, Offset: -1, Limit: 2}
			jobs := docVisibleLegacyBaseJobImagePG(test, fixture)
			if page, err := fixture.service.ReadDocVisiblePage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || !reflect.DeepEqual(page, domain.DocVisiblePage{}) {
				test.Fatal("empty legacy base skipped cold range preparation", page, err)
			}
			if jobs != docVisibleLegacyBaseJobImagePG(test, fixture) {
				test.Fatal("read-only empty selection enqueued a job")
			}
			page := readVisibleReplayReadyPG(test, fixture, hash, request)
			if page.Hash != hash || page.Base != base.Hash || page.Inherited != scenario.inherited || page.Start != scenario.inherited || page.From != scenario.inherited || page.Total != scenario.source || page.Generation == "" {
				test.Fatal("empty selection lost its real legacy binding", page)
			}
			if scenario.source == scenario.inherited && (page.To != scenario.source || len(page.Units) != 0 || len(page.Events) != 0 || page.NextCursor != nil) {
				test.Fatal("fully inherited selection exposed rows", page)
			}
			assertVisibleReplayPG(test, fixture, page)
			warm, err := fixture.service.ReadDocVisiblePage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
			if err != nil || !reflect.DeepEqual(warm, page) {
				test.Fatal("warm read-only empty selection failed", warm, err)
			}
			if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
				generation, err := fixture.storage.OpenDocRangeGeneration(read, fixture.repo, base.Hash)
				if err != nil {
					return err
				}
				version := 4
				if scenario.inherited == 0 {
					version = 3
				}
				if generation.Descriptor().Version != version || generation.Descriptor().Hash != base.Hash || generation.Descriptor().EventCount != int64(scenario.inherited) || generation.Binding().Validate() != nil {
					test.Fatal("zero-row selection bypassed a real legacy range binding", generation.Descriptor(), generation.Binding())
				}
				return nil
			}); err != nil {
				test.Fatal(err)
			}
			calls := 0
			denied := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
				calls++
				return bound, domain.ErrForbidden
			})
			if deniedPage, err := fixture.service.ReadDocVisiblePage(denied, fixture.repo, hash, visibleReplayRequest(page)); !errors.Is(err, domain.ErrForbidden) || calls != 1 || !reflect.DeepEqual(deniedPage, domain.DocVisiblePage{}) {
				test.Fatal("zero-row replay bypassed authorization", deniedPage, calls, err)
			}
			if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, base.Hash); err != nil {
				test.Fatal(err)
			}
			if revoked, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, visibleReplayRequest(page)); !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(revoked, domain.DocVisiblePage{}) {
				test.Fatal("zero-row replay bypassed legacy-base ownership", revoked, err)
			}
		})
	}
}

func TestPGDocVisibleLegacyBaseOldSelectionsRejectOwnershipAndGenerationChanges(test *testing.T) {
	for _, changed := range []string{"source", "base"} {
		for _, mutation := range []string{"ownership", "version", "generation recreation"} {
			test.Run(changed+"/"+mutation, func(test *testing.T) {
				fixture := newDocViewPGFixture(test)
				hash, base, _ := seedDocVisibleLegacyBasePG(test, fixture, 6)
				request := domain.DocVisiblePageRequest{View: "chat", Base: base.Hash, Offset: -1, Limit: 2}
				page := readVisibleReplayReadyPG(test, fixture, hash, request)
				fold := readVisibleReplayFoldPG(test, fixture, page)
				initial := domain.DocFindPageRequest{Selection: page.ReplayCursor, Query: "needle", From: page.Start, To: page.Total}
				found, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, initial)
				if err != nil || found.NextCursor == nil || len(found.Matches) != domain.DocFindPageMatches || page.NextCursor == nil {
					test.Fatal("missing real continuation fixtures", page, found, err)
				}
				visibleRequests := []domain.DocVisiblePageRequest{
					visibleReplayRequest(page), visibleReplayRequest(fold),
					{Cursor: *page.NextCursor}, {Cursor: found.Matches[0].RevealCursor},
				}
				findRequests := []domain.DocFindPageRequest{initial, {Query: "needle", Cursor: *found.NextCursor}}
				updated := hash
				if changed == "base" {
					updated = base.Hash
				}
				want := domain.ErrDocVisibleGenerationStale
				switch mutation {
				case "ownership":
					want = domain.ErrNotFound
					_, err = fixture.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, updated)
				case "version":
					_, err = fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, updated)
				case "generation recreation":
					_, err = fixture.pool.Exec(fixture.ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, updated)
				}
				if err != nil {
					test.Fatal(err)
				}
				if mutation != "ownership" {
					fresh := readDocVisibleLegacyBaseReadyPG(test, fixture, hash, request)
					if fresh.Generation == page.Generation || fresh.ReplayCursor == page.ReplayCursor || fresh.Inherited != page.Inherited {
						test.Fatal("fresh selection reused a previous source incarnation", fresh)
					}
					assertVisibleReplayPG(test, fixture, fresh)
				}
				for _, replay := range visibleRequests {
					out, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, replay)
					if !errors.Is(err, want) || !reflect.DeepEqual(out, domain.DocVisiblePage{}) {
						test.Fatal("old page, fold, continuation or reveal survived a binding change", out, err)
					}
				}
				for _, replay := range findRequests {
					out, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, replay)
					if !errors.Is(err, want) || !reflect.DeepEqual(out, domain.DocFindPage{}) {
						test.Fatal("old find selection survived a binding change", out, err)
					}
				}
			})
		}
	}
}

func TestPGDocVisibleLegacyBaseMissingCompletedPreparationRecovers(test *testing.T) {
	for _, missing := range []string{"prefix", "base generation", "source generation"} {
		test.Run(missing, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			hash, base, _ := seedDocVisibleLegacyBasePG(test, fixture, 6)
			request := domain.DocVisiblePageRequest{View: "chat", Base: base.Hash, Offset: -1, Limit: 2}
			original := readVisibleReplayReadyPG(test, fixture, hash, request)
			var version int64
			var state string
			if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state,version FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, fixture.repo, hash, base.Hash).Scan(&state, &version); err != nil || state != "completed" {
				test.Fatal("missing completed prefix fixture", state, version, err)
			}
			var err error
			if missing == "prefix" {
				_, err = fixture.pool.Exec(fixture.ctx, `DELETE FROM doc_range_boundaries_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, fixture.repo, hash, base.Hash)
			} else {
				selected := base.Hash
				if missing == "source generation" {
					selected = hash
				}
				_, err = fixture.pool.Exec(fixture.ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, selected)
			}
			if err != nil {
				test.Fatal(err)
			}
			jobs := docVisibleLegacyBaseJobImagePG(test, fixture)
			if page, err := fixture.service.ReadDocVisiblePage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || !reflect.DeepEqual(page, domain.DocVisiblePage{}) {
				test.Fatal("read-only lookup trusted missing completed preparation", page, err)
			}
			if jobs != docVisibleLegacyBaseJobImagePG(test, fixture) {
				test.Fatal("read-only recovery enqueued work")
			}
			if page, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocRangeIndexPending) || !reflect.DeepEqual(page, domain.DocVisiblePage{}) {
				test.Fatal("missing completed preparation did not requeue", page, err)
			}
			recovered := readDocVisibleLegacyBaseReadyPG(test, fixture, hash, request)
			if recovered.Base != base.Hash || recovered.Start != original.Start || recovered.Inherited != original.Inherited || recovered.To != original.To || len(recovered.Units) != len(original.Units) {
				test.Fatal("recovery changed original source selection", recovered)
			}
			assertVisibleReplayPG(test, fixture, recovered)
			if missing == "prefix" {
				var rebuiltVersion int64
				if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state,version FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, fixture.repo, hash, base.Hash).Scan(&state, &rebuiltVersion); err != nil || state != "completed" || rebuiltVersion <= version || recovered.Generation != original.Generation {
					test.Fatal("completed receipt hid a missing prefix", state, version, rebuiltVersion, err)
				}
				if !reflect.DeepEqual(recovered, original) {
					test.Fatal("prefix-only repair changed the original replay", recovered)
				}
				assertVisibleReplayPG(test, fixture, original)
			} else if recovered.Generation == original.Generation {
				test.Fatal("generation recovery reused an old replay incarnation")
			}
		})
	}
}

func TestPGDocVisibleLegacyBaseMalformedSelectedPrefixFailsClosed(test *testing.T) {
	for _, malformed := range []string{"prefix exceeds base", "invalid descriptor hash"} {
		test.Run(malformed, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			hash, base, _ := seedDocVisibleLegacyBasePG(test, fixture, 6)
			request := domain.DocVisiblePageRequest{View: "chat", Base: base.Hash, Offset: -1, Limit: 2}
			page := readVisibleReplayReadyPG(test, fixture, hash, request)
			var err error
			if malformed == "prefix exceeds base" {
				_, err = fixture.pool.Exec(fixture.ctx, `UPDATE doc_range_boundaries_v1 SET events=3 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, fixture.repo, hash, base.Hash)
			} else {
				_, err = fixture.pool.Exec(fixture.ctx, `UPDATE doc_range_boundaries_v1 SET base_generation_hash='malformed' WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, fixture.repo, hash, base.Hash)
			}
			if err != nil {
				test.Fatal(err)
			}
			jobs := docVisibleLegacyBaseJobImagePG(test, fixture)
			for _, selected := range []domain.DocVisiblePageRequest{request, visibleReplayRequest(page)} {
				out, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, selected)
				if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(out, domain.DocVisiblePage{}) {
					test.Fatal("malformed selected prefix returned inheritance or rows", out, err)
				}
			}
			found, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, domain.DocFindPageRequest{Selection: page.ReplayCursor, Query: "needle", From: page.Start, To: page.Total})
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(found, domain.DocFindPage{}) {
				test.Fatal("find accepted malformed selected prefix metadata", found, err)
			}
			if jobs != docVisibleLegacyBaseJobImagePG(test, fixture) {
				test.Fatal("malformed prefix was silently replaced or requeued")
			}
		})
	}
}
