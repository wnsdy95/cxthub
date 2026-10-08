package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type explicitAgentDocuments struct {
	*agentDocFixture
	refs  []domain.DocumentRef
	after func(*domain.SessionDoc)
}

func (r *explicitAgentDocuments) GetDocReference(ctx context.Context, ref domain.DocumentRef) (domain.SessionDoc, error) {
	r.refs = append(r.refs, ref)
	doc, ok := r.docs[ref.Hash]
	if !ok {
		return doc, domain.ErrNotFound
	}
	if doc.DocumentRef() != ref {
		return doc, domain.ErrHashMismatch
	}
	if err := domain.VerifySessionDocIdentity(ctx, doc); err != nil {
		return doc, err
	}
	if r.after != nil {
		r.after(&doc)
	}
	return doc, nil
}
func agentRootDocument(t testing.TB, legacy domain.SessionDoc) domain.SessionDoc {
	t.Helper()
	manifest, _, err := domain.ConversationManifestForCIR(legacy.CIR)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Hash, legacy.Identity = hash, domain.DocumentIdentityRootV1
	return legacy
}
func TestReadDocumentReferencePreservesSelectedIdentity(t *testing.T) {
	ctx := context.Background()
	legacy := agentDocument(t, "reference", agentMessage("user", "synthetic", 0))
	root := agentRootDocument(t, legacy)
	old := &agentDocFixture{docs: map[domain.ContentHash]domain.SessionDoc{legacy.Hash: legacy, root.Hash: root}}
	if _, err := readDocumentReference(ctx, old, root.DocumentRef()); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) || old.calls != 0 {
		t.Fatalf("root reached hash-only reader: %v calls=%d", err, old.calls)
	}
	if _, err := readDocumentReference(ctx, old, legacy.DocumentRef()); err != nil {
		t.Fatal(err)
	}
	reader := &explicitAgentDocuments{agentDocFixture: old}
	for _, doc := range []domain.SessionDoc{legacy, root} {
		got, err := readDocumentReference(ctx, reader, doc.DocumentRef())
		if err != nil || got.DocumentRef() != doc.DocumentRef() {
			t.Fatalf("read: %v %v", got.DocumentRef(), err)
		}
	}
	reader.after = func(d *domain.SessionDoc) { d.Identity = domain.DocumentIdentityLegacy }
	if _, err := readDocumentReference(ctx, reader, root.DocumentRef()); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("reader relabeled root: %v", err)
	}
	reader.after = nil
	calls := len(reader.refs)
	bad := root.DocumentRef()
	bad.Identity = "unknown"
	if _, err := readDocumentReference(ctx, reader, bad); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) || len(reader.refs) != calls {
		t.Fatalf("unknown identity read: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := readDocumentReference(canceled, reader, root.DocumentRef()); !errors.Is(err, context.Canceled) || len(reader.refs) != calls {
		t.Fatalf("canceled read: %v", err)
	}
	damaged := root
	damaged.CIR.Events = []domain.Event{agentMessage("user", "changed", 0)}
	reader.docs[root.Hash] = damaged
	if _, err := readDocumentReference(ctx, reader, root.DocumentRef()); err == nil {
		t.Fatal("current corruption hidden by previous read")
	}
}
func TestAgentContextRootFullReaderPreservesProvenance(t *testing.T) {
	service, in, history, _, _ := agentServiceFixture(t)
	doc := agentRootDocument(t, agentDocument(t, "root-session", agentMessage("user", "root question", 0), agentMessage("assistant", "root answer", 1)))
	reader := &explicitAgentDocuments{agentDocFixture: &agentDocFixture{docs: map[domain.ContentHash]domain.SessionDoc{doc.Hash: doc}}}
	service.documents = reader
	history.view.Snapshots = []domain.Snapshot{{ID: doc.Hash, DocHash: doc.Hash, DocIdentity: doc.Identity, RepoID: in.RepoID}}
	history.view.Position, in.SnapshotID = doc.Hash, doc.Hash
	in.ArtifactOnly = true
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 200000, Source: "explicit"}
	p, err := service.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Content.History) != 1 || p.Content.History[0].Source.DocIdentity != doc.Identity || reader.calls != 0 || len(reader.refs) != 1 {
		t.Fatalf("lost source identity or hash-only read: %+v refs=%v", p.Content.History, reader.refs)
	}
	for _, source := range p.Content.Sources {
		if source.DocHash == doc.Hash && source.DocIdentity != doc.Identity {
			t.Fatal("package source omitted identity")
		}
	}
	if err := p.ValidateIdentity(); err != nil {
		t.Fatal(err)
	}
}

type explicitAgentPages struct{ *agentPageFixture }

func (r explicitAgentPages) ReadAgentHistoryPage(ctx context.Context, hash domain.ContentHash, req domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error) {
	doc, ok := r.docs[hash]
	if !ok {
		return domain.AgentHistoryPage{}, domain.ErrNotFound
	}
	if doc.Identity != req.DocIdentity {
		return domain.AgentHistoryPage{}, domain.ErrHashMismatch
	}
	if err := domain.VerifySessionDocIdentity(ctx, doc); err != nil {
		return domain.AgentHistoryPage{}, err
	}
	if req.CoveredBy != "" {
		covered, ok := r.docs[req.CoveredBy]
		if !ok || covered.Identity != req.CoveredByIdentity {
			return domain.AgentHistoryPage{}, domain.ErrHashMismatch
		}
		if err := domain.VerifySessionDocIdentity(ctx, covered); err != nil {
			return domain.AgentHistoryPage{}, err
		}
	}
	page, err := r.agentPageFixture.ReadAgentHistoryPage(ctx, hash, req)
	page.DocIdentity = doc.Identity
	return page, err
}
func TestAgentContextPagedMixedIdentityCoverage(t *testing.T) {
	for _, newestRoot := range []bool{true, false} {
		t.Run(map[bool]string{true: "root-covers-legacy", false: "legacy-covers-root"}[newestRoot], func(t *testing.T) {
			service, in, history, _, _ := agentServiceFixture(t)
			prior := agentDocument(t, "same-session", agentMessage("user", "first", 0), agentMessage("assistant", "done", 1))
			latest := agentDocument(t, "same-session", append(append([]domain.Event{}, prior.CIR.Events...), agentMessage("user", "second", 2), agentMessage("assistant", "done again", 3))...)
			if newestRoot {
				latest = agentRootDocument(t, latest)
			} else {
				prior = agentRootDocument(t, prior)
			}
			pages := explicitAgentPages{&agentPageFixture{docs: map[domain.ContentHash]domain.SessionDoc{prior.Hash: prior, latest.Hash: latest}}}
			service.documents = pages
			history.view.Snapshots = nil
			for _, doc := range []domain.SessionDoc{latest, prior} {
				history.view.Snapshots = append(history.view.Snapshots, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, DocIdentity: doc.Identity, RepoID: in.RepoID})
			}
			history.view.Position, in.SnapshotID = latest.Hash, latest.Hash
			in.ArtifactOnly = true
			in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 200000, Source: "explicit"}
			p, err := service.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Content.History) != 2 {
				t.Fatalf("duplicate older prefix: %d", len(p.Content.History))
			}
			proof := false
			for _, req := range pages.requests {
				if req.CoveredBy == latest.Hash {
					proof = true
					if req.CoveredByIdentity != latest.Identity || req.DocIdentity != prior.Identity {
						t.Fatal("coverage identity stripped")
					}
				}
			}
			if !proof {
				t.Fatal("no coverage proof request")
			}
			for _, segment := range p.Content.History {
				if segment.Source.DocIdentity != latest.Identity {
					t.Fatal("source identity stripped")
				}
			}
		})
	}
}
