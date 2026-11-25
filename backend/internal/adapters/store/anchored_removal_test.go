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

// anchoredRemovalStores is what removing a session touches.
type anchoredRemovalStores interface {
	outbound.AnchoredSessionStore
	outbound.AnchoredRemovalStore
	outbound.AnchoredSummaryStore
	outbound.AnchoredPublicationStore
	PutDoc(ctx context.Context, repo domain.ContentHash, doc domain.SessionDoc) (bool, error)
}

// anchoredStoreSessionWithContent is a session whose one segment is the
// whole of a conversation of two events: both in the one document named, or
// one in each of two.
func anchoredStoreSessionWithContent(t *testing.T, repo domain.ContentHash, provider domain.ProviderKind, id, uploader, commit string, docs ...domain.ContentHash) domain.AnchoredSession {
	t.Helper()
	session := anchoredStoreSession(t, repo, provider, id, uploader, commit)
	session.Body.Segments[0].Events = 2
	session.Body.Content = &domain.AnchoredContent{Events: 2}
	for _, doc := range docs {
		session.Body.Content.Parts = append(session.Body.Content.Parts, domain.AnchoredContentPart{Doc: doc, Events: 2 / len(docs)})
	}
	session.Body = session.Body.Canonical()
	if err := session.Body.Validate(); err != nil {
		t.Fatal(err)
	}
	hash, err := session.Body.Hash()
	if err != nil {
		t.Fatal(err)
	}
	session.BodyHash = hash
	return session
}

// checkAnchoredRemovalPersistence is the contract both stores keep for
// removing a session and remembering that it was removed.
func checkAnchoredRemovalPersistence(t *testing.T, ctx context.Context, storage anchoredRemovalStores, repo domain.ContentHash) {
	t.Helper()
	a := anchoredStoreSHA("a")
	docOne, docTwo, docNone := domain.HashContent([]byte("document one")), domain.HashContent([]byte("document two")), domain.HashContent([]byte("no document"))
	// The conversation of the first session is kept in two documents.
	docLater := domain.HashContent([]byte("document one goes on"))
	for _, session := range []domain.AnchoredSession{
		anchoredStoreSessionWithContent(t, repo, domain.ProviderClaude, "session-1", "uploader-1", a, docOne, docLater),
		anchoredStoreSessionWithContent(t, repo, domain.ProviderCodex, "session-2", "uploader-1", a, docTwo),
		anchoredStoreSessionWithContent(t, repo, domain.ProviderClaude, "session-3", "uploader-2", a, docTwo),
	} {
		if err := storage.PutAnchoredSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	for _, summary := range []domain.AnchoredSummary{
		anchoredStoreSummary(t, repo, domain.ProviderClaude, "session-1", a, "Added the parser.", 10),
		anchoredStoreSummary(t, repo, domain.ProviderCodex, "session-2", a, "Reviewed the parser.", 11),
	} {
		if err := storage.PutAnchoredSummary(ctx, summary); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2031, 1, 3, 0, 0, 0, 0, time.UTC)
	open := func(provider domain.ProviderKind, id string) domain.AnchoredPublication {
		return domain.AnchoredPublication{Provider: provider, SessionID: id, Ranges: []domain.AnchoredPublishedRange{
			{FirstOrdinal: 0, LastOrdinal: 0, Events: 2, At: at, By: domain.AnchoredPublicationReason{Kind: domain.AnchoredPublishedByCommit, Commit: a}}}}
	}
	for _, publication := range []domain.AnchoredPublication{open(domain.ProviderClaude, "session-1"), open(domain.ProviderCodex, "session-2")} {
		if err := storage.PutAnchoredPublication(ctx, repo, publication); err != nil {
			t.Fatal(err)
		}
	}

	// A document is in use while a stored report names it, as any part of
	// its conversation.
	for doc, want := range map[domain.ContentHash]bool{docOne: true, docLater: true, docTwo: true, docNone: false} {
		if used, err := storage.AnchoredContentInUse(ctx, repo, doc); err != nil || used != want {
			t.Fatalf("document %s in use = %v %v, want %v", doc, used, err, want)
		}
	}

	// Removing a session removes its report, its commit links, its summaries
	// and its open ranges, and nothing of any other session.
	if err := storage.DeleteAnchoredSession(ctx, repo, domain.ProviderClaude, "session-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetAnchoredSession(ctx, repo, domain.ProviderClaude, "session-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the removed report: %v", err)
	}
	links, err := storage.ListAnchoredCommitLinks(ctx, repo, a)
	if err != nil || len(links) != 2 || links[0].SessionID == "session-1" || links[1].SessionID == "session-1" {
		t.Fatalf("commit links after the removal: %+v %v", links, err)
	}
	summaries, err := storage.ListCommitAnchoredSummaries(ctx, repo, []string{a})
	if err != nil || len(summaries) != 1 || summaries[0].SessionID != "session-2" {
		t.Fatalf("summaries after the removal: %+v %v", summaries, err)
	}
	if ranges, err := storage.GetAnchoredPublication(ctx, repo, domain.ProviderClaude, "session-1"); err != nil || len(ranges) != 0 {
		t.Fatalf("open ranges of the removed session: %+v %v", ranges, err)
	}
	if ranges, err := storage.GetAnchoredPublication(ctx, repo, domain.ProviderCodex, "session-2"); err != nil || len(ranges) != 1 {
		t.Fatalf("open ranges of another session: %+v %v", ranges, err)
	}
	// Its documents are no longer in use; one another report still names is.
	for _, doc := range []domain.ContentHash{docOne, docLater} {
		if used, err := storage.AnchoredContentInUse(ctx, repo, doc); err != nil || used {
			t.Fatalf("a document of the removed session in use = %v %v", used, err)
		}
	}
	if err := storage.DeleteAnchoredSession(ctx, repo, domain.ProviderCodex, "session-2"); err != nil {
		t.Fatal(err)
	}
	if used, err := storage.AnchoredContentInUse(ctx, repo, docTwo); err != nil || !used {
		t.Fatalf("a document another report names in use = %v %v", used, err)
	}
	// What is not stored cannot be removed, and a key that is not one is refused.
	if err := storage.DeleteAnchoredSession(ctx, repo, domain.ProviderClaude, "session-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("removing twice: %v", err)
	}
	if err := storage.DeleteAnchoredSession(ctx, repo, domain.ProviderClaude, "../escape"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a key that is not one: %v", err)
	}

	// A removal is remembered until it is taken back.
	if _, err := storage.GetAnchoredSessionRemoval(ctx, repo, domain.ProviderClaude, "session-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a session that was not removed: %v", err)
	}
	if listed, err := storage.ListAnchoredSessionRemovals(ctx, repo, "uploader-1"); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("no removals yet: %#v %v", listed, err)
	}
	first := domain.AnchoredSessionRemoval{RepoID: repo, Provider: domain.ProviderClaude, SessionID: "session-1", Uploader: "uploader-1", RemovedBy: "uploader-1", RemovedAt: at}
	second := domain.AnchoredSessionRemoval{RepoID: repo, Provider: domain.ProviderCodex, SessionID: "session-2", Uploader: "uploader-1", RemovedBy: "maintainer-1", RemovedAt: at.Add(time.Minute)}
	other := domain.AnchoredSessionRemoval{RepoID: repo, Provider: domain.ProviderClaude, SessionID: "session-9", Uploader: "uploader-2", RemovedBy: "uploader-2", RemovedAt: at}
	for _, removal := range []domain.AnchoredSessionRemoval{second, other, first} {
		if err := storage.PutAnchoredSessionRemoval(ctx, removal); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := storage.GetAnchoredSessionRemoval(ctx, repo, domain.ProviderCodex, "session-2"); err != nil || !reflect.DeepEqual(got, second) {
		t.Fatalf("read back: %+v %v", got, err)
	}
	// By tool, then identifier; only the sessions that belonged to the account.
	if listed, err := storage.ListAnchoredSessionRemovals(ctx, repo, "uploader-1"); err != nil || !reflect.DeepEqual(listed, []domain.AnchoredSessionRemoval{first, second}) {
		t.Fatalf("listed: %+v %v", listed, err)
	}
	// A removal that stands is kept as it is.
	again := first
	again.RemovedBy, again.RemovedAt = "maintainer-1", at.Add(time.Hour)
	if err := storage.PutAnchoredSessionRemoval(ctx, again); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetAnchoredSessionRemoval(ctx, repo, domain.ProviderClaude, "session-1"); err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("a removal recorded twice: %+v %v", got, err)
	}
	if err := storage.DeleteAnchoredSessionRemoval(ctx, repo, domain.ProviderClaude, "session-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetAnchoredSessionRemoval(ctx, repo, domain.ProviderClaude, "session-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a removal taken back: %v", err)
	}
	if err := storage.DeleteAnchoredSessionRemoval(ctx, repo, domain.ProviderClaude, "session-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("taking back twice: %v", err)
	}
	// What cannot be a removal is refused before it is stored.
	noUploader, noRemover, noTime, badKey, padded := first, first, first, first, first
	noUploader.Uploader, noRemover.RemovedBy, noTime.RemovedAt, badKey.SessionID, padded.RemovedBy = "", "", time.Time{}, "../escape", " maintainer-1"
	for name, removal := range map[string]domain.AnchoredSessionRemoval{"no uploader": noUploader, "no remover": noRemover, "no time": noTime, "a key that is not one": badKey, "a padded account": padded} {
		if err := storage.PutAnchoredSessionRemoval(ctx, removal); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s was stored: %v", name, err)
		}
	}

	// A document a snapshot of the repository holds is in use, whatever the
	// reports name.
	captured := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "a captured conversation " + strings.Repeat("x", 8)}}}}}}
	raw, err := domain.CanonicalBytes(captured.CIR)
	if err != nil {
		t.Fatal(err)
	}
	captured.Hash = domain.HashContent(raw)
	if _, err := storage.PutDoc(ctx, repo, captured); err != nil {
		t.Fatal(err)
	}
	if used, err := storage.AnchoredContentInUse(ctx, repo, captured.Hash); err != nil || used {
		t.Fatalf("a stored document nothing names in use = %v %v", used, err)
	}
}

func TestFSAnchoredRemovalPersistence(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	checkAnchoredRemovalPersistence(t, context.Background(), storage, domain.HashContent([]byte(t.Name())))
}
