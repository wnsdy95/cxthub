//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func denseDocDisplayEvent(kind domain.EventKind, suffix string) domain.CIREvent {
	text := strings.Repeat("a**b**", 20000) + " NEEDLE🙂 " + suffix
	event := domain.CIREvent{Kind: kind, Role: domain.RoleAssistant}
	if kind == domain.EventMessage {
		event.Blocks = []domain.ContentBlock{{Type: "text", Text: text}}
	} else {
		event.RedactedSummary = text
	}
	return event
}

func denseDocDisplayPGSelection(test *testing.T, fixture docViewPGFixture, event domain.CIREvent, legacy bool) (domain.ContentHash, domain.DocFindPageRequest) {
	test.Helper()
	fixture.cir.Envelope.CIRVersion = "2"
	request := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Query: "needle🙂", To: 1, Legacy: legacy}
	if legacy {
		hash := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{event}).Hash
		prepareLegacyFindGenerationPG(test, fixture, hash)
		request.Total = 1
		return hash, request
	}
	hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}})
	visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
	request.Selection = visible.ReplayCursor
	return hash, request
}

func denseDocDisplayPGJobs(test *testing.T, fixture docViewPGFixture) int {
	test.Helper()
	var count int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&count); err != nil {
		test.Fatal(err)
	}
	return count
}

func TestPGDocMessageDenseDisplayReadOnlyRequiresPreparationWithoutWrites(test *testing.T) {
	for _, kind := range []domain.EventKind{domain.EventMessage, domain.EventReasoning} {
		for _, legacy := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/legacy=%t", kind, legacy), func(test *testing.T) {
				fixture := newDocDisplayPGFixture(test)
				fixture.service.ConfigureDocumentDisplay(fixture.storage)
				event := denseDocDisplayEvent(kind, string(fixture.repo))
				raw, err := json.Marshal(event)
				if err != nil || len(raw) >= docViewBytes {
					test.Fatal("fixture is not a small legitimate event", len(raw), err)
				}
				if display, err := domain.BuildDocEventDisplayWithin(fixture.ctx, raw, docFindDisplayBuildBytes); !errors.Is(err, domain.ErrDocFindDisplayUnavailable) || display != nil {
					test.Fatal("dense public Markdown no longer exceeds the bounded formatter", err)
				}
				hash, request := denseDocDisplayPGSelection(test, fixture, event, legacy)
				before := legacyFindPGImage(test, fixture)
				page, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
				if !errors.Is(err, domain.ErrConversationFindPreparationRequired) || !errors.Is(err, domain.ErrDocFindDisplayUnavailable) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
					test.Fatalf("small dense event did not expose read-only preparation demand: raw_bytes=%d page=%+v err=%v", len(raw), page, err)
				}
				if denseDocDisplayPGJobs(test, fixture) != 0 || before != legacyFindPGImage(test, fixture) {
					test.Fatal("read-only dense probe wrote source, projections, or durable work")
				}
				test.Logf("kind=%s legacy=%t raw_bytes=%d read_only_jobs=0", kind, legacy, len(raw))
			})
		}
	}
}

type denseDocDisplayReadProbe struct {
	outbound.DocDisplayReader
	outbound.DocEventDisplayReader
	missing         func()
	generationReads int
	pageReads       int
}

func (probe *denseDocDisplayReadProbe) ReadDocEventDisplayGeneration(ctx context.Context, target domain.DocDisplayTarget) (domain.DocDisplayProjectionGeneration, error) {
	probe.generationReads++
	generation, err := probe.DocEventDisplayReader.ReadDocEventDisplayGeneration(ctx, target)
	if errors.Is(err, domain.ErrNotFound) && probe.missing != nil {
		probe.missing()
	}
	return generation, err
}

func (probe *denseDocDisplayReadProbe) ReadDocEventDisplayPage(ctx context.Context, target domain.DocDisplayTarget, position int) (domain.DocDisplayPage, error) {
	probe.pageReads++
	return probe.DocEventDisplayReader.ReadDocEventDisplayPage(ctx, target, position)
}

type denseDocDisplayBlobProbe struct {
	*store.PostgresStore
	rawReads int
	deny     bool
}

func (probe *denseDocDisplayBlobProbe) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	probe.rawReads++
	if probe.deny {
		return nil, domain.ErrIntegrity
	}
	return probe.PostgresStore.ReadConversationChunk(ctx, repo, hash, length)
}

func (probe *denseDocDisplayBlobProbe) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	probe.rawReads++
	if probe.deny {
		return nil, domain.ErrIntegrity
	}
	return probe.PostgresStore.GetChunk(ctx, repo, hash)
}

func (probe *denseDocDisplayBlobProbe) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	probe.rawReads++
	if probe.deny {
		return domain.SessionDoc{}, domain.ErrIntegrity
	}
	return probe.PostgresStore.GetDoc(ctx, repo, hash)
}

func TestPGDocMessageDenseDisplayDemandThenSearchLatePages(test *testing.T) {
	for _, kind := range []domain.EventKind{domain.EventMessage, domain.EventReasoning} {
		for _, legacy := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/legacy=%t", kind, legacy), func(test *testing.T) {
				fixture := newDocDisplayPGFixture(test)
				fixture.service.ConfigureDocumentDisplay(fixture.storage)
				event := denseDocDisplayEvent(kind, string(fixture.repo))
				hash, initial := denseDocDisplayPGSelection(test, fixture, event, legacy)
				missing, authorizations := false, 0
				probe := &denseDocDisplayReadProbe{DocDisplayReader: fixture.storage, DocEventDisplayReader: fixture.storage, missing: func() { missing = true }}
				fixture.service.ConfigureDocumentDisplayReads(probe)
				authorize := func(read context.Context, _ domain.ContentHash) (context.Context, error) {
					authorizations++
					if missing {
						return read, domain.ErrForbidden
					}
					return read, nil
				}
				revoked := inbound.WithDocumentReadAuthorizer(fixture.ctx, authorize)
				revoked = inbound.WithDocumentSnapshotReadAuthorizer(revoked, authorize)
				page, err := fixture.service.FindDocPage(revoked, fixture.repo, hash, initial)
				if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(page, domain.DocFindPage{}) || !missing || authorizations < 2 || denseDocDisplayPGJobs(test, fixture) != 0 {
					test.Fatal("dense fallback queued with stale authority", err, missing, authorizations)
				}
				fixture.service.ConfigureDocumentDisplay(fixture.storage)
				for attempt := 0; attempt < 2; attempt++ {
					page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, initial)
					if !errors.Is(err, domain.ErrDocDisplayPending) || !reflect.DeepEqual(page, domain.DocFindPage{}) || denseDocDisplayPGJobs(test, fixture) != 1 {
						test.Fatal("dense event did not demand exactly one durable preparation", err)
					}
				}
				target := docDisplayTargetPG(test, fixture, hash, 0)
				target.Profile = domain.DocDisplayProjectionProfile
				if target.Event.Length >= docViewBytes {
					test.Fatal("stored fixture is no longer small", target.Event.Length)
				}
				var generation domain.DocDisplayProjectionGeneration
				batches := 0
				for ; batches < 400; batches++ {
					restarted := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
					restarted.ConfigureDocumentDisplay(fixture.storage)
					if err := restarted.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
						test.Fatal("dense preparation did not resume", err)
					}
					generation, err = readDocEventDisplayGenerationPG(fixture, target)
					if err == nil {
						break
					}
					if !errors.Is(err, domain.ErrNotFound) {
						test.Fatal(err)
					}
				}
				if generation.Validate() != nil || generation.Pages.Count <= docFindPreparedWindows || batches == 0 {
					test.Fatal("dense event did not produce durable late pages", batches, generation.Pages.Count)
				}
				reader := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
				probe = &denseDocDisplayReadProbe{DocDisplayReader: fixture.storage, DocEventDisplayReader: fixture.storage}
				reader.ConfigureDocumentDisplayReads(probe)
				blobs := &denseDocDisplayBlobProbe{PostgresStore: fixture.storage}
				reader.blobs = blobs
				request := initial
				var matches []domain.DocFindMatch
				requests, rawProbeReads := 0, 0
				for ; requests < 200; requests++ {
					page, err := reader.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
					if err != nil || page.Prepared == nil || page.Hash != hash {
						test.Fatal("prepared small-event continuation failed", requests, page, err)
					}
					projection := domain.DocFindPreparedProjection
					if legacy {
						projection = domain.DocLegacyFindPreparedProjection
					}
					if page.Projection != projection {
						test.Fatal("prepared protocol changed", page.Projection)
					}
					matches = append(matches, page.Matches...)
					if requests == 0 {
						rawProbeReads = blobs.rawReads
						if rawProbeReads == 0 || page.NextCursor == nil || page.Prepared.Generation == "" || page.Prepared.Page == 0 {
							test.Fatal("small probe did not transition to native prepared progress", blobs.rawReads, page)
						}
						blobs.deny = true
					} else if blobs.rawReads != rawProbeReads {
						test.Fatal("continuation lazily reformatted the small event", requests, blobs.rawReads, rawProbeReads)
					}
					if page.NextCursor == nil {
						if page.Index != 1 || page.Prepared.Generation != "" || page.Prepared.Page != 0 || page.Byte != 0 {
							test.Fatal("terminal native progress was not reset", page)
						}
						break
					}
					request = domain.DocFindPageRequest{Representation: initial.Representation, Query: initial.Query, Legacy: legacy, Cursor: *page.NextCursor}
				}
				if requests == 200 || len(matches) != 1 {
					test.Fatal("dense search lost or duplicated late matches", requests, matches)
				}
				match := matches[0]
				if match.Index != 0 || match.Offset != 40001 || match.Length != len("NEEDLE🙂") || match.Field != 0 || match.Raw || match.PreparedPage == nil || *match.PreparedPage <= docFindPreparedWindows || match.SourceRange == nil || !strings.Contains(match.Preview, "NEEDLE🙂") || strings.Contains(match.Preview, "**") {
					test.Fatal("late match lost whole-event position, preview, or provenance", match)
				}
				reader.blobs = fixture.storage
				fragmentRequest := domain.DocFindFragmentRequest{Offset: match.SourceRange.From, Budget: 256, Selection: match.RevealCursor}
				if legacy {
					fragmentRequest.Legacy, fragmentRequest.Index, fragmentRequest.Total, fragmentRequest.Selection = true, 0, 1, ""
				}
				fragment, err := reader.ReadDocFindFragment(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, fragmentRequest)
				if err != nil || fragment.Offset != match.SourceRange.From || !strings.HasPrefix(fragment.JSON, "NEEDLE🙂") || match.SourceRange.To-match.SourceRange.From != len("NEEDLE🙂") {
					test.Fatal("prepared match changed the original source/preview protocol", fragment, err)
				}
				generationReads, pageReads := probe.generationReads, probe.pageReads
				forbidden := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(read context.Context, _ domain.ContentHash) (context.Context, error) {
					return read, domain.ErrForbidden
				})
				page, err = reader.FindDocPage(outbound.WithDocReadOnly(forbidden), fixture.repo, hash, initial)
				if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(page, domain.DocFindPage{}) || probe.generationReads != generationReads || probe.pageReads != pageReads || denseDocDisplayPGJobs(test, fixture) != 1 {
					test.Fatal("ready dense display bypassed current authorization", err)
				}
				before := legacyFindPGImage(test, fixture)
				for _, representation := range []string{"", domain.DocFindDisplayRepresentation} {
					request := initial
					request.Representation = representation
					page, err := reader.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
					if representation == domain.DocFindDisplayRepresentation {
						if !errors.Is(err, domain.ErrDocFindDisplayUnavailable) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
							test.Fatal("display-v1 was silently upgraded to prepared", page, err)
						}
					} else {
						projection := domain.DocFindProjection
						if legacy {
							projection = domain.DocLegacyFindProjection
						}
						if err != nil || page.Projection != projection || page.Prepared != nil || len(page.Matches) != 1 || page.Matches[0].PreparedPage != nil {
							test.Fatal("native source protocol changed", page, err)
						}
					}
				}
				if before != legacyFindPGImage(test, fixture) || denseDocDisplayPGJobs(test, fixture) != 1 {
					test.Fatal("old read protocols mutated source or durable work")
				}
				test.Logf("kind=%s legacy=%t raw_event_bytes=%d prepared_pages=%d worker_batches=%d search_requests=%d initial_raw_reads=%d continuation_raw_reads=0 whole_event_offset=%d prepared_page=%d", kind, legacy, target.Event.Length, generation.Pages.Count, batches+1, requests+1, rawProbeReads, match.Offset, *match.PreparedPage)
			})
		}
	}
}

func TestPGDocMessageDenseFallbackAccountsForFailedProbeReadBudget(test *testing.T) {
	blocks := make([]domain.ContentBlock, 4500)
	for index := range blocks {
		blocks[index] = domain.ContentBlock{Type: "text"}
	}
	empty := domain.CIREvent{Kind: domain.EventMessage, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat(" ", 100000)}}}
	source, selector, bytesRead, _ := docFindFixture(test, []domain.CIREvent{{Kind: domain.EventMessage, Blocks: blocks}, empty, empty})
	entry, err := source.entry(0)
	if err != nil || entry.Length >= docViewBytes {
		test.Fatal("many-empty-block fixture is not small", entry.Length, err)
	}
	raw, err := source.read(entry.Offset, entry.Length)
	if err != nil {
		test.Fatal(err)
	}
	if display, err := domain.BuildDocEventDisplayWithin(context.Background(), raw, docFindDisplayBuildBytes); !errors.Is(err, domain.ErrDocFindDisplayUnavailable) || display != nil {
		test.Fatal("many small blocks no longer exceed aggregate formatter budget", err)
	}
	*bytesRead = 0
	generation := domain.DocDisplayProjectionGeneration{Version: 1, Source: domain.DocDisplayProjectionSource{
		Version: 1, Profile: domain.DocDisplayProjectionProfile, EventHash: entry.Hash, EventBytes: entry.Length, Plan: domain.HashContent([]byte("empty-block display plan")),
	}}
	descriptors := 0
	source.eventDisplayGeneration = func(selected domain.DocEventIndex) (domain.DocDisplayProjectionGeneration, error) {
		descriptors++
		if selected != entry.DocEventIndex {
			test.Fatal("ready small fast path opened preparation", selected)
		}
		return generation, nil
	}
	source.eventDisplayPage = func(domain.DocEventIndex, int) (domain.DocDisplayPage, error) {
		test.Fatal("empty prepared display loaded a page")
		return domain.DocDisplayPage{}, domain.ErrIntegrity
	}
	selector.Version, selector.Representation, selector.Prepared = 3, domain.DocFindPreparedRepresentation, &domain.DocFindPreparedProgress{}
	page, err := scanDocFindPage(context.Background(), source, selector, "needle")
	second, failure := source.entry(1)
	if err != nil || failure != nil || descriptors != 1 || page.Index != 2 || page.NextCursor == nil || *bytesRead != entry.Length+second.Length || *bytesRead > docFindScanBytes {
		test.Fatal("failed small probe was omitted from native read budget", page, *bytesRead, descriptors, err, failure)
	}
	test.Logf("failed_probe_bytes=%d next_event_bytes=%d total_source_bytes=%d budget=%d next_event=%d", entry.Length, second.Length, *bytesRead, docFindScanBytes, page.Index)
}

func TestPGDocMessageDenseFallbackKeepsReadySmallFastPath(test *testing.T) {
	for _, kind := range []domain.EventKind{domain.EventMessage, domain.EventReasoning} {
		event := domain.CIREvent{Kind: kind, RedactedSummary: strings.Repeat("**NEEDLE🙂** ", 45)}
		if kind == domain.EventMessage {
			event.RedactedSummary = ""
			event.Blocks = []domain.ContentBlock{{Type: "text", Text: strings.Repeat("**NEEDLE🙂** ", 45)}}
		}
		source, selector, bytesRead, _ := docFindFixture(test, []domain.CIREvent{event})
		descriptors := 0
		source.eventDisplayGeneration = func(domain.DocEventIndex) (domain.DocDisplayProjectionGeneration, error) {
			descriptors++
			return domain.DocDisplayProjectionGeneration{}, domain.ErrIntegrity
		}
		selector.Version, selector.Representation, selector.Prepared, selector.QueryHash = 3, domain.DocFindPreparedRepresentation, &domain.DocFindPreparedProgress{}, domain.HashContent([]byte("needle🙂"))
		matches := collectDocFind(test, source, selector, "needle🙂", bytesRead)
		if descriptors != 0 || len(matches) != 45 {
			test.Fatal("ready tiny display was forced into durable preparation", kind, descriptors, len(matches))
		}
	}
}

func TestPGDocMessageDenseFallbackDoesNotRetryFatalOrReadErrors(test *testing.T) {
	for _, failure := range []error{domain.ErrIntegrity, domain.ErrUnauthorized, domain.ErrForbidden, context.Canceled, context.DeadlineExceeded, domain.ErrDocFindDisplayUnavailable, errors.Join(domain.ErrDocFindDisplayUnavailable, domain.ErrForbidden)} {
		test.Run(failure.Error(), func(test *testing.T) {
			source, selector, _, _ := docFindFixture(test, []domain.CIREvent{denseDocDisplayEvent(domain.EventMessage, "read-error")})
			source.read = func(int, int) ([]byte, error) { return nil, failure }
			descriptors := 0
			source.eventDisplayGeneration = func(domain.DocEventIndex) (domain.DocDisplayProjectionGeneration, error) {
				descriptors++
				return domain.DocDisplayProjectionGeneration{}, domain.ErrNotFound
			}
			selector.Version, selector.Representation, selector.Prepared = 3, domain.DocFindPreparedRepresentation, &domain.DocFindPreparedProgress{}
			page, err := scanDocFindPage(context.Background(), source, selector, "needle")
			if !errors.Is(err, failure) || descriptors != 0 || !reflect.DeepEqual(page, domain.DocFindPage{}) {
				test.Fatal("fatal/read failure triggered fallback or partial progress", descriptors, page, err)
			}
			prefix, continuation, _, _ := docFindFixture(test, []domain.CIREvent{
				{Kind: domain.EventMessage, Blocks: []domain.ContentBlock{{Type: "text", Text: "needle"}}},
				denseDocDisplayEvent(domain.EventMessage, "error-after-prefix"),
			})
			entry, err := prefix.entry(1)
			if err != nil {
				test.Fatal(err)
			}
			read := prefix.read
			prefix.read = func(offset, length int) ([]byte, error) {
				if offset >= entry.Offset {
					return nil, failure
				}
				return read(offset, length)
			}
			prefix.eventDisplayGeneration = source.eventDisplayGeneration
			continuation.Version, continuation.Representation, continuation.Prepared = 3, domain.DocFindPreparedRepresentation, &domain.DocFindPreparedProgress{}
			page, err = scanDocFindPage(context.Background(), prefix, continuation, "needle")
			if !errors.Is(err, failure) || descriptors != 0 || !reflect.DeepEqual(page, domain.DocFindPage{}) {
				test.Fatal("native prefix concealed a fatal/read failure", descriptors, page, err)
			}
		})
	}
	source, selector, _, _ := docFindFixture(test, []domain.CIREvent{denseDocDisplayEvent(domain.EventMessage, "cancel")})
	read := source.read
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source.read = func(offset, length int) ([]byte, error) { cancel(); return read(offset, length) }
	descriptors := 0
	source.eventDisplayGeneration = func(domain.DocEventIndex) (domain.DocDisplayProjectionGeneration, error) {
		descriptors++
		return domain.DocDisplayProjectionGeneration{}, domain.ErrNotFound
	}
	selector.Version, selector.Representation, selector.Prepared = 3, domain.DocFindPreparedRepresentation, &domain.DocFindPreparedProgress{}
	page, err := scanDocFindPage(ctx, source, selector, "needle")
	if !errors.Is(err, context.Canceled) || descriptors != 0 || !reflect.DeepEqual(page, domain.DocFindPage{}) {
		test.Fatal("canceled formatting probe fell back to durable preparation", descriptors, err)
	}
}

func TestPGDocMessageDenseFallbackPreservesNativePrefixProgress(test *testing.T) {
	source, selector, bytesRead, _ := docFindFixture(test, []domain.CIREvent{
		{Kind: domain.EventMessage, Blocks: []domain.ContentBlock{{Type: "text", Text: "**needle**"}}},
		denseDocDisplayEvent(domain.EventMessage, "native-prefix"),
	})
	descriptors := 0
	source.eventDisplayGeneration = func(domain.DocEventIndex) (domain.DocDisplayProjectionGeneration, error) {
		descriptors++
		return domain.DocDisplayProjectionGeneration{}, &docDisplayPreparationRequired{}
	}
	source.eventDisplayPage = func(domain.DocEventIndex, int) (domain.DocDisplayPage, error) {
		return domain.DocDisplayPage{}, domain.ErrIntegrity
	}
	selector.Version, selector.Representation, selector.Prepared = 3, domain.DocFindPreparedRepresentation, &domain.DocFindPreparedProgress{}
	page, err := scanDocFindPage(context.Background(), source, selector, "needle")
	if err != nil || page.Index != 1 || len(page.Matches) != 1 || page.NextCursor == nil || page.Prepared.Generation != "" || descriptors != 0 || *bytesRead > docFindScanBytes {
		test.Fatal("dense fallback erased native prefix progress", page, descriptors, err)
	}
	next, err := domain.DecodeDocFindSelector(*page.NextCursor)
	if err != nil {
		test.Fatal(err)
	}
	page, err = scanDocFindPage(context.Background(), source, next, "needle")
	var missing *docDisplayPreparationRequired
	if !errors.As(err, &missing) || descriptors != 1 || !reflect.DeepEqual(page, domain.DocFindPage{}) {
		test.Fatal("native continuation did not demand the deferred dense event", page, descriptors, err)
	}
}
