package backendclient

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type memoryReuseWireProbe struct {
	next        http.RoundTripper
	full, reuse int
	reuseBytes  int64
}

func (p *memoryReuseWireProbe) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut {
		if strings.Contains(r.URL.Path, "/memory-reuses/") {
			p.reuse++
			p.reuseBytes += r.ContentLength
		} else if strings.Contains(r.URL.Path, "memory-attachments/") {
			p.full++
		}
	}
	return p.next.RoundTrip(r)
}

// scripts/e2e-sync.sh runs this compiled test against its isolated server and
// synthetic account. Ordinary unit runs never connect to a development server.
func TestMemoryReuseLiveProtocol(t *testing.T) {
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
	p := &memoryReuseWireProbe{next: http.DefaultTransport}
	c.httpc = &http.Client{Transport: p, Timeout: 30 * time.Second}
	ctx := context.Background()
	var ids []domain.ContentHash
	for _, text := range []string{"memory reuse source", "memory reuse target"} {
		cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull}, Events: []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}}}
		raw, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		id := domain.HashContent(raw)
		ids = append(ids, id)
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "memory-reuse-fixture", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC()}
		if err := c.Push(ctx, repo, []domain.Snapshot{snap}, []domain.SessionDoc{{Hash: id, CIR: cir}}, nil, false, false); err != nil {
			t.Fatal(err)
		}
	}
	d := reuseTestDigest()
	d.SnapshotID = ids[0]
	d.Fragments[0].SourceSnapshot = ids[0]
	if err := c.PushMemory(ctx, repo, d); err != nil {
		t.Fatal(err)
	}
	d.SnapshotID = ids[1]
	if err := c.PushMemory(ctx, repo, d); err != nil {
		t.Fatal(err)
	}
	want, _ := domain.MemoryDigestHash(d)
	got, err := c.PullMemory(ctx, repo, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := domain.MemoryDigestHash(got)
	if hash != want || p.full != 1 || p.reuse != 1 || p.reuseBytes > 1024 {
		t.Fatalf("wire contract: hash match=%v full=%d reuse=%d reuse bytes=%d", hash == want, p.full, p.reuse, p.reuseBytes)
	}
	t.Logf("server preserved exact typed attachment; reuse request=%d bytes", p.reuseBytes)
}
