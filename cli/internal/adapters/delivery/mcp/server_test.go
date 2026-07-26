package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// --- fakes ---

type fakeGit struct{}

func (fakeGit) CurrentRepo(context.Context, string) (domain.Repo, error) {
	return domain.Repo{ID: "repo-1", DefaultBranch: "main"}, nil
}
func (fakeGit) CurrentBranch(context.Context, string) (string, error) { return "main", nil }

type fakeStore struct {
	snaps map[domain.ContentHash]domain.Snapshot
	docs  map[domain.ContentHash]domain.SessionDoc
	mems  map[domain.ContentHash]domain.MemoryDigest
	refs  map[string]domain.Ref
}

func (f fakeStore) ListSnapshots(_ context.Context, _, branch string) ([]domain.Snapshot, error) {
	var out []domain.Snapshot
	for _, s := range f.snaps {
		if branch == "" || s.Branch == branch {
			out = append(out, s)
		}
	}
	return out, nil
}
func (f fakeStore) GetSnapshot(_ context.Context, id domain.ContentHash) (domain.Snapshot, error) {
	if s, ok := f.snaps[id]; ok {
		return s, nil
	}
	return domain.Snapshot{}, domain.ErrNotFound
}
func (f fakeStore) GetDoc(_ context.Context, h domain.ContentHash) (domain.SessionDoc, error) {
	if d, ok := f.docs[h]; ok {
		return d, nil
	}
	return domain.SessionDoc{}, domain.ErrNotFound
}
func (f fakeStore) GetMemory(_ context.Context, h domain.ContentHash) (domain.MemoryDigest, error) {
	if m, ok := f.mems[h]; ok {
		return m, nil
	}
	return domain.MemoryDigest{}, domain.ErrNotFound
}
func (f fakeStore) GetRef(_ context.Context, _ string, kind domain.RefKind, name string) (domain.Ref, error) {
	if r, ok := f.refs[string(kind)+"/"+name]; ok {
		return r, nil
	}
	return domain.Ref{}, domain.ErrNotFound
}

func h(c string) domain.ContentHash { return domain.ContentHash("sha256:" + strings.Repeat(c, 64)) }

func testServer() *Server {
	snap := domain.Snapshot{
		ID: h("a"), RepoID: "repo-1", Branch: "main", DocHash: h("a"), MemoryHash: h("m"),
		Message: "feat: auth refactoring", Author: domain.TeamIdentity{Name: "alice"}, CreatedAt: time.Now(),
	}
	st := fakeStore{
		snaps: map[domain.ContentHash]domain.Snapshot{snap.ID: snap},
		docs: map[domain.ContentHash]domain.SessionDoc{h("a"): {Hash: h("a"), CIR: domain.CIRDocument{Events: []domain.Event{
			{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "Fix authentication"}}},
			{Kind: domain.EventMessage, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: "Fixed"}}},
		}}}},
		mems: map[domain.ContentHash]domain.MemoryDigest{h("m"): {SnapshotID: h("a"), Summary: "Authentication refactoring session summary", KeyFacts: []string{"JWT expiration 30 minutes"}}},
		refs: map[string]domain.Ref{"branch/main": {Kind: domain.RefBranch, Name: "main", Target: h("a")}},
	}
	return NewServer(fakeGit{}, st, nil)
}

// TestMCPProtocolRoundTrip fixes initialize → tools/list → tools/call round trip.
func TestMCPProtocolRoundTrip(t *testing.T) {
	srv := testServer()
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"context_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"memory_load","arguments":{"ref":"main"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"context_fetch","arguments":{"ref":"main"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"context_search","arguments":{"query":"auth"}}}`,
	}, "\n")
	var out bytes.Buffer
	srv.SetIO(strings.NewReader(in), &out)
	if err := srv.Run(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 6 { // notification is unresponsive — ids 1~6
		t.Fatalf("Response count %d, want 6: %s", len(lines), out.String())
	}
	// tools/list has only 4 read-only items.
	var toolsResp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &toolsResp); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range toolsResp.Result.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"context_list", "context_fetch", "memory_load", "context_search"} {
		if !names[want] {
			t.Fatalf("tool %s missing: %v", want, names)
		}
	}
	if len(names) != 4 {
		t.Fatalf("must be 4 read-only (write tool exposure forbidden): %v", names)
	}
	// commits are visible in context_list result.
	if !strings.Contains(lines[2], "auth refactoring") || !strings.Contains(lines[2], "◆mem") {
		t.Fatalf("context_list result abnormal: %s", lines[2])
	}
	// memory_load summary of digest.
	if !strings.Contains(lines[3], "Authentication refactoring session summary") || !strings.Contains(lines[3], "JWT") {
		t.Fatalf("memory_load result abnormal: %s", lines[3])
	}
	// context_fetch with meta + conversation tail.
	if !strings.Contains(lines[4], "Fix authentication") || !strings.Contains(lines[4], "Memory Summary") {
		t.Fatalf("context_fetch result abnormal: %s", lines[4])
	}
	// search is remote nil → isError content (not protocol error).
	if !strings.Contains(lines[5], `"isError":true`) || !strings.Contains(lines[5], "disconnected") {
		t.Fatalf("search disconnected notice abnormal: %s", lines[5])
	}
}
