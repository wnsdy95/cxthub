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

	// A session is archived by an account at a time, and restored. A report
	// uploaded again leaves the mark as it is, whatever it says itself.
	archivedAt := first.UpdatedAt.Add(time.Hour)
	if err := storage.SetAnchoredSessionArchive(ctx, repo, domain.ProviderClaude, "session-1", &archivedAt, "manager-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetAnchoredSession(ctx, repo, domain.ProviderClaude, "session-1"); err != nil || got.ArchivedAt == nil || !got.ArchivedAt.Equal(archivedAt) || got.ArchivedBy != "manager-1" || got.Revision != 2 {
		t.Fatalf("an archived session: %+v %v", got, err)
	}
	if got, err := storage.GetAnchoredSession(ctx, repo, domain.ProviderCodex, "session-1"); err != nil || got.ArchivedAt != nil || got.ArchivedBy != "" {
		t.Fatalf("the other session was archived with it: %+v %v", got, err)
	}
	if listed, err := storage.ListRepositoryAnchoredSessions(ctx, repo); err != nil {
		t.Fatal(err)
	} else {
		archived := 0
		for _, session := range listed {
			if session.Archived() {
				archived++
			}
		}
		if archived != 1 {
			t.Fatalf("the list holds %d archived sessions", archived)
		}
	}
	again := anchoredStoreSession(t, repo, domain.ProviderClaude, "session-1", "uploader-1", a, c)
	again.Revision, again.UpdatedAt = 3, first.UpdatedAt.Add(2*time.Hour)
	if err := storage.PutAnchoredSession(ctx, again); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetAnchoredSession(ctx, repo, domain.ProviderClaude, "session-1"); err != nil || got.Revision != 3 || got.ArchivedAt == nil || !got.ArchivedAt.Equal(archivedAt) || got.ArchivedBy != "manager-1" {
		t.Fatalf("a report uploaded again undid the archive: %+v %v", got, err)
	}
	if err := storage.SetAnchoredSessionArchive(ctx, repo, domain.ProviderClaude, "session-1", nil, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetAnchoredSession(ctx, repo, domain.ProviderClaude, "session-1"); err != nil || got.ArchivedAt != nil || got.ArchivedBy != "" || got.Revision != 3 {
		t.Fatalf("a restored session: %+v %v", got, err)
	}
	// A report that claims to be archived does not archive.
	claimed := anchoredStoreSession(t, repo, domain.ProviderClaude, "session-3", "uploader-1", a)
	claimed.ArchivedAt, claimed.ArchivedBy = &archivedAt, "uploader-1"
	if err := storage.PutAnchoredSession(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetAnchoredSession(ctx, repo, domain.ProviderClaude, "session-3"); err != nil || got.Archived() {
		t.Fatalf("a report archived its session: %+v %v", got, err)
	}
	if err := storage.SetAnchoredSessionArchive(ctx, repo, domain.ProviderClaude, "missing", &archivedAt, "manager-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("archiving a session that is not stored: %v", err)
	}
	for name, change := range map[string]func() error{
		"a time without an account": func() error {
			return storage.SetAnchoredSessionArchive(ctx, repo, domain.ProviderClaude, "session-1", &archivedAt, "")
		},
		"an account without a time": func() error {
			return storage.SetAnchoredSessionArchive(ctx, repo, domain.ProviderClaude, "session-1", nil, "manager-1")
		},
		"a key that is not one": func() error {
			return storage.SetAnchoredSessionArchive(ctx, repo, domain.ProviderClaude, "a/b", &archivedAt, "manager-1")
		},
	} {
		if err := change(); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: %v", name, err)
		}
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
