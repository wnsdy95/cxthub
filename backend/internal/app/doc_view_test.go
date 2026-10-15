package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type docViewCountStore struct {
	*store.FSStore
	chunkReads, fullReads, pageReads int
}

func (storage *docViewCountStore) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	storage.chunkReads++
	return storage.FSStore.ReadConversationChunk(ctx, repo, hash, length)
}

func (storage *docViewCountStore) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	storage.fullReads++
	return storage.FSStore.ReadVerifiedDoc(ctx, repo, hash)
}

func (storage *docViewCountStore) DocRangePage(ctx context.Context, repo domain.ContentHash, index domain.DocRangeIndex, page int) ([]domain.DocRangeEntry, error) {
	storage.pageReads++
	return storage.FSStore.DocRangePage(ctx, repo, index, page)
}

func TestDocViewPageReadsDoNotGrowWithArchive(t *testing.T) {
	for _, count := range []int{80, 800} {
		ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
		storage := store.NewFSStore(dir)
		events := []domain.CIREvent{}
		for position := 0; position < 20; position++ {
			events = append(events, historyMessage(domain.RoleUser, strings.Repeat("visible ", 60)))
		}
		for position := 0; position < count; position++ {
			events = append(events, domain.CIREvent{Kind: domain.EventToolResult, Output: strings.Repeat("hidden ", 4000)})
		}
		doc, manifest := seedRootReadDoc(t, dir, storage, repo, events...)
		if len(manifest.Chunks) < 2 {
			t.Fatal("fixture requires nonselected chunks")
		}
		if err := storage.EnsureDocRangeIndex(ctx, repo, doc.Hash); err != nil {
			t.Fatal(err)
		}
		reader := &docViewCountStore{FSStore: storage}
		service := NewService(storage, reader, nil, nil, nil)
		page, err := service.ReadDocViewEvents(ctx, repo, doc.Hash, "", 0, 20, "chat")
		if err != nil || len(page.Events) != 20 || page.Next != 20 {
			t.Fatal(page, err)
		}
		if reader.fullReads != 0 || reader.chunkReads != 1 || reader.pageReads != 1 {
			t.Fatalf("archive=%d full=%d chunks=%d pages=%d", count, reader.fullReads, reader.chunkReads, reader.pageReads)
		}
		for position, event := range page.Events {
			if event.Deferred || event.Index != position || !reflect.DeepEqual(event.Event, doc.CIR.Events[position]) {
				t.Fatal("changed visible event", position)
			}
		}
		path := rootReadObjectPath(dir, repo, "chunks", manifest.Chunks[len(manifest.Chunks)-1].Hash)
		if err := os.WriteFile(path, []byte("corrupt unrelated chunk"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := service.ReadDocViewEvents(ctx, repo, doc.Hash, "", 0, 20, "chat"); err != nil {
			t.Fatal("page unexpectedly audited unrelated chunks", err)
		}
		if _, err := service.ReadDocEvents(ctx, repo, doc.Hash, "", 0, 1); err == nil {
			t.Fatal("full audit ignored corruption")
		}
		path = rootReadObjectPath(dir, repo, "chunks", manifest.Chunks[0].Hash)
		if err := os.WriteFile(path, []byte("corrupt selected chunk"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := service.ReadDocViewEvents(ctx, repo, doc.Hash, "", 0, 20, "chat"); err == nil {
			t.Fatal("selected chunk corruption accepted")
		}
		if _, err := service.ReadDocViewEvents(ctx, h('2'), doc.Hash, "", 0, 20, "chat"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("foreign repository", err)
		}
	}
}

func TestDocViewCountsVisibleMessagesAndLoadsToolsOnDemand(t *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
	storage := store.NewFSStore(dir)
	events := []domain.CIREvent{}
	for turn := 0; turn < 20; turn++ {
		events = append(events, historyMessage(domain.RoleUser, "question"))
		for tool := 0; tool < 10; tool++ {
			events = append(events, domain.CIREvent{Kind: domain.EventToolResult, Output: strings.Repeat("hidden", 2000)})
		}
		events = append(events, historyMessage(domain.RoleAssistant, "answer"))
	}
	doc, _ := seedRootReadDoc(t, dir, storage, repo, events...)
	service := NewService(storage, storage, nil, nil, nil)
	page, err := service.ReadDocViewEvents(ctx, repo, doc.Hash, "", 0, 20, "chat")
	if err != nil || len(page.Events) != 120 || page.Next != 120 {
		t.Fatalf("events=%d next=%d error=%v", len(page.Events), page.Next, err)
	}
	visible := 0
	for _, event := range page.Events {
		if !event.Deferred {
			visible++
		} else if event.Event.Output != nil {
			t.Fatal("hidden tool eagerly loaded")
		}
	}
	if visible != 20 {
		t.Fatal("wrong screen batch", visible)
	}
	detail, err := service.ReadDocViewEvents(ctx, repo, doc.Hash, "", 1, 1, "raw")
	if err != nil || len(detail.Events) != 1 || detail.Events[0].Deferred || !reflect.DeepEqual(detail.Events[0].Event, doc.CIR.Events[1]) {
		t.Fatal("lazy detail changed", err)
	}
	page, err = service.ReadDocViewEvents(ctx, repo, doc.Hash, "", 0, 10, "prompts")
	if err != nil {
		t.Fatal(err)
	}
	visible = 0
	for _, event := range page.Events {
		if !event.Deferred {
			visible++
			if event.Event.Role != domain.RoleUser {
				t.Fatal("wrong prompt projection")
			}
		}
	}
	if visible != 10 {
		t.Fatal("prompt batch", visible)
	}
}

func TestDocViewHiddenMetadataDoesNotConsumeAFullMessageBudget(t *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
	storage := store.NewFSStore(dir)
	events := []domain.CIREvent{}
	for turn := 0; turn < 20; turn++ {
		events = append(events, historyMessage(domain.RoleUser, "visible prompt"))
		for tool := 0; tool < 20; tool++ {
			events = append(events, domain.CIREvent{Kind: domain.EventToolResult, Output: strings.Repeat("hidden output", 100)})
		}
	}
	doc, _ := seedRootReadDoc(t, dir, storage, repo, events...)
	service := NewService(storage, storage, nil, nil, nil)
	for _, mode := range []string{"chat", "prompts"} {
		page, err := service.ReadDocViewEvents(ctx, repo, doc.Hash, "", 0, 20, mode)
		if err != nil || len(page.Events) != 400 || page.Next != 400 {
			t.Fatalf("mode=%s events=%d next=%d error=%v", mode, len(page.Events), page.Next, err)
		}
		raw, err := json.Marshal(page)
		if err != nil || len(raw) > docViewBytes || strings.Contains(string(raw), "hidden output") {
			t.Fatal("hidden body inflated or leaked into page", mode, err)
		}
	}
}

func TestDocViewInheritanceUsesIndexBlocksAndFragmentsStayBounded(t *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
	storage := store.NewFSStore(dir)
	events := []domain.CIREvent{}
	for position := 0; position < 256; position++ {
		events = append(events, historyMessage(domain.RoleUser, "same prefix"))
	}
	base, _ := seedRootReadDoc(t, dir, storage, repo, events...)
	events = append(events, historyMessage(domain.RoleAssistant, strings.Repeat("\uac00🌿", 30000)))
	doc, _ := seedRootReadDoc(t, dir, storage, repo, events...)
	if err := storage.EnsureDocRangeIndex(ctx, repo, base.Hash); err != nil {
		t.Fatal(err)
	}
	if err := storage.EnsureDocRangeIndex(ctx, repo, doc.Hash); err != nil {
		t.Fatal(err)
	}
	reader := &docViewCountStore{FSStore: storage}
	service := NewService(storage, reader, nil, nil, nil)
	page, err := service.ReadDocViewEvents(ctx, repo, doc.Hash, base.Hash, -1, 20, "chat")
	if err != nil || page.Offset != 256 || page.Inherited != 256 || len(page.Events) != 1 || !page.Events[0].Deferred {
		t.Fatal(page, err)
	}
	if reader.chunkReads != 0 || reader.pageReads != 1 {
		t.Fatal("inherited prefix loaded bodies or unchanged blocks", reader.chunkReads, reader.pageReads)
	}
	var assembled strings.Builder
	for offset := 0; offset >= 0; {
		fragment, err := service.ReadDocViewFragment(ctx, repo, doc.Hash, 256, offset, 65536)
		if err != nil {
			t.Fatal(err)
		}
		if len(fragment.JSON) > 65536 || fragment.Offset != offset || fragment.Next == offset {
			t.Fatal("bad fragment", fragment)
		}
		assembled.WriteString(fragment.JSON)
		offset = fragment.Next
	}
	var event domain.CIREvent
	if json.Unmarshal([]byte(assembled.String()), &event) != nil || !reflect.DeepEqual(event, doc.CIR.Events[256]) {
		t.Fatal("fragment reconstruction lost bytes")
	}
}

func TestDocViewLegacyRequiresBulkAdmission(t *testing.T) {
	ctx, gate := admissionTestContext(10 * time.Millisecond)
	ctx = inbound.WithSystemActor(ctx)
	repo, dir := h('1'), t.TempDir()
	storage := store.NewFSStore(dir)
	legacy := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{historyMessage(domain.RoleUser, "legacy question")}}}
	legacy.CIR.Envelope.CIRVersion = "1"
	legacy.CIR.Envelope.SourceProvider = "codex"
	canonical, err := domain.CanonicalBytes(legacy.CIR)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Hash = domain.HashContent(canonical)
	if _, err := storage.PutDoc(ctx, repo, legacy); err != nil {
		t.Fatal(err)
	}
	root, _ := seedRootReadDoc(t, dir, storage, repo, historyMessage(domain.RoleUser, "root question"))
	if err := storage.EnsureDocRangeIndex(ctx, repo, root.Hash); err != nil {
		t.Fatal(err)
	}
	service := NewService(storage, storage, nil, nil, nil)
	_, releaseBulk, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBulk()
	if _, err := service.ReadDocViewEvents(ctx, repo, root.Hash, "", 0, 20, "chat"); err != nil {
		t.Fatal("warm root page blocked by bulk work", err)
	}
	for _, read := range []func() error{
		func() error {
			_, err := service.ReadDocViewEvents(ctx, repo, legacy.Hash, "", 0, 20, "chat")
			return err
		},
		func() error { _, err := service.ReadDocViewFragment(ctx, repo, legacy.Hash, 0, 0, 64); return err },
		func() error {
			_, err := service.ReadDocViewEvents(ctx, repo, root.Hash, legacy.Hash, -1, 20, "chat")
			return err
		},
	} {
		if err := read(); !errors.Is(err, domain.ErrResourceOverloaded) {
			t.Fatalf("legacy full-document read bypassed occupied bulk slot: %v", err)
		}
	}
	releaseBulk()
	if page, err := service.ReadDocViewEvents(ctx, repo, legacy.Hash, "", 0, 20, "chat"); err != nil || len(page.Events) != 1 {
		t.Fatalf("legacy retry failed: %+v %v", page, err)
	}
	assertResourcesReleased(t, gate)
}
