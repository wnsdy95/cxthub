package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func anchoredStoreSummary(t *testing.T, repo domain.ContentHash, provider domain.ProviderKind, session, commit, text string, at int) domain.AnchoredSummary {
	t.Helper()
	body := domain.AnchoredSummaryBody{Summary: text, KeyFacts: []string{"fact of " + session}, OpenTasks: []string{}}
	hash, err := body.Hash()
	if err != nil {
		t.Fatal(err)
	}
	return domain.AnchoredSummary{RepoID: repo, Provider: provider, SessionID: session, Commit: commit, BodyHash: hash, Body: body,
		CreatedAt: remoteGitStoreTime(at), UpdatedAt: remoteGitStoreTime(at)}
}

// checkAnchoredSummaryPersistence expects the sessions claude/session-1 and
// codex/session-2 to be stored.
func checkAnchoredSummaryPersistence(t *testing.T, ctx context.Context, storage outbound.AnchoredSummaryStore, repo domain.ContentHash) {
	t.Helper()
	a, b := anchoredStoreSHA("a"), anchoredStoreSHA("b")
	if listed, err := storage.ListCommitAnchoredSummaries(ctx, repo, []string{a, b}); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("an empty store: %#v %v", listed, err)
	}
	if _, err := storage.GetAnchoredSummary(ctx, repo, domain.ProviderClaude, "session-1", a); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a summary that was never stored: %v", err)
	}
	first := anchoredStoreSummary(t, repo, domain.ProviderClaude, "session-1", a, "Added the parser.", 10)
	other := anchoredStoreSummary(t, repo, domain.ProviderCodex, "session-2", a, "Reviewed the parser.", 11)
	later := anchoredStoreSummary(t, repo, domain.ProviderClaude, "session-1", b, "Fixed the parser.", 12)
	for _, summary := range []domain.AnchoredSummary{later, other, first} {
		if err := storage.PutAnchoredSummary(ctx, summary); err != nil {
			t.Fatal(err)
		}
	}
	got, err := storage.GetAnchoredSummary(ctx, repo, domain.ProviderClaude, "session-1", a)
	if err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("read back: %+v %v", got, err)
	}
	// By commit, tool, then session; only the commits asked for.
	listed, err := storage.ListCommitAnchoredSummaries(ctx, repo, []string{b, a})
	if err != nil || !reflect.DeepEqual(listed, []domain.AnchoredSummary{first, other, later}) {
		t.Fatalf("listed: %+v %v", listed, err)
	}
	if listed, err = storage.ListCommitAnchoredSummaries(ctx, repo, []string{b}); err != nil || !reflect.DeepEqual(listed, []domain.AnchoredSummary{later}) {
		t.Fatalf("one commit: %+v %v", listed, err)
	}
	if listed, err = storage.ListCommitAnchoredSummaries(ctx, repo, nil); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("no commit asked for: %#v %v", listed, err)
	}
	// Storing again replaces the text and keeps when it was first written.
	rewritten := anchoredStoreSummary(t, repo, domain.ProviderClaude, "session-1", a, "Added and documented the parser.", 10)
	rewritten.UpdatedAt = remoteGitStoreTime(20)
	if err := storage.PutAnchoredSummary(ctx, rewritten); err != nil {
		t.Fatal(err)
	}
	if got, err = storage.GetAnchoredSummary(ctx, repo, domain.ProviderClaude, "session-1", a); err != nil || !reflect.DeepEqual(got, rewritten) {
		t.Fatalf("rewritten: %+v %v", got, err)
	}
	// What cannot be a summary is refused before it is stored.
	wrongHash := first
	wrongHash.BodyHash = later.BodyHash
	empty := first
	empty.Body = domain.AnchoredSummaryBody{}
	named := first
	named.UploaderName = "alice"
	short := first
	short.Commit = "abc1234"
	for name, summary := range map[string]domain.AnchoredSummary{"a hash of other text": wrongHash, "nothing said": empty, "a field that is only read": named, "a short commit number": short} {
		if err := storage.PutAnchoredSummary(ctx, summary); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s was stored: %v", name, err)
		}
	}
}

func TestFSAnchoredSummaryPersistence(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	checkAnchoredSummaryPersistence(t, context.Background(), storage, domain.HashContent([]byte(t.Name())))
}
