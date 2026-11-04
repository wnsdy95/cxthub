//go:build postgres

package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGDocToolPolicyAutomaticDemandAndReadOnlySearch(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, name := range []string{"Edit", "Write", "Bash", "generic"} {
			test.Run(fmt.Sprintf("%s/legacy=%t", name, legacy), func(test *testing.T) {
				fixture := newDocDisplayPGFixture(test)
				probe := &denseDocDisplayBlobProbe{PostgresStore: fixture.storage}
				fixture.service = NewService(fixture.storage, probe, nil, nil, fixture.storage)
				fixture.service.ConfigureDocumentDisplay(fixture.storage)
				text := strings.Repeat("a", 150_000) + "NEEDLE🙂\nlast line\n"
				input := map[string]any{"file_path": "/long/project/src/tool/fixture.txt"}
				switch name {
				case "Edit":
					input["old_string"], input["new_string"] = text, "replacement"
				case "Write":
					input["content"] = text
				case "Bash":
					input["command"] = text
				default:
					input["values"] = []any{text, false, 0}
				}
				event := domain.CIREvent{Kind: domain.EventToolCall, ToolName: name, CallID: string(fixture.repo), Input: input}
				var hash domain.ContentHash
				request := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Query: "needle🙂", To: 1, Legacy: legacy}
				if legacy {
					hash = seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{event}).Hash
					prepareLegacyFindGenerationPG(test, fixture, hash)
					request.Total = 1
				} else {
					hash = seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}})
					visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
					request.Selection = visible.ReplayCursor
				}
				if _, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request); !errors.Is(err, domain.ErrConversationFindPreparationRequired) {
					test.Fatal("read-only call demand did not preserve no-enqueue semantics", err)
				}
				var jobs int
				if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&jobs); err != nil || jobs != 0 {
					test.Fatal("read-only call demand created a job", jobs, err)
				}
				var matches []domain.DocFindMatch
				batches := 0
				for attempt := 0; attempt < 100; attempt++ {
					before := probe.rawReads
					page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
					if probe.rawReads != before {
						test.Fatal("oversized call search fetched original chunks")
					}
					if errors.Is(err, domain.ErrDocDisplayPending) {
						if page.Matches != nil || page.NextCursor != nil {
							test.Fatal("call staging leaked into search")
						}
						restarted := NewService(fixture.storage, probe, nil, nil, fixture.storage)
						restarted.ConfigureDocumentDisplay(fixture.storage)
						if err := restarted.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
							test.Fatal("call worker lost a durable phase", batches, err)
						}
						batches++
						continue
					}
					if err != nil || page.Prepared == nil {
						test.Fatal("prepared call search failed", err)
					}
					matches = append(matches, page.Matches...)
					if page.NextCursor == nil {
						break
					}
					request = domain.DocFindPageRequest{Representation: request.Representation, Query: request.Query, Legacy: legacy, Cursor: *page.NextCursor}
				}
				raw, err := json.Marshal(event)
				if err != nil {
					test.Fatal(err)
				}
				displayed := name + "(…/src/tool/fixture.txt)"
				switch name {
				case "Edit":
					displayed += "\n− " + strings.ReplaceAll(text, "\n", "\n− ") + "\n+ replacement"
				case "Write":
					displayed += "\n+ " + strings.ReplaceAll(text, "\n", "\n+ ")
				case "Bash":
					displayed += "\n$ " + text
				default:
					body, err := json.MarshalIndent(input, "", "  ")
					if err != nil {
						test.Fatal(err)
					}
					displayed += "\n" + string(body)
				}
				expected, err := domain.FindDocText(fixture.ctx, displayed, "needle🙂", 0, domain.DocFindPageMatches)
				if err != nil || len(expected) == 0 || len(matches) != len(expected) || batches == 0 {
					test.Fatal("call policy lost late matches", batches, len(matches), len(expected), err)
				}
				for index, match := range matches {
					from := bytes.Index(raw, []byte("NEEDLE🙂"))
					span := domain.DocFindSpan{From: from, To: from + len("NEEDLE🙂")}
					if from < 0 || match.Offset != expected[index].Offset || match.Length != expected[index].Length || match.SourceRange == nil || *match.SourceRange != span || match.PreparedPage == nil || match.Raw {
						test.Fatal("call search changed rendered coordinates", match, expected[index])
					}
				}
				probe.deny = true
				readOnly := NewService(fixture.storage, probe, nil, nil, fixture.storage)
				readOnly.ConfigureDocumentDisplayReads(fixture.storage)
				request.Cursor = ""
				request.From, request.To, request.Total = 0, 1, 0
				if legacy {
					request.Total = 1
				} else {
					visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
					request.Selection = visible.ReplayCursor
				}
				for attempt := 0; attempt < 20; attempt++ {
					page, err := readOnly.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
					if err != nil {
						test.Fatal("read-only call projection read original bodies", err)
					}
					if page.NextCursor == nil {
						break
					}
					request = domain.DocFindPageRequest{Representation: request.Representation, Query: request.Query, Legacy: legacy, Cursor: *page.NextCursor}
				}
				if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_jobs_v1 WHERE repo_id=$1 AND state='completed' AND profile=$2`, fixture.repo, domain.DocDisplayProjectionProfile).Scan(&jobs); err != nil || jobs != 1 {
					test.Fatal("call did not complete exactly one event-profile job", jobs, err)
				}
				test.Logf("name=%s legacy=%t batches=%d matches=%d display_bytes=%d worker_source_reads=%d", name, legacy, batches, len(matches), len(displayed), probe.rawReads)
			})
		}
	}
}

func TestPGDocToolPolicyStringCursorSurvivesLaterPREPublication(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		test.Run(fmt.Sprintf("legacy=%t", legacy), func(test *testing.T) {
			fixture := newDocDisplayPGFixture(test)
			fixture.service.ConfigureDocumentDisplay(fixture.storage)
			event := domain.CIREvent{Kind: domain.EventToolResult, CallID: string(fixture.repo), Output: strings.Repeat("a", 200_000) + "NEEDLE🙂"}
			var hash domain.ContentHash
			request := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Query: "needle🙂", To: 1, Legacy: legacy}
			if legacy {
				hash = seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{event}).Hash
				prepareLegacyFindGenerationPG(test, fixture, hash)
				request.Total = 1
			} else {
				hash = seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}})
				visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
				request.Selection = visible.ReplayCursor
			}
			target := docDisplayTargetPG(test, fixture, hash, 0)
			target.Profile = domain.DocDisplayProjectionProfile
			if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
				test.Fatal(err)
			}
			var projected domain.DocDisplayProjectionGeneration
			for attempt := 0; attempt < 40; attempt++ {
				if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
					test.Fatal(err)
				}
				var err error
				projected, err = readDocEventDisplayGenerationPG(fixture, target)
				if err == nil {
					break
				}
				if !errors.Is(err, domain.ErrNotFound) {
					test.Fatal(err)
				}
			}
			page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
			projectedRaw, _ := projected.Encode()
			if err != nil || page.NextCursor == nil || page.Prepared == nil || page.Prepared.Generation != domain.HashContent(projectedRaw) {
				test.Fatal("event profile did not provide a pinned continuation", err, page)
			}
			cursor := *page.NextCursor
			target.Profile = ""
			if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
				test.Fatal(err)
			}
			pre := domain.DocDisplayStringGeneration{}
			for attempt := 0; attempt < 40; attempt++ {
				if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
					test.Fatal(err)
				}
				var err error
				pre, err = readDocDisplayGenerationPG(fixture, target)
				if err == nil {
					break
				}
				if !errors.Is(err, domain.ErrNotFound) {
					test.Fatal(err)
				}
			}
			preRaw, err := pre.Encode()
			if err != nil || domain.HashContent(preRaw) == domain.HashContent(projectedRaw) {
				test.Fatal("PRE did not publish a distinct valid profile", err)
			}
			fresh, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
			if err != nil || fresh.Prepared == nil || fresh.Prepared.Generation != domain.HashContent(preRaw) {
				test.Fatal("new requests stopped preferring existing PRE", err)
			}
			request = domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Query: "needle🙂", Legacy: legacy, Cursor: cursor}
			found := 0
			for attempt := 0; attempt < 20; attempt++ {
				page, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
				if err != nil {
					test.Fatal("later PRE publication displaced the pinned event generation", err)
				}
				found += len(page.Matches)
				if page.NextCursor == nil {
					break
				}
				if page.Prepared == nil || page.Prepared.Generation != domain.HashContent(projectedRaw) {
					test.Fatal("continuation silently changed profile")
				}
				request.Cursor = *page.NextCursor
			}
			if found != 1 {
				test.Fatal("pinned continuation lost or duplicated the late match", found)
			}
		})
	}
}
