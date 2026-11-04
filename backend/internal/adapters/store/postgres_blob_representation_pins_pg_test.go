//go:build postgres

package store

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func canonicalDocumentBytesPG(test *testing.T, size int) (domain.SessionDoc, []byte) {
	test.Helper()
	inner := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: test.Name()},
		Events: []domain.CIREvent{{Seq: 1, Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "x"}}}}}
	canonical, err := domain.CanonicalBytes(inner)
	if err != nil {
		test.Fatal(err)
	}
	inner.Events[0].Blocks[0].Text = strings.Repeat("i", 1+size-len(canonical))
	canonical, err = domain.CanonicalBytes(inner)
	if err != nil || len(canonical) != size {
		test.Fatal("nested canonical document must fill exactly the requested size", len(canonical), err)
	}
	document := domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: inner}
	if _, err := domain.VerifySessionDoc(document); err != nil {
		test.Fatal("nested document must be valid without a manufactured hash collision", err)
	}
	return document, canonical
}

func rootWithCanonicalDocumentChunk(test *testing.T) (rootReaderFixture, domain.SessionDoc) {
	test.Helper()
	document, canonical := canonicalDocumentBytesPG(test, domain.ConversationManifestChunkBytes)
	outer := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "outer:" + test.Name()},
		Events: []domain.CIREvent{{Seq: 1, Kind: domain.EventToolCall, CallID: "nested-call", ToolName: "nested-document",
			Input: map[string]any{"a": "", "b": json.RawMessage(canonical)}}}}
	encoded, err := domain.CanonicalBytes(outer)
	if err != nil {
		test.Fatal(err)
	}
	marker := []byte(`,"events":[`)
	stream := bytes.Index(encoded, marker) + len(marker)
	offset := bytes.Index(encoded, canonical) - stream
	if stream < len(marker) || offset < 0 {
		test.Fatal("outer canonical tool input did not preserve the nested document")
	}
	padding := (domain.ConversationManifestChunkBytes - offset%domain.ConversationManifestChunkBytes) % domain.ConversationManifestChunkBytes
	outer.Events[0].Input["a"] = strings.Repeat("p", padding)
	manifest, bodies, err := domain.ConversationManifestForCIR(outer)
	if err != nil {
		test.Fatal("outer root and its nested tool input must pass the actual canonical verifier", err)
	}
	if !bytes.Equal(bodies[document.Hash], canonical) {
		test.Fatal("actual root decomposition did not share the nested document's logical hash")
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		test.Fatal(err)
	}
	manifestBytes, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		test.Fatal(err)
	}
	outerCanonical, err := domain.CanonicalBytes(outer)
	if err != nil {
		test.Fatal(err)
	}
	return rootReaderFixture{hash: hash, manifest: manifest, manifestBytes: manifestBytes, canonical: outerCanonical, bodies: bodies, cir: outer}, document
}

func TestRootRepresentationActualCanonicalHashOverlap(test *testing.T) {
	fixture, document := rootWithCanonicalDocumentChunk(test)
	if len(fixture.bodies) < 2 || len(fixture.bodies[document.Hash]) != domain.ConversationManifestChunkBytes {
		test.Fatal("fixture does not exercise a real cross-kind dependency")
	}
}

func TestRootRepresentationPGPutDocPreservesOwnedDependency(test *testing.T) {
	for _, phase := range []string{"prepared", "published"} {
		test.Run(phase, func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			storage, ctx := chunkReusePG(test)
			ctx = rootPublicationContext(ctx)
			fixture, document := rootWithCanonicalDocumentChunk(test)
			job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, "")
			publication := prepareRootStreamingPG(test, storage, ctx, job, directory)
			if phase == "published" {
				if err := publication.Complete(ctx, job, time.Now()); err != nil {
					test.Fatal(err)
				}
			}
			before, err := storage.ReadConversationChunk(ctx, job.RepoID, document.Hash, domain.ConversationManifestChunkBytes)
			if err != nil || !bytes.Equal(before, fixture.bodies[document.Hash]) {
				test.Fatal("root dependency is not initially readable", err)
			}
			other := domain.HashContent([]byte("document-owner:" + test.Name()))
			if _, err := storage.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
				test.Fatal(err)
			}
			if _, err := storage.PutDoc(ctx, other, document); err != nil {
				test.Fatal("valid document reuse must not fail merely because another repo owns the same raw chunk", err)
			}
			after, err := storage.ReadConversationChunk(ctx, job.RepoID, document.Hash, domain.ConversationManifestChunkBytes)
			if err != nil || !bytes.Equal(after, before) {
				test.Fatal("valid PutDoc changed another root's owned content-addressed dependency", err)
			}
			if _, err := storage.GetDoc(ctx, other, document.Hash); err != nil {
				test.Fatal("shared raw representation must remain a readable document", err)
			}
			if phase == "prepared" {
				if err := publication.Complete(ctx, job, time.Now()); err != nil {
					test.Fatal("unmodified root dependency required manual repair or a new preparation", err)
				}
			}
			assertRangePages(test, ctx, storage, job.RepoID, fixture)
			verified, err := storage.ReadVerifiedDoc(ctx, job.RepoID, job.DocHash)
			if err != nil {
				test.Fatal("completed root no longer verifies after a legitimate document write", err)
			}
			assertRootProof(test, verified, fixture)
		})
	}
}
