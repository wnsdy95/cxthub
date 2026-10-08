package store

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// This plan is derived only from an owned domain proof. It carries no storage
// authority: completion must still retain and compare every current dependency.
type preparedRootDoc struct {
	manifest  domain.ConversationManifest
	canonical []byte
}

func prepareRootPublication(ctx context.Context, doc domain.VerifiedSessionDoc) (preparedRootDoc, error) {
	if err := ctx.Err(); err != nil {
		return preparedRootDoc{}, err
	}
	if !doc.Valid() {
		return preparedRootDoc{}, domain.ErrIntegrity
	}
	if doc.DocumentRef().Identity != domain.DocumentIdentityRootV1 {
		return preparedRootDoc{}, domain.ErrUnsupportedDocumentIdentity
	}
	manifest, ok := doc.ConversationManifest()
	if !ok {
		return preparedRootDoc{}, domain.ErrIntegrity
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		return preparedRootDoc{}, err
	}
	return preparedRootDoc{manifest: manifest, canonical: raw}, ctx.Err()
}

func publicationJobMatches(j domain.DocFinalizationJob, doc domain.VerifiedSessionDoc) error {
	if err := j.Validate(); err != nil {
		return err
	}
	if !doc.Valid() || j.DocumentRef() != doc.DocumentRef() {
		return domain.ErrIntegrity
	}
	if j.DocIdentity == domain.DocumentIdentityRootV1 {
		representation, err := j.Representation()
		if err != nil {
			return err
		}
		root, err := prepareRootPublication(context.Background(), doc)
		if err != nil {
			return err
		}
		if !bytes.Equal(representation.RootManifest, root.canonical) {
			return domain.ErrIntegrity
		}
	}
	return nil
}

func publicationJobChunks(j domain.DocFinalizationJob) ([]domain.ContentHash, error) {
	if err := j.Validate(); err != nil {
		return nil, err
	}
	if j.DocIdentity == domain.DocumentIdentityLegacy {
		return j.Manifest.Chunks, nil
	}
	representation, err := j.Representation()
	if err != nil {
		return nil, err
	}
	manifest, err := representation.ConversationManifest()
	if err != nil {
		return nil, err
	}
	chunks := make([]domain.ContentHash, len(manifest.Chunks))
	for i, chunk := range manifest.Chunks {
		chunks[i] = chunk.Hash
	}
	return chunks, nil
}

func (p preparedRootDoc) compareCurrent(ctx context.Context, doc domain.VerifiedSessionDoc, load func(context.Context, domain.ContentHash) ([]byte, error)) error {
	offset := 0
	for _, chunk := range p.manifest.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := load(ctx, chunk.Hash)
		if err != nil {
			return err
		}
		body, err := rootChunkBytes(ctx, raw, chunk.Bytes)
		if err != nil {
			return err
		}
		owned, err := doc.EventStreamRange(ctx, offset, int(chunk.Bytes))
		if err != nil {
			return err
		}
		if !bytes.Equal(body, owned) {
			return fmt.Errorf("%w: current root dependency differs from proof", domain.ErrIntegrity)
		}
		offset += int(chunk.Bytes)
	}
	return ctx.Err()
}

func (p preparedRootDoc) matchesStored(ctx context.Context, raw []byte) error {
	manifest, recognized, err := storedConversationManifest(ctx, raw)
	if err != nil {
		return err
	}
	if !recognized {
		return domain.ErrIntegrity
	}
	canonical, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, p.canonical) {
		return domain.ErrIntegrity
	}
	return ctx.Err()
}

// Caller holds the queue and sweep/retention locks through the completed job
// receipt. No missing dependency is reconstructed from the proof.
func (s *FSStore) putRootDocJob(ctx context.Context, repo domain.ContentHash, doc domain.VerifiedSessionDoc, fence func() error) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	root, err := prepareRootPublication(ctx, doc)
	if err != nil {
		return err
	}
	path := s.docPath(repo, doc.Hash())
	current, err := os.ReadFile(path)
	missing := os.IsNotExist(err)
	if err != nil && !missing {
		return err
	}
	if !missing {
		if err := root.matchesStored(ctx, current); err != nil {
			return err
		}
	}
	if err := root.compareCurrent(ctx, doc, s.ownedFSChunkReader(repo)); err != nil {
		return err
	}
	// Validation and lock waiting may outlive the lease. Check the current
	// claim again immediately before making the root visible.
	if err := fence(); err != nil {
		return err
	}
	if missing {
		if err := writeAtomic(path, docCompress(root.canonical)); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.putVerifiedReadIndex(repo, doc); err != nil {
		return err
	}
	return ctx.Err()
}
