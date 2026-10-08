package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLegacySerializationCurrentBytesAndCancellation(t *testing.T) {
	doc := domain.CIRDocument{}
	for i := 0; i < 3; i++ {
		doc.Events = append(doc.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: i,
			Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat(string(rune('a'+i)), 512<<10)}}})
	}
	raw, err := domain.CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := domain.PlanDocChunks(raw)
	if !ok || len(plan.Bodies) < 2 {
		t.Fatal("fixture must span distinct chunks")
	}
	manifest, err := json.Marshal(plan.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	stored := docCompress(manifest)
	bodies := map[domain.ContentHash][]byte{}
	for hash, body := range plan.Bodies {
		bodies[hash] = docCompress(body)
	}
	want, repo := domain.HashContent(raw), domain.HashContent([]byte(t.Name()))
	last := plan.Manifest.Chunks[len(plan.Manifest.Chunks)-1]
	for _, warm := range []bool{false, true} {
		for _, mode := range []string{"valid", "corrupt", "missing", "cancel-before", "cancel-last-read"} {
			t.Run(fmtLegacyCase(warm, mode), func(t *testing.T) {
				var cache docProofCache
				load := func(_ context.Context, hash domain.ContentHash) ([]byte, error) { return bodies[hash], nil }
				if warm {
					if _, err := cache.verifyStored(context.Background(), repo, want, stored, load); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "cancel-before" {
					cancel()
				}
				reads := map[domain.ContentHash]int{}
				proof, err := cache.verifyStored(ctx, repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
					reads[hash]++
					if hash == last {
						switch mode {
						case "corrupt":
							return docCompress([]byte("changed-current-body")), nil
						case "missing":
							return nil, domain.ErrNotFound
						case "cancel-last-read":
							cancel()
						}
					}
					return bodies[hash], nil
				})
				if mode == "valid" {
					if err != nil || !proof.Valid() || proof.Hash() != want {
						t.Fatal("exact legacy proof failed", err)
					}
				} else {
					wantErr := domain.ErrIntegrity
					if mode == "missing" {
						wantErr = domain.ErrNotFound
					} else if strings.HasPrefix(mode, "cancel-") {
						wantErr = context.Canceled
					}
					if !errors.Is(err, wantErr) || proof.Valid() {
						t.Fatalf("want invalid proof and %v, got %v", wantErr, err)
					}
				}
				if mode == "cancel-before" {
					if len(reads) != 0 {
						t.Fatal("read after initial cancellation")
					}
				} else if len(reads) != len(bodies) {
					t.Fatal("current distinct body coverage changed")
				}
				for _, n := range reads {
					if n != 1 {
						t.Fatal("body read more than once")
					}
				}
			})
		}
	}
}

func fmtLegacyCase(warm bool, mode string) string {
	if warm {
		return "warm/" + mode
	}
	return "cold/" + mode
}
