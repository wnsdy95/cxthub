package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestContextDeliverySelectedDisplayPromotion(t *testing.T) {
	for _, change := range []string{"message API", "branch label only"} {
		t.Run(change, func(t *testing.T) {
			svc, st := newFsckSvc(t)
			ctx := systemTestContext()
			repo := hh(t.Name())
			if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
				t.Fatal(err)
			}
			doc := domain.SessionDoc{CIR: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "synthetic-display-promotion"},
				Events:   []domain.CIREvent{{Seq: 0, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "selected synthetic content"}}}},
			}}
			raw, err := domain.CanonicalBytes(doc.CIR)
			if err != nil {
				t.Fatal(err)
			}
			doc.Hash = domain.HashContent(raw)
			if _, err := st.PutDoc(ctx, repo, doc); err != nil {
				t.Fatal(err)
			}
			snap := domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Branch: "main", Message: domain.HookMessagePrefix + "checkpoint", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, SessionID: doc.CIR.Envelope.SessionOriginID}
			if change == "branch label only" {
				snap.Branch = domain.StashBranchLabel
			}
			if err := st.PutSnapshot(ctx, snap); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.PutMemoryDigestCAS(ctx, repo, domain.MemoryDigest{SnapshotID: snap.ID, Summary: "selected synthetic memory"}); err != nil {
				t.Fatal(err)
			}
			ref := domain.Ref{Kind: domain.RefBranch, Name: "main", BranchID: "main-identity", RepoID: repo, Target: snap.ID}
			if err := st.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
				t.Fatal(err)
			}
			segmentPublish(t, svc, repo, snap.ID, 1)
			selection := domain.ContextSelection{Scope: "current", Branch: "main", Position: "main", CodeCommit: effectiveOID(1)}
			query := func() domain.ContextQueryView {
				v, err := svc.QueryContext(ctx, repo, selection)
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			memory := func() domain.EffectiveMemoryPage {
				v, err := svc.QueryEffectiveMemory(ctx, repo, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: snap.ID, CodeCommit: effectiveOID(1)}, Content: "prompt", Limit: 50})
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			before, beforeMemory := query(), memory()
			storedBefore, err := st.GetSnapshot(ctx, repo, snap.ID)
			if err != nil {
				t.Fatal(err)
			}
			historyBefore, err := svc.ListHistory(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			docBefore, err := st.GetDoc(ctx, repo, snap.DocHash)
			if err != nil {
				t.Fatal(err)
			}
			if domain.ValidateContentHash(before.DeliveryStateHash) != nil || domain.ValidateContentHash(beforeMemory.DeliveryStateHash) != nil {
				t.Fatal("missing initial delivery proof")
			}
			if change == "message API" {
				// Exercise the application command used by the HTTP promotion API.
				if err := svc.PromoteSnapshotMessage(ctx, repo, snap.ID, "synthetic commit caption"); err != nil {
					t.Fatal(err)
				}
			} else {
				// Exercise the supported stash-to-commit storage promotion, keeping
				// Message identical so only the scalar branch label is promoted.
				promoted := storedBefore
				promoted.Branch = "main"
				if err := st.PutSnapshot(ctx, promoted); err != nil {
					t.Fatal(err)
				}
				if err := st.AdvanceRepositoryRevision(ctx, repo, false); err != nil {
					t.Fatal(err)
				}
			}
			after, afterMemory := query(), memory()
			storedAfter, err := st.GetSnapshot(ctx, repo, snap.ID)
			if err != nil {
				t.Fatal(err)
			}
			historyAfter, err := svc.ListHistory(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			docAfter, err := st.GetDoc(ctx, repo, snap.DocHash)
			if err != nil {
				t.Fatal(err)
			}
			if change == "message API" {
				if storedAfter.Message == storedBefore.Message || storedAfter.Branch != storedBefore.Branch {
					t.Fatal("fixture did not isolate message promotion")
				}
				storedAfter.Message = storedBefore.Message
			} else {
				if storedAfter.Branch == storedBefore.Branch || storedAfter.Message != storedBefore.Message {
					t.Fatal("fixture did not isolate branch promotion")
				}
				storedAfter.Branch = storedBefore.Branch
			}
			if !reflect.DeepEqual(storedBefore, storedAfter) || !reflect.DeepEqual(docBefore, docAfter) || !reflect.DeepEqual(historyBefore, historyAfter) {
				t.Fatal("display promotion changed immutable source metadata/content or receipts")
			}
			if !reflect.DeepEqual(before.Inclusion, after.Inclusion) || !reflect.DeepEqual(before.Semantics, after.Semantics) || !reflect.DeepEqual(beforeMemory.Items, afterMemory.Items) {
				t.Fatal("display promotion changed selected semantic content")
			}
			if before.StateHash == after.StateHash || beforeMemory.StateHash == afterMemory.StateHash || after.Revision.Graph <= before.Revision.Graph {
				t.Fatal("legacy state/revision fences did not change")
			}
			if before.DeliveryStateHash != after.DeliveryStateHash || beforeMemory.DeliveryStateHash != afterMemory.DeliveryStateHash {
				t.Fatal("display-only promotion invalidated delivery proof")
			}
			segmentPublish(t, svc, repo, snap.ID, 2)
			if query().DeliveryStateHash == after.DeliveryStateHash {
				t.Fatal("selected publication provenance change was hidden")
			}
		})
	}
}

func TestContextDeliveryIgnoresUnselectedTraffic(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh(t.Name())
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	snap := segmentCapture(t, st, repo, []string{"selected"})
	ref := domain.Ref{Kind: domain.RefBranch, Name: "main", BranchID: "main-id", RepoID: repo, Target: snap.ID}
	if err := st.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	selection := domain.ContextSelection{Scope: "current", Branch: "main", Position: "main", CodeCommit: effectiveOID(1)}
	query := func() domain.ContextQueryView {
		v, err := svc.QueryContext(ctx, repo, selection)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := query()
	if domain.ValidateContentHash(before.DeliveryStateHash) != nil {
		t.Fatal("missing proof")
	}
	// A worker branches from the same capture, then publishes somewhere else.
	// The selected source now has an extra display membership, but no new proof
	// about its contents, code, or integration.
	other := ref
	other.Name, other.BranchID = "worker", "worker-id"
	if err := st.CompareAndSwapRef(ctx, repo, other, ""); err != nil {
		t.Fatal(err)
	}
	elsewhere := segmentCapture(t, st, repo, []string{"unrelated"})
	event := domain.HistoryEvent{ID: strings.Repeat("e", 32), RepoID: string(repo), Branch: "worker", BranchID: "worker-id", Kind: "position", Source: snap.ID, Target: elsewhere.ID, GitAfter: effectiveOID(2), CreatedAt: time.Unix(2, 0).UTC()}
	if err := st.ApplyHistoryEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyHistoryEvent(ctx, prPublication(event)); err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceRepositoryRevision(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceEvidenceRevision(ctx, repo); err != nil {
		t.Fatal(err)
	}
	after := query()
	if after.DeliveryStateHash != before.DeliveryStateHash {
		t.Fatal("unrelated worker changed selected delivery proof")
	}
	if reflect.DeepEqual(after.Snapshots[0].Branches, before.Snapshots[0].Branches) || after.StateHash == before.StateHash {
		t.Fatal("fixture did not exercise projected membership/legacy state drift")
	}
	if after.Revision.Graph <= before.Revision.Graph || after.Revision.Evidence <= before.Revision.Evidence {
		t.Fatal("fixture did not advance both global clocks")
	}
	// Publication provenance of the selected source really is part of delivery.
	segmentPublish(t, svc, repo, snap.ID, 3)
	published := query()
	if published.DeliveryStateHash == after.DeliveryStateHash {
		t.Fatal("selected publication omitted")
	}
	if _, err := svc.PutMemoryDigestCAS(ctx, repo, domain.MemoryDigest{SnapshotID: snap.ID, Summary: "selected new memory"}); err != nil {
		t.Fatal(err)
	}
	if query().DeliveryStateHash == published.DeliveryStateHash {
		t.Fatal("selected memory attachment omitted")
	}
}

func TestEffectiveMemoryDeliveryCompleteAcrossPagesAndGlobalDrift(t *testing.T) {
	f := newEffectiveFixture(t)
	ctx := systemTestContext()
	req := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: f.root, CodeCommit: effectiveOID(3)}, Limit: 1}
	query := func(req domain.EffectiveMemoryRequest) domain.EffectiveMemoryPage {
		v, err := f.svc.QueryEffectiveMemory(ctx, f.repo, req)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := query(req)
	if domain.ValidateContentHash(first.DeliveryStateHash) != nil || first.NextCursor == "" {
		t.Fatal("missing complete proof or pagination")
	}
	for cursor := first.NextCursor; cursor != ""; {
		nextReq := req
		nextReq.Cursor = cursor
		next := query(nextReq)
		if next.DeliveryStateHash != first.DeliveryStateHash {
			t.Fatal("page-local delivery hash")
		}
		cursor = next.NextCursor
	}
	for _, mutate := range []func() error{
		func() error { return f.st.AdvanceRepositoryRevision(ctx, f.repo, true) },
		func() error { return f.st.AdvanceRepositoryRevision(ctx, f.repo, false) },
		func() error { return f.st.AdvanceEvidenceRevision(ctx, f.repo) },
		func() error {
			id := hh("unrelated capture")
			if err := f.st.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: f.repo, DocHash: id}); err != nil {
				return err
			}
			f.publish(t, id, 8, "worker")
			return nil
		},
	} {
		if err := mutate(); err != nil {
			t.Fatal(err)
		}
		if query(req).DeliveryStateHash != first.DeliveryStateHash {
			t.Fatal("global revision/history entered selected proof")
		}
	}
	stale := req
	stale.Cursor = first.NextCursor
	if _, err := f.svc.QueryEffectiveMemory(ctx, f.repo, stale); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
		t.Fatal("legacy cursor fence changed", err)
	}
	// B is outside page one; adding its exact accepted publication must change
	// the complete proof without changing the first returned item.
	f.publish(t, f.b, 3, "another-b-publication")
	after := query(req)
	if after.DeliveryStateHash == first.DeliveryStateHash || !reflect.DeepEqual(after.Items, first.Items) {
		t.Fatal("outside-page provenance not bound independently of first page")
	}
}

func TestEffectiveMemoryDeliveryOutsidePageAssessmentAndBoundedFallback(t *testing.T) {
	f := newEffectiveFixture(t)
	ctx := systemTestContext()
	// Keep a rationale on page one while later code evidence changes.
	f.digest.Fragments[0].Claims[0], f.digest.Fragments[0].Claims[1] = f.digest.Fragments[0].Claims[1], f.digest.Fragments[0].Claims[0]
	f.digest.PreviousMemoryHash = f.memory
	if _, err := f.svc.PutMemoryDigestCAS(ctx, f.repo, f.digest); err != nil {
		t.Fatal(err)
	}
	req := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: f.root, CodeCommit: effectiveOID(99)}, Limit: 1}
	before, err := f.svc.QueryEffectiveMemory(ctx, f.repo, req)
	if err != nil {
		t.Fatal(err)
	}
	f.reader.deltas[effectiveOID(99)] = domain.GitCommitDelta{Commit: effectiveOID(99), Parent: effectiveOID(3), Parents: []string{effectiveOID(3)}, Complete: true}
	scans, err := NewGitScans(f.svc, f.reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scans.ObservePush(ctx, "https://github.com/example/effective-memory", "refs/heads/main", "", effectiveOID(99), false, "new-evidence"); err != nil {
		t.Fatal(err)
	}
	if err := scans.Process(ctx, 100); err != nil {
		t.Fatal(err)
	}
	after, err := f.svc.QueryEffectiveMemory(ctx, f.repo, req)
	if err != nil {
		t.Fatal(err)
	}
	if after.DeliveryStateHash == before.DeliveryStateHash || !reflect.DeepEqual(before.Items, after.Items) {
		t.Fatal("outside-page status change not bound")
	}
	all := req
	all.Limit = 50
	resolved, err := f.svc.QueryEffectiveMemory(ctx, f.repo, all)
	if err != nil || itemByText(t, resolved, "A feature").State != "applied" {
		t.Fatal("fixture did not resolve status", err)
	}

	items, err := effectiveMemoryItems(f.digest, "")
	if err != nil {
		t.Fatal(err)
	}
	history, err := f.svc.ListHistory(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := f.svc.newCodeEvidence(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	evidence.reads = maxCodeEvidenceReads
	resolver, err := newMemoryIntegration(f.svc, evidence, f.repo, history)
	if err != nil {
		t.Fatal(err)
	}
	page := domain.EffectiveMemoryPage{Selection: req.Selection, Total: len(items)}
	proof, err := effectiveMemoryDeliveryHash(ctx, resolver, page, items)
	if err != nil || proof != "" || !evidence.limited {
		t.Fatal("budget-degraded projection received proof", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := effectiveMemoryDeliveryHash(canceled, resolver, page, items); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type deliveryEvidenceReadSpy struct {
	*store.FSStore
	reads int
}

func (s *deliveryEvidenceReadSpy) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, id string) (domain.GitCommitTree, error) {
	s.reads++
	return s.FSStore.GetGitCommitTree(ctx, repo, origin, id)
}

func TestEffectiveMemoryDeliveryManyClaimsCacheCost(t *testing.T) {
	f := newEffectiveFixture(t)
	ctx := systemTestContext()
	digest := domain.MemoryDigest{SnapshotID: f.root, PreviousMemoryHash: f.memory, ClaimsVersion: 1}
	for fragment := 0; fragment < 8; fragment++ {
		part := domain.MemoryFragment{SourceSnapshot: f.a}
		for claim := 0; claim < 256; claim++ {
			part.Claims = append(part.Claims, domain.MemoryClaim{Kind: "code", Text: fmt.Sprintf("claim %d/%d", fragment, claim), Code: &domain.MemoryCodeScope{Commit: effectiveOID(2), Paths: []string{"a", "b"}}})
		}
		digest.Fragments = append(digest.Fragments, part)
	}
	if _, err := f.svc.PutMemoryDigestCAS(ctx, f.repo, digest); err != nil {
		t.Fatal(err)
	}
	spy := &deliveryEvidenceReadSpy{FSStore: f.st}
	f.svc.meta = spy
	req := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: f.root, CodeCommit: effectiveOID(3)}, Limit: 1}
	start := time.Now()
	first, err := f.svc.QueryEffectiveMemory(ctx, f.repo, req)
	cold, coldReads := time.Since(start), spy.reads
	if err != nil || first.Total != 2048 || first.DeliveryStateHash == "" {
		t.Fatal("many-claims fixture", first.Total, err)
	}
	req.Cursor = first.NextCursor
	spy.reads = 0
	start = time.Now()
	next, err := f.svc.QueryEffectiveMemory(ctx, f.repo, req)
	warm := time.Since(start)
	if err != nil || next.DeliveryStateHash != first.DeliveryStateHash {
		t.Fatal("proof not reused across pages", err)
	}
	if spy.reads == 0 || coldReads != 2*spy.reads {
		t.Fatalf("full proof repeated or page skipped: cold reads=%d continuation=%d", coldReads, spy.reads)
	}
	t.Logf("2048 code claims, limit=1: cold=%s continuation=%s commit reads=%d/%d; complete assessment cached by full state", cold, warm, coldReads, spy.reads)
	if _, ok := f.svc.deliveryCache.get(first.StateHash); !ok {
		t.Fatal("missing generation cache")
	}
	if err := f.st.AdvanceEvidenceRevision(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	req.Cursor = ""
	spy.reads = 0
	changed, err := f.svc.QueryEffectiveMemory(ctx, f.repo, req)
	if err != nil || changed.DeliveryStateHash != first.DeliveryStateHash || spy.reads != coldReads {
		t.Fatal("cache survived different evidence generation", err)
	}
}

func TestEffectiveMemoryDeliveryCacheBoundsAndAbsence(t *testing.T) {
	var cache effectiveMemoryDeliveryCache
	cache.put(hh("absent"), "")
	if proof, ok := cache.get(hh("absent")); !ok || proof != "" {
		t.Fatal("bounded absence not cacheable")
	}
	for i := 0; i < 65; i++ {
		cache.put(hh(fmt.Sprint(i)), hh("proof"))
	}
	if len(cache.entries) != 64 {
		t.Fatal("unbounded cache")
	}
	if _, ok := cache.get(hh("absent")); ok {
		t.Fatal("old generation not evicted")
	}
}

func TestEffectiveMemoryDeliveryCacheBypassedInsideWrite(t *testing.T) {
	f := newEffectiveFixture(t)
	ctx := systemTestContext()
	spy := &deliveryEvidenceReadSpy{FSStore: f.st}
	f.svc.meta = spy
	req := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: f.root, CodeCommit: effectiveOID(3)}, Limit: 1}
	first, err := f.svc.QueryEffectiveMemory(ctx, f.repo, req)
	if err != nil {
		t.Fatal(err)
	}
	coldReads := spy.reads
	inside := context.WithValue(ctx, afterCommitKey{}, &afterCommitActions{})
	spy.reads = 0
	got, err := f.svc.QueryEffectiveMemory(inside, f.repo, req)
	if err != nil || got.DeliveryStateHash != first.DeliveryStateHash || spy.reads != coldReads {
		t.Fatal("nested write reused committed cache", err)
	}
	f.svc.deliveryCache.entries = nil
	if _, err := f.svc.QueryEffectiveMemory(inside, f.repo, req); err != nil {
		t.Fatal(err)
	}
	if len(f.svc.deliveryCache.entries) != 0 {
		t.Fatal("nested write published uncommitted proof")
	}
	if _, err := f.svc.QueryEffectiveMemory(ctx, f.repo, req); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.svc.deliveryCache.get(first.StateHash); !ok {
		t.Fatal("committed read did not repopulate cache")
	}
}

// A transactional adapter which has no positive transaction-mode capability.
type unknownDeliveryTransactionStore struct{ *store.FSStore }

func (s *unknownDeliveryTransactionStore) WithinRepository(ctx context.Context, _ domain.ContentHash, fn func(context.Context) error) error {
	return fn(ctx)
}
func (s *unknownDeliveryTransactionStore) WithinReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

func TestEffectiveMemoryDeliveryCacheUnknownTransactionMode(t *testing.T) {
	f := newEffectiveFixture(t)
	first := f.query(t, 3)
	f.svc.deliveryCache.entries = []effectiveMemoryDeliveryCacheEntry{{first.StateHash, hh("must not reuse")}}
	f.svc.meta = &unknownDeliveryTransactionStore{f.st}
	if got := f.query(t, 3); got.DeliveryStateHash != first.DeliveryStateHash {
		t.Fatal("unknown transaction mode reused cache")
	}
	f.svc.deliveryCache.entries = nil
	if got := f.query(t, 3); got.DeliveryStateHash != first.DeliveryStateHash || len(f.svc.deliveryCache.entries) != 0 {
		t.Fatal("unknown transaction mode published cache")
	}
}
