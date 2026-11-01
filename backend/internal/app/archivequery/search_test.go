package archivequery_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/archivequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func searchFixtureIndex(index domain.DocReadIndex, query string, after, limit int) []domain.DocEventIndex {
	hits := []domain.DocEventIndex{}
	for _, event := range index.Events {
		if event.Index > after && event.Text != "" && strings.Contains(strings.ToLower(event.Text), query) {
			hits = append(hits, event)
			if len(hits) == limit {
				break
			}
		}
	}
	return hits
}

func TestArchiveSearchPreservesOldestAttributionEventOrdinalsAndWire(test *testing.T) {
	for _, mode := range []string{"indexed-legacy", "legacy-source", "root"} {
		test.Run(mode, func(test *testing.T) {
			queries, ports, config := archiveFixture(test)
			root := mode == "root"
			inherited := archiveMessage("앞말 NEEDLE inherited", 0)
			parent := archiveDocument(test, ports, root, "origin", 0,
				inherited,
				domain.CIREvent{Kind: domain.EventToolCall, CallID: "lookup", ToolName: "synthetic", Input: map[string]any{"text": "needle hidden input"}},
				domain.CIREvent{Kind: domain.EventToolResult, CallID: "lookup", Output: "needle hidden output"},
				domain.CIREvent{Kind: domain.EventReasoning, Seq: 12, RedactedSummary: "needle 생각"}, inherited)
			childEvents := append([]domain.CIREvent{}, parent.CIR.Events...)
			childEvents = append(childEvents, archiveMessage("needle child 🌿", 27))
			child := archiveDocument(test, ports, root, "followup", 1, childEvents...)
			ports.snapshots[0].Message = "Needle commit"
			ports.snapshots[1].Message = "author match only"
			ports.snapshots[1].Author.Name = "NEEDLE author"
			ports.snapshots[0], ports.snapshots[1] = ports.snapshots[1], ports.snapshots[0]
			if root {
				ports.candidates = map[domain.ContentHash]bool{}
				ports.documents = nil
				ports.indexes = nil
			}
			if mode == "legacy-source" {
				config.Documents, _ = documentquery.NewRawReader(documentquery.RawConfig{Profile: documentquery.LegacyCompatibility, Snapshots: ports, Legacy: ports})
				queries, _ = archivequery.New(config)
			}
			output, err := queries.Search(context.Background(), inbound.SearchInput{RepoID: ports.repo, Query: "  NEEDLE  "})
			if err != nil || output.Truncated || len(output.Hits) != 5 || ports.query != "needle" || !reflect.DeepEqual(ports.scope.calls, []string{"document"}) {
				test.Fatal("search output, normalization, or scope changed", output, err, ports.calls)
			}
			if output.Hits[0].Kind != "commit" || output.Hits[0].SnapshotID != parent.Hash || output.Hits[1].Kind != "commit" || output.Hits[1].SnapshotID != child.Hash {
				test.Fatal("commit-before-event or oldest-first ordering changed", output.Hits)
			}
			ordinals := []int{0, 4, 5}
			owners := []domain.SessionDoc{parent, parent, child}
			events := []domain.CIREvent{parent.CIR.Events[0], parent.CIR.Events[3], child.CIR.Events[5]}
			for position, ordinal := range ordinals {
				hit := output.Hits[position+2]
				event := events[position]
				if hit.EventIndex == nil || *hit.EventIndex != ordinal || hit.SnapshotID != owners[position].Hash || hit.Seq != event.Seq || hit.Role != string(event.Role) || hit.Snippet != archivequery.SearchSnippet(domain.SearchableEventText(event), "needle") {
					test.Fatal("dedup, event ordinal, or readable metadata changed", hit)
				}
				if _, err := time.Parse(time.RFC3339, hit.CreatedAt); err != nil || !strings.HasSuffix(hit.CreatedAt, "Z") {
					test.Fatal("search timestamp is not UTC RFC3339", hit.CreatedAt, err)
				}
			}
			raw, err := json.Marshal(output)
			if err != nil {
				test.Fatal(err)
			}
			var wire struct {
				Hits []map[string]json.RawMessage `json:"hits"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				test.Fatal(err)
			}
			if _, exists := wire.Hits[0]["event_index"]; exists {
				test.Fatal("commit acquired a wire event index")
			}
			if string(wire.Hits[2]["event_index"]) != "0" {
				test.Fatal("zero event ordinal was omitted from wire output", string(raw))
			}
			verified := 0
			for _, call := range ports.calls {
				if call == "verified" {
					verified++
				}
			}
			if root && (verified != 2 || len(ports.pages) != 0) {
				test.Fatal("root source was reverified per page or searched through legacy index", ports.calls)
			}
		})
	}
}

func TestArchiveSearchCandidatesAreExplicitAndLegacyOnly(test *testing.T) {
	queries, ports, config := archiveFixture(test)
	legacy := archiveDocument(test, ports, false, "legacy", 0, archiveMessage("needle legacy", 0))
	root := archiveDocument(test, ports, true, "root", 1, archiveMessage("needle root", 1))
	ports.candidates = map[domain.ContentHash]bool{}
	output, err := queries.Search(context.Background(), inbound.SearchInput{RepoID: ports.repo, Query: "needle"})
	if err != nil || len(output.Hits) != 1 || output.Hits[0].SnapshotID != root.Hash || len(ports.pages) != 0 {
		test.Fatal("empty candidates incorrectly excluded roots or included legacy", output, err)
	}
	ports.calls, ports.scope.calls = nil, nil
	config.Candidates = nil
	queries, err = archivequery.New(config)
	if err != nil {
		test.Fatal(err)
	}
	output, err = queries.Search(context.Background(), inbound.SearchInput{RepoID: ports.repo, Query: "needle"})
	if err != nil || len(output.Hits) != 2 || output.Hits[0].SnapshotID != legacy.Hash {
		test.Fatal("omitted candidate capability was inferred from another port", output, err)
	}
	for _, call := range ports.calls {
		if call == "candidates" {
			test.Fatal("archive rediscovered an omitted candidate capability")
		}
	}
}

func TestArchiveSearchPagingDedupLimitsAndExactTruncation(test *testing.T) {
	queries, ports, _ := archiveFixture(test)
	events := make([]domain.CIREvent, 201)
	for position := range events {
		events[position] = archiveMessage("needle repeated", 0)
	}
	events[200] = archiveMessage("needle after duplicate page", 0)
	document := archiveDocument(test, ports, false, "main", 0, events...)
	output, err := queries.Search(context.Background(), inbound.SearchInput{RepoID: ports.repo, Query: "needle", Limit: 2})
	if err != nil || len(output.Hits) != 2 || output.Truncated || output.Hits[1].EventIndex == nil || *output.Hits[1].EventIndex != 200 {
		test.Fatal("duplicate pages changed exact-limit semantics", output, err)
	}
	if !reflect.DeepEqual(ports.pages, []archivePage{{hash: document.Hash, after: -1, limit: 200}, {hash: document.Hash, after: 199, limit: 200}}) {
		test.Fatal("bounded page size or after ordinal changed", ports.pages)
	}
	queries, ports, _ = archiveFixture(test)
	for position := range events {
		events[position] = archiveMessage(fmt.Sprintf("needle %03d", position), position)
	}
	archiveDocument(test, ports, false, "main", 0, events...)
	for _, limit := range []int{0, -1, 201, 1, 200} {
		output, err := queries.Search(context.Background(), inbound.SearchInput{RepoID: ports.repo, Query: "needle", Limit: limit})
		want := limit
		if limit <= 0 || limit > 200 {
			want = 50
		}
		if err != nil || len(output.Hits) != want || !output.Truncated {
			test.Fatal("default/max limit or overflow detection changed", limit, output, err)
		}
	}
}

func TestArchiveSearchPreservesPartialErrorPolicyAndFailsClosedRoots(test *testing.T) {
	for _, atomic := range []bool{false, true} {
		for _, stage := range []string{"snapshots", "candidates", "snapshot", "verified", "events"} {
			test.Run(fmt.Sprintf("atomic=%v/%s", atomic, stage), func(test *testing.T) {
				_, ports, config := archiveFixture(test)
				config.AtomicRead = atomic
				root := stage == "verified"
				archiveDocument(test, ports, root, "main", 0, archiveMessage("needle body", 0))
				ports.snapshots[0].Message = "needle commit"
				ports.failStage, ports.failure = stage, domain.ErrIntegrity
				queries, err := archivequery.New(config)
				if err != nil {
					test.Fatal(err)
				}
				output, err := queries.Search(context.Background(), inbound.SearchInput{RepoID: ports.repo, Query: "needle"})
				want := 1
				if atomic || stage == "snapshots" {
					want = 0
				}
				if !errors.Is(err, domain.ErrIntegrity) || len(output.Hits) != want || output.Truncated {
					test.Fatal("source error, partial output, or atomic rollback changed", output, err)
				}
				if root {
					for _, call := range ports.calls {
						if call == "legacy" || call == "events" || call == "index" {
							test.Fatal("root error silently fell back to another source", ports.calls)
						}
					}
				}
			})
		}
	}
	queries, ports, _ := archiveFixture(test)
	archiveDocument(test, ports, true, "main", 0, archiveMessage("needle", 0))
	ports.failStage, ports.failure = "snapshot", domain.ErrNotFound
	if _, err := queries.Search(context.Background(), inbound.SearchInput{RepoID: ports.repo, Query: "needle"}); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("missing selected root snapshot was treated as legacy", err)
	}
}

func TestArchiveSearchRootUsesCurrentProofAndOwnedProjection(test *testing.T) {
	queries, ports, _ := archiveFixture(test)
	document := archiveDocument(test, ports, true, "main", 0, archiveMessage("needle owned", 0))
	ports.verifiedAfter = func() { delete(ports.proofs, document.Hash) }
	first, err := queries.Search(context.Background(), inbound.SearchInput{RepoID: ports.repo, Query: "needle"})
	if err != nil || len(first.Hits) != 1 {
		test.Fatal("search lost its owned verified projection", first, err)
	}
	if _, err := queries.Search(context.Background(), inbound.SearchInput{RepoID: ports.repo, Query: "needle"}); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("search reused a stale root proof on another request", err)
	}
}

func TestArchiveSearchValidationEmptyWireAndSnippet(test *testing.T) {
	queries, ports, _ := archiveFixture(test)
	for _, query := range []string{"", "x", strings.Repeat("한", 257)} {
		if _, err := queries.Search(context.Background(), inbound.SearchInput{RepoID: ports.repo, Query: query}); !errors.Is(err, domain.ErrValidation) || len(ports.calls) != 0 {
			test.Fatal("invalid term reached metadata", err, ports.calls)
		}
	}
	output, err := queries.Search(context.Background(), inbound.SearchInput{RepoID: ports.repo, Query: "없는"})
	raw, marshalErr := json.Marshal(output)
	if err != nil || marshalErr != nil || string(raw) != `{"hits":[],"truncated":false}` {
		test.Fatal("empty search wire changed", string(raw), err, marshalErr)
	}
	text := strings.Repeat("가", 80) + "\nNEEDLE\t" + strings.Repeat("나", 180)
	snippet := archivequery.SearchSnippet(text, "needle")
	if !strings.HasPrefix(snippet, "…") || !strings.HasSuffix(snippet, "…") || len([]rune(snippet)) != 122 || strings.ContainsAny(snippet, "\n\t") || !strings.Contains(snippet, "NEEDLE") {
		test.Fatal("rune-safe normalized snippet contract changed", snippet)
	}
}
