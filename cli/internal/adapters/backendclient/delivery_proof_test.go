package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestDeliveryProofWireOptionalAndValidated(t *testing.T) {
	for _, endpoint := range []string{"context", "memory"} {
		for _, mode := range []string{"legacy", "proof", "malformed"} {
			t.Run(endpoint+"/"+mode, func(t *testing.T) {
				hash := domain.HashContent([]byte("source"))
				proof := domain.ContentHash("")
				if mode == "proof" {
					proof = domain.HashContent([]byte("delivery"))
				}
				if mode == "malformed" {
					proof = "private-invalid-proof"
				}
				calls := 0
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.Method != http.MethodGet {
						t.Error("unexpected mutation")
					}
					if endpoint == "context" {
						_ = json.NewEncoder(w).Encode(domain.ContextQueryView{Version: 1, StateHash: hash, DeliveryStateHash: proof, Position: hash, Snapshots: []domain.Snapshot{{ID: hash, RepoID: string(hash)}}})
					} else {
						_ = json.NewEncoder(w).Encode(domain.EffectiveMemoryPage{StateHash: hash, DeliveryStateHash: proof})
					}
				}))
				defer ts.Close()
				c := NewBackendClient(func() string { return ts.URL }, func() string { return "synthetic" }, domain.TeamIdentity{})
				var got domain.ContentHash
				var err error
				if endpoint == "context" {
					var v domain.ContextQueryView
					v, err = c.QueryContext(context.Background(), string(hash), domain.ContextSelection{Position: string(hash)})
					got = v.DeliveryStateHash
				} else {
					var v domain.EffectiveMemoryPage
					v, err = c.QueryEffectiveMemory(context.Background(), string(hash), domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: hash, CodeCommit: strings.Repeat("a", 40)}, Content: "prompt", Limit: 50})
					got = v.DeliveryStateHash
				}
				if mode == "malformed" {
					if !errors.Is(err, domain.ErrHashMismatch) || strings.Contains(err.Error(), string(proof)) {
						t.Fatal("malformed proof accepted or leaked", err)
					}
				} else if err != nil || got != proof {
					t.Fatal("proof lost", got, err)
				}
				if calls != 1 {
					t.Fatal("unexpected retry", calls)
				}
			})
		}
	}
}
