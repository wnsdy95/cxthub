//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func messageDisplayPGEvent(mode, prefix string, padding int) domain.CIREvent {
	if mode == "seed" {
		prefix = "[cxt seed] Branch-switch context: " + prefix
	}
	return domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleAssistant, CompactSummary: mode == "compact",
		Blocks: []domain.ContentBlock{
			{Type: "text", Text: prefix + strings.Repeat("a", padding)},
			{Type: "text", Text: "**NEEDLE🙂"},
			{Type: "text", Text: "TAIL**"},
			{Type: "**asset**", Text: "summary-slot-only"},
			{Type: "text"},
			{Type: "text", Text: "after [label](https://hidden-needle.invalid)"},
		}}
}

func TestPGDocMessageDisplayFindRestartsOrdinaryCompactAndSeed(test *testing.T) {
	for _, mode := range []string{"ordinary", "compact", "seed"} {
		for _, legacy := range []bool{false, true} {
			name := mode + "/root"
			if legacy {
				name = mode + "/legacy"
			}
			test.Run(name, func(test *testing.T) {
				fixture := newDocDisplayPGFixture(test)
				fixture.cir.Envelope.CIRVersion = "2"
				fixture.service.ConfigureDocumentDisplay(fixture.storage)
				event := messageDisplayPGEvent(mode, "public-prefix ", 150_000)
				event.AgentMessage = true
				event.Locked = &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: string(fixture.repo) + strings.Repeat("opaque-only ", 50_000)}
				document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}}
				document.Envelope.CIRVersion = "2"
				var hash domain.ContentHash
				request := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Query: "needle🙂", To: 1, Legacy: legacy}
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
					test.Fatal("message did not demand durable preparation", err)
				}
				if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
					test.Fatal(err)
				}
				if _, err := readDocEventDisplayGenerationPG(fixture, target); !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("partial message became readable", err)
				}
				var generation domain.DocDisplayProjectionGeneration
				batches := 1
				for ; batches < 150; batches++ {
					restarted := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
					restarted.ConfigureDocumentDisplay(fixture.storage)
					if err := restarted.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
						test.Fatal("message checkpoint did not resume", batches, err)
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
					test.Fatal("message preparation did not converge", batches, generation)
				}
				var display strings.Builder
				if _, err := repositoryReadForRepo(fixture.ctx, fixture.service, fixture.repo, func(read context.Context) (bool, error) {
					for index := 0; index < generation.Pages.Count; index++ {
						page, err := fixture.storage.ReadDocEventDisplayPage(read, target, index)
						if err != nil {
							return false, err
						}
						display.WriteString(page.Text)
					}
					return true, nil
				}); err != nil {
					test.Fatal(err)
				}
				small := messageDisplayPGEvent(mode, "public-prefix ", 3)
				raw, err := json.Marshal(small)
				if err != nil {
					test.Fatal(err)
				}
				oracle, err := domain.BuildDocEventDisplay(fixture.ctx, raw)
				if err != nil {
					test.Fatal(err)
				}
				want := strings.Replace(oracle.Text(), "public-prefix aaa", "public-prefix "+strings.Repeat("a", 150_000), 1)
				if display.String() != want || strings.Contains(display.String(), "opaque-only") || strings.Contains(display.String(), "hidden-needle") {
					test.Fatal("message/summary semantics changed", mode, display.Len(), len(want))
				}
				var sourceRaw []byte
				if err := fixture.pool.QueryRow(fixture.ctx, `SELECT bytes FROM doc_display_sources_v1 WHERE event_hash=$1 AND profile=$2`, target.Event.Hash, target.DisplayProfile()).Scan(&sourceRaw); err != nil {
					test.Fatal(err)
				}
				var source domain.DocEventDisplaySource
				if json.Unmarshal(sourceRaw, &source) != nil || source.Validate() != nil || source.Kind != domain.EventMessage || source.Summary != (mode != "ordinary") {
					test.Fatal("authoritative summary mode was lost", source)
				}
				var matches []domain.DocFindMatch
				finished := false
				for attempt := 0; attempt < 50; attempt++ {
					page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
					if err != nil {
						test.Fatal(err)
					}
					matches = append(matches, page.Matches...)
					if page.NextCursor == nil {
						finished = true
						break
					}
					request = domain.DocFindPageRequest{Representation: request.Representation, Query: request.Query, Legacy: legacy, Cursor: *page.NextCursor}
				}
				if !finished || len(matches) != 1 || matches[0].Offset <= 128<<10 || matches[0].SourceRange == nil || matches[0].PreparedPage == nil || matches[0].Raw {
					test.Fatal("prepared message find lost late public text or source provenance", matches)
				}
				test.Logf("mode=%s legacy=%t batches=%d pages=%d display=%d", mode, legacy, batches+1, generation.Pages.Count, display.Len())
			})
		}
	}
}
