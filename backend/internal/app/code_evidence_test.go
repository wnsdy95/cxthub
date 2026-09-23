package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"testing"
)

func TestCodeEvidenceCacheIsLazyScopedAndBounded(t *testing.T) {
	f := newEffectiveFixture(t)
	ctx := systemTestContext()
	e, err := f.svc.newCodeEvidence(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.relation(ctx, effectiveOID(2), effectiveOID(3)); err != nil || got != "ancestor" || e.reads != 1 {
		t.Fatal("direct parent scanned the whole history", got, e.reads, err)
	}
	if got, err := e.relation(ctx, effectiveOID(1), effectiveOID(3)); err != nil || got != "ancestor" || e.reads != 2 {
		t.Fatal("ancestor walk did not resume", got, e.reads, err)
	}
	in := domain.CodeSelection{CodeCommit: effectiveOID(3), SourceCommit: effectiveOID(2), Paths: []string{"a", "b"}}
	if _, err = e.query(ctx, in, domain.RepositoryRevision{}); err != nil {
		t.Fatal(err)
	}
	reads := e.reads
	for i := 0; i < 100; i++ {
		if _, err = e.query(ctx, in, domain.RepositoryRevision{}); err != nil {
			t.Fatal(err)
		}
	}
	if e.reads != reads {
		t.Fatal("repeated claims repeated database reads", reads, e.reads)
	}
	if _, err = e.GetGitCommitTree(ctx, hh("other repo"), e.origin, in.CodeCommit); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("cache leaked another repository", err)
	}
	if _, err = e.GetGitTreeNode(ctx, e.repo, "other origin", "missing"); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("origin fence", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = e.GetGitCommitTree(canceled, e.repo, e.origin, in.CodeCommit); !errors.Is(err, context.Canceled) {
		t.Fatal("cached result ignored cancellation", err)
	}
	e.reads = maxCodeEvidenceReads
	if got, err := e.relation(ctx, effectiveOID(1), effectiveOID(99)); err != nil || got != "unknown" || !e.limited {
		t.Fatal("read bound asserted absence", got, err)
	}
}
