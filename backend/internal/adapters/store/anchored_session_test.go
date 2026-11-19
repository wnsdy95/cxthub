package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func anchoredStoreSHA(letter string) string { return strings.Repeat(letter, 40) }

func anchoredStoreSession(t *testing.T, repo domain.ContentHash, provider domain.ProviderKind, id, uploader string, commits ...string) domain.AnchoredSession {
	t.Helper()
	body := domain.AnchoredSessionBody{RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder}
	for index, sha := range commits {
		body.Segments = append(body.Segments, domain.AnchoredSegment{
			AnchoredRange: domain.AnchoredRange{First: "first", Last: "last", FirstOrdinal: index, LastOrdinal: index},
			Commits:       []domain.AnchoredCommit{{SHA: sha, Rule: domain.AnchoredLinkPrintedNumber}},
		})
	}
	body.Records = len(commits)
	body = body.Canonical()
	hash, err := body.Hash()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	return domain.AnchoredSession{RepoID: repo, Provider: provider, SessionID: id, Uploader: uploader, Revision: 1, BodyHash: hash, CreatedAt: at, UpdatedAt: at, Body: body}
}

// checkAnchoredSessionPersistence is the contract both stores keep.
func checkAnchoredSessionPersistence(t *testing.T, ctx context.Context, storage outbound.AnchoredSessionStore, repo domain.ContentHash) {
	t.Helper()
	a, b, c := anchoredStoreSHA("a"), anchoredStoreSHA("b"), anchoredStoreSHA("c")
	first := anchoredStoreSession(t, repo, domain.ProviderClaude, "session-1", "uploader-1", a, b)
	other := anchoredStoreSession(t, repo, domain.ProviderCodex, "session-1", "uploader-2", a)
	for _, session := range []domain.AnchoredSession{first, other} {
		if err := storage.PutAnchoredSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	got, err := storage.GetAnchoredSession(ctx, repo, domain.ProviderClaude, "session-1")
	if err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("stored report:\n got %+v\nwant %+v\n%v", got, first, err)
	}
	if _, err := storage.GetAnchoredSession(ctx, repo, domain.ProviderClaude, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing report: %v", err)
	}
	if listed, err := storage.ListAnchoredSessions(ctx, repo, "uploader-1"); err != nil || len(listed) != 1 || listed[0].SessionID != "session-1" || listed[0].Provider != domain.ProviderClaude {
		t.Fatalf("one uploader's reports: %+v %v", listed, err)
	}
	// The repository-wide list holds every uploader's reports.
	if all, err := storage.ListRepositoryAnchoredSessions(ctx, repo); err != nil || len(all) < 1 || all[0].RepoID != repo {
		t.Fatalf("every uploader's reports: %+v %v", all, err)
	} else {
		uploaders := map[string]int{}
		for _, session := range all {
			uploaders[session.Uploader]++
		}
		if own, _ := storage.ListAnchoredSessions(ctx, repo, "uploader-1"); uploaders["uploader-1"] != len(own) {
			t.Fatalf("uploader-1 has %d reports, the repository-wide list holds %d of them", len(own), uploaders["uploader-1"])
		}
	}
	if listed, err := storage.ListAnchoredSessions(ctx, repo, "nobody"); err != nil || len(listed) != 0 {
		t.Fatalf("an unknown uploader's reports: %+v %v", listed, err)
	}
	links, err := storage.ListAnchoredCommitLinks(ctx, repo, a)
	if err != nil || len(links) != 2 || links[0].Provider != domain.ProviderClaude || links[1].Provider != domain.ProviderCodex || links[0].SegmentNo != 1 {
		t.Fatalf("links of a commit two sessions are cut at: %+v %v", links, err)
	}

	// Replacing a report replaces its commit index entries with it.
	replaced := anchoredStoreSession(t, repo, domain.ProviderClaude, "session-1", "uploader-1", c)
	replaced.Revision, replaced.UpdatedAt = 2, first.UpdatedAt.Add(time.Minute)
	if err := storage.PutAnchoredSession(ctx, replaced); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetAnchoredSession(ctx, repo, domain.ProviderClaude, "session-1"); err != nil || got.Revision != 2 || !got.CreatedAt.Equal(first.CreatedAt) || got.BodyHash != replaced.BodyHash {
		t.Fatalf("replaced report: %+v %v", got, err)
	}
	if links, err := storage.ListAnchoredCommitLinks(ctx, repo, b); err != nil || len(links) != 0 {
		t.Fatalf("a replaced report left an index entry: %+v %v", links, err)
	}
	if links, err := storage.ListAnchoredCommitLinks(ctx, repo, a); err != nil || len(links) != 1 || links[0].Provider != domain.ProviderCodex {
		t.Fatalf("the other session's entry: %+v %v", links, err)
	}
	if links, err := storage.ListAnchoredCommitLinks(ctx, repo, c); err != nil || len(links) != 1 || links[0].SessionID != "session-1" {
		t.Fatalf("the new entry: %+v %v", links, err)
	}

	// A report whose hash is not the hash of its body is never stored.
	forged := anchoredStoreSession(t, repo, domain.ProviderClaude, "session-2", "uploader-1", a)
	forged.BodyHash = domain.HashContent([]byte("something else"))
	if err := storage.PutAnchoredSession(ctx, forged); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("a forged hash: %v", err)
	}
	unattributed := anchoredStoreSession(t, repo, domain.ProviderClaude, "session-2", " ", a)
	if err := storage.PutAnchoredSession(ctx, unattributed); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a report without an uploader: %v", err)
	}
	if _, err := storage.ListAnchoredCommitLinks(ctx, repo, "abc1234"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a short commit number: %v", err)
	}
}

func TestFSAnchoredSessionPersistence(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	checkAnchoredSessionPersistence(t, context.Background(), storage, repo)
	checkRestoredAnchoredSessionPersistence(t, context.Background(), storage, repo)
}

func TestFSAnchoredSessionNeverUsesAnIdentifierAsAPath(t *testing.T) {
	dir := t.TempDir()
	storage := NewFSStore(dir)
	repo := domain.HashContent([]byte(t.Name()))
	session := anchoredStoreSession(t, repo, domain.ProviderClaude, "..", "uploader-1")
	if err := storage.PutAnchoredSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	path := storage.anchoredSessionPath(repo, domain.ProviderClaude, "..")
	if !strings.HasPrefix(path, storage.anchoredSessionDir(repo, domain.ProviderClaude)+string(filepath.Separator)) || strings.Contains(filepath.Base(path), "..") {
		t.Fatalf("path = %s", path)
	}
	// A report changed on disk is an integrity failure, not a different report.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"uploader-1"`, `"someone-else"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetAnchoredSession(context.Background(), repo, domain.ProviderClaude, ".."); err != nil || got.Uploader != "someone-else" {
		// The uploader is attribution outside the hashed body; changing the body must fail.
		t.Fatalf("attribution outside the body: %+v %v", got, err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"working-folder"`, `"origin-url"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetAnchoredSession(context.Background(), repo, domain.ProviderClaude, ".."); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("a body changed on disk: %v", err)
	}
}

// checkRestoredAnchoredSessionPersistence: a report rebuilt from captures is
// stored, and leads from its commits, like any other.
func checkRestoredAnchoredSessionPersistence(t *testing.T, ctx context.Context, storage outbound.AnchoredSessionStore, repo domain.ContentHash) {
	t.Helper()
	sha := anchoredStoreSHA("d")
	body, ok := domain.RestoreAnchoredSession([]domain.AnchoredCapture{{Snapshot: domain.HashContent([]byte("capture")), Doc: domain.HashContent([]byte("capture")),
		Identity: domain.DocumentIdentityLegacy, CreatedAt: time.Date(2031, 1, 2, 3, 0, 0, 0, time.UTC), Commits: []string{sha}, Events: 3, Prefix: true}})
	if !ok {
		t.Fatal("nothing restored")
	}
	hash, err := body.Hash()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	restored := domain.AnchoredSession{RepoID: repo, Provider: domain.ProviderClaude, SessionID: "session-restored", Uploader: domain.AnchoredRestoredUploader,
		Revision: 1, BodyHash: hash, CreatedAt: at, UpdatedAt: at, Body: body}
	if err := storage.PutAnchoredSession(ctx, restored); err != nil {
		t.Fatal(err)
	}
	got, err := storage.GetAnchoredSession(ctx, repo, domain.ProviderClaude, "session-restored")
	if err != nil || got.Uploader != domain.AnchoredRestoredUploader || got.Body.Scope != domain.AnchoredScopeCaptures {
		t.Fatalf("restored report: %+v %v", got, err)
	}
	links, err := storage.ListAnchoredCommitLinks(ctx, repo, sha)
	if err != nil || len(links) != 1 || links[0].Rule != domain.AnchoredLinkCapturedAtCommit || links[0].SessionID != "session-restored" {
		t.Fatalf("links of the restored commit: %+v %v", links, err)
	}
}
