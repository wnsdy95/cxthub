package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

const deltaEpoch = "e66213bd-003e-47d1-b965-e690036bfb08"

type deltaFixture struct {
	t       *testing.T
	repo    string
	base    string
	client  *BackendClient
	store   *storage.FileStore
	entries []domain.CatalogEntry
	calls   []domain.CatalogRequest
	capRaw  string
	hook    func(*http.Request, domain.CatalogRequest, int) (*http.Response, error)
}

func newDeltaFixture(t *testing.T, count int) *deltaFixture {
	t.Helper()
	f := &deltaFixture{t: t, repo: domain.HashContent([]byte("catalog delta repo")), base: "https://delta.invalid/api/v1", store: storage.NewFileStore(t.TempDir())}
	f.capRaw = fmt.Sprintf(`{"id":%q,"default_branch":"main","catalog_version":1}`, f.repo)
	value, _ := json.Marshal(map[string]int{"context_protocol": 1})
	f.entries = append(f.entries, domain.CatalogEntry{Kind: "protocol", Key: f.repo, Value: value})
	for i := range count {
		id := domain.HashContent([]byte(fmt.Sprint(i)))
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: f.repo, Message: fmt.Sprint(i)}
		value, _ := json.Marshal(snap)
		f.entries = append(f.entries, domain.CatalogEntry{Kind: "snapshot", Key: id, Value: value})
	}
	sort.Slice(f.entries, func(i, j int) bool {
		if f.entries[i].Kind != f.entries[j].Kind {
			return f.entries[i].Kind < f.entries[j].Kind
		}
		return f.entries[i].Key < f.entries[j].Key
	})
	f.restart()
	return f
}
func deltaReply(r *http.Request, status int, value any) *http.Response {
	raw, _ := json.Marshal(value)
	if s, ok := value.(string); ok {
		raw = []byte(s)
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw)), Request: r}
}
func (f *deltaFixture) restart() {
	f.client = NewBackendClient(func() string { return f.base }, func() string { return "synthetic-token" }, domain.TeamIdentity{})
	f.client.SetCatalogCacheStore(f.store)
	f.client.httpc.Transport = catalogRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			f.t.Fatal("missing fresh token")
		}
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/"+f.repo {
			return deltaReply(r, 200, f.capRaw), nil
		}
		var request domain.CatalogRequest
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pull/catalog") {
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				f.t.Fatal(err)
			}
			f.calls = append(f.calls, request)
			if f.hook != nil {
				if response, err := f.hook(r, request, len(f.calls)); response != nil || err != nil {
					return response, err
				}
			}
			start := 0
			if request.Cursor != "" {
				if _, err := fmt.Sscanf(request.Cursor, "page-%d", &start); err != nil {
					f.t.Fatal(err)
				}
			}
			page := domain.CatalogPage{Version: 1, RepoID: f.repo, Epoch: deltaEpoch, Mode: "baseline", Entries: []domain.CatalogEntry{}}
			if request.After != nil {
				page.Mode = "delta"
				page.Through = request.After.Sequence
			} else {
				end := min(start+request.Limit, len(f.entries))
				page.Entries = append(page.Entries, f.entries[start:end]...)
				if end < len(f.entries) {
					page.NextCursor = fmt.Sprintf("page-%d", end)
				}
			}
			if page.NextCursor == "" {
				page.Checkpoint = &domain.CatalogCheckpoint{Version: 1, RepoID: f.repo, Epoch: page.Epoch, Sequence: page.Through}
			}
			return deltaReply(r, 200, page), nil
		}
		if f.hook != nil {
			if response, err := f.hook(r, request, -1); response != nil || err != nil {
				return response, err
			}
		}
		f.t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		return nil, nil
	})
}
func (f *deltaFixture) read() ([]domain.Snapshot, error) {
	return f.client.ReadSnapshotCatalog(context.Background(), f.repo)
}
func (f *deltaFixture) cacheRevision() domain.ContentHash {
	f.t.Helper()
	state, err := f.store.ReadCatalogCache(context.Background(), f.repo, f.client.SyncRemoteIdentity())
	if err != nil {
		f.t.Fatal(err)
	}
	return state.Revision
}
func TestCatalogDeltaResumesTransactionAndCatchesUp(t *testing.T) {
	f := newDeltaFixture(t, 300)
	f.hook = func(r *http.Request, q domain.CatalogRequest, n int) (*http.Response, error) {
		if n == 2 {
			return nil, context.DeadlineExceeded
		}
		return nil, nil
	}
	if got, err := f.read(); !errors.Is(err, context.DeadlineExceeded) || got != nil {
		t.Fatalf("failed acquisition=%d %v", len(got), err)
	}
	state, err := f.store.ReadCatalogCache(context.Background(), f.repo, f.client.SyncRemoteIdentity())
	if err != nil || state.Checkpoint != nil || len(state.Entries) != 0 || len(state.Pending) != 1 {
		t.Fatalf("partial page acknowledged: %+v %v", state, err)
	}
	f.hook = nil
	f.calls = nil
	f.restart()
	got, err := f.read()
	if err != nil || len(got) != 300 || len(f.calls) != 2 || f.calls[0].Cursor != "page-256" || f.calls[1].After == nil {
		t.Fatalf("resume count=%d calls=%+v err=%v", len(got), f.calls, err)
	}
	f.calls = nil
	warm, err := f.read()
	if err != nil || !reflect.DeepEqual(got, warm) || len(f.calls) != 1 || f.calls[0].After == nil {
		t.Fatalf("warm=%d calls=%v err=%v", len(warm), f.calls, err)
	}
	obs, err := f.store.ReadRemoteObservation(context.Background(), f.repo, f.client.SyncRemoteIdentity())
	if err != nil || obs.Revision != "" || len(obs.Snapshots)+len(obs.History)+len(obs.Refs) != 0 {
		t.Fatalf("catalog applied verified state: %+v %v", obs, err)
	}
	snaps, err := f.store.ListSnapshots(context.Background(), f.repo, "")
	if err != nil || len(snaps) != 0 {
		t.Fatalf("catalog applied snapshot records: %d %v", len(snaps), err)
	}
}
func TestCatalogDeltaWarmMemoryAndDeletion(t *testing.T) {
	f := newDeltaFixture(t, 2)
	got, err := f.read()
	if err != nil {
		t.Fatal(err)
	}
	changed := got[0]
	changed.MemoryHash = domain.HashContent([]byte("new memory version"))
	raw, _ := json.Marshal(changed)
	f.hook = func(r *http.Request, q domain.CatalogRequest, n int) (*http.Response, error) {
		if q.After == nil || q.After.Sequence != 0 {
			t.Fatalf("not incremental %+v", q)
		}
		entries := []domain.CatalogEntry{{Sequence: 1, Kind: "snapshot", Key: changed.ID, Value: raw}, {Sequence: 1, Kind: "snapshot", Key: got[1].ID, Deleted: true}}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
		return deltaReply(r, 200, domain.CatalogPage{Version: 1, RepoID: f.repo, Epoch: deltaEpoch, Through: 1, Mode: "delta", Entries: entries, Checkpoint: &domain.CatalogCheckpoint{Version: 1, RepoID: f.repo, Epoch: deltaEpoch, Sequence: 1}}), nil
	}
	next, err := f.read()
	if err != nil || len(next) != 1 || next[0].MemoryHash != changed.MemoryHash {
		t.Fatalf("change=%+v err=%v", next, err)
	}
}
func TestCatalogDeltaResetPreservesCompleteUntilReplacement(t *testing.T) {
	f := newDeltaFixture(t, 1)
	if _, err := f.read(); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	f.hook = func(r *http.Request, q domain.CatalogRequest, n int) (*http.Response, error) {
		if n == 1 {
			return deltaReply(r, 409, `{"error":{"code":"reset_required"}}`), nil
		}
		if q.After != nil || q.Cursor != "" {
			t.Fatalf("reset did not start baseline %+v", q)
		}
		return nil, context.DeadlineExceeded
	}
	if _, err := f.read(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	state, err := f.store.ReadCatalogCache(context.Background(), f.repo, f.client.SyncRemoteIdentity())
	if err != nil || state.Checkpoint == nil || len(state.Entries) != 2 || len(state.Pending) != 0 {
		t.Fatalf("lost completed image: %+v %v", state, err)
	}
	f.calls = nil
	f.hook = func(r *http.Request, q domain.CatalogRequest, n int) (*http.Response, error) {
		return deltaReply(r, 409, `{"error":{"code":"reset_required"}}`), nil
	}
	if _, err := f.read(); err == nil || len(f.calls) != 2 {
		t.Fatalf("unbounded resets: %d %v", len(f.calls), err)
	}
}
func TestCatalogDeltaFailuresNeverDowngradeOrAdvance(t *testing.T) {
	for _, status := range []int{401, 403, 404, 409, 500, 501} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newDeltaFixture(t, 1)
			if _, err := f.read(); err != nil {
				t.Fatal(err)
			}
			before := f.cacheRevision()
			f.hook = func(r *http.Request, q domain.CatalogRequest, n int) (*http.Response, error) {
				return deltaReply(r, status, `{"error":{"code":"not_catalog_reset"}}`), nil
			}
			if got, err := f.read(); err == nil || got != nil {
				t.Fatalf("error became success: %+v %v", got, err)
			}
			if after := f.cacheRevision(); after != before {
				t.Fatal("failed page advanced progress")
			}
		})
	}
}
func TestCatalogCapabilityRejectsMalformedAndUnknownVersion(t *testing.T) {
	for _, value := range []string{`null`, `"1"`, `2`, `-1`, `1,"catalog_version":0`, `1,"CATALOG_VERSION":0`} {
		t.Run(value, func(t *testing.T) {
			f := newDeltaFixture(t, 1)
			f.capRaw = fmt.Sprintf(`{"id":%q,"default_branch":"main","catalog_version":%s}`, f.repo, value)
			if got, err := f.read(); err == nil || got != nil || len(f.calls) != 0 {
				t.Fatalf("bad capability accepted %s %v", value, err)
			}
		})
	}
}
func TestCatalogCapabilityLegacyUsesExistingPermissionGate(t *testing.T) {
	f := newMetadataFixture(t, 1)
	f.client.SetCatalogCacheStore(f.store)
	previous := f.client.httpc.Transport
	f.client.httpc.Transport = catalogRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, "/manifest") {
			return deltaReply(r, 200, map[string]any{"id": f.man.RepoID, "default_branch": ""}), nil
		}
		return previous.RoundTrip(r)
	})
	if got, err := f.read(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("legacy %d %v", len(got), err)
	}
	f.pullStatus = 403
	if got, err := f.read(context.Background()); err == nil || got != nil {
		t.Fatal("legacy fallback bypassed fresh pull permission")
	}
}
func TestCatalogDeltaRejectsEndpointChange(t *testing.T) {
	f := newDeltaFixture(t, 1)
	f.hook = func(r *http.Request, q domain.CatalogRequest, n int) (*http.Response, error) {
		f.base = "https://other.invalid/api/v1"
		return nil, nil
	}
	if _, err := f.read(); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatal(err)
	}
	old, err := f.store.ReadCatalogCache(context.Background(), f.repo, "https://delta.invalid/api/v1")
	if err != nil || old.Revision != "" {
		t.Fatalf("mixed endpoint persisted: %+v %v", old, err)
	}
}
func TestCatalogDeltaCannotSkipDocumentVerification(t *testing.T) {
	f := newDeltaFixture(t, 1)
	f.hook = func(r *http.Request, q domain.CatalogRequest, n int) (*http.Response, error) {
		if n == -1 {
			return deltaReply(r, 200, pullResp{Docs: []domain.SessionDoc{{Hash: domain.HashContent([]byte("invalid document"))}}}), nil
		}
		return nil, nil
	}
	if snaps, docs, _, err := f.client.Pull(context.Background(), f.repo, nil, nil); err == nil || snaps != nil || docs != nil {
		t.Fatalf("invalid body trusted: %+v %+v %v", snaps, docs, err)
	}
	state, err := f.store.ReadCatalogCache(context.Background(), f.repo, f.client.SyncRemoteIdentity())
	if err != nil || state.Checkpoint == nil {
		t.Fatalf("metadata not independently retained: %v", err)
	}
}

func TestCatalogDeltaSplitTransactionDoesNotAcknowledgeEarly(t *testing.T) {
	f := newDeltaFixture(t, 2)
	before, err := f.read()
	if err != nil {
		t.Fatal(err)
	}
	changes := make([]domain.CatalogEntry, 0, 2)
	for _, snap := range before {
		snap.Message = "updated"
		raw, _ := json.Marshal(snap)
		changes = append(changes, domain.CatalogEntry{Sequence: 1, Kind: "snapshot", Key: snap.ID, Value: raw})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Key < changes[j].Key })
	interrupted := false
	f.hook = func(r *http.Request, q domain.CatalogRequest, n int) (*http.Response, error) {
		p := domain.CatalogPage{Version: 1, RepoID: f.repo, Epoch: deltaEpoch, Through: 1, Mode: "delta", Entries: []domain.CatalogEntry{}}
		switch {
		case q.Cursor == "transaction-tail":
			if !interrupted {
				interrupted = true
				return nil, context.DeadlineExceeded
			}
			p.Entries = changes[1:]
			p.Checkpoint = &domain.CatalogCheckpoint{Version: 1, RepoID: f.repo, Epoch: deltaEpoch, Sequence: 1}
		case q.After != nil && q.After.Sequence == 0:
			p.Entries = changes[:1]
			p.NextCursor = "transaction-tail"
		case q.After != nil && q.After.Sequence == 1:
			p.Checkpoint = q.After
		default:
			t.Fatalf("unexpected checkpoint %+v", q)
		}
		return deltaReply(r, 200, p), nil
	}
	if _, err := f.read(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	state, err := f.store.ReadCatalogCache(context.Background(), f.repo, f.client.SyncRemoteIdentity())
	if err != nil || state.Checkpoint.Sequence != 0 || len(state.Pending) != 1 {
		t.Fatalf("early ack %+v %v", state, err)
	}
	_, stored, err := domain.CatalogManifest(f.repo, state.Entries)
	if err != nil || !reflect.DeepEqual(stored, before) {
		t.Fatal("incomplete transaction changed complete image")
	}
	f.restart()
	got, err := f.read()
	if err != nil || len(got) != 2 || got[0].Message != "updated" || got[1].Message != "updated" {
		t.Fatalf("resumed %+v %v", got, err)
	}
}

func TestCatalogDeltaInvalidFinalImagePreservesCheckpoint(t *testing.T) {
	f := newDeltaFixture(t, 1)
	if _, err := f.read(); err != nil {
		t.Fatal(err)
	}
	before := f.cacheRevision()
	f.hook = func(r *http.Request, q domain.CatalogRequest, n int) (*http.Response, error) {
		p := domain.CatalogPage{Version: 1, RepoID: f.repo, Epoch: deltaEpoch, Through: 1, Mode: "delta", Entries: []domain.CatalogEntry{{Sequence: 1, Kind: "protocol", Key: f.repo, Deleted: true}}, Checkpoint: &domain.CatalogCheckpoint{Version: 1, RepoID: f.repo, Epoch: deltaEpoch, Sequence: 1}}
		return deltaReply(r, 200, p), nil
	}
	if _, err := f.read(); err == nil {
		t.Fatal("deleted required protocol accepted")
	}
	if f.cacheRevision() != before {
		t.Fatal("invalid complete image committed")
	}
}

func TestCatalogDeltaFullPullStillReturnsUnverifiedMetadata(t *testing.T) {
	f := newDeltaFixture(t, 2)
	known, err := f.read()
	if err != nil {
		t.Fatal(err)
	}
	ids := []domain.ContentHash{}
	states := map[domain.ContentHash]domain.ContentHash{}
	for _, snap := range known {
		ids = append(ids, snap.ID)
		states[snap.ID], _ = domain.SnapshotStateHash(snap)
	}
	snaps, docs, _, err := f.client.Pull(context.Background(), f.repo, nil, ids)
	if err != nil || len(docs) != 0 || !reflect.DeepEqual(snaps, known) {
		t.Fatalf("metadata skipped before app verification: %+v %v", snaps, err)
	}
	snaps, docs, _, err = f.client.Pull(context.Background(), f.repo, states, ids)
	if err != nil || len(snaps) != 0 || len(docs) != 0 {
		t.Fatalf("unchanged full pull %+v %v", snaps, err)
	}
}

type catalogWhitespaceReader struct{}

func (catalogWhitespaceReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func TestCatalogDeltaReducesOversizedPageWithoutAcknowledgingIt(t *testing.T) {
	for _, continuation := range []bool{false, true} {
		t.Run(fmt.Sprint(continuation), func(t *testing.T) {
			count := 3
			if continuation {
				count = 260
			}
			f := newDeltaFixture(t, count)
			oversized := false
			retried := false
			f.hook = func(r *http.Request, q domain.CatalogRequest, n int) (*http.Response, error) {
				if continuation && q.Cursor == "" {
					return nil, nil
				}
				if !oversized {
					oversized = true
					state, err := f.store.ReadCatalogCache(context.Background(), f.repo, f.client.SyncRemoteIdentity())
					if err != nil || state.Checkpoint != nil {
						t.Fatalf("premature checkpoint %+v %v", state, err)
					}
					response := deltaReply(r, 200, domain.CatalogPage{Version: 1, RepoID: f.repo, Epoch: deltaEpoch, Mode: "baseline", Entries: f.entries[:1], NextCursor: "would-be-new-cursor"})
					// Whitespace is legal JSON framing; the response is too large before
					// decoding. The next attempt must use the original cursor, not this one.
					response.Body = io.NopCloser(io.MultiReader(response.Body, io.LimitReader(catalogWhitespaceReader{}, catalogResponseBytes)))
					return response, nil
				}
				if q.Limit != catalogPageLimit/2 || q.Cursor == "would-be-new-cursor" || (continuation && q.Cursor != "page-256") {
					t.Fatalf("unsafe retry %+v", q)
				}
				retried = true
				return nil, nil
			}
			got, err := f.read()
			if err != nil || len(got) != count || !oversized || !retried {
				t.Fatalf("bounded retry count=%d err=%v", len(got), err)
			}
		})
	}
}

func TestCatalogDeltaOversizeRetriesHaveALowerBound(t *testing.T) {
	f := newDeltaFixture(t, 1)
	f.hook = func(r *http.Request, q domain.CatalogRequest, n int) (*http.Response, error) {
		return nil, errBoundedResponse
	}
	if _, err := f.read(); err == nil || !strings.Contains(err.Error(), "catalog entry exceeds") {
		t.Fatalf("single-entry size error=%v", err)
	}
	wants := []int{256, 128, 64, 32, 16, 8, 4, 2, 1}
	if len(f.calls) != len(wants) {
		t.Fatalf("unbounded retry %v", f.calls)
	}
	for i, q := range f.calls {
		if q.Limit != wants[i] || q.After != nil || q.Cursor != "" {
			t.Fatalf("retry advanced %+v", q)
		}
	}
	state, err := f.store.ReadCatalogCache(context.Background(), f.repo, f.client.SyncRemoteIdentity())
	if err != nil || state.Revision != "" {
		t.Fatalf("oversize committed progress %+v %v", state, err)
	}
}
