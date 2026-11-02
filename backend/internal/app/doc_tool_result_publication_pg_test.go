//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGDocStructuredResultDemandPublishesAndFindsRootAndLegacy(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		test.Run(fmt.Sprintf("legacy=%t", legacy), func(test *testing.T) {
			fixture := newDocDisplayPGFixture(test)
			probe := &denseDocDisplayBlobProbe{PostgresStore: fixture.storage}
			fixture.service = NewService(fixture.storage, probe, nil, nil, fixture.storage)
			fixture.service.ConfigureDocumentDisplay(fixture.storage)
			event := domain.CIREvent{Kind: domain.EventToolResult, CallID: string(fixture.repo), Output: map[string]any{"payload": strings.Repeat("a", 150_000) + "NEEDLE🙂", "zero": 0, "false": false}}
			var hash domain.ContentHash
			request := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Query: "needle🙂", From: 0, To: 1, Legacy: legacy}
			if legacy {
				hash = seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{event}).Hash
				prepareLegacyFindGenerationPG(test, fixture, hash)
				request.Total = 1
			} else {
				hash = seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}})
				visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
				request.Selection = visible.ReplayCursor
			}
			countJobs := func() int {
				var count int
				if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&count); err != nil {
					test.Fatal(err)
				}
				return count
			}
			if _, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request); !errors.Is(err, domain.ErrConversationFindPreparationRequired) || countJobs() != 0 {
				test.Fatal("read-only demand wrote work", err)
			}
			preRejected, demands := false, 0
			var matches []domain.DocFindMatch
			for attempt := 0; attempt < 80; attempt++ {
				before := probe.rawReads
				page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
				if probe.rawReads != before {
					test.Fatal("prepared request fetched original conversation chunks")
				}
				if errors.Is(err, domain.ErrDocDisplayPending) {
					demands++
					if page.Matches != nil || page.Prepared != nil || page.NextCursor != nil {
						test.Fatal("pending request leaked staging")
					}
					restarted := NewService(fixture.storage, probe, nil, nil, fixture.storage)
					restarted.ConfigureDocumentDisplay(fixture.storage)
					if err := restarted.ProcessDocDisplayJobs(fixture.ctx, 1); errors.Is(err, domain.ErrDocFindDisplayUnavailable) {
						if preRejected {
							test.Fatal("structured event job was rejected after PRE fallback")
						}
						preRejected = true
					} else if err != nil {
						test.Fatal("restarted worker lost progress", err)
					}
					continue
				}
				if err != nil || page.Prepared == nil {
					test.Fatal("automatic structured prepared search failed", err)
				}
				matches = append(matches, page.Matches...)
				if page.NextCursor == nil {
					break
				}
				request = domain.DocFindPageRequest{Representation: request.Representation, Query: request.Query, Legacy: legacy, Cursor: *page.NextCursor}
			}
			if !preRejected || demands < 2 || len(matches) != 1 || matches[0].SourceRange == nil || matches[0].PreparedPage == nil || matches[0].Offset < 150_000 || countJobs() != 2 {
				test.Fatal("automatic fallback lost its scope or late match", preRejected, demands, matches, countJobs())
			}
			target := docDisplayTargetPG(test, fixture, hash, 0)
			target.Profile = domain.DocDisplayProjectionProfile
			generation, err := readDocEventDisplayGenerationPG(fixture, target)
			if err != nil || generation.Pages.Count < 2 {
				test.Fatal("structured result did not publish multiple pages", err)
			}
			var rejected, completed int
			if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FILTER(WHERE state='rejected' AND reason='unsupported_display' AND profile=$2),
 count(*) FILTER(WHERE state='completed' AND profile=$3) FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo, domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile).Scan(&rejected, &completed); err != nil || rejected != 1 || completed != 1 {
				test.Fatal("fallback did not preserve separate profile states", rejected, completed, err)
			}
			readOnly := NewService(fixture.storage, probe, nil, nil, fixture.storage)
			readOnly.ConfigureDocumentDisplayReads(fixture.storage)
			request.Cursor = ""
			request.From, request.To, request.Total = 0, 1, 1
			if !legacy {
				request.Total = 0
				visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
				request.Selection = visible.ReplayCursor
			}
			found := 0
			for attempt := 0; attempt < 20; attempt++ {
				page, err := readOnly.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
				if err != nil {
					test.Fatal("read-only prepared projection was not reusable", err)
				}
				found += len(page.Matches)
				if page.NextCursor == nil {
					break
				}
				request = domain.DocFindPageRequest{Representation: request.Representation, Query: request.Query, Legacy: legacy, Cursor: *page.NextCursor}
			}
			if found != 1 || countJobs() != 2 {
				test.Fatal("read-only projection read lost a match or created work", found, countJobs())
			}
			test.Logf("legacy=%t demands=%d published_pages=%d text_bytes=%d worker_source_reads=%d", legacy, demands, generation.Pages.Count, generation.Source.TextBytes, probe.rawReads)
		})
	}
}

func TestPGDocStructuredResultDemandCapabilityAndTerminalRejection(test *testing.T) {
	for _, preOnly := range []bool{true, false} {
		test.Run(fmt.Sprintf("PRE-only=%t", preOnly), func(test *testing.T) {
			fixture := newDocDisplayPGFixture(test)
			fixture.service.ConfigureDocumentDisplay(fixture.storage)
			if preOnly {
				fixture.service.ConfigureDocumentDisplayReads(&docPreparedFindReadProbe{DocDisplayReader: fixture.storage})
			} else {
				fixture.service.ConfigureDocumentDisplay(&unsupportedToolProjectionWorker{PostgresStore: fixture.storage})
			}
			event := domain.CIREvent{Kind: domain.EventToolResult, CallID: string(fixture.repo), Output: map[string]any{"opaque": strings.Repeat("value", 40_000)}}
			hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}})
			visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
			request := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Query: "hidden", Selection: visible.ReplayCursor, To: 1}
			terminal := false
			for attempt := 0; attempt < 20; attempt++ {
				_, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
				if errors.Is(err, domain.ErrDocFindDisplayUnavailable) {
					terminal = true
					break
				}
				if !errors.Is(err, domain.ErrDocDisplayPending) {
					test.Fatal("unexpected demand outcome", err)
				}
				if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil && !errors.Is(err, domain.ErrDocFindDisplayUnavailable) {
					test.Fatal(err)
				}
			}
			if !terminal {
				test.Fatal("unsupported demand did not reach a terminal state")
			}
			var jobs, rejected int
			var versions int64
			readCounts := func() {
				if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*),count(*) FILTER(WHERE state='rejected' AND reason='unsupported_display'),coalesce(sum(version),0)
 FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&jobs, &rejected, &versions); err != nil {
					test.Fatal(err)
				}
			}
			readCounts()
			want := 2
			if preOnly {
				want = 1
			}
			if jobs != want || rejected != want {
				test.Fatal("reader capability or terminal rejection was ignored", jobs, rejected, want)
			}
			previousVersions := versions
			for repeat := 0; repeat < 3; repeat++ {
				if _, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocFindDisplayUnavailable) {
					test.Fatal("terminal rejection changed on retry", err)
				}
			}
			readCounts()
			if jobs != want || rejected != want || versions != previousVersions {
				test.Fatal("ordinary demand reopened a rejected job", jobs, rejected, versions, previousVersions)
			}
		})
	}
}

type unsupportedToolProjectionWorker struct {
	*store.PostgresStore
}

func (*unsupportedToolProjectionWorker) LoadDocEventDisplay(context.Context, domain.DocDisplayJob) (domain.DocEventDisplayPreparation, error) {
	return domain.DocEventDisplayPreparation{}, domain.ErrDocFindDisplayUnavailable
}
