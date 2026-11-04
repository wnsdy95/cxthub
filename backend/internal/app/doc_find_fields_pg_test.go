//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func newDocFindFieldsPGFixture(test *testing.T) docViewPGFixture {
	test.Helper()
	fixture := newDocViewPGFixture(test)
	fixture.service.ConfigureDocumentFindFields(fixture.storage)
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := fixture.pool.Exec(ctx, `DELETE FROM doc_find_fields_jobs_v1 WHERE repo_id=$1`, fixture.repo); err != nil {
			test.Error(err)
		}
	})
	return fixture
}

func TestPGDocFindFieldsPreparesDurablyAndNeverSearchesLockedState(test *testing.T) {
	fixture := newDocFindFieldsPGFixture(test)
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{
		historyMessage(domain.RoleUser, "intro"),
		{Kind: domain.EventReasoning, RedactedSummary: strings.Repeat("Needle ", 45), Locked: &domain.LockedBlob{
			Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: string(fixture.repo) + strings.Repeat("needle secret ", 160_000)}},
		{Kind: domain.EventReasoning, RedactedSummary: string(fixture.repo) + strings.Repeat("x", 270_000) + "needle🙂" + strings.Repeat("\ub05d", 30_000) + "NEEDLE🙂"},
		{Kind: domain.EventReasoning, Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: strings.Repeat("needle ", 50_000)}},
		{Kind: domain.EventToolResult, Output: "needle"},
	}}
	for index := range document.Events {
		document.Events[index].Seq = index
	}
	hash := seedDocViewBoundaryPG(test, fixture, document)
	visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
	request := domain.DocFindPageRequest{Selection: visible.ReplayCursor, Query: "needle", From: 1, To: visible.Total}
	observed := &visibleCostStore{docViewPGStore: &docViewPGStore{PostgresStore: fixture.storage}}
	fixture.service.blobs = observed
	for attempt := 0; attempt < 2; attempt++ {
		page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
		if !errors.Is(err, domain.ErrDocFindFieldsPending) || !reflect.DeepEqual(page, domain.DocFindPage{}) || observed.chunkReads != 0 {
			test.Fatal("missing projection read ciphertext or was not queued", page, err, observed)
		}
	}
	var jobs int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_find_fields_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND state='waiting'`, fixture.repo, hash).Scan(&jobs); err != nil || jobs != 1 {
		test.Fatal("find preparation was not durably deduplicated", jobs, err)
	}
	restarted := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
	restarted.ConfigureDocumentFindFields(fixture.storage)
	if err := restarted.ProcessDocFindFieldsJobs(fixture.ctx, 1); err != nil {
		test.Fatal("restarted worker could not recover queue", err)
	}
	var completed int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_find_fields_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND state='completed'`, fixture.repo, hash).Scan(&completed); err != nil || completed != 1 {
		test.Fatal(completed, err)
	}
	observed.chunkReads, observed.chunkBytes = 0, 0
	var matches []domain.DocFindMatch
	for attempt := 0; attempt < 30; attempt++ {
		page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
		if err != nil || page.Generation != visible.Generation {
			test.Fatal(page, err)
		}
		for _, match := range page.Matches {
			if match.Index == 3 || match.Index == 1 && match.Raw || match.Index == 2 && !match.Raw {
				test.Fatal("locked-state match or wrong field coordinates", match)
			}
			reveal, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{Cursor: match.RevealCursor})
			if err != nil || reveal.Generation != visible.Generation || len(reveal.Units) != 1 || reveal.Units[0].Event == nil || reveal.Units[0].Event.Index != match.Index {
				test.Fatal("prepared match did not reveal original event", match, reveal, err)
			}
		}
		matches = append(matches, page.Matches...)
		if page.NextCursor == nil {
			break
		}
		request = domain.DocFindPageRequest{Query: "needle", Cursor: *page.NextCursor}
	}
	if len(matches) != 48 {
		test.Fatal("lost or duplicated searchable summary", len(matches))
	}
	request = domain.DocFindPageRequest{Selection: visible.ReplayCursor, Query: "secret", To: visible.Total}
	for attempt := 0; attempt < 30; attempt++ {
		page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
		if err != nil || len(page.Matches) != 0 {
			test.Fatal("locked payload was searchable", page, err)
		}
		if page.NextCursor == nil {
			break
		}
		request = domain.DocFindPageRequest{Query: request.Query, Cursor: *page.NextCursor}
	}
	eventHash, err := repositoryReadForRepo(fixture.ctx, fixture.service, fixture.repo, func(read context.Context) (domain.ContentHash, error) {
		source, err := fixture.service.docViewSource(read, fixture.repo, hash, false)
		if err != nil {
			return "", err
		}
		entry, err := source.entry(1)
		return entry.Hash, err
	})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM doc_find_fields_v1 WHERE event_hash=$1`, eventHash); err != nil {
		test.Fatal(err)
	}
	request = domain.DocFindPageRequest{Selection: visible.ReplayCursor, Query: "needle", From: 1, To: 2}
	if _, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocFindFieldsPending) {
		test.Fatal("missing derived row could not restart preparation", err)
	}
	if err := restarted.ProcessDocFindFieldsJobs(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
	if err != nil || len(page.Matches) != domain.DocFindPageMatches || page.NextCursor == nil {
		test.Fatal("lost sidecar was not rebuilt", len(page.Matches), err)
	}
}

func TestPGDocFindFieldsQueueReauthorizesAndReadOnlyDoesNotEnqueue(test *testing.T) {
	fixture := newDocFindFieldsPGFixture(test)
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{
		{Kind: domain.EventReasoning, RedactedSummary: "needle", Locked: &domain.LockedBlob{
			Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: string(fixture.repo) + strings.Repeat("x", 200_000)}},
	}}
	hash := seedDocViewBoundaryPG(test, fixture, document)
	visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
	request := domain.DocFindPageRequest{Selection: visible.ReplayCursor, Query: "needle", To: visible.Total}
	if _, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request); !errors.Is(err, domain.ErrDocFindFieldsUnavailable) {
		test.Fatal("read-only request attempted preparation", err)
	}
	authorizations := 0
	ctx := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
		authorizations++
		if authorizations == 2 {
			return nil, domain.ErrForbidden
		}
		return ctx, nil
	})
	if _, err := fixture.service.FindDocPage(ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrForbidden) || authorizations != 2 {
		test.Fatal("enqueue did not reauthorize outside the read snapshot", err, authorizations)
	}
	var jobs int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_find_fields_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&jobs); err != nil || jobs != 0 {
		test.Fatal("denied/read-only request persisted work", jobs, err)
	}
}

func TestPGDocFindFieldsWorkerRefreshesChangedSourceWithoutUserRetry(test *testing.T) {
	fixture := newDocFindFieldsPGFixture(test)
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{
		{Kind: domain.EventReasoning, RedactedSummary: "needle", Locked: &domain.LockedBlob{
			Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: string(fixture.repo) + strings.Repeat("x", 200_000)}},
	}}
	hash := seedDocViewBoundaryPG(test, fixture, document)
	visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
	if _, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, domain.DocFindPageRequest{
		Selection: visible.ReplayCursor, Query: "needle", To: visible.Total}); !errors.Is(err, domain.ErrDocFindFieldsPending) {
		test.Fatal(err)
	}
	job, err := fixture.storage.ClaimDocFindFields(fixture.ctx, time.Now().UTC(), docJobLease)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash); err != nil {
		test.Fatal(err)
	}
	readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
	if err := fixture.service.runDocFindFieldsJob(fixture.ctx, job); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("superseded worker was not fenced", err)
	}
	var state string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state FROM doc_find_fields_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2`, fixture.repo, hash).Scan(&state); err != nil || state != "waiting" {
		test.Fatal("source refresh required another user request", state, err)
	}
	if err := fixture.service.ProcessDocFindFieldsJobs(fixture.ctx, 1); err != nil {
		test.Fatal("refreshed worker did not recover", err)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state FROM doc_find_fields_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2`, fixture.repo, hash).Scan(&state); err != nil || state != "completed" {
		test.Fatal(state, err)
	}
}

type docFindFieldsCompletionHook struct {
	outbound.DocFindFieldsStore
	before func()
}

func (storage *docFindFieldsCompletionHook) FinishDocFindFields(ctx context.Context, job domain.DocRangeIndexJob, now time.Time) error {
	if job.State == "completed" && storage.before != nil {
		before := storage.before
		storage.before = nil
		before()
	}
	return storage.DocFindFieldsStore.FinishDocFindFields(ctx, job, now)
}

func TestPGDocFindFieldsCompletionConflictDoesNotKeepRepositoryRunning(test *testing.T) {
	fixture := newDocFindFieldsPGFixture(test)
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{
		{Kind: domain.EventReasoning, RedactedSummary: "needle", Locked: &domain.LockedBlob{
			Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: string(fixture.repo) + strings.Repeat("x", 200_000)}},
	}}
	hash := seedDocViewBoundaryPG(test, fixture, document)
	readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
	if _, err := fixture.storage.EnqueueDocFindFields(fixture.ctx, fixture.repo, hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.ProcessDocFindFieldsJobs(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.EnqueueDocFindFields(fixture.ctx, fixture.repo, hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	job, err := fixture.storage.ClaimDocFindFields(fixture.ctx, time.Now().UTC(), docJobLease)
	if err != nil {
		test.Fatal(err)
	}
	fixture.service.ConfigureDocumentFindFields(&docFindFieldsCompletionHook{DocFindFieldsStore: fixture.storage, before: func() {
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash); err != nil {
			test.Fatal(err)
		}
		readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
	}})
	if err := fixture.service.runDocFindFieldsJob(fixture.ctx, job); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("completion source change was not fenced", err)
	}
	var state string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state FROM doc_find_fields_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2`, fixture.repo, hash).Scan(&state); err != nil || state != "waiting" {
		test.Fatal("completion conflict left repository permanently running", state, err)
	}
	if err := fixture.service.ProcessDocFindFieldsJobs(fixture.ctx, 1); err != nil {
		test.Fatal("completion retry could not recover", err)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state FROM doc_find_fields_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2`, fixture.repo, hash).Scan(&state); err != nil || state != "completed" {
		test.Fatal(state, err)
	}
}

func TestPGDocFindFieldsAgentMessagePublicBlocksOnly(test *testing.T) {
	fixture := newDocFindFieldsPGFixture(test)
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{
		{Kind: domain.EventMessage, Role: domain.RoleAssistant, AgentMessage: true, AgentAuthor: "reviewer",
			Blocks: []domain.ContentBlock{{Type: "text", Text: "public Needle🙂"}},
			Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: string(fixture.repo) + strings.Repeat("secret ", 40_000)}},
		{Kind: domain.EventMessage, Role: domain.RoleAssistant, AgentMessage: true,
			Blocks: []domain.ContentBlock{{Type: "text", Text: string(fixture.repo) + strings.Repeat("\ub05d", 70_000) + "NEEDLE🙂"}},
			Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: "secret needle🙂"}},
	}}
	document.Envelope.CIRVersion = "2"
	for index := range document.Events {
		document.Events[index].Seq = index
	}
	hash := seedDocViewBoundaryPG(test, fixture, document)
	visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
	request := domain.DocFindPageRequest{Selection: visible.ReplayCursor, Query: "needle🙂", To: visible.Total}
	if _, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocFindFieldsPending) {
		test.Fatal("oversized agent message did not request safe field preparation", err)
	}
	if err := fixture.service.ProcessDocFindFieldsJobs(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	var matches []domain.DocFindMatch
	for attempt := 0; attempt < 10; attempt++ {
		page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
		if err != nil {
			test.Fatal(err)
		}
		matches = append(matches, page.Matches...)
		if page.NextCursor == nil {
			break
		}
		request = domain.DocFindPageRequest{Query: request.Query, Cursor: *page.NextCursor}
	}
	if len(matches) != 2 || matches[0].Raw || !matches[1].Raw {
		test.Fatal("agent-message search included encrypted payload or lost public blocks", matches)
	}
}
