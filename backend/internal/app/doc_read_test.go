package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type countedReadStore struct {
	*store.FSStore
	whole, chunks int
}

func (s *countedReadStore) GetDoc(ctx context.Context, r, h domain.ContentHash) (domain.SessionDoc, error) {
	s.whole++
	return s.FSStore.GetDoc(ctx, r, h)
}
func (s *countedReadStore) GetChunk(ctx context.Context, r, h domain.ContentHash) ([]byte, error) {
	s.chunks++
	return s.FSStore.GetChunk(ctx, r, h)
}

func TestIndexedFragmentsBoundOversizedUnicodeEventAndPreserveBytes(t *testing.T) {
	ctx := systemTestContext()
	repo := h('1')
	fs := store.NewFSStore(t.TempDir())
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("\uac80\uc0c9🌿", 200000)}}}}}}
	raw, _ := domain.CanonicalBytes(doc.CIR)
	doc.Hash = domain.HashContent(raw)
	if _, err := fs.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	spy := &countedReadStore{FSStore: fs}
	svc := NewService(fs, spy, nil, nil)
	var result strings.Builder
	index, offset := 0, 0
	for calls := 0; calls < 1000; calls++ {
		before := spy.chunks
		page, err := svc.ReadDocFragments(ctx, repo, doc.Hash, index, offset, 12, 12<<10)
		if err != nil {
			t.Fatal(err)
		}
		if spy.whole != 0 || spy.chunks-before > 2 {
			t.Fatalf("unbounded read: whole=%d chunks=%d", spy.whole, spy.chunks-before)
		}
		size := 0
		for _, f := range page.Fragments {
			if f.Offset != result.Len() {
				t.Fatal("fragment gap")
			}
			result.WriteString(f.JSON)
			size += len(f.JSON)
		}
		if size > 12<<10 || size == 0 {
			t.Fatal("invalid fragment budget")
		}
		index, offset = page.NextIndex, page.NextOffset
		if index == page.Total {
			break
		}
	}
	var got domain.CIREvent
	if err := json.Unmarshal([]byte(result.String()), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, doc.CIR.Events[0]) {
		t.Fatal("fragments changed event")
	}
	if _, err := svc.ReadDocFragments(ctx, h('2'), doc.Hash, 0, 0, 12, 12000); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign fragments: %v", err)
	}
}
