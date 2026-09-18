package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestMemoryPublicationUsesStagedChunkedDocument(t *testing.T) {
	ctx := context.Background()
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, WorkspaceID: "memory-chunks"}); err != nil {
		t.Fatal(err)
	}
	cir := pendingGCCIR(domain.ProviderCodex, strings.Repeat("preserved archive ", 90000))
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.HashContent(raw)
	plan, ok := domain.PlanDocChunks(raw)
	if !ok {
		t.Fatal("fixture not chunked")
	}
	for _, hash := range plan.Order {
		if _, err := svc.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: []inbound.ChunkObject{{Hash: hash, Data: plan.Bodies[hash]}}}); err != nil {
			t.Fatal(err)
		}
	}
	in := inbound.MemoryPublication{
		Objects: inbound.CommitInput{RepoID: repo, Snapshots: []domain.Snapshot{{ID: id, DocHash: id, RepoID: repo}}, ChunkedDocs: []inbound.ChunkedDoc{{Hash: id, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}}},
		Memory:  domain.MemoryDigest{SnapshotID: id, Summary: "memory bound to staged document"},
	}
	if _, err := svc.PublishMemoryArchive(ctx, in); err != nil {
		t.Fatal(err)
	}
	doc, err := svc.GetDoc(ctx, repo, id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := domain.CanonicalBytes(doc.CIR)
	if err != nil || string(got) != string(raw) {
		t.Fatal("staged archive differs")
	}
}

func TestMemoryPublicationRestoresCollectedCaptureAndPreservesArchive(t *testing.T) {
	ctx := context.Background()
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, WorkspaceID: "memory-test"}); err != nil {
		t.Fatal(err)
	}
	old := putPendingGCCapture(t, st, repo, pendingGCCIR(domain.ProviderCodex, "first"))
	doc, err := st.GetDoc(ctx, repo, old.DocHash)
	if err != nil {
		t.Fatal(err)
	}
	next := putPendingGCCapture(t, st, repo, pendingGCCIR(domain.ProviderCodex, "first", "next"))
	p := domain.Pending{Provider: old.Provider, SessionID: old.SessionID, Target: old.ID}
	if err := svc.PutPending(ctx, repo, p.SessionID, p); err != nil {
		t.Fatal(err)
	}
	neg, err := svc.Negotiate(ctx, inbound.PushNegotiateInput{RepoID: repo, SnapshotHaves: []domain.ContentHash{old.ID}, DocHaves: []domain.ContentHash{old.DocHash}})
	if err != nil || len(neg.SnapshotWants)+len(neg.DocWants) != 0 {
		t.Fatalf("not advertised as present: %+v %v", neg, err)
	}
	p.Target = next.ID
	if err := svc.PutPending(ctx, repo, p.SessionID, p); err != nil {
		t.Fatal(err)
	}
	digest := domain.MemoryDigest{SnapshotID: old.ID, Summary: "separately authored memory"}
	if _, err := svc.PutMemoryDigestCAS(ctx, repo, digest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-request race not reproduced: %v", err)
	}
	in := inbound.MemoryPublication{Objects: inbound.CommitInput{RepoID: repo, Snapshots: []domain.Snapshot{old}, Docs: []domain.SessionDoc{doc}}, Memory: digest}
	hash, err := svc.PublishMemoryArchive(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := svc.PublishMemoryArchive(ctx, in); err != nil || again != hash {
		t.Fatalf("retry: %s %v", again, err)
	}
	// Even another observer moving the pending pointer back and forward cannot
	// collect the restored archive once its independent memory is attached.
	p.Target = old.ID
	if err := svc.PutPending(ctx, repo, p.SessionID, p); err != nil {
		t.Fatal(err)
	}
	p.Target = next.ID
	if err := svc.PutPending(ctx, repo, p.SessionID, p); err != nil {
		t.Fatal(err)
	}
	assertPendingGCCapture(t, st, old)
	got, err := svc.GetMemoryDigest(ctx, repo, old.ID)
	if err != nil || got.Summary != digest.Summary {
		t.Fatalf("memory lost: %+v %v", got, err)
	}
	refs, err := st.ListRefs(ctx, repo)
	if err != nil || len(refs) != 0 {
		t.Fatalf("archive restoration moved refs: %+v %v", refs, err)
	}
	advanced := digest
	advanced.PreviousMemoryHash, advanced.Summary = hash, "concurrent update"
	latest, err := svc.PutMemoryDigestCAS(ctx, repo, advanced)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishMemoryArchive(ctx, in); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale root overwrote current memory: %v", err)
	}
	snap, err := st.GetSnapshot(ctx, repo, old.ID)
	if err != nil || snap.MemoryHash != latest {
		t.Fatalf("pointer changed: %+v %v", snap, err)
	}
}

func TestMemoryPublicationRejectsInvalidArchiveBeforePersistence(t *testing.T) {
	for _, reason := range []string{"different memory source", "causal child", "forged graft", "invalid memory", "invalid doc", "foreign repo", "extra document"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			svc, st := newFsckSvc(t)
			repo := hh(t.Name())
			if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, WorkspaceID: "memory-test"}); err != nil {
				t.Fatal(err)
			}
			cir := pendingGCCIR(domain.ProviderClaude, "archive")
			raw, err := domain.CanonicalBytes(cir)
			if err != nil {
				t.Fatal(err)
			}
			id := domain.HashContent(raw)
			in := inbound.MemoryPublication{Objects: inbound.CommitInput{RepoID: repo, Snapshots: []domain.Snapshot{{ID: id, DocHash: id, RepoID: repo}}, Docs: []domain.SessionDoc{{Hash: id, CIR: cir}}}, Memory: domain.MemoryDigest{SnapshotID: id, Summary: "root"}}
			switch reason {
			case "different memory source":
				in.Memory.SnapshotID = hh("other")
			case "causal child":
				in.Memory.PreviousMemoryHash = hh("prior")
			case "forged graft":
				in.Objects.Snapshots[0].GraftParents = []domain.ContentHash{hh("graft")}
			case "invalid memory":
				in.Memory.ClaimsVersion = 99
			case "invalid doc":
				in.Objects.Docs[0].CIR.Envelope.SessionOriginID = "changed"
			case "foreign repo":
				in.Objects.Snapshots[0].RepoID = hh("foreign")
			case "extra document":
				in.Objects.Docs = append(in.Objects.Docs, in.Objects.Docs[0])
			}
			if _, err := svc.PublishMemoryArchive(ctx, in); err == nil {
				t.Fatal("invalid publication accepted")
			}
			if _, err := st.GetSnapshot(ctx, repo, id); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("snapshot persisted: %v", err)
			}
			if _, err := st.GetDoc(ctx, repo, id); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("document persisted: %v", err)
			}
		})
	}
}
