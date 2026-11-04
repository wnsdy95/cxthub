package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func putSearchPositionDoc(test *testing.T, storage *store.FSStore, repo domain.ContentHash, events ...domain.CIREvent) domain.SessionDoc {
	test.Helper()
	document := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: historyEnvelope(), Events: append([]domain.CIREvent{}, events...)}}
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	document.Hash = domain.HashContent(canonical)
	if _, err := storage.PutDoc(systemTestContext(), repo, document); err != nil {
		test.Fatal(err)
	}
	return document
}

func TestSearchEventIndexMatchesReadableEvents(suite *testing.T) {
	for _, scenario := range []struct {
		name           string
		identity       domain.DocumentIdentity
		sequenceOffset int
	}{
		{name: "legacy_zero_and_duplicate_sequences", identity: domain.DocumentIdentityLegacy},
		{name: "legacy_renumbered_sequences", identity: domain.DocumentIdentityLegacy, sequenceOffset: 700},
		{name: "verified_root", identity: domain.DocumentIdentityRootV1},
	} {
		suite.Run(scenario.name, func(test *testing.T) {
			ctx, repo, directory := systemTestContext(), h('1'), test.TempDir()
			storage := store.NewFSStore(directory)
			service := NewService(storage, storage, nil, nil, nil)
			events := []domain.CIREvent{
				historyMessage(domain.RoleUser, "needle first message"),
				{Kind: domain.EventToolCall, CallID: "lookup", ToolName: "synthetic", Input: map[string]any{"text": "needle archival input"}},
				{Kind: domain.EventToolResult, CallID: "lookup", Output: "needle archival output"},
				historyMessage(domain.RoleAssistant, "needle reply"),
				{Kind: domain.EventReasoning, RedactedSummary: "needle reasoning"},
				{Kind: domain.EventMessage, Seq: 41, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: "needle last message"}}},
			}
			for position := range events {
				events[position].Seq += scenario.sequenceOffset
			}
			var document domain.SessionDoc
			if scenario.identity == domain.DocumentIdentityRootV1 {
				document, _ = seedRootReadDocWithIdentity(test, directory, storage, repo, nil, events...)
			} else {
				document = putSearchPositionDoc(test, storage, repo, events...)
			}
			snapshot := domain.Snapshot{
				ID: document.Hash, RepoID: repo, DocHash: document.Hash, DocIdentity: document.Identity,
				Branch: "main", Message: "needle commit", CreatedAt: time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC),
			}
			if err := storage.PutSnapshot(ctx, snapshot); err != nil {
				test.Fatal(err)
			}
			output, err := service.Search(ctx, inbound.SearchInput{RepoID: repo, Query: "NEEDLE"})
			if err != nil {
				test.Fatal(err)
			}
			positions := []int{0, 3, 4, 5}
			if len(output.Hits) != len(positions)+1 || output.Truncated {
				test.Fatalf("unexpected readable search results: %+v", output)
			}
			if output.Hits[0].Kind != "commit" || output.Hits[0].EventIndex != nil {
				test.Fatalf("commit acquired an event ordinal: %+v", output.Hits[0])
			}
			for resultIndex, position := range positions {
				hit := output.Hits[resultIndex+1]
				event := document.CIR.Events[position]
				if hit.Kind != "event" || hit.EventIndex == nil || *hit.EventIndex != position {
					test.Fatalf("event ordinal %d lost: %+v", position, hit)
				}
				if hit.SnapshotID != snapshot.ID || hit.Branch != snapshot.Branch || hit.CreatedAt != snapshot.CreatedAt.Format(time.RFC3339) || hit.Seq != event.Seq || hit.Role != string(event.Role) || hit.Snippet != searchSnippet(domain.SearchableEventText(event), "needle") {
					test.Fatalf("event %d attribution or readable metadata changed: %+v", position, hit)
				}
				page, err := service.ReadDocEvents(ctx, repo, document.Hash, "", *hit.EventIndex, 1)
				if err != nil || page.Offset != position || len(page.Events) != 1 {
					test.Fatalf("event ordinal %d did not resolve to the matched event: %+v error=%v", position, page, err)
				}
				if resolved := page.Events[0]; resolved.Kind != event.Kind || resolved.Seq != event.Seq || resolved.Role != event.Role || domain.SearchableEventText(resolved) != domain.SearchableEventText(event) {
					test.Fatalf("event ordinal %d resolved to a different message or reasoning event: %+v", position, resolved)
				}
			}
			encoded, err := json.Marshal(output)
			if err != nil {
				test.Fatal(err)
			}
			var wire struct {
				Hits []map[string]json.RawMessage `json:"hits"`
			}
			if err := json.Unmarshal(encoded, &wire); err != nil {
				test.Fatal(err)
			}
			if _, present := wire.Hits[0]["event_index"]; present {
				test.Fatal("commit event_index must be omitted")
			}
			for resultIndex, position := range positions {
				var ordinal int
				if err := json.Unmarshal(wire.Hits[resultIndex+1]["event_index"], &ordinal); err != nil || ordinal != position {
					test.Fatalf("wire event ordinal %d missing or changed: %s error=%v", position, encoded, err)
				}
			}
		})
	}
}

func TestSearchEventIndexDeduplicatesSnapshotAttribution(test *testing.T) {
	ctx, repo := systemTestContext(), h('1')
	storage := store.NewFSStore(test.TempDir())
	service := NewService(storage, storage, nil, nil, nil)
	inherited := domain.CIREvent{Kind: domain.EventMessage, Seq: 7, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "needle inherited"}}}
	parent := putSearchPositionDoc(test, storage, repo,
		historyMessage(domain.RoleUser, "setup"),
		domain.CIREvent{Kind: domain.EventToolCall, CallID: "lookup", ToolName: "synthetic"},
		inherited, inherited,
		domain.CIREvent{Kind: domain.EventReasoning, Seq: 9, RedactedSummary: "needle rationale"},
	)
	childEvents := append([]domain.CIREvent{}, parent.CIR.Events...)
	childEvents = append(childEvents, domain.CIREvent{Kind: domain.EventMessage, Seq: 30, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: "needle appended"}}})
	child := putSearchPositionDoc(test, storage, repo, childEvents...)
	sharedEvents := append([]domain.CIREvent{}, parent.CIR.Events...)
	sharedEvents = append(sharedEvents, domain.CIREvent{Kind: domain.EventMessage, Seq: 10, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: "unrelated sibling"}}})
	shared := putSearchPositionDoc(test, storage, repo, sharedEvents...)
	created := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	snapshots := []domain.Snapshot{
		{ID: parent.Hash, RepoID: repo, DocHash: parent.Hash, Branch: "origin", Message: "initial", CreatedAt: created},
		{ID: shared.Hash, RepoID: repo, DocHash: shared.Hash, Branch: "shared", Message: "shared history", Parents: []domain.ContentHash{parent.Hash}, CreatedAt: created.Add(time.Minute)},
		{ID: child.Hash, RepoID: repo, DocHash: child.Hash, Branch: "followup", Message: "extended", Parents: []domain.ContentHash{shared.Hash}, CreatedAt: created.Add(2 * time.Minute)},
	}
	for snapshotIndex := len(snapshots) - 1; snapshotIndex >= 0; snapshotIndex-- {
		if err := storage.PutSnapshot(ctx, snapshots[snapshotIndex]); err != nil {
			test.Fatal(err)
		}
	}
	positions := []int{2, 4, 5}
	owners := []domain.Snapshot{snapshots[0], snapshots[0], snapshots[2]}
	for attempt := 0; attempt < 3; attempt++ {
		output, err := service.Search(ctx, inbound.SearchInput{RepoID: repo, Query: "needle"})
		if err != nil {
			test.Fatal(err)
		}
		if len(output.Hits) != len(positions) || output.Truncated {
			test.Fatalf("duplicate or inherited results changed: %+v", output)
		}
		for resultIndex, position := range positions {
			hit, owner := output.Hits[resultIndex], owners[resultIndex]
			if hit.Kind != "event" || hit.EventIndex == nil || *hit.EventIndex != position || hit.SnapshotID != owner.ID || hit.Branch != owner.Branch || hit.CreatedAt != owner.CreatedAt.Format(time.RFC3339) {
				test.Fatalf("attempt %d event %d lost oldest-snapshot attribution: %+v", attempt, position, hit)
			}
			page, err := service.ReadDocEvents(ctx, repo, owner.DocHash, "", *hit.EventIndex, 1)
			if err != nil || len(page.Events) != 1 {
				test.Fatalf("attributed event %d cannot be read: %+v error=%v", position, page, err)
			}
			if event := page.Events[0]; hit.Seq != event.Seq || hit.Role != string(event.Role) || hit.Snippet != searchSnippet(domain.SearchableEventText(event), "needle") {
				test.Fatalf("attributed ordinal %d resolves to a different event: %+v", position, hit)
			}
		}
	}
}
