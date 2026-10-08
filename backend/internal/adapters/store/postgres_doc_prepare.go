//go:build postgres

package store

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// All fields are derived here from immutable verified bytes. This value is
// adapter-private: it is neither a stored trust flag nor client-provided input.
// Heavy canonical scanning and index planning precede repository/row locks.
type preparedDocPG struct {
	doc     domain.VerifiedSessionDoc
	root    *preparedRootDoc
	chunks  domain.DocChunkPlan
	chunked bool
	payload []byte
	read    preparedReadIndexPG
}

type preparedReadIndexPG struct {
	hash       domain.ContentHash
	blocks     []domain.DocReadBlock
	envelope   []byte
	eventCount int
}

func prepareReadIndexPG(ctx context.Context, doc domain.VerifiedSessionDoc) (preparedReadIndexPG, error) {
	if err := ctx.Err(); err != nil {
		return preparedReadIndexPG{}, err
	}
	plan, err := doc.PlanReadIndexContext(ctx)
	if err != nil {
		return preparedReadIndexPG{}, err
	}
	if err := ctx.Err(); err != nil {
		return preparedReadIndexPG{}, err
	}
	env, err := json.Marshal(plan.Envelope())
	if err != nil {
		return preparedReadIndexPG{}, err
	}
	return preparedReadIndexPG{doc.Hash(), plan.Blocks(), env, plan.EventCount()}, ctx.Err()
}

func prepareVerifiedDocPG(ctx context.Context, doc domain.VerifiedSessionDoc) (preparedDocPG, error) {
	if doc.DocumentRef().Identity != domain.DocumentIdentityLegacy {
		return preparedDocPG{}, domain.ErrUnsupportedDocumentIdentity
	}
	if err := ctx.Err(); err != nil {
		return preparedDocPG{}, err
	}
	if !doc.Valid() {
		return preparedDocPG{}, domain.ErrIntegrity
	}
	out := preparedDocPG{doc: doc}
	out.chunks, out.chunked = doc.ChunkPlan()
	if err := ctx.Err(); err != nil {
		return preparedDocPG{}, err
	}
	var payload []byte
	if out.chunked {
		var err error
		payload, err = json.Marshal(out.chunks.Manifest)
		if err != nil {
			return preparedDocPG{}, err
		}
	} else {
		payload = doc.Bytes()
	}
	out.payload = docCompress(payload)
	var err error
	out.read, err = prepareReadIndexPG(ctx, doc)
	if err != nil {
		return preparedDocPG{}, err
	}
	return out, ctx.Err()
}
