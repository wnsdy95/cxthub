package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type effectiveFixture struct {
	svc                      *Service
	st                       *store.FSStore
	repo, root, a, b, memory domain.ContentHash
	digest                   domain.MemoryDigest
	reader                   *scanReader
	next                     int
}

func effectiveOID(n int) string { return fmt.Sprintf("%040x", n) }
func newEffectiveFixture(t *testing.T) *effectiveFixture {
	t.Helper()
	ctx := context.Background()
	svc, st := newFsckSvc(t)
	f := &effectiveFixture{svc: svc, st: st, repo: hh(t.Name()), root: hh("root"), a: hh("A"), b: hh("B")}
	origin := "https://github.com/example/effective-memory"
	if _, err := st.PutRepo(ctx, domain.Repo{ID: f.repo, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	old := domain.GitEntry{OID: effectiveOID(101), Mode: "100644"}
	fresh := domain.GitEntry{OID: effectiveOID(102), Mode: "100755"}
	other := domain.GitEntry{OID: effectiveOID(103), Mode: "100644"}
	change := func(path string, before, after domain.GitEntry) domain.GitPathChange {
		return domain.GitPathChange{Path: path, Before: before, After: after}
	}
	delta := func(n, parent int, changes ...domain.GitPathChange) domain.GitCommitDelta {
		d := domain.GitCommitDelta{Commit: effectiveOID(n), Parents: []string{}, Complete: true, Changes: changes}
		if parent != 0 {
			d.Parent = effectiveOID(parent)
			d.Parents = []string{d.Parent}
		}
		return d
	}
	f.reader = &scanReader{deltas: map[string]domain.GitCommitDelta{
		effectiveOID(1): delta(1, 0, change("a", domain.GitEntry{}, old), change("b", domain.GitEntry{}, old)),
		effectiveOID(2): delta(2, 1, change("a", old, fresh), change("b", old, fresh)),
		effectiveOID(3): delta(3, 2, change("x", domain.GitEntry{}, other)),
		effectiveOID(4): delta(4, 3, change("a", fresh, old), change("b", fresh, old)),
		effectiveOID(5): delta(5, 4, change("a", old, fresh), change("b", old, fresh)),
		effectiveOID(6): delta(6, 3, change("a", fresh, old)), effectiveOID(7): delta(7, 3, change("a", fresh, other)),
		effectiveOID(8): delta(8, 1, change("a", old, fresh), change("b", old, fresh)), effectiveOID(9): delta(9, 1, change("a", old, fresh), change("b", old, fresh))}}
	scans, err := NewGitScans(svc, f.reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, tip := range []int{5, 6, 7, 8, 9} {
		if _, err = scans.ObservePush(ctx, origin, "refs/heads/main", "", effectiveOID(tip), false, fmt.Sprint(tip)); err != nil {
			t.Fatal(err)
		}
	}
	if err = scans.Process(ctx, 100); err != nil {
		t.Fatal(err)
	}
	for _, id := range []domain.ContentHash{f.a, f.b, f.root} {
		snap := domain.Snapshot{ID: id, RepoID: f.repo, DocHash: id}
		if id == f.root {
			snap.Parents = []domain.ContentHash{f.a, f.b}
		}
		if err = st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	f.digest = domain.MemoryDigest{SnapshotID: f.root, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{
		{SourceSnapshot: f.a, Summary: "old untyped prose", Claims: []domain.MemoryClaim{{Kind: "code", Text: "A feature", Code: &domain.MemoryCodeScope{Commit: effectiveOID(2), Paths: []string{"a", "b"}}}, {Kind: "rationale", Text: "why A was tried"}}},
		{SourceSnapshot: f.b, Claims: []domain.MemoryClaim{{Kind: "code", Text: "B feature", Code: &domain.MemoryCodeScope{Commit: effectiveOID(3), Paths: []string{"x"}}}}}}}
	f.memory, err = svc.PutMemoryDigestCAS(ctx, f.repo, f.digest)
	if err != nil {
		t.Fatal(err)
	}
	f.publish(t, f.a, 2, "branch-a")
	f.publish(t, f.b, 3, "branch-b")
	return f
}
func (f *effectiveFixture) publish(t *testing.T, snap domain.ContentHash, code int, branch string) domain.HistoryEvent {
	t.Helper()
	f.next++
	e := domain.HistoryEvent{ID: fmt.Sprintf("%032x", f.next), RepoID: string(f.repo), BranchID: branch, Branch: branch, Kind: "position", Target: snap, GitAfter: effectiveOID(code), CreatedAt: time.Date(2026, 1, 1, 0, 0, f.next, 0, time.UTC)}
	if err := f.st.ApplyHistoryEvent(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	p := prPublication(e)
	if err := f.st.ApplyHistoryEvent(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}
func (f *effectiveFixture) query(t *testing.T, code int) domain.EffectiveMemoryPage {
	t.Helper()
	out, err := f.svc.QueryEffectiveMemory(context.Background(), f.repo, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: f.root, CodeCommit: effectiveOID(code)}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func itemByText(t *testing.T, out domain.EffectiveMemoryPage, text string) domain.EffectiveMemoryItem {
	t.Helper()
	for _, item := range out.Items {
		if item.Text == text {
			return item
		}
	}
	t.Fatalf("missing %q in %+v", text, out)
	return domain.EffectiveMemoryItem{}
}
func TestEffectiveMemoryReversalAndIndependentSelections(t *testing.T) {
	f := newEffectiveFixture(t)
	ctx := context.Background()
	history, _ := f.svc.ListHistory(ctx, f.repo)
	refs, _ := f.svc.ListRefs(ctx, f.repo)
	revision, _ := f.svc.RepositoryRevision(ctx, f.repo)
	f.reader.offline = true
	reads := f.reader.reads
	for _, tt := range []struct {
		code int
		a, b string
	}{{3, "applied", "applied"}, {4, "inactive", "applied"}, {5, "applied", "applied"}, {6, "review", "applied"}, {7, "review", "applied"}, {1, "inactive", "inactive"}, {8, "review", "inactive"}, {99, "review", "review"}} {
		out := f.query(t, tt.code)
		if a, b := itemByText(t, out, "A feature"), itemByText(t, out, "B feature"); a.State != tt.a || b.State != tt.b {
			t.Fatalf("code %d: A=%+v B=%+v", tt.code, a, b)
		}
		if itemByText(t, out, "why A was tried").State != "retained" {
			t.Fatal("lost rationale")
		}
		if itemByText(t, out, "old untyped prose").State != "review" {
			t.Fatal("legacy prose treated as verified code")
		}
	}
	if itemByText(t, f.query(t, 3), "A feature").State != "applied" {
		t.Fatal("selection leaked")
	}
	afterHistory, _ := f.svc.ListHistory(ctx, f.repo)
	afterRefs, _ := f.svc.ListRefs(ctx, f.repo)
	afterRevision, _ := f.svc.RepositoryRevision(ctx, f.repo)
	if !reflect.DeepEqual(history, afterHistory) || !reflect.DeepEqual(refs, afterRefs) || revision != afterRevision || reads != f.reader.reads {
		t.Fatal("read query wrote state or contacted provider")
	}
}
func (f *effectiveFixture) receipt(t *testing.T, source domain.ContentHash) domain.HistoryEvent {
	t.Helper()
	f.next++
	e := domain.HistoryEvent{ID: fmt.Sprintf("%032x", f.next), RepoID: string(f.repo), BranchID: "base", Branch: "main", SourceBranchID: "branch-a", Kind: "pr-merge", Source: source, Target: source, CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), PR: &domain.PullRequestMerge{Number: 1, HeadBranch: "branch-a", BaseBranch: "main", HeadSHA: effectiveOID(2), MergeSHA: effectiveOID(9)}}
	if err := f.st.ApplyHistoryEvent(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return e
}
func (f *effectiveFixture) complete(t *testing.T, e domain.HistoryEvent) {
	t.Helper()
	id := sha256.Sum256([]byte(e.ID + ":completed"))
	e.ID = fmt.Sprintf("%x", id[:16])
	e.PRCompleted = true
	if err := f.st.ApplyHistoryEvent(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}
func TestEffectiveMemorySquashRequiresCompletedImmutableProof(t *testing.T) {
	for _, mode := range []string{"exact", "natural", "graft", "wrong-git", "wrong-identity"} {
		t.Run(mode, func(t *testing.T) {
			f := newEffectiveFixture(t)
			source := f.a
			if mode == "natural" || mode == "graft" {
				source = hh("receipt tip")
				snap := domain.Snapshot{ID: source, DocHash: source, RepoID: f.repo}
				if mode == "natural" {
					snap.Parents = []domain.ContentHash{f.a}
				} else {
					snap.GraftParents = []domain.ContentHash{f.a}
					snap.GraftSeq = 1
				}
				if err := f.st.PutSnapshot(context.Background(), snap); err != nil {
					t.Fatal(err)
				}
			}
			if itemByText(t, f.query(t, 9), "A feature").Reason != "equivalent_without_integration" {
				t.Fatal("same bytes proved merge")
			}
			receipt := f.receipt(t, source)
			if itemByText(t, f.query(t, 9), "A feature").State != "review" {
				t.Fatal("pending receipt proved merge")
			}
			if mode == "wrong-git" || mode == "wrong-identity" {
				receipt.ID = fmt.Sprintf("%032x", 100)
				if mode == "wrong-git" {
					p := *receipt.PR
					p.HeadSHA = effectiveOID(8)
					receipt.PR = &p
				} else {
					receipt.SourceBranchID = "another-branch"
				}
				if err := f.st.ApplyHistoryEvent(context.Background(), receipt); err != nil {
					t.Fatal(err)
				}
			}
			f.complete(t, receipt)
			got := itemByText(t, f.query(t, 9), "A feature")
			if mode == "exact" || mode == "natural" {
				if got.State != "applied" || got.IntegrationReceipt == "" {
					t.Fatalf("valid squash missing: %+v", got)
				}
				if itemByText(t, f.query(t, 8), "A feature").State != "review" {
					t.Fatal("receipt applied outside selected merge ancestry")
				}
			} else if got.State != "review" {
				t.Fatalf("unproven integration: %+v", got)
			}
		})
	}
}
func TestEffectiveMemoryRequiresExactSourcePublication(t *testing.T) {
	f := newEffectiveFixture(t)
	f.digest.PreviousMemoryHash = f.memory
	f.digest.Fragments[0].Claims[0].Code.Commit = effectiveOID(8)
	if _, err := f.svc.PutMemoryDigestCAS(context.Background(), f.repo, f.digest); err != nil {
		t.Fatal(err)
	}
	if got := itemByText(t, f.query(t, 8), "A feature"); got.Reason != "source_publication_missing" {
		t.Fatal(got)
	}
	f.publish(t, f.a, 8, "branch-a")
	if got := itemByText(t, f.query(t, 8), "A feature"); got.State != "applied" {
		t.Fatal(got)
	}
	old, err := f.svc.QueryEffectiveMemory(context.Background(), f.repo, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: f.root, CodeCommit: effectiveOID(8), MemoryHash: f.memory}})
	if err != nil || itemByText(t, old, "A feature").State != "review" {
		t.Fatal(old, err)
	}
}
func TestEffectiveMemoryCursorPinsStateExceptPending(t *testing.T) {
	for _, change := range []string{"pending", "memory", "history", "evidence", "code", "graph"} {
		t.Run(change, func(t *testing.T) {
			f := newEffectiveFixture(t)
			ctx := context.Background()
			req := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: f.root, CodeCommit: effectiveOID(3)}, Limit: 1}
			first, err := f.svc.QueryEffectiveMemory(ctx, f.repo, req)
			if err != nil || first.NextCursor == "" {
				t.Fatal(first, err)
			}
			req.Cursor = first.NextCursor
			switch change {
			case "pending":
				err = f.st.AdvanceRepositoryRevision(ctx, f.repo, true)
			case "graph":
				err = f.st.AdvanceRepositoryRevision(ctx, f.repo, false)
			case "evidence":
				err = f.st.AdvanceEvidenceRevision(ctx, f.repo)
			case "history":
				f.publish(t, f.a, 8, "branch-a")
			case "code":
				req.Selection.CodeCommit = effectiveOID(4)
			case "memory":
				f.digest.PreviousMemoryHash = f.memory
				f.digest.Fragments[0].Summary = "new historical summary"
				_, err = f.svc.PutMemoryDigestCAS(ctx, f.repo, f.digest)
			}
			if err != nil {
				t.Fatal(err)
			}
			next, err := f.svc.QueryEffectiveMemory(ctx, f.repo, req)
			if change == "pending" {
				if err != nil || next.StateHash != first.StateHash || next.Items[0].ID == first.Items[0].ID {
					t.Fatal(next, err)
				}
			} else if !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("stale cursor survived %s: %v", change, err)
			}
		})
	}
}
func TestEffectiveMemoryLegacyPaginationPreservesUTF8AndRepeatedPieces(t *testing.T) {
	f := newEffectiveFixture(t)
	ctx := context.Background()
	text := strings.Repeat("\ud55c\uad6d\uc5b4 ", 100000)
	d := domain.MemoryDigest{SnapshotID: f.root, ClaimsVersion: 1, PreviousMemoryHash: f.memory, Summary: text}
	hash, err := f.svc.PutMemoryDigestCAS(ctx, f.repo, d)
	if err != nil {
		t.Fatal(err)
	}
	req := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: f.root, CodeCommit: effectiveOID(3), MemoryHash: hash}, Limit: 50}
	var joined strings.Builder
	ids := map[domain.ContentHash]bool{}
	pages := 0
	for {
		out, err := f.svc.QueryEffectiveMemory(ctx, f.repo, req)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		bytes := 0
		for _, item := range out.Items {
			raw, _ := json.Marshal(item)
			bytes += len(raw)
			if !utf8.ValidString(item.Text) || ids[item.ID] || item.State != "review" || len(item.Text) > 8192 {
				t.Fatal("invalid fragment", item.ID)
			}
			ids[item.ID] = true
			joined.WriteString(item.Text)
		}
		if bytes > effectiveMemoryPageBytes {
			t.Fatal("page exceeded bound", bytes)
		}
		if out.NextCursor == "" {
			break
		}
		req.Cursor = out.NextCursor
	}
	if pages < 2 || joined.String() != text {
		t.Fatal("legacy content lost/repeated", pages, joined.Len(), len(text))
	}
	req.Selection.SnapshotID = f.a
	req.Cursor = ""
	if _, err = f.svc.QueryEffectiveMemory(ctx, f.repo, req); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("foreign snapshot pin accepted", err)
	}
}

func TestEffectiveMemoryReadBoundEndsPageBeforeChangingLaterClaim(t *testing.T) {
	f := newEffectiveFixture(t)
	ctx := context.Background()
	evidence, err := f.svc.newCodeEvidence(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	history, err := f.svc.ListHistory(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := newMemoryIntegration(f.svc, evidence, f.repo, history)
	if err != nil {
		t.Fatal(err)
	}
	items, err := effectiveMemoryItems(f.digest)
	if err != nil {
		t.Fatal(err)
	}
	// Emulate a preceding expensive claim that consumed the page budget. The
	// next claim has not been emitted and must get a fresh budget next page.
	evidence.reads = maxCodeEvidenceReads
	out := domain.EffectiveMemoryPage{Selection: domain.EffectiveMemorySelection{SnapshotID: f.root, CodeCommit: effectiveOID(3)}, StateHash: hh("generation"), Items: []domain.EffectiveMemoryItem{{ID: hh("preceding item")}}}
	got, err := resolveEffectiveMemoryPage(ctx, resolver, out, items, 0, 20)
	if err != nil || len(got.Items) != 1 || got.NextCursor == "" {
		t.Fatal("later claim mislabeled from shared budget", got, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(got.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	var cursor effectiveMemoryCursor
	if err = json.Unmarshal(raw, &cursor); err != nil || cursor.Index != 0 {
		t.Fatal("claim skipped", cursor, err)
	}
	fresh, err := f.svc.newCodeEvidence(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err = newMemoryIntegration(f.svc, fresh, f.repo, history)
	if err != nil {
		t.Fatal(err)
	}
	out.Items = []domain.EffectiveMemoryItem{}
	next, err := resolveEffectiveMemoryPage(ctx, resolver, out, items, cursor.Index, 1)
	if err != nil || next.Items[0].State != "applied" {
		t.Fatal("fresh page failed to verify", next, err)
	}
}

func TestEffectiveMemoryCursorRejectsNewOriginBinding(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh(t.Name())
	id := hh("origin-binding-root")
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: repo, DocHash: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutMemoryDigest(ctx, repo, domain.MemoryDigest{SnapshotID: id, Summary: "historical summary", KeyFacts: []string{"historical fact"}}); err != nil {
		t.Fatal(err)
	}
	req := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: id, CodeCommit: effectiveOID(1)}, Limit: 1}
	page, err := svc.QueryEffectiveMemory(ctx, repo, req)
	if err != nil || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	// Binding an initially absent origin is supported; replacing an existing
	// origin is intentionally not an ordinary PutRepo operation.
	if _, err = st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: "https://github.com/example/new-origin"}); err != nil {
		t.Fatal(err)
	}
	req.Cursor = page.NextCursor
	if _, err = svc.QueryEffectiveMemory(ctx, repo, req); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("cursor ignored origin binding", err)
	}
}
