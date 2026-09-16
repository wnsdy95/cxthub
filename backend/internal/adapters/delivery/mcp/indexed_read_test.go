package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"reflect"
	"strings"
	"testing"
)

type indexedPageBackend struct {
	pageBackend
	reader *app.Service
	whole  int
}

func (b *indexedPageBackend) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	b.whole++
	return domain.SessionDoc{}, fmt.Errorf("whole archive reads forbidden")
}
func (b *indexedPageBackend) ReadDocEvents(ctx context.Context, r, h, base domain.ContentHash, o, n int) (domain.DocEventPage, error) {
	return b.reader.ReadDocEvents(ctx, r, h, base, o, n)
}
func (b *indexedPageBackend) ReadDocFragments(ctx context.Context, r, h domain.ContentHash, i, o, n, budget int) (domain.DocFragmentPage, error) {
	return b.reader.ReadDocFragments(ctx, r, h, i, o, n, budget)
}
func (b *indexedPageBackend) SearchDocEvents(ctx context.Context, r, h domain.ContentHash, q string, a, n int) ([]domain.DocEventIndex, error) {
	return b.reader.SearchDocEvents(ctx, r, h, q, a, n)
}

func TestIndexedMCPFetchAndSearchPreserveCursorContract(t *testing.T) {
	ctx := context.Background()
	repo := domain.Repo{ID: pageHash(1)}
	fs := store.NewFSStore(t.TempDir())
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{}}}
	for i := 0; i < 4; i++ {
		doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: i, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("needle \uac80\uc0c9 ", 4000)}}})
	}
	raw, _ := domain.CanonicalBytes(doc.CIR)
	doc.Hash = domain.HashContent(raw)
	if _, err := fs.PutDoc(ctx, repo.ID, doc); err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo.ID, Branch: "main"}
	b := &indexedPageBackend{pageBackend: pageBackend{fakeContextBackend: fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {snap}}}}, reader: app.NewService(fs, fs, nil, nil, nil)}
	s := &Server{context: b}
	a := toolArgs{Ref: string(snap.ID), Events: 2}
	parts := make([]string, 4)
	for calls := 0; calls < 100; calls++ {
		raw, err := s.eventPage(ctx, repo, a)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Snapshot  domain.ContentHash `json:"snapshot_id"`
			Doc       domain.ContentHash `json:"doc_hash"`
			Fragments []eventFragment    `json:"fragments"`
			Next      string             `json:"next_cursor"`
		}
		if err = json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		if page.Snapshot != snap.ID || page.Doc != doc.Hash {
			t.Fatal("fetch wire identity changed")
		}
		size := 0
		for _, f := range page.Fragments {
			if len(parts[f.Index]) != f.Offset {
				t.Fatal("fragment gap")
			}
			parts[f.Index] += f.JSON
			size += len(f.JSON)
		}
		if size > pageBytes {
			t.Fatal("fragment budget exceeded")
		}
		if page.Next == "" {
			break
		}
		a.Cursor = page.Next
	}
	for i, part := range parts {
		var got domain.CIREvent
		if err := json.Unmarshal([]byte(part), &got); err != nil || !reflect.DeepEqual(got, doc.CIR.Events[i]) {
			t.Fatalf("event %d changed: %v", i, err)
		}
	}
	legacy, err := cursorFor(string(repo.ID), "context_fetch", a)
	if err != nil {
		t.Fatal(err)
	}
	legacy.FragmentFormat = ""
	old := a
	old.Cursor = encodeCursor(legacy)
	if _, err = s.eventPage(ctx, repo, old); err == nil {
		t.Fatal("legacy byte offsets accepted for changed encoding")
	}
	a = toolArgs{Repository: string(repo.ID), Query: "needle", Limit: 2}
	seen := map[int]bool{}
	for calls := 0; calls < 10; calls++ {
		raw, err := s.searchPage(ctx, repo, a)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Hits []struct {
				Seq int `json:"seq"`
			} `json:"hits"`
			Next string `json:"next_cursor"`
		}
		if err = json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		for _, hit := range page.Hits {
			if seen[hit.Seq] {
				t.Fatal("duplicate search result")
			}
			seen[hit.Seq] = true
		}
		if page.Next == "" {
			break
		}
		a.Cursor = page.Next
	}
	if len(seen) != 4 || b.whole != 0 {
		t.Fatalf("results=%d whole=%d", len(seen), b.whole)
	}
}
