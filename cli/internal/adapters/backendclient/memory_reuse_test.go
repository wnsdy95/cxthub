package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type reuseTestServer struct {
	mu                                  sync.Mutex
	objects                             map[domain.ContentHash]domain.MemoryDigest
	full, reused, fullBytes, reuseBytes int
	status                              int
	badAck                              bool
}

func (s *reuseTestServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects == nil {
		s.objects = map[domain.ContentHash]domain.MemoryDigest{}
	}
	raw, _ := io.ReadAll(r.Body)
	var d domain.MemoryDigest
	if strings.Contains(r.URL.Path, "/memory-reuses/") {
		s.reused++
		s.reuseBytes += len(raw)
		if s.status != 0 {
			w.WriteHeader(s.status)
			return
		}
		var in memoryReuseRequest
		if err := json.Unmarshal(raw, &in); err != nil {
			w.WriteHeader(422)
			return
		}
		var ok bool
		d, ok = s.objects[in.BaseHash]
		if !ok {
			http.NotFound(w, r)
			return
		}
		d.SnapshotID = domain.ContentHash(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		d.PreviousMemoryHash, d.Provider = in.PreviousMemoryHash, in.Provider
		h, _ := domain.MemoryDigestHash(d)
		if in.Version != 1 || h != in.MemoryHash {
			w.WriteHeader(422)
			return
		}
	} else {
		s.full++
		s.fullBytes += len(raw)
		if err := json.Unmarshal(raw, &d); err != nil {
			w.WriteHeader(422)
			return
		}
	}
	hash, _ := domain.MemoryDigestHash(d)
	s.objects[hash] = d
	if s.badAck && strings.Contains(r.URL.Path, "/memory-reuses/") {
		hash = domain.HashContent([]byte("incorrect"))
	}
	_ = json.NewEncoder(w).Encode(map[string]domain.ContentHash{"memory_hash": hash})
}

func reuseTestDigest() domain.MemoryDigest {
	id := domain.HashContent([]byte("source"))
	return domain.MemoryDigest{SnapshotID: id, Provider: domain.ProviderCodex, Summary: strings.Repeat("synthetic project memory. ", 20000), KeyFacts: []string{"keep this"}, OpenTasks: []string{}, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: id, Claims: []domain.MemoryClaim{{Kind: "rationale", Text: "original provenance"}}}}}
}

func TestMemoryReuseReducesWireBytesPreservingIdentity(t *testing.T) {
	s := &reuseTestServer{}
	ts := httptest.NewServer(http.HandlerFunc(s.serve))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	repo := string(domain.HashContent([]byte(t.Name())))
	d := reuseTestDigest()
	for i := 0; i < 12; i++ {
		d.SnapshotID = domain.HashContent([]byte(fmt.Sprintf("capture-%d", i)))
		if i%2 == 0 {
			d.Provider = domain.ProviderClaude
		} else {
			d.Provider = domain.ProviderCodex
		}
		if err := c.PushMemory(context.Background(), repo, d); err != nil {
			t.Fatal(err)
		}
		want, _ := domain.MemoryDigestHash(d)
		got, _ := domain.MemoryDigestHash(s.objects[want])
		if got != want {
			t.Fatal("digest/provenance changed")
		}
	}
	if s.full != 1 || s.reused != 11 || s.reuseBytes >= 11*1024 {
		t.Fatalf("full=%d reused=%d reuse bytes=%d", s.full, s.reused, s.reuseBytes)
	}
	baseline := s.fullBytes * 12
	t.Logf("12 synthetic attachments: full baseline=%d bytes; transferred=%d bytes (%d full + %d reuse)", baseline, s.fullBytes+s.reuseBytes, s.fullBytes, s.reuseBytes)
}

func TestMemoryReuseFallbackAndFailure(t *testing.T) {
	for _, status := range []int{404, 405, 401, 403, 409, 422, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s := &reuseTestServer{status: status}
			ts := httptest.NewServer(http.HandlerFunc(s.serve))
			defer ts.Close()
			c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
			repo := string(domain.HashContent([]byte(t.Name())))
			d := reuseTestDigest()
			if err := c.PushMemory(context.Background(), repo, d); err != nil {
				t.Fatal(err)
			}
			d.SnapshotID = domain.HashContent([]byte("next"))
			err := c.PushMemory(context.Background(), repo, d)
			fallback := status == 404 || status == 405
			if (err == nil) != fallback {
				t.Fatalf("status=%d error=%v", status, err)
			}
			if s.reused != 1 || (s.full == 2) != fallback {
				t.Fatalf("full=%d reuse=%d", s.full, s.reused)
			}
			if fallback {
				d.SnapshotID = domain.HashContent([]byte("third"))
				if err := c.PushMemory(context.Background(), repo, d); err != nil {
					t.Fatal(err)
				}
				if s.reused != 1 || s.full != 3 {
					t.Fatal("repeated unsupported probe")
				}
			}
		})
	}
	s := &reuseTestServer{badAck: true}
	ts := httptest.NewServer(http.HandlerFunc(s.serve))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	repo := string(domain.HashContent([]byte(t.Name())))
	d := reuseTestDigest()
	if err := c.PushMemory(context.Background(), repo, d); err != nil {
		t.Fatal(err)
	}
	d.SnapshotID = domain.HashContent([]byte("next"))
	if err := c.PushMemory(context.Background(), repo, d); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("wrong acknowledgement accepted: %v", err)
	}
	if s.full != 1 {
		t.Fatal("integrity failure fell back")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.PushMemory(ctx, repo, d); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled push: %v", err)
	}
}

func TestMemoryReuseScopesAndChangedBody(t *testing.T) {
	s := &reuseTestServer{}
	ts := httptest.NewServer(http.HandlerFunc(s.serve))
	defer ts.Close()
	other := httptest.NewServer(http.HandlerFunc(s.serve))
	defer other.Close()
	base, token := ts.URL, "credential-a"
	c := NewBackendClient(func() string { return base }, func() string { return token }, domain.TeamIdentity{})
	repo := string(domain.HashContent([]byte("repo-a")))
	d := reuseTestDigest()
	push := func() {
		t.Helper()
		if err := c.PushMemory(context.Background(), repo, d); err != nil {
			t.Fatal(err)
		}
	}
	push()
	token = "credential-b"
	push()
	repo = string(domain.HashContent([]byte("repo-b")))
	push()
	base = other.URL
	push()
	d.Fragments[0].Claims[0].Text = "different decision"
	push()
	d.GraftCoverage = &domain.MemoryGraftCoverage{ProjectionVersion: 1}
	push()
	d.KeyFacts = nil
	push()
	d.OpenTasks = nil
	push()
	if s.full != 8 || s.reused != 0 {
		t.Fatalf("cross-scope/body reuse: full=%d reuse=%d", s.full, s.reused)
	}
	// Small bodies use the original endpoint directly.
	d = domain.MemoryDigest{SnapshotID: d.SnapshotID, Summary: "small"}
	push()
	push()
	if s.full != 10 || s.reused != 0 {
		t.Fatal("small digest needlessly negotiated")
	}
}

func TestMemoryReuseConcurrentHintsAreBounded(t *testing.T) {
	s := &reuseTestServer{}
	ts := httptest.NewServer(http.HandlerFunc(s.serve))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	repo := string(domain.HashContent([]byte(t.Name())))
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := reuseTestDigest()
			d.Summary += fmt.Sprint(i)
			d.SnapshotID = domain.HashContent([]byte(fmt.Sprint(i)))
			if err := c.PushMemory(context.Background(), repo, d); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(c.memoryReuse.entries) > memoryReuseEntries {
		t.Fatal("unbounded hints")
	}
}
