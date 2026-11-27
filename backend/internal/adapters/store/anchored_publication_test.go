package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// checkAnchoredPublicationPersistence is the contract both stores keep. The
// sessions named here must exist in the store that requires them to.
func checkAnchoredPublicationPersistence(t *testing.T, ctx context.Context, storage outbound.AnchoredPublicationStore, repo domain.ContentHash) {
	t.Helper()
	at := time.Date(2031, 1, 3, 0, 0, 0, 0, time.UTC)
	if ranges, err := storage.GetAnchoredPublication(ctx, repo, domain.ProviderClaude, "session-1"); err != nil || ranges == nil || len(ranges) != 0 {
		t.Fatalf("nothing open yet: %#v %v", ranges, err)
	}
	if listed, err := storage.ListAnchoredPublications(ctx, repo); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("no publications yet: %#v %v", listed, err)
	}
	first := domain.AnchoredPublication{Provider: domain.ProviderCodex, SessionID: "session-2", Ranges: []domain.AnchoredPublishedRange{
		{FirstOrdinal: 0, LastOrdinal: 5, FirstEvent: 0, Events: 10, At: at, By: domain.AnchoredPublicationReason{Kind: domain.AnchoredPublishedBySpawn, Parent: "session-1"}},
	}}
	second := domain.AnchoredPublication{Provider: domain.ProviderClaude, SessionID: "session-1", Ranges: []domain.AnchoredPublishedRange{
		{FirstOrdinal: 0, LastOrdinal: 1, Events: 6, At: at, By: domain.AnchoredPublicationReason{Kind: domain.AnchoredPublishedByCommit, Commit: strings.Repeat("a", 40)}},
	}}
	for _, publication := range []domain.AnchoredPublication{first, second} {
		if err := storage.PutAnchoredPublication(ctx, repo, publication); err != nil {
			t.Fatal(err)
		}
	}
	if ranges, err := storage.GetAnchoredPublication(ctx, repo, domain.ProviderClaude, "session-1"); err != nil || !reflect.DeepEqual(ranges, second.Ranges) {
		t.Fatalf("one session's open ranges: %+v %v", ranges, err)
	}
	// The same identifier under another tool is another session.
	if ranges, err := storage.GetAnchoredPublication(ctx, repo, domain.ProviderCodex, "session-1"); err != nil || len(ranges) != 0 {
		t.Fatalf("another tool's session: %+v %v", ranges, err)
	}
	// A session's list is replaced whole.
	second.Ranges = append(second.Ranges, domain.AnchoredPublishedRange{FirstOrdinal: 4, LastOrdinal: 4, FirstEvent: 6, Events: 2, At: at.Add(time.Hour),
		By: domain.AnchoredPublicationReason{Kind: domain.AnchoredPublishedByBranchPush, Branch: "main"}})
	if err := storage.PutAnchoredPublication(ctx, repo, second); err != nil {
		t.Fatal(err)
	}
	if listed, err := storage.ListAnchoredPublications(ctx, repo); err != nil || !reflect.DeepEqual(listed, []domain.AnchoredPublication{second, first}) {
		t.Fatalf("by tool then identifier:\n got %+v\nwant %+v\n%v", listed, []domain.AnchoredPublication{second, first}, err)
	}
	for name, refused := range map[string]domain.AnchoredPublication{
		"no range":           {Provider: domain.ProviderClaude, SessionID: "session-1"},
		"an unknown tool":    {Provider: "other", SessionID: "session-1", Ranges: first.Ranges},
		"a range of no kind": {Provider: domain.ProviderClaude, SessionID: "session-1", Ranges: []domain.AnchoredPublishedRange{{FirstOrdinal: 0, LastOrdinal: 1, At: at}}},
	} {
		if err := storage.PutAnchoredPublication(ctx, repo, refused); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s was stored: %v", name, err)
		}
	}
}

func TestFSAnchoredPublicationPersistence(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	checkAnchoredPublicationPersistence(t, context.Background(), storage, domain.HashContent([]byte(t.Name())))
}
