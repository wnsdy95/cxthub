//go:build postgres

package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func readDocEventDisplayGenerationPG(fixture docViewPGFixture, target domain.DocDisplayTarget) (domain.DocDisplayProjectionGeneration, error) {
	return repositoryReadForRepo(fixture.ctx, fixture.service, fixture.repo, func(read context.Context) (domain.DocDisplayProjectionGeneration, error) {
		return fixture.storage.ReadDocEventDisplayGeneration(read, target)
	})
}

func TestPGDocEventDisplayFindPreparesAndResumesWithoutStagingLeaks(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "root"
		if legacy {
			name = "legacy"
		}
		test.Run(name, func(test *testing.T) {
			fixture := newDocDisplayPGFixture(test)
			fixture.service.ConfigureDocumentDisplay(fixture.storage)
			summary := strings.Repeat("a", 150_000) + " **NEEDLE🙂** [visible label](https://hidden-only.invalid)\n\nfinished"
			document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventReasoning,
				RedactedSummary: summary, Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: string(fixture.repo) + strings.Repeat("opaque-only ", 30_000)}}}}
			var hash domain.ContentHash
			request := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Query: "needle🙂", From: 0, To: 1, Legacy: legacy}
			if legacy {
				hash = seedLegacyFindPGDocument(test, fixture, document.Events).Hash
				prepareLegacyFindGenerationPG(test, fixture, hash)
				request.Total = 1
			} else {
				hash = seedDocViewBoundaryPG(test, fixture, document)
				visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
				request.Selection = visible.ReplayCursor
			}
			target := docDisplayTargetPG(test, fixture, hash, 0)
			target.Profile = domain.DocDisplayProjectionProfile
			if _, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocDisplayPending) {
				test.Fatal("reasoning did not request durable preparation", err)
			}
			if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
				test.Fatal(err)
			}
			if _, err := readDocEventDisplayGenerationPG(fixture, target); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("staging was readable", err)
			}
			var generation domain.DocDisplayProjectionGeneration
			batches := 1
			for ; batches < 150; batches++ {
				restarted := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
				restarted.ConfigureDocumentDisplay(fixture.storage)
				if err := restarted.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
					test.Fatal("restart lost a durable phase", batches, err)
				}
				var err error
				generation, err = readDocEventDisplayGenerationPG(fixture, target)
				if err == nil {
					break
				}
				if !errors.Is(err, domain.ErrNotFound) {
					test.Fatal(err)
				}
			}
			if generation.Validate() != nil || generation.Pages.Count < 2 {
				test.Fatal("generation never completed", batches, generation)
			}
			var matches []domain.DocFindMatch
			for attempt := 0; attempt < 50; attempt++ {
				page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
				if err != nil || page.Projection != domain.DocFindPreparedProjection && page.Projection != domain.DocLegacyFindPreparedProjection {
					test.Fatal("prepared search failed", err, page)
				}
				matches = append(matches, page.Matches...)
				if page.NextCursor == nil {
					break
				}
				request = domain.DocFindPageRequest{Representation: request.Representation, Query: request.Query, Legacy: legacy, Cursor: *page.NextCursor}
			}
			if len(matches) != 1 || matches[0].SourceRange == nil || matches[0].Offset <= 128<<10 || matches[0].PreparedPage == nil || matches[0].Field != 0 {
				test.Fatal("formatted text after the old bound was lost", matches)
			}
			displayed := ""
			if _, err := repositoryReadForRepo(fixture.ctx, fixture.service, fixture.repo, func(read context.Context) (bool, error) {
				for index := 0; index < generation.Pages.Count; index++ {
					page, err := fixture.storage.ReadDocEventDisplayPage(read, target, index)
					if err != nil {
						return false, err
					}
					displayed += page.Text
				}
				return true, nil
			}); err != nil {
				test.Fatal(err)
			}
			want := strings.Repeat("a", 150_000) + " NEEDLE🙂 visible label\n\nfinished"
			if displayed != want || strings.Contains(displayed, "opaque-only") || strings.Contains(displayed, "hidden-only") {
				test.Fatal("public projection exposed hidden text or changed Markdown", len(displayed), len(want))
			}
			var completed, receipts int
			if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_jobs_v1 WHERE repo_id=$1 AND profile=$2 AND state='completed'`, fixture.repo, domain.DocDisplayProjectionProfile).Scan(&completed); err != nil {
				test.Fatal(err)
			}
			if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_generations_v1 WHERE event_hash=$1 AND profile=$2`, target.Event.Hash, domain.DocDisplayProjectionProfile).Scan(&receipts); err != nil || completed != 1 || receipts != 1 {
				test.Fatal("publication was not atomic and unique", completed, receipts, err)
			}
			test.Logf("batches=%d pages=%d text_bytes=%d legacy=%v", batches+1, generation.Pages.Count, generation.Source.TextBytes, legacy)
		})
	}
}

func TestPGDocEventDisplayEmptyReasoningPublishesNoOpaqueText(test *testing.T) {
	fixture := newDocDisplayPGFixture(test)
	fixture.service.ConfigureDocumentDisplay(fixture.storage)
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventReasoning,
		Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: string(fixture.repo) + strings.Repeat("private ", 1000)}}}}
	hash := seedDocViewBoundaryPG(test, fixture, document)
	target := docDisplayTargetPG(test, fixture, hash, 0)
	target.Profile = domain.DocDisplayProjectionProfile
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	generation, err := readDocEventDisplayGenerationPG(fixture, target)
	if err != nil || generation.Source.TextBytes != 0 || generation.Pages.Count != 0 {
		test.Fatal("empty reasoning was not empty", generation, err)
	}
	if _, err := fixture.storage.ReadDocEventDisplayGeneration(fixture.ctx, target); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("unpinned read accepted", err)
	}
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal("duplicate demand failed", err)
	}
	var active int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_jobs_v1 WHERE repo_id=$1 AND state<>'completed'`, fixture.repo).Scan(&active); err != nil || active != 0 {
		test.Fatal("duplicate demand reopened a valid receipt", active, err)
	}
}

type docEventDisplayAcknowledgementProbe struct {
	outbound.DocDisplayStore
	outbound.DocEventDisplayStore
	afterSave  func() error
	beforeSave func() error
}

func (probe *docEventDisplayAcknowledgementProbe) SaveDocEventDisplay(ctx context.Context, job domain.DocDisplayJob, step domain.DocEventDisplayStep) (domain.DocEventDisplayPreparation, error) {
	if probe.beforeSave != nil {
		if err := probe.beforeSave(); err != nil {
			return domain.DocEventDisplayPreparation{}, err
		}
	}
	state, err := probe.DocEventDisplayStore.SaveDocEventDisplay(ctx, job, step)
	if err == nil && probe.afterSave != nil {
		return domain.DocEventDisplayPreparation{}, probe.afterSave()
	}
	return state, err
}

func TestPGDocEventDisplayLostAcknowledgementResumesCommittedCheckpoint(test *testing.T) {
	fixture := newDocDisplayPGFixture(test)
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventReasoning,
		RedactedSummary: "**visible**", Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: string(fixture.repo) + strings.Repeat("private ", 20_000)}}}}
	hash := seedDocViewBoundaryPG(test, fixture, document)
	target := docDisplayTargetPG(test, fixture, hash, 0)
	target.Profile = domain.DocDisplayProjectionProfile
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	lost := errors.New("synthetic lost acknowledgement")
	probe := &docEventDisplayAcknowledgementProbe{DocDisplayStore: fixture.storage, DocEventDisplayStore: fixture.storage, afterSave: func() error { return lost }}
	fixture.service.ConfigureDocumentDisplay(probe)
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); !errors.Is(err, lost) {
		test.Fatal("lost acknowledgement was not observed", err)
	}
	var raw []byte
	var checkpoint domain.ContentHash
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT preparation_hash,preparation FROM doc_display_jobs_v1 WHERE repo_id=$1 AND profile=$2`, fixture.repo, domain.DocDisplayProjectionProfile).Scan(&checkpoint, &raw); err != nil {
		test.Fatal(err)
	}
	state, err := domain.DecodeDocEventDisplayPreparation(checkpoint, raw)
	if err != nil || state.Active == "" || state.Phase != "discovery" {
		test.Fatal("saved discovery progress was lost", state, err)
	}
	if _, err := readDocEventDisplayGenerationPG(fixture, target); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("lost acknowledgement published staging", err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET next_attempt=clock_timestamp() WHERE repo_id=$1`, fixture.repo); err != nil {
		test.Fatal(err)
	}
	fixture.service.ConfigureDocumentDisplay(fixture.storage)
	for attempt := 0; attempt < 8; attempt++ {
		if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
			test.Fatal(err)
		}
		generation, err := readDocEventDisplayGenerationPG(fixture, target)
		if err == nil {
			if generation.Source.TextBytes != len("visible") {
				test.Fatal(generation)
			}
			return
		}
		if !errors.Is(err, domain.ErrNotFound) {
			test.Fatal(err)
		}
	}
	test.Fatal("persisted checkpoint did not converge")
}

func TestPGDocEventDisplaySourceAndLeaseFenceStagedObjects(test *testing.T) {
	for _, change := range []string{"source", "lease"} {
		test.Run(change, func(test *testing.T) {
			fixture := newDocDisplayPGFixture(test)
			document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventReasoning,
				RedactedSummary: "**visible**", Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: string(fixture.repo) + strings.Repeat("private ", 20_000)}}}}
			hash := seedDocViewBoundaryPG(test, fixture, document)
			target := docDisplayTargetPG(test, fixture, hash, 0)
			target.Profile = domain.DocDisplayProjectionProfile
			if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
				test.Fatal(err)
			}
			probe := &docEventDisplayAcknowledgementProbe{DocDisplayStore: fixture.storage, DocEventDisplayStore: fixture.storage}
			probe.beforeSave = func() error {
				probe.beforeSave = nil
				if change == "source" {
					_, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash)
					return err
				}
				_, err := docDisplayLeaseMutationPG(fixture.ctx, fixture.pool, fixture.repo, `UPDATE doc_display_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, fixture.repo)
				return err
			}
			fixture.service.ConfigureDocumentDisplay(probe)
			if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("stale work was committed", err)
			}
			var staged, generations int
			if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_objects_v1 object JOIN doc_display_sources_v1 source ON source.source_hash=object.source_hash WHERE source.event_hash=$1 AND source.profile=$2`, target.Event.Hash, target.DisplayProfile()).Scan(&staged); err != nil {
				test.Fatal(err)
			}
			if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_generations_v1 WHERE event_hash=$1 AND profile=$2`, target.Event.Hash, target.DisplayProfile()).Scan(&generations); err != nil || staged != 0 || generations != 0 {
				test.Fatal("failed fence leaked staged or completed objects", staged, generations, err)
			}
		})
	}
}

type docEventDisplayReadProbe struct {
	outbound.DocDisplayReader
	outbound.DocEventDisplayReader
	missing func()
}

func (probe *docEventDisplayReadProbe) ReadDocEventDisplayGeneration(ctx context.Context, target domain.DocDisplayTarget) (domain.DocDisplayProjectionGeneration, error) {
	generation, err := probe.DocEventDisplayReader.ReadDocEventDisplayGeneration(ctx, target)
	if errors.Is(err, domain.ErrNotFound) && probe.missing != nil {
		probe.missing()
	}
	return generation, err
}

func TestPGDocEventDisplayReauthorizesDemandAndReadOnlyDoesNotWrite(test *testing.T) {
	fixture := newDocDisplayPGFixture(test)
	fixture.service.ConfigureDocumentDisplay(fixture.storage)
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventReasoning,
		RedactedSummary: strings.Repeat("visible ", 20_000), Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: string(fixture.repo)}}}}
	hash := seedDocViewBoundaryPG(test, fixture, document)
	visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
	request := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Selection: visible.ReplayCursor, Query: "absent", To: 1}
	count := func() int {
		var jobs int
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&jobs); err != nil {
			test.Fatal(err)
		}
		return jobs
	}
	if _, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request); !errors.Is(err, domain.ErrConversationFindPreparationRequired) || count() != 0 {
		test.Fatal("read-only demand mutated the queue", err)
	}
	missing, checks := false, 0
	fixture.service.ConfigureDocumentDisplayReads(&docEventDisplayReadProbe{DocDisplayReader: fixture.storage, DocEventDisplayReader: fixture.storage, missing: func() { missing = true }})
	ctx := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(read context.Context, _ domain.ContentHash) (context.Context, error) {
		checks++
		if missing {
			return read, domain.ErrForbidden
		}
		return read, nil
	})
	if _, err := fixture.service.FindDocPage(ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrForbidden) || count() != 0 || checks < 2 || !missing {
		test.Fatal("revocation before enqueue was bypassed", err, checks)
	}
	fixture.service.ConfigureDocumentDisplay(fixture.storage)
	if _, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocDisplayPending) || count() != 1 {
		test.Fatal(err)
	}
	for attempt := 0; attempt < 30; attempt++ {
		if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
			test.Fatal(err)
		}
		if _, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request); err == nil {
			break
		} else if !errors.Is(err, domain.ErrDocDisplayPending) {
			test.Fatal(err)
		}
	}
	readOnly := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
	readOnly.ConfigureDocumentDisplayReads(fixture.storage)
	page, err := readOnly.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
	if err != nil || count() != 1 || page.Prepared == nil {
		test.Fatal("read-only composition could not read committed pages", page, err)
	}
	page, err = readOnly.FindDocPage(outbound.WithDocReadOnly(ctx), fixture.repo, hash, request)
	if !errors.Is(err, domain.ErrForbidden) || page.Matches != nil || page.NextCursor != nil {
		test.Fatal("prepared cache bypassed current permission", page, err)
	}
}

func TestPGDocEventDisplayPublicationAndReceiptRollbackTogether(test *testing.T) {
	fixture := newDocDisplayPGFixture(test)
	fixture.service.ConfigureDocumentDisplay(fixture.storage)
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventReasoning,
		RedactedSummary: "**published**", Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: string(fixture.repo)}}}}
	hash := seedDocViewBoundaryPG(test, fixture, document)
	target := docDisplayTargetPG(test, fixture, hash, 0)
	target.Profile = domain.DocDisplayProjectionProfile
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	identifier := pgx.Identifier{domain.NewID("event_publication_")}.Sanitize()
	if _, err := fixture.pool.Exec(fixture.ctx, `CREATE FUNCTION `+identifier+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic completion failure'; END; $$`); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := fixture.pool.Exec(cleanup, `DROP TRIGGER IF EXISTS `+identifier+` ON doc_display_jobs_v1; DROP FUNCTION IF EXISTS `+identifier+`()`); err != nil {
			test.Error(err)
		}
	})
	if _, err := fixture.pool.Exec(fixture.ctx, `CREATE TRIGGER `+identifier+` BEFORE UPDATE OF state ON doc_display_jobs_v1
 FOR EACH ROW WHEN (NEW.repo_id='`+string(fixture.repo)+`' AND NEW.profile='public-event-display-v1' AND NEW.state='completed') EXECUTE FUNCTION `+identifier+`()`); err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err == nil || !strings.Contains(err.Error(), "synthetic completion failure") {
		test.Fatal("publication failure was not injected", err)
	}
	if _, err := readDocEventDisplayGenerationPG(fixture, target); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("generation committed without completion receipt", err)
	}
	var raw []byte
	var checkpoint domain.ContentHash
	var status string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state,preparation_hash,preparation FROM doc_display_jobs_v1 WHERE repo_id=$1 AND profile=$2`, fixture.repo, target.DisplayProfile()).Scan(&status, &checkpoint, &raw); err != nil {
		test.Fatal(err)
	}
	state, err := domain.DecodeDocEventDisplayPreparation(checkpoint, raw)
	if err != nil || status != "retrying" || state.Phase != "completed" {
		test.Fatal("failed publication lost reusable preparation or claimed completion", status, state, err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `DROP TRIGGER `+identifier+` ON doc_display_jobs_v1`); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET next_attempt=clock_timestamp() WHERE repo_id=$1`, fixture.repo); err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
		test.Fatal("failed publication could not retry", err)
	}
	generation, err := readDocEventDisplayGenerationPG(fixture, target)
	if err != nil || generation.Source.TextBytes != len("published") {
		test.Fatal("publication retry lost content", generation, err)
	}
	var receipts int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_jobs_v1 WHERE repo_id=$1 AND profile=$2 AND state='completed'`, fixture.repo, target.DisplayProfile()).Scan(&receipts); err != nil || receipts != 1 {
		test.Fatal("publication retry did not record exactly one completed effect", receipts, err)
	}
}
