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
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type metadataFixture struct {
	t           *testing.T
	store       *storage.FileStore
	client      *BackendClient
	man         domain.Manifest
	snaps       map[domain.ContentHash]domain.Snapshot
	batches     []int
	reads       int
	status      int
	pullStatus  int
	permissions int
	base        string
	onBatch     func(int, *pullResp) error
	plan        *domain.BranchPullPlan
}

func newMetadataFixture(t *testing.T, count int) *metadataFixture {
	t.Helper()
	f := &metadataFixture{t: t, store: storage.NewFileStore(t.TempDir()), status: 200, pullStatus: 200, base: "https://catalog.invalid/api/v1", snaps: map[domain.ContentHash]domain.Snapshot{}}
	f.man = domain.Manifest{RepoID: domain.HashContent([]byte("metadata checkpoint repo")), SnapshotStates: map[domain.ContentHash]domain.ContentHash{}}
	for i := range count {
		id := domain.HashContent([]byte(fmt.Sprint(i)))
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: f.man.RepoID, Message: fmt.Sprint(i)}
		f.man.SnapshotIndex = append(f.man.SnapshotIndex, id)
		f.snaps[id] = snap
		f.man.SnapshotStates[id], _ = domain.SnapshotStateHash(snap)
	}
	f.client = NewBackendClient(func() string { return f.base }, func() string { return "synthetic-token" }, domain.TeamIdentity{})
	f.client.SetMetadataCheckpointStore(f.store)
	f.client.httpc.Transport = catalogRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Fatal("request missing fresh authorization")
		}
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		var value any
		status := f.status
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest") {
			f.reads++
			value = f.man
		} else if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pull/branch-plan") && f.plan != nil {
			value, status = f.plan, f.pullStatus
		} else if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pull/objects") {
			var in pullReq
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Fatal(err)
			}
			status = f.pullStatus
			if len(in.SnapshotWants) > 256 || len(in.DocWants)+len(in.DocManifestWants)+len(in.ChunkWants) != 0 {
				t.Fatalf("unexpected request: %+v", in)
			}
			if len(in.SnapshotWants) == 0 {
				f.permissions++
			} else {
				f.batches = append(f.batches, len(in.SnapshotWants))
			}
			response := pullResp{}
			for _, id := range in.SnapshotWants {
				response.Snapshots = append(response.Snapshots, f.snaps[id])
			}
			if f.onBatch != nil {
				if err := f.onBatch(len(f.batches), &response); err != nil {
					return nil, err
				}
			}
			value = response
		} else {
			t.Fatalf("unexpected route %s %s", r.Method, r.URL.Path)
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw)), Request: r}, nil
	})
	return f
}

func (f *metadataFixture) read(ctx context.Context) ([]domain.Snapshot, error) {
	return f.client.ReadSnapshotCatalog(ctx, f.man.RepoID)
}

func TestMetadataCheckpointFreshManifestAndChangedRecords(t *testing.T) {
	ctx := context.Background()
	f := newMetadataFixture(t, 259)
	first, err := f.read(ctx)
	if err != nil || len(first) != 259 || !reflect.DeepEqual(f.batches, []int{256, 3}) {
		t.Fatalf("cold count=%d batches=%v err=%v", len(first), f.batches, err)
	}
	f.batches = nil
	warm, err := f.read(ctx)
	if err != nil || !reflect.DeepEqual(first, warm) || len(f.batches) != 0 || f.reads != 2 {
		t.Fatalf("warm batches=%v reads=%d err=%v", f.batches, f.reads, err)
	}
	id := f.man.SnapshotIndex[5]
	changed := f.snaps[id]
	changed.MemoryHash = domain.HashContent([]byte("new independent memory"))
	f.snaps[id] = changed
	f.man.SnapshotStates[id], _ = domain.SnapshotStateHash(changed)
	got, err := f.read(ctx)
	if err != nil || !reflect.DeepEqual(f.batches, []int{1}) || got[5].MemoryHash != changed.MemoryHash {
		t.Fatalf("changed batches=%v err=%v", f.batches, err)
	}
	// A manifest removal must not resurrect a retained checkpoint candidate.
	f.man.SnapshotIndex = f.man.SnapshotIndex[1:]
	delete(f.man.SnapshotStates, first[0].ID)
	f.batches = nil
	got, err = f.read(ctx)
	if err != nil || len(got) != 258 || len(f.batches) != 0 || got[0].ID != first[1].ID {
		t.Fatalf("removed catalog count=%d batches=%v err=%v", len(got), f.batches, err)
	}
	obs, err := f.store.ReadRemoteObservation(ctx, f.man.RepoID, f.client.SyncRemoteIdentity())
	if err != nil || obs.Revision != "" || len(obs.Snapshots)+len(obs.Refs)+len(obs.History) != 0 {
		t.Fatalf("metadata acquisition published verified state: %+v %v", obs, err)
	}
	local, err := f.store.ListSnapshots(ctx, f.man.RepoID, "")
	if err != nil || len(local) != 0 {
		t.Fatalf("metadata acquisition adopted snapshots: %d %v", len(local), err)
	}
}

func TestMetadataCheckpointResumesOnlyCompletedBatches(t *testing.T) {
	for _, mode := range []string{"cancel", "changed-token", "omitted", "duplicate", "unsolicited-body"} {
		t.Run(mode, func(t *testing.T) {
			f := newMetadataFixture(t, 259)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.onBatch = func(n int, response *pullResp) error {
				if n != 2 {
					return nil
				}
				switch mode {
				case "cancel":
					cancel()
					return ctx.Err()
				case "changed-token":
					response.Snapshots[0].Message = "raced"
				case "omitted":
					response.Snapshots = response.Snapshots[1:]
				case "duplicate":
					response.Snapshots[1] = response.Snapshots[0]
				case "unsolicited-body":
					response.Docs = []domain.SessionDoc{{}}
				}
				return nil
			}
			got, err := f.read(ctx)
			if err == nil || got != nil || (mode == "cancel" && !errors.Is(err, context.Canceled)) {
				t.Fatalf("failed acquisition succeeded: %d %v", len(got), err)
			}
			checkpoint, err := f.store.ReadMetadataCheckpoint(context.Background(), f.man.RepoID, f.client.SyncRemoteIdentity())
			if err != nil || len(checkpoint.Snapshots) != 256 {
				t.Fatalf("partial checkpoint count=%d err=%v", len(checkpoint.Snapshots), err)
			}
			f.onBatch, f.batches = nil, nil
			got, err = f.read(context.Background())
			if err != nil || len(got) != 259 || !reflect.DeepEqual(f.batches, []int{3}) {
				t.Fatalf("resume count=%d batches=%v err=%v", len(got), f.batches, err)
			}
		})
	}
}

func TestMetadataCheckpointFreshAuthorizationAndEndpointIsolation(t *testing.T) {
	f := newMetadataFixture(t, 1)
	ctx := context.Background()
	if _, err := f.read(ctx); err != nil {
		t.Fatal(err)
	}
	f.batches = nil
	f.status = 403
	if got, err := f.read(ctx); err == nil || got != nil || len(f.batches) != 0 {
		t.Fatalf("cache bypassed authorization: %v %v", got, err)
	}
	f.status = 200
	for _, status := range []int{401, 403} {
		f.pullStatus = status
		if got, err := f.read(ctx); err == nil || got != nil || len(f.batches) != 0 {
			t.Fatalf("viewer manifest bypassed pull authorization: %v %v", got, err)
		}
	}
	if f.permissions != 2 {
		t.Fatalf("missing fresh pull permission checks: %d", f.permissions)
	}
	f.pullStatus = 200
	f.base = "https://another.invalid/api/v1"
	if _, err := f.read(ctx); err != nil || !reflect.DeepEqual(f.batches, []int{1}) {
		t.Fatalf("endpoint isolation batches=%v err=%v", f.batches, err)
	}
	f.batches = nil
	f.base = "https://changing.invalid/api/v1"
	f.onBatch = func(_ int, _ *pullResp) error {
		f.base = "https://changed.invalid/api/v1"
		return nil
	}
	if _, err := f.read(ctx); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("endpoint switched within acquisition: %v", err)
	}
	for _, remote := range []string{"https://changing.invalid/api/v1", "https://changed.invalid/api/v1"} {
		checkpoint, err := f.store.ReadMetadataCheckpoint(ctx, f.man.RepoID, remote)
		if err != nil || checkpoint.Revision != "" {
			t.Fatalf("mixed endpoint persisted checkpoint: %+v %v", checkpoint, err)
		}
	}
}

func TestMetadataCheckpointDoesNotOmitUnverifiedPullRecords(t *testing.T) {
	ctx := context.Background()
	f := newMetadataFixture(t, 2)
	if _, err := f.read(ctx); err != nil {
		t.Fatal(err)
	}
	f.batches = nil
	// The application requested both snapshots. Their cached metadata must be
	// returned so the application still checks each current local document.
	got, docs, _, err := f.client.Pull(ctx, f.man.RepoID, nil, f.man.SnapshotIndex)
	if err != nil || len(got) != 2 || len(docs) != 0 || len(f.batches) != 0 {
		t.Fatalf("pull count=%d docs=%d batches=%v err=%v", len(got), len(docs), f.batches, err)
	}
	// Old servers cannot authenticate a metadata checkpoint with state tokens.
	f.man.SnapshotStates = nil
	for range 2 {
		got, _, _, err = f.client.Pull(ctx, f.man.RepoID, nil, f.man.SnapshotIndex)
		if err != nil || len(got) != 2 {
			t.Fatalf("legacy pull count=%d err=%v", len(got), err)
		}
	}
	if !reflect.DeepEqual(f.batches, []int{2, 2}) {
		t.Fatalf("legacy reused unauthenticated checkpoint: %v", f.batches)
	}
}

func TestMetadataCheckpointRejectsUnsolicitedPermissionPayload(t *testing.T) {
	f := newMetadataFixture(t, 1)
	ctx := context.Background()
	if _, err := f.read(ctx); err != nil {
		t.Fatal(err)
	}
	f.onBatch = func(_ int, response *pullResp) error {
		response.Snapshots = []domain.Snapshot{f.snaps[f.man.SnapshotIndex[0]]}
		return nil
	}
	if got, err := f.read(ctx); !errors.Is(err, domain.ErrHashMismatch) || got != nil {
		t.Fatalf("unsolicited permission payload accepted: %v %v", got, err)
	}
}

func TestMetadataCheckpointContentionPreservesWinnerAndAcquisition(t *testing.T) {
	ctx := context.Background()
	f := newMetadataFixture(t, 1)
	id := domain.HashContent([]byte("concurrent acquisition"))
	winner := domain.Snapshot{ID: id, DocHash: id, RepoID: f.man.RepoID}
	f.onBatch = func(_ int, _ *pullResp) error {
		_, err := f.store.AppendMetadataCheckpoint(ctx, "", f.man.RepoID, f.client.SyncRemoteIdentity(), []domain.Snapshot{winner}, nil)
		return err
	}
	got, err := f.read(ctx)
	if err != nil || len(got) != 1 || got[0].ID != f.man.SnapshotIndex[0] {
		t.Fatalf("optional cache contention aborted acquisition: %v %v", got, err)
	}
	checkpoint, err := f.store.ReadMetadataCheckpoint(ctx, f.man.RepoID, f.client.SyncRemoteIdentity())
	if err != nil || len(checkpoint.Snapshots) != 1 || checkpoint.Snapshots[0].ID != id {
		t.Fatalf("losing writer overwrote winning checkpoint: %+v %v", checkpoint, err)
	}
}

func TestMetadataCheckpointObservedDeletionRequiresFreshReappearance(t *testing.T) {
	ctx := context.Background()
	f := newMetadataFixture(t, 1)
	id := f.man.SnapshotIndex[0]
	first := f.snaps[id]
	first.CreatedAt = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f.snaps[id] = first
	if _, err := f.read(ctx); err != nil {
		t.Fatal(err)
	}
	f.man.SnapshotIndex, f.man.SnapshotStates = nil, nil
	if got, err := f.read(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty full catalog: %v %v", got, err)
	}
	checkpoint, err := f.store.ReadMetadataCheckpoint(ctx, f.man.RepoID, f.client.SyncRemoteIdentity())
	if err != nil || len(checkpoint.Snapshots) != 0 {
		t.Fatalf("observed deletion was not retired: %+v %v", checkpoint, err)
	}
	recreated := first
	recreated.CreatedAt = first.CreatedAt.Add(time.Hour)
	f.snaps[id] = recreated
	state, _ := domain.SnapshotStateHash(first)
	f.man.SnapshotIndex = []domain.ContentHash{id}
	f.man.SnapshotStates = map[domain.ContentHash]domain.ContentHash{id: state}
	f.batches = nil
	got, err := f.read(ctx)
	if err != nil || len(got) != 1 || !got[0].CreatedAt.Equal(recreated.CreatedAt) || !reflect.DeepEqual(f.batches, []int{1}) {
		t.Fatalf("reappearance reused deleted metadata: %v batches=%v err=%v", got, f.batches, err)
	}
}

func TestMetadataCheckpointPartialPlanRetainsOtherBranches(t *testing.T) {
	ctx := context.Background()
	f := newMetadataFixture(t, 2)
	if _, err := f.read(ctx); err != nil {
		t.Fatal(err)
	}
	id := f.man.SnapshotIndex[0]
	ref := domain.Ref{RepoID: f.man.RepoID, Kind: domain.RefBranch, Name: "feature", Target: id}
	f.plan = &domain.BranchPullPlan{Version: 1, RepoID: f.man.RepoID, Branch: "feature", SelectedRef: ref, Refs: []domain.Ref{ref}, SnapshotIndex: []domain.ContentHash{id}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{id: f.man.SnapshotStates[id]}}
	f.batches = nil
	_, got, err := f.client.PullSelectedBranchTo(ctx, f.man.RepoID, domain.BranchPullRequest{Version: 1, Branch: "feature"}, nil, []domain.ContentHash{id}, stagedPullDocs{})
	if err != nil || len(got) != 1 || len(f.batches) != 0 {
		t.Fatalf("partial acquisition: %v batches=%v err=%v", got, f.batches, err)
	}
	checkpoint, err := f.store.ReadMetadataCheckpoint(ctx, f.man.RepoID, f.client.SyncRemoteIdentity())
	if err != nil || len(checkpoint.Snapshots) != 2 {
		t.Fatalf("partial plan pruned another branch: %+v %v", checkpoint, err)
	}
}

func TestMetadataCheckpointZeroWantsStillRequiresPullPermission(t *testing.T) {
	for _, count := range []int{0, 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newMetadataFixture(t, count)
			f.pullStatus = http.StatusForbidden
			got, docs, refs, err := f.client.Pull(context.Background(), f.man.RepoID, f.man.SnapshotStates, f.man.SnapshotIndex)
			if err == nil || got != nil || docs != nil || refs != nil || f.permissions != 1 {
				t.Fatalf("zero-wants pull bypassed permission: %v %v permissions=%d", got, err, f.permissions)
			}
		})
	}
}
