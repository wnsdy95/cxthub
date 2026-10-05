package backendclient

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type branchPullWireProbe struct {
	next                  http.RoundTripper
	plans, objects, broad int
}

func (p *branchPullWireProbe) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/pull/branch-plan"):
		p.plans++
	case strings.HasSuffix(r.URL.Path, "/pull/objects"):
		p.objects++
	case strings.HasSuffix(r.URL.Path, "/manifest"), strings.HasSuffix(r.URL.Path, "/history"):
		p.broad++
	}
	return p.next.RoundTrip(r)
}

// The existing sync E2E fixture supplies only its synthetic loopback account.
func TestBranchPullLiveProtocol(t *testing.T) {
	base := os.Getenv("CXT_MEMORY_REUSE_TEST_URL")
	if base == "" {
		t.Skip("isolated sync E2E server required")
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Scheme != "http" || u.Port() == "" {
		t.Fatal("isolated loopback URL required")
	}
	token, repo := os.Getenv("CXT_MEMORY_REUSE_TEST_TOKEN"), os.Getenv("CXT_MEMORY_REUSE_TEST_REPO")
	if token == "" || repo == "" {
		t.Fatal("synthetic fixture authority required")
	}
	c := NewBackendClient(func() string { return base }, func() string { return token }, domain.TeamIdentity{})
	p := &branchPullWireProbe{next: http.DefaultTransport}
	c.httpc = &http.Client{Transport: p, Timeout: 30 * time.Second}
	ctx := context.Background()
	caps, err := c.PullCapabilities(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CXT_BRANCH_PULL_TEST_REQUIRED") == "1" && caps.BranchPlanVersion != 1 {
		t.Fatal("PostgreSQL fixture did not advertise branch plan")
	}
	if caps.BranchPlanVersion == 0 {
		t.Skip("FS fixture uses existing full transfer")
	}
	if caps.BranchPlanVersion != 1 {
		t.Fatal("unknown branch plan version")
	}
	var ids []domain.ContentHash
	for _, branch := range []string{"branch-pull-wire", "branch-pull-unrelated"} {
		cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull}, Events: []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: branch}}}}}
		raw, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		id := domain.HashContent(raw)
		ids = append(ids, id)
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: branch, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC()}
		ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: branch, Target: id}
		if err := c.Push(ctx, repo, []domain.Snapshot{snap}, []domain.SessionDoc{{Hash: id, CIR: cir}}, []domain.Ref{ref}, false, false); err != nil {
			t.Fatal(err)
		}
	}
	p.plans, p.objects, p.broad = 0, 0, 0
	req := domain.BranchPullRequest{Version: 1, Branch: "branch-pull-wire"}
	receiver := stagedPullDocs{}
	plan, snaps, err := c.PullSelectedBranchTo(ctx, repo, req, nil, nil, receiver)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.SnapshotIndex, ids[:1]) || len(snaps) != 1 || len(receiver) != 1 || p.plans != 1 || p.objects == 0 || p.broad != 0 {
		t.Fatalf("cold plan: nodes=%d metadata=%d docs=%d plans=%d objects=%d broad=%d", len(plan.SnapshotIndex), len(snaps), len(receiver), p.plans, p.objects, p.broad)
	}
	p.plans, p.objects, p.broad = 0, 0, 0
	warm := stagedPullDocs{}
	plan, snaps, err = c.PullSelectedBranchTo(ctx, repo, req, plan.SnapshotStates, ids[:1], warm)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.SnapshotIndex, ids[:1]) || len(snaps) != 0 || len(warm) != 0 || p.plans != 1 || p.objects != 0 || p.broad != 0 {
		t.Fatalf("warm plan: nodes=%d metadata=%d docs=%d plans=%d objects=%d broad=%d", len(plan.SnapshotIndex), len(snaps), len(warm), p.plans, p.objects, p.broad)
	}
	t.Log("actual PG API/CLI: selected node only; unchanged warm plan transfers no objects; no full catalog/history queries")
}
