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

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docDisplayCatalogTableImagePG struct {
	Rows  int64
	Image string
	Hash  domain.ContentHash
}

func docDisplayCatalogImagePG(test *testing.T, fixture docViewPGFixture) map[string]docDisplayCatalogTableImagePG {
	test.Helper()
	result := make(map[string]docDisplayCatalogTableImagePG)
	for _, table := range []string{
		"doc_tool_tree_sources_v1", "doc_tool_tree_nodes_v1",
		"doc_tool_key_sources_v1", "doc_tool_key_observations_v1", "doc_tool_key_members_v1",
		"doc_tool_order_sources_v1", "doc_tool_order_plans_v1",
	} {
		var value docDisplayCatalogTableImagePG
		statement := `SELECT count(*),coalesce(jsonb_agg(to_jsonb(entry)
 ORDER BY to_jsonb(entry)::text COLLATE "C"),'[]'::jsonb)::text FROM ` + pgx.Identifier{table}.Sanitize() + ` entry`
		if err := fixture.pool.QueryRow(fixture.ctx, statement).Scan(&value.Rows, &value.Image); err != nil {
			test.Fatal("private catalog image", table, err)
		}
		value.Hash = domain.HashContent([]byte(value.Image))
		result[table] = value
	}
	return result
}

func assertDocDisplayCatalogImagePG(test *testing.T, fixture docViewPGFixture, before map[string]docDisplayCatalogTableImagePG) {
	test.Helper()
	after := docDisplayCatalogImagePG(test, fixture)
	for table, expected := range before {
		if actual := after[table]; actual != expected {
			test.Fatalf("private catalog changed: table=%s rows=%d->%d hash=%s->%s", table, expected.Rows, actual.Rows, expected.Hash, actual.Hash)
		}
	}
}

type docDisplayCatalogReceiptPG struct {
	State       string
	Version     int64
	Source      domain.ContentHash
	Checkpoint  domain.ContentHash
	Preparation []byte
}

type docDisplayCatalogReuseProbePG struct {
	*store.PostgresStore
	saveCalls     int
	completeCalls int
}

func (probe *docDisplayCatalogReuseProbePG) SaveDocEventDisplay(ctx context.Context, job domain.DocDisplayJob, step domain.DocEventDisplayStep) (domain.DocEventDisplayPreparation, error) {
	probe.saveCalls++
	return probe.PostgresStore.SaveDocEventDisplay(ctx, job, step)
}

func (probe *docDisplayCatalogReuseProbePG) CompleteDocEventDisplay(ctx context.Context, job domain.DocDisplayJob, checkpoint domain.ContentHash, now time.Time) error {
	probe.completeCalls++
	return probe.PostgresStore.CompleteDocEventDisplay(ctx, job, checkpoint, now)
}

func readDocDisplayCatalogReceiptPG(test *testing.T, fixture docViewPGFixture, target domain.DocDisplayTarget) docDisplayCatalogReceiptPG {
	test.Helper()
	var receipt docDisplayCatalogReceiptPG
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state,version,source_hash,preparation_hash,preparation
 FROM doc_display_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4`,
		fixture.repo, target.DocHash, target.Event.Index, target.DisplayProfile()).Scan(
		&receipt.State, &receipt.Version, &receipt.Source, &receipt.Checkpoint, &receipt.Preparation); err != nil {
		test.Fatal("actual worker receipt", err)
	}
	state, err := domain.DecodeDocEventDisplayPreparation(receipt.Checkpoint, receipt.Preparation)
	if err != nil || state.Phase != "completed" || state.ToolTree == nil || state.ToolKeys == nil || state.ToolOrders == nil || state.Source.EventHash != target.Event.Hash {
		test.Fatal("worker did not retain a completed tree/key/order checkpoint", state, err)
	}
	owner, err := state.Source.Hash()
	if err != nil || owner != receipt.Source {
		test.Fatal("completed checkpoint lost its content-addressed owner", owner, receipt.Source, err)
	}
	return receipt
}

func finishDocDisplayCatalogJobPG(test *testing.T, fixture docViewPGFixture, target domain.DocDisplayTarget) int {
	test.Helper()
	for batch := 1; batch <= 200; batch++ {
		restarted := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
		restarted.ConfigureDocumentDisplay(fixture.storage)
		if err := restarted.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
			test.Fatal("actual restarted catalog worker", batch, err)
		}
		var state, reason string
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state,reason FROM doc_display_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4`,
			fixture.repo, target.DocHash, target.Event.Index, target.DisplayProfile()).Scan(&state, &reason); err != nil {
			test.Fatal(err)
		}
		if state == "completed" {
			return batch
		}
		if state != "waiting" {
			test.Fatal("catalog worker did not yield a resumable batch", batch, state, reason)
		}
	}
	test.Fatal("actual catalog worker did not complete")
	return 0
}

func readDocDisplayCatalogPagesPG(test *testing.T, fixture docViewPGFixture, target domain.DocDisplayTarget) (domain.DocDisplayProjectionGeneration, []domain.DocDisplayPage) {
	test.Helper()
	var pages []domain.DocDisplayPage
	generation, err := repositoryReadForRepo(fixture.ctx, fixture.service, fixture.repo, func(read context.Context) (domain.DocDisplayProjectionGeneration, error) {
		generation, err := fixture.storage.ReadDocEventDisplayGeneration(read, target)
		if err != nil {
			return domain.DocDisplayProjectionGeneration{}, err
		}
		length := 0
		for position := 0; position < generation.Pages.Count; position++ {
			page, err := fixture.storage.ReadDocEventDisplayPage(read, target, position)
			if err != nil {
				return domain.DocDisplayProjectionGeneration{}, err
			}
			if page.Index != position || page.From != length {
				test.Fatal("prepared catalog pages lost their contiguous positions", position, page.Index, page.From, length)
			}
			length += len(page.Text)
			pages = append(pages, page)
		}
		if length != generation.Source.TextBytes {
			test.Fatal("prepared catalog pages lost display bytes", length, generation.Source.TextBytes)
		}
		return generation, nil
	})
	if err != nil || generation.Validate() != nil || len(pages) == 0 {
		test.Fatal("catalog generation is not actually readable", generation, len(pages), err)
	}
	return generation, pages
}

func TestPGDocDisplayCatalogReuseAcrossRepositoriesAndCompletedRepair(test *testing.T) {
	test.Setenv("CXT_TEST_DSN", a11IsolatedPGDSN(test, context.Background()))
	left, right := newDocDisplayPGFixture(test), newDocDisplayPGFixture(test)
	rows := make([]any, 32)
	for index := range rows {
		rows[index] = map[string]any{
			"ordinal": index,
			"text":    fmt.Sprintf("catalog-row-%02d🙂", index),
			"padding": strings.Repeat("p", 5000),
			"nested":  []any{true, nil, map[string]any{"10": "ten", "2": "two", "value": "shared"}},
		}
	}
	event := domain.CIREvent{Kind: domain.EventToolCall, ToolName: "generic", CallID: "same-event-catalog-reuse", Input: map[string]any{"rows": rows}}
	leftHash := seedDocViewBoundaryPG(test, left, domain.CIRDocument{Envelope: left.cir.Envelope, Events: []domain.CIREvent{event}})
	leftTarget := docDisplayTargetPG(test, left, leftHash, 0)
	leftTarget.Profile = domain.DocDisplayProjectionProfile
	if leftTarget.Event.Length <= docViewBytes {
		test.Fatal("reuse fixture must actually select prepared search rather than the supported small-event inline path")
	}
	if queued, err := left.storage.EnqueueDocDisplay(left.ctx, leftTarget, time.Now().UTC()); err != nil || queued.State != "waiting" {
		test.Fatal("first actual worker was not queued", queued, err)
	}
	firstBatches := finishDocDisplayCatalogJobPG(test, left, leftTarget)
	firstReceipt := readDocDisplayCatalogReceiptPG(test, left, leftTarget)
	if firstReceipt.State != "completed" {
		test.Fatal("first worker lacks an actual completed receipt", firstReceipt.State)
	}
	firstGeneration, firstPages := readDocDisplayCatalogPagesPG(test, left, leftTarget)
	var displayed strings.Builder
	for _, page := range firstPages {
		displayed.WriteString(page.Text)
	}
	if strings.Count(displayed.String(), "catalog-row-") != len(rows) || !strings.Contains(displayed.String(), "catalog-row-31") {
		test.Fatal("first worker did not render every structured row")
	}
	baseline := docDisplayCatalogImagePG(test, left)
	for table, image := range baseline {
		if image.Rows == 0 {
			test.Fatal("regression fixture left a private catalog table unexercised", table)
		}
		test.Logf("catalog=%s rows=%d canonical_hash=%s", table, image.Rows, image.Hash)
	}
	var incomplete int
	if err := left.pool.QueryRow(left.ctx, `SELECT count(*) FROM (
 SELECT state FROM doc_tool_tree_sources_v1 UNION ALL SELECT state FROM doc_tool_key_sources_v1
 UNION ALL SELECT state FROM doc_tool_order_sources_v1) sources WHERE state<>'complete'`).Scan(&incomplete); err != nil || incomplete != 0 {
		test.Fatal("first worker did not seal all three private catalogs", incomplete, err)
	}
	assertGenerationCount := func(want int) {
		test.Helper()
		var count int
		if err := left.pool.QueryRow(left.ctx, `SELECT count(*) FROM doc_display_generations_v1 WHERE event_hash=$1 AND profile=$2`,
			leftTarget.Event.Hash, leftTarget.DisplayProfile()).Scan(&count); err != nil || count != want {
			test.Fatal("shared serving descriptor count", count, want, err)
		}
	}
	assertGenerationCount(1)
	rightHash := seedDocViewBoundaryPG(test, right, domain.CIRDocument{Envelope: right.cir.Envelope, Events: []domain.CIREvent{event}})
	rightTarget := docDisplayTargetPG(test, right, rightHash, 0)
	rightTarget.Profile = domain.DocDisplayProjectionProfile
	if left.repo == right.repo || leftHash == rightHash || leftTarget.Event.Hash != rightTarget.Event.Hash || leftTarget.Event.Length != rightTarget.Event.Length {
		test.Fatal("fixture does not select the same exact event from different repositories and documents")
	}
	sharedGeneration, sharedPages := readDocDisplayCatalogPagesPG(test, right, rightTarget)
	if !reflect.DeepEqual(sharedGeneration, firstGeneration) || !reflect.DeepEqual(sharedPages, firstPages) {
		test.Fatal("second repository did not reuse the published generation and source mappings")
	}
	assertDocDisplayCatalogImagePG(test, right, baseline)
	second, err := right.storage.EnqueueDocDisplay(right.ctx, rightTarget, time.Now().UTC())
	if err != nil || second.State != "waiting" {
		test.Fatal("same-event demand did not queue its own second worker receipt", second, err)
	}
	reuseProbe := &docDisplayCatalogReuseProbePG{PostgresStore: right.storage}
	restarted := NewService(right.storage, right.storage, nil, nil, right.storage)
	restarted.ConfigureDocumentDisplay(reuseProbe)
	if err := restarted.ProcessDocDisplayJobs(right.ctx, 1); err != nil {
		test.Fatal("second actual worker failed its single reuse batch", err)
	}
	secondReceipt := readDocDisplayCatalogReceiptPG(test, right, rightTarget)
	if secondReceipt.State != "completed" || reuseProbe.saveCalls != 0 || reuseProbe.completeCalls != 1 {
		test.Fatal("second worker must complete in one batch without reconstruction", secondReceipt.State, reuseProbe.saveCalls, reuseProbe.completeCalls)
	}
	if secondReceipt.State != "completed" || secondReceipt.Source != firstReceipt.Source {
		test.Fatal("second actual receipt did not retain the shared content-addressed owner", secondReceipt.State, secondReceipt.Source)
	}
	sharedGeneration, sharedPages = readDocDisplayCatalogPagesPG(test, right, rightTarget)
	if !reflect.DeepEqual(sharedGeneration, firstGeneration) || !reflect.DeepEqual(sharedPages, firstPages) {
		test.Fatal("second actual worker changed the shared generation or source mappings")
	}
	assertDocDisplayCatalogImagePG(test, right, baseline)
	assertGenerationCount(1)
	if duplicate, err := right.storage.EnqueueDocDisplay(right.ctx, rightTarget, time.Now().UTC()); err != nil || duplicate.State != "completed" || duplicate.Version != secondReceipt.Version {
		test.Fatal("duplicate same-event demand reopened a completed receipt", duplicate, err)
	}
	if _, err := right.storage.ReadDocEventDisplayGeneration(right.ctx, rightTarget); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("shared generation bypassed the pinned-read guard", err)
	}
	stale := rightTarget
	stale.Binding.SourceVersion++
	if _, err := readDocEventDisplayGenerationPG(right, stale); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("shared generation bypassed the current source binding", err)
	}
	if _, err := right.storage.EnqueueDocDisplay(right.ctx, stale, time.Now().UTC()); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("shared catalog demand admitted a stale source binding", err)
	}
	foreign := leftTarget
	foreign.RepoID = right.repo
	if _, err := readDocEventDisplayGenerationPG(right, foreign); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("shared event authorized another repository's document", err)
	}
	visible := readVisibleReplayReadyPG(test, right, rightHash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
	request := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Query: "catalog-row-00", Selection: visible.ReplayCursor, To: 1}
	probe := &denseDocDisplayReadProbe{DocDisplayReader: right.storage, DocEventDisplayReader: right.storage}
	right.service.ConfigureDocumentDisplayReads(probe)
	authorizations := 0
	denied := inbound.WithDocumentReadAuthorizer(right.ctx, func(read context.Context, repo domain.ContentHash) (context.Context, error) {
		authorizations++
		if repo != right.repo {
			test.Fatal("authorization checked the wrong repository", repo)
		}
		return read, domain.ErrForbidden
	})
	page, err := right.service.FindDocPage(outbound.WithDocReadOnly(denied), right.repo, rightHash, request)
	if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(page, domain.DocFindPage{}) || authorizations == 0 || probe.generationReads != 0 || probe.pageReads != 0 {
		test.Fatal("shared completed generation bypassed current authorization", page, authorizations, probe, err)
	}
	page, err = right.service.FindDocPage(outbound.WithDocReadOnly(right.ctx), right.repo, rightHash, request)
	if err != nil || len(page.Matches) != 1 || page.Matches[0].PreparedPage == nil || page.Matches[0].SourceRange == nil || probe.generationReads == 0 || probe.pageReads == 0 {
		test.Fatal("authorized prepared search did not use the shared generation", page, probe, err)
	}
	deleted, err := left.pool.Exec(left.ctx, `DELETE FROM doc_display_generations_v1 WHERE event_hash=$1 AND profile=$2`, leftTarget.Event.Hash, leftTarget.DisplayProfile())
	if err != nil || deleted.RowsAffected() != 1 {
		test.Fatal("fixture did not remove the actual serving descriptor", deleted, err)
	}
	assertGenerationCount(0)
	for _, current := range []struct {
		fixture docViewPGFixture
		target  domain.DocDisplayTarget
	}{{left, leftTarget}, {right, rightTarget}} {
		if _, err := readDocEventDisplayGenerationPG(current.fixture, current.target); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("deleted generation remained readable", current.fixture.repo, err)
		}
	}
	if revoked, err := left.pool.Exec(left.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, left.repo, leftHash); err != nil || revoked.RowsAffected() != 1 {
		test.Fatal("fixture did not revoke the original source ownership", revoked, err)
	}
	if _, err := left.storage.EnqueueDocDisplay(left.ctx, leftTarget, time.Now().UTC()); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("completed checkpoint repaired without current source ownership", err)
	}
	if receipt := readDocDisplayCatalogReceiptPG(test, left, leftTarget); !reflect.DeepEqual(receipt, firstReceipt) {
		test.Fatal("rejected source-less repair changed the completed receipt")
	}
	assertDocDisplayCatalogImagePG(test, left, baseline)
	assertGenerationCount(0)
	if _, err := left.pool.Exec(left.ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, left.repo, leftHash); err != nil {
		test.Fatal(err)
	}
	reopened, err := left.storage.EnqueueDocDisplay(left.ctx, leftTarget, time.Now().UTC())
	if err != nil || reopened.State != "waiting" || reopened.Version != firstReceipt.Version+1 || reopened.Attempts != 0 {
		test.Fatal("actual completed checkpoint was not reopened for descriptor repair", reopened, err)
	}
	waiting := readDocDisplayCatalogReceiptPG(test, left, leftTarget)
	if waiting.State != "waiting" || waiting.Source != firstReceipt.Source || waiting.Checkpoint != firstReceipt.Checkpoint || !reflect.DeepEqual(waiting.Preparation, firstReceipt.Preparation) {
		test.Fatal("repair demand discarded the actual completed checkpoint")
	}
	if repairBatches := finishDocDisplayCatalogJobPG(test, left, leftTarget); repairBatches != 1 {
		test.Fatal("completed-checkpoint repair rebuilt instead of republishing", repairBatches)
	}
	repaired := readDocDisplayCatalogReceiptPG(test, left, leftTarget)
	if repaired.State != "completed" || repaired.Version != reopened.Version+1 || repaired.Source != firstReceipt.Source || repaired.Checkpoint != firstReceipt.Checkpoint || !reflect.DeepEqual(repaired.Preparation, firstReceipt.Preparation) {
		test.Fatal("actual worker repair changed the completed checkpoint")
	}
	for _, current := range []struct {
		fixture docViewPGFixture
		target  domain.DocDisplayTarget
	}{{left, leftTarget}, {right, rightTarget}} {
		generation, pages := readDocDisplayCatalogPagesPG(test, current.fixture, current.target)
		if !reflect.DeepEqual(generation, firstGeneration) || !reflect.DeepEqual(pages, firstPages) {
			test.Fatal("completed-checkpoint repair changed shared display text or source mappings", current.fixture.repo)
		}
	}
	assertGenerationCount(1)
	assertDocDisplayCatalogImagePG(test, left, baseline)
	var completed, jobs int
	if err := left.pool.QueryRow(left.ctx, `SELECT count(*),count(*) FILTER(WHERE state='completed') FROM doc_display_jobs_v1`).Scan(&jobs, &completed); err != nil || jobs != 2 || completed != 2 {
		test.Fatal("reuse or repair duplicated durable jobs or failed to finish them", jobs, completed, err)
	}
	test.Logf("structured_rows=%d first_worker_batches=%d second_worker_batches=1 second_save_calls=%d second_complete_calls=%d repair_batches=1 private_catalog_tables=7", len(rows), firstBatches, reuseProbe.saveCalls, reuseProbe.completeCalls)
}
