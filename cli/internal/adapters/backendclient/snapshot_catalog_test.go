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

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type catalogRoundTrip func(*http.Request) (*http.Response, error)

func (f catalogRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise real request encoding/response validation with no socket or server.
func TestIncomingSnapshotCatalogWire(t *testing.T) {
	for _, mode := range []string{"valid", "empty", "changed-token", "wrong-repo", "wrong-id", "duplicate", "omitted", "missing-tokens", "unsolicited-body", "denied", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			repo := string(domain.HashContent([]byte("catalog repo")))
			man := domain.Manifest{RepoID: repo, SnapshotStates: map[domain.ContentHash]domain.ContentHash{}}
			byID := map[domain.ContentHash]domain.Snapshot{}
			count := 259
			if mode == "empty" {
				count = 0
			}
			for i := 0; i < count; i++ {
				id := domain.HashContent([]byte(fmt.Sprint(i)))
				snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Message: fmt.Sprint(i)}
				man.SnapshotIndex = append(man.SnapshotIndex, id)
				man.SnapshotStates[id], _ = domain.SnapshotStateHash(snap)
				byID[id] = snap
			}
			if mode == "missing-tokens" {
				man.SnapshotStates = nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var batches []int
			c := NewBackendClient(func() string { return "https://catalog.invalid" }, func() string { return "synthetic-token" }, domain.TeamIdentity{})
			c.httpc.Transport = catalogRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer synthetic-token" {
					t.Error("authorization missing")
				}
				if err := r.Context().Err(); err != nil {
					return nil, err
				}
				var value any
				status := 200
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest") {
					value = man
				} else if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pull/objects") {
					var in pullReq
					if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
						t.Fatal(err)
					}
					if len(in.SnapshotWants) == 0 || len(in.SnapshotWants) > 256 || len(in.DocWants)+len(in.DocManifestWants)+len(in.ChunkWants) != 0 {
						t.Fatalf("body requests in metadata catalog: %+v", in)
					}
					batches = append(batches, len(in.SnapshotWants))
					var snaps []domain.Snapshot
					for _, id := range in.SnapshotWants {
						snaps = append(snaps, byID[id])
					}
					switch mode {
					case "changed-token":
						snaps[0].Message = "changed"
					case "wrong-repo":
						snaps[0].RepoID = string(domain.HashContent([]byte("other")))
					case "wrong-id":
						snaps[0].ID = domain.HashContent([]byte("other"))
						snaps[0].DocHash = snaps[0].ID
					case "duplicate":
						snaps[1] = snaps[0]
					case "omitted":
						snaps = snaps[1:]
					case "denied":
						status = 403
					case "canceled":
						cancel()
						return nil, ctx.Err()
					}
					response := pullResp{Snapshots: snaps}
					if mode == "unsolicited-body" {
						response.Docs = []domain.SessionDoc{{}}
					}
					value = response
				} else {
					t.Fatalf("unexpected route %s %s", r.Method, r.URL.Path)
				}
				body, _ := json.Marshal(value)
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
			})
			got, err := c.ReadSnapshotCatalog(ctx, repo)
			if mode == "valid" || mode == "empty" {
				if err != nil || len(got) != count {
					t.Fatalf("catalog=%d err=%v", len(got), err)
				}
				if mode == "valid" && !reflect.DeepEqual(batches, []int{256, 3}) {
					t.Fatalf("batches %v", batches)
				}
			} else {
				if err == nil || got != nil {
					t.Fatalf("invalid catalog succeeded: %d %v", len(got), err)
				}
				if mode == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if len(batches) > 1 {
					t.Fatalf("failed catalog continued: %v", batches)
				}
			}
		})
	}
}
