package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func legacyProofStreamFixture(t testing.TB) (domain.ContentHash, domain.DocChunkPlan) {
	t.Helper()
	doc := domain.CIRDocument{}
	for ordinal := range 6 {
		doc.Events = append(doc.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: ordinal,
			Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat(string(rune('A'+ordinal))+" MiXeD text ", 18000)}}})
	}
	canonical, err := domain.CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := domain.PlanDocChunks(canonical)
	if !ok || len(plan.Bodies) < 3 {
		t.Fatal("fixture needs distinct V2 chunks")
	}
	return domain.HashContent(canonical), plan
}

func legacyProofPhysicalKey(repo, want domain.ContentHash, stored []byte, manifest domain.DocChunkManifest, bodies map[domain.ContentHash][]byte) docProofKey {
	fingerprint := sha256.New()
	fingerprint.Write([]byte("cxt-stored-doc-proof-v1\x00"))
	fingerprint.Write([]byte(domain.HashContent(stored)))
	seen := map[domain.ContentHash]bool{}
	for _, hash := range manifest.Chunks {
		if !seen[hash] {
			seen[hash] = true
			fingerprint.Write([]byte(hash))
			fingerprint.Write([]byte(domain.HashContent(bodies[hash])))
		}
	}
	return docProofKey{repo: repo, expected: want, representation: domain.ContentHash("sha256:" + hex.EncodeToString(fingerprint.Sum(nil)))}
}

func TestLegacyProofStreamRepackingAndReusedLoaderBuffer(t *testing.T) {
	want, plan := legacyProofStreamFixture(t)
	repo := domain.HashContent([]byte(t.Name()))
	var cache docProofCache
	for _, compressed := range []bool{false, true, false} {
		bodies := map[domain.ContentHash][]byte{}
		for hash, body := range plan.Bodies {
			if compressed {
				body = docCompress(body)
			}
			bodies[hash] = body
		}
		stored, err := json.Marshal(plan.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		if compressed {
			stored = docCompress(stored)
		}
		for range 2 {
			reads := map[domain.ContentHash]int{}
			scratch := make([]byte, domain.ChunkTarget*2)
			proof, err := cache.verifyStored(context.Background(), repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
				reads[hash]++
				clear(scratch)
				copy(scratch, bodies[hash])
				return scratch[:len(bodies[hash])], nil
			})
			if err != nil || !proof.Valid() || proof.Hash() != want {
				t.Fatalf("compressed=%v reused loader: %v %v", compressed, proof, err)
			}
			if len(reads) != len(bodies) {
				t.Fatal("did not read every distinct current owned chunk")
			}
			for hash, count := range reads {
				if count != 1 {
					t.Fatalf("unchecked second storage read for %s: %d", hash, count)
				}
			}
			if _, ok := cache.get(legacyProofPhysicalKey(repo, want, stored, plan.Manifest, bodies)); !ok {
				t.Fatal("exact physical fingerprint changed")
			}
		}
	}
	if len(cache.proofs) != 2 {
		t.Fatalf("repacking should retain two physical proofs, got %d", len(cache.proofs))
	}
}

func TestLegacyProofStreamLateFailureAndCleanup(t *testing.T) {
	want, plan := legacyProofStreamFixture(t)
	repo := domain.HashContent([]byte(t.Name()))
	stored, err := json.Marshal(plan.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	last := plan.Manifest.Chunks[len(plan.Manifest.Chunks)-1]
	for _, warm := range []bool{false, true} {
		for _, failure := range []string{"corrupt", "missing", "cancel"} {
			t.Run(fmtLegacyCase(warm, failure), func(t *testing.T) {
				directory := t.TempDir()
				t.Setenv("TMPDIR", directory)
				var cache docProofCache
				if warm {
					if _, err := cache.verifyStored(context.Background(), repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
						return plan.Bodies[hash], nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				before := len(cache.proofs)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				reads := map[domain.ContentHash]int{}
				proof, err := cache.verifyStored(ctx, repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
					reads[hash]++
					if hash == last && reads[hash] == 1 {
						switch failure {
						case "corrupt":
							body := slices.Clone(plan.Bodies[hash])
							body[0] ^= 1
							return docCompress(body), nil
						case "missing":
							return nil, domain.ErrNotFound
						case "cancel":
							cancel()
						}
					}
					return plan.Bodies[hash], nil
				})
				wantErr := domain.ErrIntegrity
				if failure == "missing" {
					wantErr = domain.ErrNotFound
				} else if failure == "cancel" {
					wantErr = context.Canceled
				}
				if !errors.Is(err, wantErr) || proof.Valid() || len(cache.proofs) != before {
					t.Fatalf("failure minted a proof: %v %v", proof, err)
				}
				if len(reads) != len(plan.Bodies) {
					t.Fatal("fixture did not fail at the final current chunk")
				}
				for hash, count := range reads {
					if count != 1 {
						t.Fatalf("storage reread %s: %d", hash, count)
					}
				}
				remaining, err := os.ReadDir(directory)
				if err != nil || len(remaining) != 0 {
					t.Fatalf("temporary bytes remain: %v %v", remaining, err)
				}
			})
		}
	}
}

func TestLegacyProofStreamCancellationBeforeRead(t *testing.T) {
	want, plan := legacyProofStreamFixture(t)
	stored, _ := json.Marshal(plan.Manifest)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var cache docProofCache
	proof, err := cache.verifyStored(ctx, domain.HashContent([]byte(t.Name())), want, stored, func(context.Context, domain.ContentHash) ([]byte, error) {
		t.Fatal("read after cancellation")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) || proof.Valid() {
		t.Fatalf("cancellation ignored: %v %v", proof, err)
	}
}

func TestLegacyProofStreamBoundsDecodedChunk(t *testing.T) {
	_, plan := legacyProofStreamFixture(t)
	body := bytes.Repeat([]byte("x"), maxLegacyReadBytes+1)
	hash := domain.HashContent(body)
	compressed := docCompress(body)
	manifest := domain.DocChunkManifest{Format: domain.ChunkFormatV2, Envelope: plan.Manifest.Envelope, Chunks: []domain.ContentHash{hash}}
	stored, _ := json.Marshal(manifest)
	var cache docProofCache
	proof, err := cache.verifyStored(context.Background(), domain.HashContent([]byte(t.Name())), hash, stored, func(context.Context, domain.ContentHash) ([]byte, error) {
		return compressed, nil
	})
	if !errors.Is(err, domain.ErrDocumentResourceLimit) || proof.Valid() || len(cache.proofs) != 0 {
		t.Fatalf("oversized chunk was not bounded: %v %v", proof, err)
	}
}

func TestLegacyProofStreamSpoolCreationFailure(t *testing.T) {
	want, plan := legacyProofStreamFixture(t)
	stored, _ := json.Marshal(plan.Manifest)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "absent"))
	var cache docProofCache
	proof, err := cache.verifyStored(context.Background(), domain.HashContent([]byte(t.Name())), want, stored, func(context.Context, domain.ContentHash) ([]byte, error) {
		t.Fatal("read without a bounded spool")
		return nil, nil
	})
	if err == nil || proof.Valid() || len(cache.proofs) != 0 {
		t.Fatalf("spool failure did not fail closed: %v %v", proof, err)
	}
}

func TestLegacyProofStreamUsesOnlyPhysicalSpool(t *testing.T) {
	want, plan := legacyProofStreamFixture(t)
	repo := domain.HashContent([]byte(t.Name()))
	stored, _ := json.Marshal(plan.Manifest)
	for _, warm := range []bool{false, true} {
		t.Run(fmtLegacyCase(warm, "physical-only"), func(t *testing.T) {
			var cache docProofCache
			if warm {
				if _, err := cache.verifyStored(context.Background(), repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
					return docCompress(plan.Bodies[hash]), nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			directory := t.TempDir()
			t.Setenv("TMPDIR", directory)
			reads := 0
			proof, err := cache.verifyStored(context.Background(), repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
				reads++
				files, err := os.ReadDir(directory)
				if err != nil || len(files) != 1 {
					t.Fatalf("want only the physical spool, got %v %v", files, err)
				}
				if reads == len(plan.Bodies) {
					t.Setenv("TMPDIR", filepath.Join(directory, "absent"))
				}
				return docCompress(plan.Bodies[hash]), nil
			})
			if err != nil || proof.Hash() != want || reads != len(plan.Bodies) {
				t.Fatalf("reference reread or spooled canonical bytes: %v %v reads=%d", proof, err, reads)
			}
			remaining, err := os.ReadDir(directory)
			if err != nil || len(remaining) != 0 {
				t.Fatalf("reference retained temporary bytes: %v %v", remaining, err)
			}
		})
	}
}

func TestLegacyProofStreamRejectsMislabeledOwnedChunk(t *testing.T) {
	want, plan := legacyProofStreamFixture(t)
	repo := domain.HashContent([]byte(t.Name()))
	original := plan.Manifest.Chunks[0]
	wrong := domain.HashContent([]byte("wrong chunk address"))
	plan.Manifest.Chunks[0] = wrong
	stored, _ := json.Marshal(plan.Manifest)
	var cache docProofCache
	proof, err := cache.verifyStored(context.Background(), repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		if hash == wrong {
			return plan.Bodies[original], nil
		}
		return plan.Bodies[hash], nil
	})
	if !errors.Is(err, domain.ErrIntegrity) || proof.Valid() || len(cache.proofs) != 0 {
		t.Fatalf("matching document hash bypassed chunk address validation: %v %v", proof, err)
	}
}

func TestLegacyProofStreamEmptyV2(t *testing.T) {
	canonical, err := domain.CanonicalBytes(domain.CIRDocument{})
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		Envelope json.RawMessage `json:"envelope"`
	}
	if err := json.Unmarshal(canonical, &fields); err != nil {
		t.Fatal(err)
	}
	want, repo := domain.HashContent(canonical), domain.HashContent([]byte(t.Name()))
	for _, chunks := range [][]domain.ContentHash{nil, {}} {
		manifest := domain.DocChunkManifest{Format: domain.ChunkFormatV2, Envelope: fields.Envelope, Chunks: chunks}
		stored, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		for _, compressed := range []bool{false, true} {
			current := stored
			if compressed {
				current = docCompress(current)
			}
			var cache docProofCache
			for range 2 {
				proof, err := cache.verifyStored(context.Background(), repo, want, current, func(context.Context, domain.ContentHash) ([]byte, error) {
					t.Fatal("empty legacy compatibility path loaded a chunk")
					return nil, nil
				})
				if err != nil || proof.Hash() != want {
					t.Fatalf("empty V2 compatibility: %v %v", proof, err)
				}
				proof, err = cache.verifyStoredLegacyStream(context.Background(), repo, want, domain.HashContent(current), manifest, nil)
				if err != nil || proof.Hash() != want {
					t.Fatalf("empty V2 stream: %v %v", proof, err)
				}
			}
		}
	}
}

func TestLegacyProofStreamSinkBoundsAndCancellation(t *testing.T) {
	ctx := context.Background()
	var sink legacyProofStreamSink
	body := []byte("discarded bytes")
	if err := sink.Append(ctx, body); err != nil || sink.size != len(body) {
		t.Fatalf("discard accounting: size=%d err=%v", sink.size, err)
	}
	clear(body)
	if raw, err := sink.Read(ctx, 0, sink.size); !errors.Is(err, domain.ErrIntegrity) || raw != nil {
		t.Fatalf("reference sink exposed readable canonical bytes: %q %v", raw, err)
	}
	sink.size = domain.MaxFinalizedDocBytes - 1
	if err := sink.Append(ctx, []byte{1}); err != nil || sink.size != domain.MaxFinalizedDocBytes {
		t.Fatalf("exact byte limit: size=%d err=%v", sink.size, err)
	}
	if err := sink.Append(ctx, []byte{1}); !errors.Is(err, domain.ErrDocumentResourceLimit) || sink.size != domain.MaxFinalizedDocBytes {
		t.Fatalf("byte limit exceeded: size=%d err=%v", sink.size, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	sink.size = 0
	if err := sink.Append(cancelled, body); !errors.Is(err, context.Canceled) || sink.size != 0 {
		t.Fatalf("cancelled append changed accounting: size=%d err=%v", sink.size, err)
	}
	if raw, err := sink.Read(cancelled, 0, 0); !errors.Is(err, context.Canceled) || raw != nil {
		t.Fatalf("cancelled read: %q %v", raw, err)
	}
}
