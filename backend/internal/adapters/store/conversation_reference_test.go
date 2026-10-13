package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestStoredConversationReferenceReusableLoader(test *testing.T) {
	fixture := rootFixture(test, strings.Repeat("a", 3*domain.ConversationManifestChunkBytes))
	for _, compressed := range []bool{false, true} {
		stored := make(map[domain.ContentHash][]byte)
		for hash, body := range fixture.bodies {
			if compressed {
				body = docCompress(body)
			}
			stored[hash] = body
		}
		var buffer []byte
		calls := 0
		proof, err := verifyStoredConversationReference(context.Background(), &domain.CanonicalDocVerifier{}, fixture.hash, fixture.manifest, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
			if hash != fixture.manifest.Chunks[calls].Hash {
				test.Fatal("changed chunk occurrence order")
			}
			calls++
			buffer = append(buffer[:0], stored[hash]...)
			return buffer, nil
		})
		clear(buffer)
		if err != nil || calls != len(fixture.manifest.Chunks) || !proof.Matches(domain.Snapshot{ID: fixture.hash, DocHash: fixture.hash, DocIdentity: domain.DocumentIdentityRootV1}) {
			test.Fatal("stored reference", compressed, calls, proof, err)
		}
	}
}

func TestStoredConversationReferenceRejectsCurrentCorruption(test *testing.T) {
	fixture := rootFixture(test, strings.Repeat("a", 3*domain.ConversationManifestChunkBytes))
	for _, mode := range []string{"tail", "short", "long", "truncated-zstd", "repeated-occurrence"} {
		test.Run(mode, func(test *testing.T) {
			calls := 0
			seen := make(map[domain.ContentHash]bool)
			proof, err := verifyStoredConversationReference(context.Background(), &domain.CanonicalDocVerifier{}, fixture.hash, fixture.manifest, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
				calls++
				body := bytes.Clone(fixture.bodies[hash])
				if mode == "repeated-occurrence" && seen[hash] {
					body[len(body)-1] ^= 1
				}
				seen[hash] = true
				if calls == len(fixture.manifest.Chunks) {
					switch mode {
					case "tail":
						body[len(body)-1] ^= 1
					case "short":
						body = body[:len(body)-1]
					case "long":
						body = append(body, 'x')
					case "truncated-zstd":
						stored := docCompress(body)
						return stored[:len(stored)-1], nil
					}
				}
				return docCompress(body), nil
			})
			if !errors.Is(err, domain.ErrIntegrity) || proof.Valid() {
				test.Fatal("corrupt current bytes produced reference", proof, err)
			}
			if mode != "repeated-occurrence" && calls != len(fixture.manifest.Chunks) {
				test.Fatal("did not inspect final chunk", calls)
			}
		})
	}
}

func TestStoredConversationReferencePreservesLoaderErrorsAndCancellation(test *testing.T) {
	fixture := rootFixture(test, "reference error contracts")
	for _, marker := range []error{errors.New("storage unavailable"), domain.ErrNotFound, domain.ErrConflict, context.Canceled, context.DeadlineExceeded} {
		proof, err := verifyStoredConversationReference(context.Background(), &domain.CanonicalDocVerifier{}, fixture.hash, fixture.manifest, func(context.Context, domain.ContentHash) ([]byte, error) {
			return nil, marker
		})
		if err != marker || proof.Valid() {
			test.Fatal("storage error changed", proof, err, marker)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	proof, err := verifyStoredConversationReference(ctx, &domain.CanonicalDocVerifier{}, fixture.hash, fixture.manifest, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		cancel()
		return docCompress(fixture.bodies[hash]), nil
	})
	cancel()
	if !errors.Is(err, context.Canceled) || proof.Valid() {
		test.Fatal("canceled decoding produced reference", proof, err)
	}
}

func TestStoredConversationReferenceRejectsConflictingRepeatedLengths(test *testing.T) {
	fixture := rootFixture(test, strings.Repeat("a", 3*domain.ConversationManifestChunkBytes))
	if fixture.manifest.Chunks[1].Hash != fixture.manifest.Chunks[2].Hash {
		test.Fatal("fixture requires repeated occurrences")
	}
	fixture.manifest.Chunks[2].Bytes--
	proof, err := verifyStoredConversationReference(context.Background(), &domain.CanonicalDocVerifier{}, fixture.hash, fixture.manifest, func(context.Context, domain.ContentHash) ([]byte, error) {
		test.Fatal("read before rejecting inconsistent lengths")
		return nil, nil
	})
	if !errors.Is(err, domain.ErrIntegrity) || proof.Valid() {
		test.Fatal("conflicting declared lengths produced reference", proof, err)
	}
}

func TestStoredConversationReferenceFSOwnershipAndTail(test *testing.T) {
	ctx := context.Background()
	storage := NewFSStore(test.TempDir())
	repo := domain.HashContent([]byte(test.Name()))
	fixture := rootFixture(test, strings.Repeat("a", 3*domain.ConversationManifestChunkBytes))
	seedRootFS(test, storage, repo, fixture)
	for attempt := 0; attempt < 2; attempt++ {
		proof, err := storage.VerifyStoredDoc(ctx, repo, fixture.hash)
		if err != nil || proof.DocumentRef() != (domain.DocumentRef{Hash: fixture.hash, Identity: domain.DocumentIdentityRootV1}) {
			test.Fatal("FS reference", proof, err)
		}
	}
	if proof, err := storage.VerifyStoredDoc(ctx, domain.HashContent([]byte("other repository")), fixture.hash); !errors.Is(err, domain.ErrNotFound) || proof.Valid() {
		test.Fatal("foreign ownership produced reference", proof, err)
	}
	tail := fixture.manifest.Chunks[len(fixture.manifest.Chunks)-1].Hash
	path := storage.chunkPath(repo, tail)
	corrupt := bytes.Clone(fixture.bodies[tail])
	corrupt[len(corrupt)-1] ^= 1
	if err := writeAtomic(path, docCompress(corrupt)); err != nil {
		test.Fatal(err)
	}
	if proof, err := storage.VerifyStoredDoc(ctx, repo, fixture.hash); !errors.Is(err, domain.ErrIntegrity) || proof.Valid() {
		test.Fatal("current tail corruption produced reference", proof, err)
	}
	if err := os.Remove(path); err != nil {
		test.Fatal(err)
	}
	if proof, err := storage.VerifyStoredDoc(ctx, repo, fixture.hash); !errors.Is(err, domain.ErrNotFound) || proof.Valid() {
		test.Fatal("missing owned tail produced reference", proof, err)
	}
}
