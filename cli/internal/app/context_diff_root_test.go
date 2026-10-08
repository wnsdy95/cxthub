package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type rootContextDiffReader struct {
	*workingReadFixture
	reads []domain.DocumentRef
}

func (r *rootContextDiffReader) GetDocReference(ctx context.Context, ref domain.DocumentRef) (domain.SessionDoc, error) {
	r.reads = append(r.reads, ref)
	d, err := r.GetDoc(ctx, ref.Hash)
	if err != nil {
		return d, err
	}
	if d.DocumentRef() != ref {
		return d, domain.ErrHashMismatch
	}
	return d, domain.VerifySessionDocIdentity(ctx, d)
}
func contextRootDoc(t *testing.T, f *workingReadFixture, n int) domain.SessionDoc {
	t.Helper()
	d := workingDoc(t, "session", n)
	d.CIR.Envelope.CIRVersion = domain.CIRVersionV2
	m, _, err := domain.ConversationManifestForCIR(d.CIR)
	if err != nil {
		t.Fatal(err)
	}
	d.Hash, err = domain.ConversationManifestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	d.Identity = domain.DocumentIdentityRootV1
	f.docs[d.Hash] = d
	f.snapshots[d.Hash] = domain.Snapshot{ID: d.Hash, DocHash: d.Hash, DocIdentity: d.Identity, RepoID: f.repo, Provider: domain.ProviderCodex, SessionID: "session"}
	return d
}
func TestRootContextDiffCarriesStagedSelectedAndPendingReferences(t *testing.T) {
	for _, kind := range []string{"staged", "selected", "finalized"} {
		t.Run(kind, func(t *testing.T) {
			s, f := newContextDiffFixture(t)
			r := &rootContextDiffReader{workingReadFixture: f}
			s.docs = r
			before := contextRootDoc(t, f, 2)
			after := contextRootDoc(t, f, 3)
			entry := f.entry(t, 2)
			entry.DocHash = before.Hash
			entry.DocIdentity = before.Identity
			if kind == "staged" {
				f.index.Entries = []domain.StagedSession{entry}
				f.index = f.index.WithRevision()
			} else {
				f.position.Snapshot = before.Hash
				f.history.Position = before.Hash
				f.history.Snapshots = []domain.Snapshot{f.snapshots[before.Hash]}
				if kind == "finalized" {
					index := f.index
					index.Entries = []domain.StagedSession{entry}
					index = index.WithRevision()
					f.commits = []domain.StagingCommit{{Version: 3, ID: "root", Index: index, Position: f.position, LocalFinalized: true}}
				}
			}
			f.pending = []domain.Pending{{RepoID: f.repo, Provider: domain.ProviderCodex, SessionID: "session", Target: after.Hash}}
			out, err := s.Diff(context.Background(), inbound.ContextDiffInput{})
			if err != nil || len(out.Changes) != 1 {
				t.Fatal(out, err)
			}
			c := out.Changes[0]
			if c.Before != before.Hash || c.After != after.Hash || c.BeforeIdentity != before.Identity || c.AfterIdentity != after.Identity || c.State != "extended" || c.Added.Start != 2 || c.Added.End != 3 {
				t.Fatal(c)
			}
			for _, ref := range r.reads {
				if ref.Identity != domain.DocumentIdentityRootV1 {
					t.Fatal("identity stripped before read", ref)
				}
			}
			if kind == "staged" {
				out, err = s.Diff(context.Background(), inbound.ContextDiffInput{Staged: true})
				if err != nil || out.Changes[0].AfterIdentity != before.Identity {
					t.Fatal(out, err)
				}
			}
		})
	}
}
func TestRootContextDiffMissingWrongAndStrippedMetadataFailClosed(t *testing.T) {
	for _, kind := range []string{"missing", "stripped", "wrong-id", "wrong-repo", "wrong-session", "corrupt-body"} {
		t.Run(kind, func(t *testing.T) {
			s, f := newContextDiffFixture(t)
			r := &rootContextDiffReader{workingReadFixture: f}
			s.docs = r
			doc := contextRootDoc(t, f, 2)
			f.pending = []domain.Pending{{RepoID: f.repo, Provider: domain.ProviderCodex, SessionID: "session", Target: doc.Hash}}
			snap := f.snapshots[doc.Hash]
			switch kind {
			case "missing":
				delete(f.snapshots, doc.Hash)
			case "stripped":
				snap.DocIdentity = ""
				f.snapshots[doc.Hash] = snap
			case "wrong-id":
				snap.ID = domain.HashContent([]byte("wrong"))
				f.snapshots[doc.Hash] = snap
			case "wrong-repo":
				snap.RepoID = string(domain.HashContent([]byte("wrong")))
				f.snapshots[doc.Hash] = snap
			case "wrong-session":
				snap.SessionID = "other"
				f.snapshots[doc.Hash] = snap
			case "corrupt-body":
				doc.CIR.Events[0].Role = "assistant"
				f.docs[doc.Hash] = doc
			}
			out, err := s.Diff(context.Background(), inbound.ContextDiffInput{})
			if kind == "missing" {
				if err != nil || len(out.Changes) != 1 || out.Changes[0].State != "unavailable" || out.Changes[0].CountsKnown || len(r.reads) != 0 {
					t.Fatal(out, err, r.reads)
				}
				return
			}
			if !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatal("invalid identity accepted", out, err)
			}
		})
	}
}
