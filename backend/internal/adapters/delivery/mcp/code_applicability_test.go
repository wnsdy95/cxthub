package mcp

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
	"testing"
)

type codeQuery struct {
	repo     domain.ContentHash
	received domain.CodeSelection
}

func (q *codeQuery) QueryCodeApplicability(_ context.Context, repo domain.ContentHash, in domain.CodeSelection) (domain.CodeApplicability, error) {
	if repo != q.repo {
		return domain.CodeApplicability{}, domain.ErrForbidden
	}
	if err := in.Validate(); err != nil {
		return domain.CodeApplicability{}, err
	}
	q.received = in
	return domain.CodeApplicability{Selection: in, Relation: "unknown", Paths: []domain.CodePathState{{Path: in.Paths[0], State: "unknown", Reason: "incomplete_ancestry"}}}, nil
}
func TestCodeApplicabilityMCPForwardsExplicitSelection(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	q := &codeQuery{repo: repo.ID}
	s := &Server{}
	s.SetCodeApplicability(q)
	a := toolArgs{CodeCommit: strings.Repeat("a", 40), SourceCommit: strings.Repeat("b", 40), SourceParent: strings.Repeat("c", 40), Paths: []string{"file.go"}}
	raw, e := s.codeApplicabilityPage(systemTestContext(), repo, a)
	if e != nil {
		t.Fatal(e)
	}
	var got domain.CodeApplicability
	if e = json.Unmarshal([]byte(raw), &got); e != nil {
		t.Fatal(e)
	}
	if got.Relation != "unknown" || got.Paths[0].State != "unknown" || q.received.CodeCommit != a.CodeCommit || q.received.SourceParent != a.SourceParent {
		t.Fatal("adapter reinterpreted server evidence", got)
	}
	if _, e = s.codeApplicabilityPage(systemTestContext(), domain.Repo{ID: pageHash(2)}, a); e == nil {
		t.Fatal("repository binding lost")
	}
	a.Paths = make([]string, 21)
	if _, e = s.codeApplicabilityPage(systemTestContext(), repo, a); e == nil {
		t.Fatal("unbounded path scope")
	}
}
