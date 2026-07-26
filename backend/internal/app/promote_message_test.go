package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// TestPromoteSnapshotMessage enforces unidirectional promotion rules for hook labels:
// hook prefix → commit message promotion allowed, non-prefixed snapshots only allow the same message (idempotent),
// any rewriting or reverse promotion to hook prefix is denied.
func TestPromoteSnapshotMessage(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	svc := NewService(st, st, nil, nil, st)
	repo := domain.HashContent([]byte("repo"))

	doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
	doc.CIR.Envelope.CIRVersion = "1"
	doc.CIR.Envelope.SourceProvider = "claude"
	doc.CIR.Events = []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Seq: 0,
		Blocks: []domain.ContentBlock{{Type: "text", Text: "work"}}}}
	cb, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(cb)
	if _, err := st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main",
		Message: domain.HookMessagePrefix + "checkpoint", Provider: "claude"}
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}

	// hook prefix → commit message promotion allowed.
	if err := svc.PromoteSnapshotMessage(ctx, repo, snap.ID, "feat: real commit [git abc1234]"); err != nil {
		t.Fatalf("promotion failed: %v", err)
	}
	got, err := st.GetSnapshot(ctx, repo, snap.ID)
	if err != nil || got.Message != "feat: real commit [git abc1234]" {
		t.Fatalf("promotion not reflected: %q %v", got.Message, err)
	}
	// idempotent retries (same message) allowed.
	if err := svc.PromoteSnapshotMessage(ctx, repo, snap.ID, "feat: real commit [git abc1234]"); err != nil {
		t.Fatalf("idempotent retry denied: %v", err)
	}
	// Refuse to rewrite messages after promotion.
	if err := svc.PromoteSnapshotMessage(ctx, repo, snap.ID, "rewritten"); err == nil {
		t.Fatal("Non-hook label rewrite is not allowed")
	}
	// Refuse reverse promotion back to a hook-prefixed message.
	if err := svc.PromoteSnapshotMessage(ctx, repo, snap.ID, domain.HookMessagePrefix+"again"); err == nil {
		t.Fatal("hook-prefixed message was unexpectedly allowed")
	}
	// Empty or excessive messages are not allowed — length is measured in characters (same unit as openapi maxLength).
	if err := svc.PromoteSnapshotMessage(ctx, repo, snap.ID, "  "); err == nil {
		t.Fatal("empty message was unexpectedly allowed")
	}
	if err := svc.PromoteSnapshotMessage(ctx, repo, snap.ID, strings.Repeat("x", 2001)); err == nil {
		t.Fatal("oversized message was unexpectedly allowed")
	}
	// Promotion is not allowed in conflicting state (409 Contract) — must be surfaced as ErrConflict.
	if err := svc.PromoteSnapshotMessage(ctx, repo, snap.ID, "rewritten again"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("Promotion not possible, not ErrConflict: %v", err)
	}
}

// TestPromoteRuneLength ensures that the length limit is in runes, not bytes —
// to prevent regressions where multi-byte messages like Korean were rejected even though they were shorter than the document limit (2000 characters).
func TestPromoteRuneLength(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	svc := NewService(st, st, nil, nil, st)
	repo := domain.HashContent([]byte("rune-repo"))

	doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
	doc.CIR.Envelope.CIRVersion = "1"
	doc.CIR.Envelope.SourceProvider = "claude"
	doc.CIR.Events = []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Seq: 0,
		Blocks: []domain.ContentBlock{{Type: "text", Text: "work"}}}}
	cb, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(cb)
	if _, err := st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main",
		Message: domain.HookMessagePrefix + "checkpoint", Provider: "claude"}
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	// 1,500 multibyte characters = 3,000 bytes, which a byte-count limit would incorrectly reject.
	multibyte := strings.Repeat("é", 1500)
	if err := svc.PromoteSnapshotMessage(ctx, repo, snap.ID, multibyte); err != nil {
		t.Fatalf("message of 2,000 or fewer Unicode characters rejected: %v", err)
	}
}

// TestUpdateSnapshotMessageStoreCAS fixes the storage layer CAS: in concurrent promotion races,
// only one side should win (last-write-wins blocking), and the other side should return ErrConflict. Both updates must not be lost even when running concurrently with AddGraftParents (review P1 —
// by locking only the promotion function, lost-update conflicts with other meta updates remain).
func TestUpdateSnapshotMessageStoreCAS(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte("cas-repo"))
	id := domain.HashContent([]byte("cas-snap"))
	snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main",
		Message: domain.HookMessagePrefix + "checkpoint", Provider: "claude"}
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}

	// Concurrent promotion: exactly one out of N different messages must succeed.
	const n = 8
	graft := domain.HashContent([]byte("graft-parent"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{
		ID: graft, DocHash: graft, RepoID: repo, Branch: "main", Provider: "claude",
	}); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- st.UpdateSnapshotMessage(ctx, repo, id, fmt.Sprintf("commit message %d", i))
		}(i)
	}
	// GraftParents updates also compete in the same channel — if lost, it indicates a broken lock sharing.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := st.AddGraftParents(ctx, repo, id, []domain.ContentHash{graft}); err != nil {
			t.Errorf("AddGraftParents: %v", err)
		}
	}()
	wg.Wait()
	close(errs)

	won, lost := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, domain.ErrConflict):
			lost++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 || lost != n-1 {
		t.Fatalf("CAS violation: success %d / conflicts %d (want 1/%d)", won, lost, n-1)
	}
	got, err := st.GetSnapshot(ctx, repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got.Message, domain.HookMessagePrefix) {
		t.Fatalf("Promotion not reflected: %q", got.Message)
	}
	found := false
	for _, g := range got.GraftParents {
		if g == graft {
			found = true
		}
	}
	if !found {
		t.Fatalf("GraftParents lost in concurrent update: %v", got.GraftParents)
	}
}
