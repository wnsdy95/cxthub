package app

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func discoveryHistoryEntry(t *testing.T, event domain.HistoryEvent) domain.CatalogEntry {
	t.Helper()
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return domain.CatalogEntry{Sequence: 1, Kind: "history", Key: event.ID, Value: raw}
}

func TestGitAssociationDiscoveryExtractsOriginalPublicationsOnly(t *testing.T) {
	repo, capture := hh("discovery repo"), hh("original capture")
	ordinary := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "feature", Branch: "feature", Kind: "position", Source: capture, Target: capture, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(100, 0).UTC()}
	publication := prPublication(ordinary)
	publication.MemoryHash, publication.MemoryPinned = hh("pinned memory"), true
	page := domain.CatalogPage{RepoID: repo, Entries: []domain.CatalogEntry{discoveryHistoryEntry(t, ordinary), discoveryHistoryEntry(t, publication), {Kind: "snapshot", Key: string(capture)}}}
	got, err := gitAssociationPublications(page)
	if err != nil || !reflect.DeepEqual(got, []domain.HistoryEvent{publication}) {
		t.Fatalf("discovery changed original provenance: %+v %v", got, err)
	}
	for _, scenario := range []string{"deleted-history", "malformed-history", "foreign-repository", "wrong-id", "duplicate-id"} {
		t.Run(scenario, func(t *testing.T) {
			entry := discoveryHistoryEntry(t, publication)
			next := domain.CatalogPage{RepoID: repo, Entries: []domain.CatalogEntry{entry}}
			switch scenario {
			case "deleted-history":
				next.Entries[0].Deleted = true
			case "malformed-history":
				next.Entries[0].Value = []byte(`{"kind":"publish"}`)
			case "foreign-repository":
				next.RepoID = hh("other repo")
			case "wrong-id":
				next.Entries[0].Key = strings.Repeat("f", 32)
			case "duplicate-id":
				next.Entries = append(next.Entries, entry)
			}
			if got, err := gitAssociationPublications(next); !errors.Is(err, domain.ErrIntegrity) || got != nil {
				t.Fatalf("bad metadata accepted: %+v %v", got, err)
			}
		})
	}
}
