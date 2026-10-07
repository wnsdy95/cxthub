package backendclient

import (
	"context"
	"encoding/json"
	"io"
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
	next                                           http.RoundTripper
	repoPath                                       string
	plans, permissions, objects, broad, unexpected int
}

func (p *branchPullWireProbe) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == p.repoPath+"/pull/branch-plan":
		p.plans++
	case r.URL.Path == p.repoPath+"/pull/objects":
		if branchPullPermissionRequest(r) {
			p.permissions++
		} else {
			p.objects++
		}
	case r.URL.Path == p.repoPath+"/pull/chunks":
		p.objects++
	case strings.HasSuffix(r.URL.Path, "/manifest"), strings.HasSuffix(r.URL.Path, "/history"):
		p.broad++
	default:
		p.unexpected++
	}
	return p.next.RoundTrip(r)
}

func branchPullPermissionRequest(r *http.Request) bool {
	// BackendClient provides GetBody for its JSON requests. Inspect a copy so
	// the transport receives the original body, headers, and content length.
	// Uninspectable or malformed requests remain transfers, never permissions.
	if r.Method != http.MethodPost || r.GetBody == nil {
		return false
	}
	body, err := r.GetBody()
	if err != nil {
		return false
	}
	defer body.Close()
	var request pullReq
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		return false
	}
	return len(request.SnapshotWants)+len(request.DocWants)+len(request.DocManifestWants)+len(request.ChunkWants)+len(request.ChunkFormatsSupported) == 0 &&
		reflect.DeepEqual(request.CIRVersionsSupported, domain.SupportedCIRVersions())
}

func TestBranchPullWireProbeClassifiesPermissionWithoutChangingWire(t *testing.T) {
	repoPath := "/api/v1/repos/" + string(domain.HashContent([]byte("wire probe repository")))
	id := domain.HashContent([]byte("wire probe object"))
	wire := func(request pullReq) string {
		request.CIRVersionsSupported = domain.SupportedCIRVersions()
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	permission := wire(pullReq{})
	permissionBody := " \n" + permission + "\n"
	for _, test := range []struct {
		name      string
		method    string
		path      string
		body      string
		noGetBody bool
		want      [5]int // plans, permissions, transfers, broad, unexpected
	}{
		{name: "permission", body: permissionBody, want: [5]int{0, 1, 0, 0, 0}},
		{name: "snapshot", body: wire(pullReq{SnapshotWants: []domain.ContentHash{id}}), want: [5]int{0, 0, 1, 0, 0}},
		{name: "document", body: wire(pullReq{DocWants: []domain.ContentHash{id}}), want: [5]int{0, 0, 1, 0, 0}},
		{name: "document-manifest", body: wire(pullReq{DocManifestWants: []domain.ContentHash{id}}), want: [5]int{0, 0, 1, 0, 0}},
		{name: "chunk", body: wire(pullReq{ChunkWants: []domain.ContentHash{id}}), want: [5]int{0, 0, 1, 0, 0}},
		{name: "chunk-endpoint", path: repoPath + "/pull/chunks", body: permissionBody, want: [5]int{0, 0, 1, 0, 0}},
		{name: "mixed", body: wire(pullReq{SnapshotWants: []domain.ContentHash{id}, DocWants: []domain.ContentHash{id}}), want: [5]int{0, 0, 1, 0, 0}},
		{name: "unknown-field", body: strings.TrimSuffix(permission, "}") + `,"unexpected_wants":[]}`, want: [5]int{0, 0, 1, 0, 0}},
		{name: "chunk-formats", body: wire(pullReq{ChunkFormatsSupported: []string{"unexpected"}}), want: [5]int{0, 0, 1, 0, 0}},
		{name: "missing-protocol", body: `{}`, want: [5]int{0, 0, 1, 0, 0}},
		{name: "malformed", body: strings.TrimSuffix(permission, "}"), want: [5]int{0, 0, 1, 0, 0}},
		{name: "trailing-json", body: permissionBody + `{}`, want: [5]int{0, 0, 1, 0, 0}},
		{name: "wrong-method", method: http.MethodGet, body: permissionBody, want: [5]int{0, 0, 1, 0, 0}},
		{name: "no-body-copy", noGetBody: true, body: permissionBody, want: [5]int{0, 0, 1, 0, 0}},
		{name: "other-repository", path: "/api/v1/repos/other/pull/objects", body: permissionBody, want: [5]int{0, 0, 0, 0, 1}},
		{name: "branch-plan", path: repoPath + "/pull/branch-plan", body: `{"version":1,"branch":"feature"}`, want: [5]int{1, 0, 0, 0, 0}},
		{name: "manifest", method: http.MethodGet, path: repoPath + "/manifest", body: `{}`, want: [5]int{0, 0, 0, 1, 0}},
		{name: "history", method: http.MethodGet, path: repoPath + "/history", body: `{}`, want: [5]int{0, 0, 0, 1, 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			method, path := test.method, test.path
			if method == "" {
				method = http.MethodPost
			}
			if path == "" {
				path = repoPath + "/pull/objects"
			}
			req, err := http.NewRequest(method, "http://probe.invalid"+path, strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer synthetic-probe-token")
			if test.noGetBody {
				req.GetBody = nil
			}
			originalBody, originalHeaders, originalLength := req.Body, req.Header.Clone(), req.ContentLength
			forwarded := 0
			probe := &branchPullWireProbe{repoPath: repoPath, next: catalogRoundTrip(func(sent *http.Request) (*http.Response, error) {
				forwarded++
				if sent != req || sent.Body != originalBody || sent.ContentLength != originalLength || !reflect.DeepEqual(sent.Header, originalHeaders) {
					t.Error("probe changed the forwarded request")
				}
				raw, err := io.ReadAll(sent.Body)
				_ = sent.Body.Close()
				if err != nil || string(raw) != test.body {
					t.Errorf("probe changed body bytes: got %q, want %q, error=%v", raw, test.body, err)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header), Request: sent}, nil
			})}
			response, err := probe.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			got := [5]int{probe.plans, probe.permissions, probe.objects, probe.broad, probe.unexpected}
			if forwarded != 1 || got != test.want {
				t.Fatalf("forwarded=%d plans/permissions/transfers/broad/unexpected=%v, want %v", forwarded, got, test.want)
			}
		})
	}
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
	p := &branchPullWireProbe{next: http.DefaultTransport, repoPath: strings.TrimRight(u.Path, "/") + c.reposPath(repo)}
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
	var branchIDs []string
	for _, branch := range []string{"branch-pull-wire", "branch-pull-unrelated"} {
		cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull}, Events: []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: branch}}}}}
		raw, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		id := domain.HashContent(raw)
		ids = append(ids, id)
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: branch, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC()}
		if err := c.Push(ctx, repo, []domain.Snapshot{snap}, []domain.SessionDoc{{Hash: id, CIR: cir}}, nil, false, false); err != nil {
			t.Fatal(err)
		}
		// Fixture branches are new identities, including in protocol-1 repos.
		// Upload dependencies first, then use the normal durable birth command.
		branchID := strings.ReplaceAll(domain.NewSessionID(), "-", "")
		branchIDs = append(branchIDs, branchID)
		birth := domain.HistoryEvent{ID: branchID, RepoID: repo, Kind: "birth", Branch: branch, BranchID: branchID, Source: id, Target: id, CreatedAt: snap.CreatedAt}
		if err := c.PushHistoryEvent(ctx, birth); err != nil {
			t.Fatal(err)
		}
	}
	p.plans, p.permissions, p.objects, p.broad, p.unexpected = 0, 0, 0, 0, 0
	req := domain.BranchPullRequest{Version: 1, Branch: "branch-pull-wire"}
	receiver := stagedPullDocs{}
	plan, snaps, err := c.PullSelectedBranchTo(ctx, repo, req, nil, nil, receiver)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SelectedRef.BranchID != branchIDs[0] || plan.SelectedRef.Name != req.Branch || plan.SelectedRef.Target != ids[0] || !reflect.DeepEqual(plan.SnapshotIndex, ids[:1]) || len(snaps) != 1 || len(receiver) != 1 || p.plans != 1 || p.permissions != 0 || p.objects == 0 || p.broad != 0 || p.unexpected != 0 {
		t.Fatalf("cold plan: nodes=%d metadata=%d docs=%d plans=%d permissions=%d transfers=%d broad=%d unexpected=%d", len(plan.SnapshotIndex), len(snaps), len(receiver), p.plans, p.permissions, p.objects, p.broad, p.unexpected)
	}
	p.plans, p.permissions, p.objects, p.broad, p.unexpected = 0, 0, 0, 0, 0
	warm := stagedPullDocs{}
	plan, snaps, err = c.PullSelectedBranchTo(ctx, repo, req, plan.SnapshotStates, ids[:1], warm)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SelectedRef.BranchID != branchIDs[0] || plan.SelectedRef.Name != req.Branch || plan.SelectedRef.Target != ids[0] || !reflect.DeepEqual(plan.SnapshotIndex, ids[:1]) || len(snaps) != 0 || len(warm) != 0 || p.plans != 1 || p.permissions != 1 || p.objects != 0 || p.broad != 0 || p.unexpected != 0 {
		t.Fatalf("warm plan: nodes=%d metadata=%d docs=%d plans=%d permissions=%d transfers=%d broad=%d unexpected=%d", len(plan.SnapshotIndex), len(snaps), len(warm), p.plans, p.permissions, p.objects, p.broad, p.unexpected)
	}
	t.Log("actual PG API/CLI: selected node only; unchanged warm plan checks pull permission once and transfers no objects; no full catalog/history queries")
}
