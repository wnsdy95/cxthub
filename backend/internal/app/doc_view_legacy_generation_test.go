package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func docViewLegacyGenerationEventBytes(test *testing.T, doc domain.SessionDoc, position int) []byte {
	test.Helper()
	verified, err := domain.VerifySessionDoc(doc)
	if err != nil {
		test.Fatal(err)
	}
	plan, err := verified.PlanReadIndexContext(systemTestContext())
	if err != nil {
		test.Fatal(err)
	}
	index, err := plan.BuildMetadataContext(systemTestContext())
	if err != nil {
		test.Fatal(err)
	}
	entry := index.Events[position]
	raw, err := verified.EventStreamRange(systemTestContext(), entry.Offset, entry.Length)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func assertDocViewLegacyGenerationSelectedCost(test *testing.T, cost legacyFindGenerationCosts) {
	test.Helper()
	if cost.opens != 1 || cost.pages != 1 || cost.directories > 2 ||
		cost.metadataBytes > domain.MaxDocRangeGenerationBytes+domain.MaxDocRangePageBytes+2*domain.MaxDocRangeDirectoryBytes ||
		cost.manifests != 1 || cost.manifestBytes == 0 || cost.chunks < 1 || cost.chunks > 2 ||
		cost.chunkBytes > 2*domain.ChunkTarget || cost.indexes != 0 || cost.whole != 0 || cost.boundaries != 0 || cost.writes != 0 {
		test.Fatal("ready legacy read bypassed selected generation metadata", cost)
	}
}

func assertDocViewLegacyGenerationFailure(test *testing.T, service *Service, storage *legacyFindGenerationStore, hash domain.ContentHash, failure error) {
	test.Helper()
	ctx := outbound.WithDocReadOnly(systemTestContext())
	storage.cost = legacyFindGenerationCosts{}
	page, err := service.ReadDocViewEvents(ctx, storage.repo, hash, "", 0, 1, "chat")
	if !errors.Is(err, failure) || len(page.Events) != 0 || storage.cost.indexes != 0 || storage.cost.whole != 0 ||
		storage.cost.manifests != 0 || storage.cost.chunks != 0 || storage.cost.writes != 0 {
		test.Fatal("legacy body failure selected compatibility or returned content", page, storage.cost, err)
	}
	storage.cost = legacyFindGenerationCosts{}
	fragment, err := service.ReadDocViewFragment(ctx, storage.repo, hash, 0, 0, 64)
	if !errors.Is(err, failure) || fragment != (domain.DocViewFragment{}) || storage.cost.indexes != 0 || storage.cost.whole != 0 ||
		storage.cost.manifests != 0 || storage.cost.chunks != 0 || storage.cost.writes != 0 {
		test.Fatal("legacy fragment failure selected compatibility or returned content", fragment, storage.cost, err)
	}
}

func TestDocViewLegacyGenerationReadyBodyAndFragmentCostsStaySelected(test *testing.T) {
	var bodyCosts, fragmentCosts []legacyFindGenerationCosts
	for _, count := range []int{2048, 20480} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			storage := newLegacyFindGenerationStore(test)
			events := make([]domain.CIREvent, count)
			for index := range events {
				events[index] = historyMessage(domain.RoleUser, fmt.Sprintf("legacy row %08d: ", index)+strings.Repeat("selected archival body ", 8))
			}
			doc := storage.seed(test, events)
			service := NewService(storage, storage, nil, nil, nil)
			ctx := outbound.WithDocReadOnly(systemTestContext())
			start, limit := count-32, 20
			for _, mode := range []string{"chat", "prompts", "all", "raw"} {
				storage.cost = legacyFindGenerationCosts{}
				page, err := service.ReadDocViewEvents(ctx, storage.repo, doc.Hash, "", start, limit, mode)
				if err != nil || page.Hash != doc.Hash || page.DocIdentity != domain.DocumentIdentityLegacy ||
					page.Total != count || page.Offset != start || page.Next != start+limit || page.Inherited != 0 || len(page.Events) != limit ||
					!reflect.DeepEqual(page.Envelope, doc.CIR.Envelope) {
					test.Fatal("ready legacy body changed identity, envelope or selected range", mode, page, err)
				}
				for position, event := range page.Events {
					if event.Index != start+position || event.Deferred || !reflect.DeepEqual(event.Event, doc.CIR.Events[start+position]) {
						test.Fatal("ready legacy body changed archival event", mode, position, event)
					}
				}
				assertDocViewLegacyGenerationSelectedCost(test, storage.cost)
				test.Logf("events=%d mode=%s body cost=%+v", count, mode, storage.cost)
				if mode == "chat" {
					bodyCosts = append(bodyCosts, storage.cost)
				}
			}
			index := count - 1
			raw := docViewLegacyGenerationEventBytes(test, doc, index)
			for _, offset := range []int{0, 64, len(raw) - 17} {
				storage.cost = legacyFindGenerationCosts{}
				fragment, err := service.ReadDocViewFragment(ctx, storage.repo, doc.Hash, index, offset, 64)
				end := min(offset+64, len(raw))
				next := end
				if end == len(raw) {
					next = -1
				}
				if err != nil || fragment.Index != index || fragment.Offset != offset || fragment.Total != len(raw) ||
					fragment.Next != next || fragment.JSON != string(raw[offset:end]) {
					test.Fatal("ready legacy fragment changed archival bytes or continuation", fragment, err)
				}
				assertDocViewLegacyGenerationSelectedCost(test, storage.cost)
				test.Logf("events=%d offset=%d fragment cost=%+v", count, offset, storage.cost)
				if offset == 0 {
					fragmentCosts = append(fragmentCosts, storage.cost)
				}
			}
		})
	}
	for _, costs := range [][]legacyFindGenerationCosts{bodyCosts, fragmentCosts} {
		if len(costs) != 2 || costs[1].metadataBytes >= costs[0].metadataBytes*2 || costs[1].manifestBytes <= costs[0].manifestBytes {
			test.Fatal("selected metadata must stay bounded while full manifest growth remains visible", costs)
		}
	}
	test.Log("Adapter counters do not include SQL directory work, peak RSS or cold fallback. Full legacy manifests and verified physical chunks remain retained read costs; this is not full R1 cost acceptance.")
}

func TestDocViewLegacyGenerationErrorsNeverSelectCompatibility(test *testing.T) {
	for _, failure := range []error{domain.ErrIntegrity, domain.ErrNotFound, domain.ErrForbidden, domain.ErrConflict,
		domain.ErrUnsupportedDocumentIdentity, context.Canceled, context.DeadlineExceeded} {
		test.Run(failure.Error(), func(test *testing.T) {
			storage := newLegacyFindGenerationStore(test)
			doc := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "ready legacy body")})
			storage.fixtures[doc.Hash].failure = fmt.Errorf("current generation: %w", failure)
			service := NewService(storage, storage, nil, nil, nil)
			assertDocViewLegacyGenerationFailure(test, service, storage, doc.Hash, failure)
		})
	}
}

func TestDocViewLegacyGenerationMissingOrUnpinnedKeepsExplicitCompatibility(test *testing.T) {
	for _, missing := range []bool{false, true} {
		test.Run(fmt.Sprintf("missing=%t", missing), func(test *testing.T) {
			storage := newLegacyFindGenerationStore(test)
			doc := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "compatibility body")})
			wantOpens := 0
			if missing {
				storage.fixtures[doc.Hash].failure = fmt.Errorf("absent generation: %w", domain.ErrDocRangeIndexUnavailable)
				wantOpens = 1
			} else {
				storage.readonly = false
			}
			service := NewService(storage, storage, nil, nil, nil)
			ctx := outbound.WithDocReadOnly(systemTestContext())
			storage.cost = legacyFindGenerationCosts{}
			page, err := service.ReadDocViewEvents(ctx, storage.repo, doc.Hash, "", 0, 1, "chat")
			if err != nil || page.Hash != doc.Hash || page.DocIdentity != domain.DocumentIdentityLegacy || page.Next != -1 ||
				len(page.Events) != 1 || !reflect.DeepEqual(page.Events[0].Event, doc.CIR.Events[0]) || !reflect.DeepEqual(page.Envelope, doc.CIR.Envelope) ||
				storage.cost.opens != wantOpens || storage.cost.indexes != 1 || storage.cost.pages != 0 || storage.cost.whole != 0 || storage.cost.writes != 0 {
				test.Fatal("explicit legacy compatibility changed or became a write", page, storage.cost, err)
			}
			raw := docViewLegacyGenerationEventBytes(test, doc, 0)
			storage.cost = legacyFindGenerationCosts{}
			fragment, err := service.ReadDocViewFragment(ctx, storage.repo, doc.Hash, 0, 0, 64)
			if err != nil || fragment.Index != 0 || fragment.Offset != 0 || fragment.Total != len(raw) || fragment.JSON != string(raw[:64]) ||
				storage.cost.opens != wantOpens || storage.cost.indexes != 1 || storage.cost.pages != 0 || storage.cost.whole != 0 || storage.cost.writes != 0 {
				test.Fatal("explicit legacy fragment compatibility changed or became a write", fragment, storage.cost, err)
			}
		})
	}
}

func TestDocViewLegacyGenerationRejectsMalformedMetadata(test *testing.T) {
	for _, mutation := range []string{"wrong hash", "root descriptor", "invalid binding", "invalid envelope", "corrupt directory", "corrupt page"} {
		test.Run(mutation, func(test *testing.T) {
			storage := newLegacyFindGenerationStore(test)
			doc := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "ready legacy body")})
			fixture := storage.fixtures[doc.Hash]
			switch mutation {
			case "wrong hash":
				fixture.generation.Hash = h('9')
			case "root descriptor":
				fixture.generation.Version, fixture.generation.SourceIdentity = 2, nil
				fixture.generation.ChunkPages = domain.DocRangeDirectoryRoot{Hash: h('8'), Count: 1}
				if err := fixture.generation.Validate(doc.Hash); err != nil {
					test.Fatal("root descriptor fixture is malformed", err)
				}
			case "invalid binding":
				fixture.binding.SourceID = "00000000-0000-0000-0000-000000000000"
			case "invalid envelope":
				fixture.generation.Envelope = json.RawMessage(`{"cir_version":2,"source_provider":"codex"}`)
			case "corrupt directory":
				fixture.objects[fixture.generation.EventPages.Hash] = []byte("corrupt")
			case "corrupt page":
				reader := &legacyFindGenerationRead{storage: storage, fixture: fixture}
				hash, err := reader.PageHash(systemTestContext(), 0)
				if err != nil {
					test.Fatal(err)
				}
				fixture.objects[hash] = []byte("corrupt")
			}
			service := NewService(storage, storage, nil, nil, nil)
			assertDocViewLegacyGenerationFailure(test, service, storage, doc.Hash, domain.ErrIntegrity)
		})
	}
	for _, mutation := range []string{"short page", "empty page", "wrong position", "read failure"} {
		test.Run(mutation, func(test *testing.T) {
			storage := &legacyFindGenerationMalformedStore{legacyFindGenerationStore: newLegacyFindGenerationStore(test), mutation: mutation}
			doc := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "first"), historyMessage(domain.RoleAssistant, "second")})
			service := NewService(storage, storage, nil, nil, nil)
			assertDocViewLegacyGenerationFailure(test, service, storage.legacyFindGenerationStore, doc.Hash, domain.ErrIntegrity)
		})
	}
}

func TestDocViewLegacyGenerationSnapshotlessReadyReadsPreserveEnvelope(test *testing.T) {
	storage := newLegacyFindGenerationStore(test)
	doc := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "snapshotless legacy body")})
	metadata := &legacyFindGenerationMeta{FSStore: storage.FSStore, failure: domain.ErrNotFound}
	service := NewService(metadata, storage, nil, nil, nil)
	ctx := outbound.WithDocReadOnly(systemTestContext())
	storage.cost = legacyFindGenerationCosts{}
	page, err := service.ReadDocViewEvents(ctx, storage.repo, doc.Hash, "", 0, 1, "chat")
	if err != nil || page.Hash != doc.Hash || page.DocIdentity != domain.DocumentIdentityLegacy || page.Total != 1 || page.Next != -1 ||
		len(page.Events) != 1 || !reflect.DeepEqual(page.Events[0].Event, doc.CIR.Events[0]) || !reflect.DeepEqual(page.Envelope, doc.CIR.Envelope) {
		test.Fatal("snapshotless ready body lost legacy identity or envelope", page, err)
	}
	assertDocViewLegacyGenerationSelectedCost(test, storage.cost)
	storage.cost = legacyFindGenerationCosts{}
	fragment, err := service.ReadDocViewFragment(ctx, storage.repo, doc.Hash, 0, 0, 64)
	if err != nil || fragment.Index != 0 || fragment.Offset != 0 || len(fragment.JSON) != 64 {
		test.Fatal("snapshotless ready fragment became unavailable", fragment, err)
	}
	assertDocViewLegacyGenerationSelectedCost(test, storage.cost)
	storage.fixtures[doc.Hash].owned = false
	assertDocViewLegacyGenerationFailure(test, service, storage, doc.Hash, domain.ErrNotFound)
}

func TestDocViewLegacyGenerationCurrentOwnershipAndConflictsAreRechecked(test *testing.T) {
	storage := newLegacyFindGenerationStore(test)
	doc := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "current legacy body")})
	service := NewService(storage, storage, nil, nil, nil)
	ctx := outbound.WithDocReadOnly(systemTestContext())
	if _, err := service.ReadDocViewEvents(ctx, storage.repo, doc.Hash, "", 0, 1, "chat"); err != nil {
		test.Fatal(err)
	}
	storage.fixtures[doc.Hash].binding.SourceVersion++
	storage.cost = legacyFindGenerationCosts{}
	if page, err := service.ReadDocViewEvents(ctx, storage.repo, doc.Hash, "", 0, 1, "chat"); err != nil || len(page.Events) != 1 {
		test.Fatal("fresh ready generation remained bound to an older read", page, err)
	}
	assertDocViewLegacyGenerationSelectedCost(test, storage.cost)
	storage.fixtures[doc.Hash].failure = domain.ErrConflict
	assertDocViewLegacyGenerationFailure(test, service, storage, doc.Hash, domain.ErrConflict)
	storage.fixtures[doc.Hash].failure = nil
	storage.fixtures[doc.Hash].owned = false
	assertDocViewLegacyGenerationFailure(test, service, storage, doc.Hash, domain.ErrNotFound)
}

func TestDocViewLegacyGenerationUsesDurableBaseBoundaryWithoutPrefixBodies(test *testing.T) {
	storage := newLegacyFindGenerationStore(test)
	prefix := make([]domain.CIREvent, 2048)
	for index := range prefix {
		prefix[index] = historyMessage(domain.RoleUser, fmt.Sprintf("inherited row %08d", index))
	}
	base := storage.seed(test, prefix)
	events := append(append([]domain.CIREvent{}, base.CIR.Events...), historyMessage(domain.RoleAssistant, "new archival answer"))
	doc := storage.seed(test, events)
	storage.prefix = len(prefix)
	service := NewService(storage, storage, nil, nil, nil)
	ctx := outbound.WithDocReadOnly(systemTestContext())
	storage.cost = legacyFindGenerationCosts{}
	page, err := service.ReadDocViewEvents(ctx, storage.repo, doc.Hash, base.Hash, -1, 20, "chat")
	if err != nil || page.Hash != doc.Hash || page.DocIdentity != domain.DocumentIdentityLegacy || page.Offset != len(prefix) ||
		page.Inherited != len(prefix) || page.Total != len(events) || page.Next != -1 || len(page.Events) != 1 ||
		page.Events[0].Index != len(prefix) || !reflect.DeepEqual(page.Events[0].Event, doc.CIR.Events[len(prefix)]) ||
		!reflect.DeepEqual(page.Envelope, doc.CIR.Envelope) {
		test.Fatal("ready legacy body lost its persisted inherited boundary", page, err)
	}
	if storage.cost.opens != 2 || storage.cost.boundaries != 1 || storage.cost.pages != 1 || storage.cost.manifests != 1 ||
		storage.cost.chunks < 1 || storage.cost.chunks > 2 || storage.cost.indexes != 0 || storage.cost.whole != 0 || storage.cost.writes != 0 {
		test.Fatal("ready legacy pair replayed inherited metadata or bodies", storage.cost)
	}
	for _, revoked := range []domain.ContentHash{doc.Hash, base.Hash} {
		storage.fixtures[revoked].owned = false
		storage.cost = legacyFindGenerationCosts{}
		page, err := service.ReadDocViewEvents(ctx, storage.repo, doc.Hash, base.Hash, -1, 20, "chat")
		if !errors.Is(err, domain.ErrNotFound) || len(page.Events) != 0 || storage.cost.indexes != 0 || storage.cost.whole != 0 ||
			storage.cost.manifests != 0 || storage.cost.chunks != 0 || storage.cost.writes != 0 {
			test.Fatal("ready legacy pair bypassed current source/base ownership", revoked, page, storage.cost, err)
		}
		storage.fixtures[revoked].owned = true
	}
	for _, invalid := range []int{-1, len(prefix) + 1} {
		storage.prefix = invalid
		storage.cost = legacyFindGenerationCosts{}
		page, err := service.ReadDocViewEvents(ctx, storage.repo, doc.Hash, base.Hash, -1, 20, "chat")
		if !errors.Is(err, domain.ErrIntegrity) || len(page.Events) != 0 || storage.cost.boundaries != 1 || storage.cost.pages != 0 ||
			storage.cost.indexes != 0 || storage.cost.whole != 0 || storage.cost.manifests != 0 || storage.cost.writes != 0 {
			test.Fatal("invalid persisted legacy boundary was accepted or bypassed", invalid, page, storage.cost, err)
		}
	}
}

func TestDocViewLegacyGenerationMatchesCompatibilityClassification(test *testing.T) {
	storage := newLegacyFindGenerationStore(test)
	summary := historyMessage(domain.RoleUser, "A compacted project summary")
	summary.CompactSummary = true
	events := []domain.CIREvent{
		historyMessage(domain.RoleUser, "A real user prompt"),
		{Kind: domain.EventToolCall, ToolName: "Bash", Input: map[string]any{"command": "synthetic command"}},
		{Kind: domain.EventToolResult, Output: "synthetic output"},
		{Kind: domain.EventReasoning, RedactedSummary: "A public reasoning summary"},
		{Kind: domain.EventReasoning},
		summary,
		historyMessage(domain.RoleAssistant, "The final answer"),
	}
	for index := range events {
		events[index].Seq = index
	}
	document := storage.seed(test, events)
	service := NewService(storage, storage, nil, nil, nil)
	ctx := outbound.WithDocReadOnly(systemTestContext())
	for _, mode := range []string{"chat", "prompts", "all", "raw"} {
		test.Run(mode, func(test *testing.T) {
			storage.readonly = true
			prepared, err := service.ReadDocViewEvents(ctx, storage.repo, document.Hash, "", 0, 20, mode)
			if err != nil {
				test.Fatal(err)
			}
			storage.readonly = false
			compatible, err := service.ReadDocViewEvents(ctx, storage.repo, document.Hash, "", 0, 20, mode)
			if err != nil || !reflect.DeepEqual(prepared, compatible) {
				test.Fatal("prepared legacy classification or event content differs from compatibility", prepared, compatible, err)
			}
		})
	}
}

func TestDocViewLegacyGenerationKeepsBulkGuardForOriginalBytes(test *testing.T) {
	ctx, gate := admissionTestContext(10 * time.Millisecond)
	ctx = inbound.WithSystemActor(ctx)
	storage := newLegacyFindGenerationStore(test)
	body := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "guarded original bytes")})
	metadata := storage.seed(test, []domain.CIREvent{{Kind: domain.EventToolCall, ToolName: "Bash", Input: map[string]any{"command": "not loaded"}}})
	service := NewService(storage, storage, nil, nil, nil)
	_, releaseBulk, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer releaseBulk()
	for _, fragment := range []bool{false, true} {
		storage.cost = legacyFindGenerationCosts{}
		if fragment {
			_, err = service.ReadDocViewFragment(ctx, storage.repo, body.Hash, 0, 0, 64)
		} else {
			_, err = service.ReadDocViewEvents(ctx, storage.repo, body.Hash, "", 0, 1, "chat")
		}
		if !errors.Is(err, domain.ErrResourceOverloaded) || storage.cost.manifests != 0 || storage.cost.chunks != 0 || storage.cost.whole != 0 || storage.cost.indexes != 0 {
			test.Fatal("ready generation bypassed the bulk guard before reading legacy bytes", fragment, storage.cost, err)
		}
	}
	storage.cost = legacyFindGenerationCosts{}
	page, err := service.ReadDocViewEvents(ctx, storage.repo, metadata.Hash, "", 0, 1, "all")
	if err != nil || len(page.Events) != 1 || !page.Events[0].Deferred || storage.cost.pages != 1 ||
		storage.cost.manifests != 0 || storage.cost.chunks != 0 || storage.cost.whole != 0 || storage.cost.indexes != 0 {
		test.Fatal("metadata-only ready page was blocked by unrelated bulk work", page, storage.cost, err)
	}
	releaseBulk()
	if page, err = service.ReadDocViewEvents(ctx, storage.repo, body.Hash, "", 0, 1, "chat"); err != nil || len(page.Events) != 1 {
		test.Fatal("legacy body did not recover after bulk capacity returned", page, err)
	}
	gate.mu.Lock()
	active, bulk := gate.active, gate.bulk
	gate.mu.Unlock()
	if active != 0 || bulk != 0 {
		test.Fatal("legacy body leaked its resource permit", active, bulk)
	}
}
