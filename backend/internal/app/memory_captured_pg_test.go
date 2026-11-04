//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type capturedMemoryPagePG struct {
	request domain.EffectiveMemoryRequest
	page    domain.EffectiveMemoryPage
}

func capturedMemoryPagesPG(test *testing.T, service *Service, repo domain.ContentHash, captured *CapturedEffectiveMemory, request domain.EffectiveMemoryRequest) []capturedMemoryPagePG {
	test.Helper()
	ctx := systemTestContext()
	var pages []capturedMemoryPagePG
	var expectedItems []domain.EffectiveMemoryItem
	for scheduleIndex, limits := range [][]int{{1}, {2}, {50}, {1, 3, 2, 0, 50}} {
		request.Cursor = ""
		var items []domain.EffectiveMemoryItem
		seen := map[string]bool{}
		for pageIndex := 0; ; pageIndex++ {
			request.Limit = limits[pageIndex%len(limits)]
			want, err := service.QueryEffectiveMemory(ctx, repo, request)
			if err != nil {
				test.Fatalf("live page %d, limits %v: %v", pageIndex, limits, err)
			}
			got, err := captured.Project(ctx, request)
			if err != nil || !reflect.DeepEqual(got, want) {
				test.Fatalf("full frozen/live page mismatch, limits %v, page %d: %v\ngot %+v\nwant %+v", limits, pageIndex, err, got, want)
			}
			if want.StateHash == "" || want.LineageHash == "" || want.DeliveryStateHash == "" {
				test.Fatalf("page omitted continuation, lineage, or complete delivery proof: %+v", want)
			}
			pages = append(pages, capturedMemoryPagePG{request: request, page: want})
			items = append(items, want.Items...)
			if want.NextCursor == "" {
				if len(items) != want.Total {
					test.Fatalf("pagination lost items: got %d, total %d", len(items), want.Total)
				}
				break
			}
			if len(want.Items) == 0 || seen[want.NextCursor] || len(items) >= want.Total {
				test.Fatal("pagination did not make bounded progress")
			}
			seen[want.NextCursor] = true
			request.Cursor = got.NextCursor
		}
		if scheduleIndex == 0 {
			expectedItems = items
		} else if !reflect.DeepEqual(items, expectedItems) {
			test.Fatalf("item order or assessment depends on page limits %v", limits)
		}
	}
	if len(pages) == 0 || pages[0].page.Total < 3 || pages[0].page.NextCursor == "" {
		test.Fatal("fixture must exercise off-page items and continuation cursors")
	}
	return pages
}

func assertFrozenMemoryPagesPG(test *testing.T, captured *CapturedEffectiveMemory, pages []capturedMemoryPagePG) {
	test.Helper()
	for _, expected := range pages {
		got, err := captured.Project(systemTestContext(), expected.request)
		if err != nil || !reflect.DeepEqual(got, expected.page) {
			test.Fatalf("completed capture changed for limit %d, cursor %q: %v\ngot %+v\nwant %+v", expected.request.Limit, expected.request.Cursor, err, got, expected.page)
		}
	}
}

func assertCapturedMemoryDependenciesPG(test *testing.T, captured *CapturedEffectiveMemory, hashes ...domain.ContentHash) {
	test.Helper()
	slices.Sort(hashes)
	want := make([]CapturedMemoryDependency, 0, len(hashes))
	for _, hash := range hashes {
		want = append(want, CapturedMemoryDependency{Hash: hash, Available: true})
	}
	got := captured.Dependencies()
	if !reflect.DeepEqual(got, want) {
		test.Fatalf("capture did not retain exactly the demanded objects: got %+v, want %+v", got, want)
	}
	if len(got) > 0 {
		got[0].Available = false
		if !reflect.DeepEqual(captured.Dependencies(), want) {
			test.Fatal("dependency accessor exposed mutable captured state")
		}
	}
}

func capturedMemoryDigestPG(snapshot domain.ContentHash, label string) domain.MemoryDigest {
	return domain.MemoryDigest{SnapshotID: snapshot, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{
		SourceSnapshot: snapshot, Summary: label + " summary", KeyFacts: []string{label + " fact"}, OpenTasks: []string{label + " task"},
		Claims: []domain.MemoryClaim{
			{Kind: "rationale", Text: label + " rationale"},
			{Kind: "decision", Text: label + " decision"},
			{Kind: "code", Text: label + " code", Code: &domain.MemoryCodeScope{Commit: effectiveOID(1), Paths: []string{"file"}}},
		},
	}}}
}

func TestPGCapturedMemoryStoredAndLineageParity(test *testing.T) {
	service, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	connection, err := pgxpool.New(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Close()
	parent := collaborationSnapshot(test, storage, repo, "captured parent")
	snapshot := collaborationSnapshot(test, storage, repo, "captured child", parent)
	unrelated := collaborationSnapshot(test, storage, repo, "unrelated corrupt memory")
	parentMemory, err := service.PutMemoryDigestCAS(ctx, repo, capturedMemoryDigestPG(parent, "parent"))
	if err != nil {
		test.Fatal(err)
	}
	memory, err := service.PutMemoryDigestCAS(ctx, repo, capturedMemoryDigestPG(snapshot, "selected"))
	if err != nil {
		test.Fatal(err)
	}
	unrelatedMemory, err := service.PutMemoryDigestCAS(ctx, repo, capturedMemoryDigestPG(unrelated, "unrelated"))
	if err != nil {
		test.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, unrelatedMemory, []byte(`{"corrupt":true}`)); err != nil {
		test.Fatal(err)
	}
	before, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	type savedCapture struct {
		captured *CapturedEffectiveMemory
		pages    []capturedMemoryPagePG
	}
	var saved []savedCapture
	for _, mode := range []string{"stored", "lineage"} {
		for _, content := range []string{"all", "claims", "prompt"} {
			test.Run(mode+"/"+content, func(test *testing.T) {
				request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, CodeCommit: effectiveOID(1)}, Content: content, Limit: 1}
				dependencies := []domain.ContentHash{parentMemory, memory}
				if mode == "stored" {
					request.Selection.MemoryHash = memory
					dependencies = []domain.ContentHash{memory}
				}
				captured, err := service.CaptureEffectiveMemorySource(ctx, repo, request, nil)
				if err != nil {
					test.Fatal("capture", err)
				}
				if captured.Generation() != before {
					test.Fatalf("capture changed source markers: got %+v, want %+v", captured.Generation(), before)
				}
				assertCapturedMemoryDependenciesPG(test, captured, dependencies...)
				pages := capturedMemoryPagesPG(test, service, repo, captured, request)
				restored := roundTripMemoryCapturePG(test, storage, collaborationDSN(test), captured)
				assertFrozenMemoryPagesPG(test, restored, pages)
				if mode == "stored" && pages[0].page.LineageHash != memory {
					test.Fatal("stored selection was replaced by derived lineage")
				}
				saved = append(saved, savedCapture{captured: captured, pages: pages})
			})
		}
	}
	if len(saved) != 6 {
		test.Fatal("not all selection/content captures completed")
	}
	if after, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo); err != nil || after != before {
		test.Fatal("capture or projection wrote repository state", err)
	}
	updated := capturedMemoryDigestPG(snapshot, "replacement")
	updated.PreviousMemoryHash = memory
	if _, err := service.PutMemoryDigestCAS(ctx, repo, updated); err != nil {
		test.Fatal(err)
	}
	for _, entry := range saved {
		request, previous := entry.pages[0].request, entry.pages[0].page
		current, err := service.QueryEffectiveMemory(ctx, repo, request)
		if err != nil || current.StateHash == previous.StateHash {
			test.Fatal("live query missed committed attachment generation", err)
		}
		if request.Selection.MemoryHash == "" && (current.LineageHash == previous.LineageHash || current.DeliveryStateHash == previous.DeliveryStateHash) {
			test.Fatal("live lineage retained replaced attachment knowledge")
		}
		if request.Selection.MemoryHash != "" && (current.LineageHash != previous.LineageHash || !reflect.DeepEqual(current.Items, previous.Items) || current.DeliveryStateHash != previous.DeliveryStateHash) {
			test.Fatal("stored original followed the mutable latest attachment")
		}
		request.Cursor = previous.NextCursor
		if _, err := service.QueryEffectiveMemory(ctx, repo, request); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
			test.Fatal("live continuation crossed source generations", err)
		}
		assertFrozenMemoryPagesPG(test, entry.captured, entry.pages)
	}
	if _, err := connection.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='memory'`, repo, memory); err != nil {
		test.Fatal(err)
	}
	for _, entry := range saved {
		request := entry.pages[0].request
		_, err := service.QueryEffectiveMemory(ctx, repo, request)
		if request.Selection.MemoryHash != "" && !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("live stored read ignored deleted ownership", err)
		}
		if request.Selection.MemoryHash == "" && err != nil {
			test.Fatal("live lineage read an obsolete attachment", err)
		}
	}
	storage.Close()
	for _, entry := range saved {
		assertFrozenMemoryPagesPG(test, entry.captured, entry.pages)
		if entry.captured.Generation() != before {
			test.Fatal("completed capture's generation followed live mutations")
		}
	}
}

func TestPGCapturedMemoryBranchUsesGitAndPRPinsNotLatestAttachment(test *testing.T) {
	fixture, connection := graphSourceQueryFixturePG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	worker, repo, storage := fixture.basis.worker, fixture.basis.repo, fixture.storage.PostgresStore
	pr := domain.PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: fixture.admission.OriginalHead, MergeSHA: fixture.proof.Contribution.IntegrationSHA}
	if _, err := worker.core.SubmitPRPromotion(ctx, repo.ID, pr); err != nil {
		test.Fatal(err)
	}
	remote := &githubHistoryRemote{GitHubApp: worker.remote, pages: map[string]domain.GitHubPRHistoryPage{"": {
		RepositoryID: 91, HeadRepositoryID: 91, PullRequestID: "PR_7", PullRequestDatabaseID: 701, Number: 7,
		BaseBranch: pr.BaseBranch, HeadBranch: pr.HeadBranch, HeadSHA: pr.HeadSHA, MergeSHA: pr.MergeSHA, Merged: true, MergedAt: time.Unix(300, 0).UTC(),
		Rewrites: []domain.GitHubHeadRewrite{{ID: "original-head-restored", Before: strings.Repeat("6", 40), After: pr.HeadSHA, CreatedAt: time.Unix(200, 0).UTC()}},
	}}}
	if err := worker.reconcilePRHistory(ctx, remote, fixture.storage, historyWork(test, worker, fixture.storage, repo)); err != nil {
		test.Fatal(err)
	}
	view := fixture.query(test)
	fixture.assertPinnedMemory(test, view.Inclusion, true)
	source, err := storage.GetSnapshot(ctx, repo.ID, fixture.admission.Capture)
	if err != nil || source.MemoryHash == "" || source.MemoryHash == fixture.publication.MemoryHash {
		test.Fatal("fixture needs a distinct latest source attachment", err)
	}
	prPins, err := storage.ListPRContributionMemories(ctx, repo.ID)
	if err != nil || len(prPins) != 1 || prPins[0].MemoryHash != fixture.publication.MemoryHash {
		test.Fatal("fixture needs the immutable PR pin", err)
	}
	gitPins, err := storage.ListGitContributionMemories(ctx, repo.ID)
	if err != nil || len(gitPins) != 1 || gitPins[0].MemoryHash != fixture.publication.MemoryHash {
		test.Fatal("fixture needs the immutable Git pin", err)
	}
	previous, err := CaptureGraphSource(ctx, storage, repo.ID, nil)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, _, err := previous.prepareInputs(ctx); err != nil {
		test.Fatal("retain validated PR metadata", err)
	}
	memory, err := worker.core.PutMemoryDigestCAS(ctx, repo.ID, capturedMemoryDigestPG(fixture.main.Target, "destination"))
	if err != nil {
		test.Fatal(err)
	}
	assertReadOnly := fixture.assertReadOnly(test)
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: fixture.main.Target, CodeCommit: view.Inclusion.CodeCommit}, Content: "claims", Limit: 1}
	captured, err := worker.core.CaptureEffectiveMemorySource(ctx, repo.ID, request, &previous)
	if err != nil {
		test.Fatal("incremental memory capture", err)
	}
	if captured.Generation().Graph.Sequence <= previous.Checkpoint().Sequence {
		test.Fatal("capture did not incorporate the attachment delta")
	}
	assertCapturedMemoryDependenciesPG(test, captured, memory, fixture.publication.MemoryHash)
	pages := capturedMemoryPagesPG(test, worker.core, repo.ID, captured, request)
	fixture.assertMerge(test, pages[0].page.Inclusion, "included", true)
	fullRequest := request
	fullRequest.Limit = 50
	full, err := captured.Project(ctx, fullRequest)
	if err != nil {
		test.Fatal(err)
	}
	pinnedCount := 0
	for _, item := range full.Items {
		if item.Text == "unpublished mutable pointer must stay invisible" {
			test.Fatal("branch memory followed the latest source attachment")
		}
		if item.Text == "original immutable pin" {
			pinnedCount++
			if item.SourceSnapshot != fixture.admission.Capture || item.Code == nil || item.Code.Commit != fixture.admission.OriginalHead {
				test.Fatal("captured pin lost original provenance")
			}
		}
	}
	if pinnedCount != 1 {
		test.Fatalf("mixed Git/PR pins duplicated or omitted knowledge: %d", pinnedCount)
	}
	baseline, err := worker.core.CaptureEffectiveMemorySource(ctx, repo.ID, request, nil)
	if err != nil || baseline.Generation() != captured.Generation() || !reflect.DeepEqual(baseline.Dependencies(), captured.Dependencies()) {
		test.Fatal("baseline and incremental memory captures disagree", err)
	}
	assertFrozenMemoryPagesPG(test, baseline, pages)
	assertReadOnly()
	restored := roundTripMemoryCapturePG(test, storage, fixture.basis.dsn, captured)
	assertFrozenMemoryPagesPG(test, restored, pages)
	if result, err := connection.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='memory'`, repo.ID, source.MemoryHash); err != nil || result.RowsAffected() != 1 {
		test.Fatal("delete unused latest attachment ownership", err)
	}
	current, err := worker.core.QueryEffectiveMemory(ctx, repo.ID, fullRequest)
	if err != nil || current.StateHash == full.StateHash || current.LineageHash != full.LineageHash || current.DeliveryStateHash != full.DeliveryStateHash || !reflect.DeepEqual(current.Items, full.Items) {
		test.Fatal("memory-only generation change did not preserve immutable branch knowledge", err)
	}
	request.Cursor = pages[0].page.NextCursor
	if _, err := worker.core.QueryEffectiveMemory(ctx, repo.ID, request); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
		test.Fatal("v3 continuation crossed a memory-only generation change", err)
	}
	request.Cursor = ""
	for _, statement := range []string{
		`DELETE FROM git_contribution_memories WHERE repo_id=$1`,
		`DELETE FROM pr_contribution_memories WHERE repo_id=$1`,
	} {
		result, err := connection.Exec(ctx, statement, repo.ID)
		if err != nil || result.RowsAffected() != 1 {
			test.Fatal("delete captured contribution pin", err)
		}
	}
	if _, err := worker.core.QueryEffectiveMemory(ctx, repo.ID, fullRequest); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("live branch read did not fail closed on removed contribution pins", err)
	}
	if partial, err := worker.core.CaptureEffectiveMemorySource(ctx, repo.ID, request, nil); !errors.Is(err, domain.ErrConflict) || partial != nil {
		test.Fatal("fresh capture exposed incomplete contribution memory", err)
	}
	generation, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo.ID)
	if err != nil || generation.Graph != captured.Generation().Graph || generation.Sequence <= captured.Generation().Sequence {
		test.Fatal("pin deletion did not advance the independent memory generation", err)
	}
	storage.Close()
	assertFrozenMemoryPagesPG(test, captured, pages)
	assertFrozenMemoryPagesPG(test, baseline, pages)
	assertFrozenMemoryPagesPG(test, restored, pages)
}

func TestPGCapturedMemoryMissingCausalParentIsNotAnUnknownDependency(test *testing.T) {
	fixture, connection := graphSourceQueryFixturePG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	service, repo, storage := fixture.basis.worker.core, fixture.basis.repo.ID, fixture.storage.PostgresStore
	publication := publishGitSupplement(test, fixture)
	if err := fixture.scans.ProcessContributions(ctx, 8); err != nil {
		test.Fatal(err)
	}
	pins, err := storage.ListGitContributionMemories(ctx, repo)
	if err != nil || len(pins) != 2 || !slices.ContainsFunc(pins, func(pin domain.GitContributionMemory) bool {
		return pin.PublicationID == publication.ID && pin.MemoryHash == publication.MemoryHash
	}) {
		test.Fatal("fixture needs the accepted causal supplement pin", err)
	}
	source, err := storage.GetSnapshot(ctx, repo, fixture.admission.Capture)
	if err != nil {
		test.Fatal(err)
	}
	continued := collaborationSnapshot(test, storage, repo, "independent source ancestry", fixture.main.Target, fixture.admission.Capture)
	fixture.selectMain(test, continued, strings.Repeat("e", 40), strings.Repeat("0", 32))
	memory, err := service.PutMemoryDigestCAS(ctx, repo, capturedMemoryDigestPG(continued, "independent destination"))
	if err != nil {
		test.Fatal(err)
	}
	unknown, err := storage.PutMemory(ctx, repo, capturedMemoryDigestPG(continued, "undemanded stored object"))
	if err != nil {
		test.Fatal(err)
	}
	parent := fixture.publication.MemoryHash
	if result, err := connection.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='memory'`, repo, parent); err != nil || result.RowsAffected() != 1 {
		test.Fatal("remove requested causal parent ownership", err)
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: continued, CodeCommit: strings.Repeat("e", 40)}, Content: "claims", Limit: 1}
	captured, err := service.CaptureEffectiveMemorySource(ctx, repo, request, nil)
	if err != nil {
		test.Fatal("capture independent lineage with an unavailable causal pin parent", err)
	}
	want := []CapturedMemoryDependency{
		{Hash: parent, Available: false},
		{Hash: publication.MemoryHash, Available: true},
		{Hash: source.MemoryHash, Available: true},
		{Hash: memory, Available: true},
	}
	slices.SortFunc(want, func(left, right CapturedMemoryDependency) int {
		return strings.Compare(string(left.Hash), string(right.Hash))
	})
	if got := captured.Dependencies(); !reflect.DeepEqual(got, want) {
		test.Fatalf("requested missing parent was not retained separately from undemanded memory: got %+v, want %+v", got, want)
	}
	witnesses, err := captured.ObjectWitnesses(ctx)
	if err != nil || len(witnesses) != len(want) || !slices.ContainsFunc(witnesses, func(witness domain.MemoryObjectWitness) bool {
		return witness.Hash == parent && !witness.Available && witness.Root == nil
	}) {
		test.Fatal("absent causal parent did not retain a current-source witness", err)
	}
	if err := service.ValidateCapturedMemorySource(ctx, repo, captured); err != nil {
		test.Fatal("observed absent dependency was not current", err)
	}
	pages := capturedMemoryPagesPG(test, service, repo, captured, request)
	restored := roundTripMemoryCapturePG(test, storage, fixture.basis.dsn, captured)
	assertFrozenMemoryPagesPG(test, restored, pages)
	if _, err := connection.Exec(ctx, `INSERT INTO repo_blobs(repo_id,hash,kind) VALUES($1,$2,'memory')`, repo, parent); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.GetMemory(ctx, repo, parent); err != nil {
		test.Fatal("causal parent did not become live again", err)
	}
	if err := service.ValidateCapturedMemorySource(ctx, repo, captured); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("newly available dependency did not invalidate capture", err)
	}
	current, err := service.QueryEffectiveMemory(ctx, repo, request)
	if err != nil || current.StateHash == pages[0].page.StateHash {
		test.Fatal("live read missed restored causal parent ownership", err)
	}
	storage.Close()
	if _, err := captured.reader.GetMemory(ctx, repo, parent); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("captured missing dependency was not frozen as unavailable", err)
	}
	if _, err := captured.reader.GetMemory(ctx, repo, unknown); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("unknown dependency was mistaken for a captured absence", err)
	}
	if !reflect.DeepEqual(captured.Dependencies(), want) {
		test.Fatal("restoration or unknown lookup changed captured dependency availability")
	}
	assertFrozenMemoryPagesPG(test, captured, pages)
	assertFrozenMemoryPagesPG(test, restored, pages)
}

type capturedMemoryGenerationPG struct {
	*store.PostgresStore
	afterFirst func(context.Context) error
	reads      int
}

func (storage *capturedMemoryGenerationPG) ReadRepositoryMemorySourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryMemorySourceGeneration, error) {
	if !storage.InReadOnlyTransaction(ctx) {
		return domain.RepositoryMemorySourceGeneration{}, domain.ErrConflict
	}
	generation, err := storage.PostgresStore.ReadRepositoryMemorySourceGeneration(ctx, repo)
	storage.reads++
	if err == nil && storage.reads == 1 && storage.afterFirst != nil {
		err = storage.afterFirst(ctx)
	}
	return generation, err
}

func TestPGCapturedMemoryInterleavedWriteUsesOneOwnedReadSnapshot(test *testing.T) {
	service, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	connection, err := pgxpool.New(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Close()
	snapshot := collaborationSnapshot(test, storage, repo, "interleaved memory capture")
	memory, err := service.PutMemoryDigestCAS(ctx, repo, capturedMemoryDigestPG(snapshot, "before"))
	if err != nil {
		test.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, CodeCommit: effectiveOID(1)}, Limit: 1}
	before, err := service.CaptureEffectiveMemorySource(ctx, repo, request, nil)
	if err != nil {
		test.Fatal(err)
	}
	pages := capturedMemoryPagesPG(test, service, repo, before, request)
	observed := &capturedMemoryGenerationPG{PostgresStore: storage, afterFirst: func(bound context.Context) error {
		if !storage.InReadOnlyTransaction(bound) || storage.InTransaction(ctx) {
			test.Fatal("capture did not own a distinct read-only transaction")
		}
		updated := capturedMemoryDigestPG(snapshot, "after")
		updated.PreviousMemoryHash = memory
		if _, err := service.PutMemoryDigestCAS(ctx, repo, updated); err != nil {
			return err
		}
		if _, err := connection.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, memory, []byte(`{"corrupt":true}`)); err != nil {
			return err
		}
		_, err := connection.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='memory'`, repo, memory)
		return err
	}}
	capturer := NewService(observed, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	captured, err := capturer.CaptureEffectiveMemorySource(ctx, repo, request, nil)
	if err != nil {
		test.Fatal("interleaved capture", err)
	}
	if observed.reads < 2 || captured.Generation() != before.Generation() {
		test.Fatalf("capture mixed source markers across commits: reads=%d, got %+v, want %+v", observed.reads, captured.Generation(), before.Generation())
	}
	assertCapturedMemoryDependenciesPG(test, captured, memory)
	assertFrozenMemoryPagesPG(test, captured, pages)
	witnesses, err := captured.ObjectWitnesses(ctx)
	priorWitnesses, priorErr := before.ObjectWitnesses(ctx)
	if err != nil || priorErr != nil || !reflect.DeepEqual(witnesses, priorWitnesses) {
		test.Fatal("interleaved write mixed witness and captured bytes", err, priorErr)
	}
	if err := service.ValidateCapturedMemorySource(ctx, repo, captured); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("historical read snapshot was treated as fresh source validity", err)
	}
	current, err := service.QueryEffectiveMemory(ctx, repo, request)
	if err != nil || current.StateHash == pages[0].page.StateHash || current.DeliveryStateHash == pages[0].page.DeliveryStateHash || reflect.DeepEqual(current.Items, pages[0].page.Items) {
		test.Fatal("fresh live read missed the interleaved commit", err)
	}
	fresh, err := service.CaptureEffectiveMemorySource(ctx, repo, request, nil)
	if err != nil || fresh.Generation() == captured.Generation() {
		test.Fatal("fresh capture reused the old generation", err)
	}
	got, err := fresh.Project(ctx, request)
	if err != nil || !reflect.DeepEqual(got, current) {
		test.Fatal("fresh capture differs from the new live generation", err)
	}
	storage.Close()
	assertFrozenMemoryPagesPG(test, captured, pages)
}

func TestPGCapturedMemoryBorrowedReadAcceptedWriteRefused(test *testing.T) {
	service, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	snapshot := collaborationSnapshot(test, storage, repo, "borrowed capture")
	memory, err := service.PutMemoryDigestCAS(ctx, repo, capturedMemoryDigestPG(snapshot, "committed"))
	if err != nil {
		test.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, MemoryHash: memory, CodeCommit: effectiveOID(1)}, Limit: 1}
	generation, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	var captured *CapturedEffectiveMemory
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		var err error
		captured, err = service.CaptureEffectiveMemorySource(bound, repo, request, nil)
		return err
	}); err != nil {
		test.Fatal("owned borrowed read was refused", err)
	}
	if captured.Generation() != generation {
		test.Fatal("borrowed read lost its source markers")
	}
	pages := capturedMemoryPagesPG(test, service, repo, captured, request)
	rollback := errors.New("roll back uncommitted memory")
	err = storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		updated := capturedMemoryDigestPG(snapshot, "uncommitted")
		updated.PreviousMemoryHash = memory
		if _, err := service.PutMemoryDigestCAS(bound, repo, updated); err != nil {
			return err
		}
		partial, err := service.CaptureEffectiveMemorySource(bound, repo, request, nil)
		if !errors.Is(err, domain.ErrCatalogUnsupported) || partial != nil {
			test.Fatalf("borrowed write exposed a completed capture: capture=%v, error=%v", partial != nil, err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		test.Fatal("write refusal escaped its owning transaction", err)
	}
	if after, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo); err != nil || after != generation {
		test.Fatal("refused write capture published an uncommitted generation", err)
	}
	storage.Close()
	assertFrozenMemoryPagesPG(test, captured, pages)
}
