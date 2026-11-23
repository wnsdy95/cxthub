package domain

import (
	"context"
	"errors"
	"fmt"
)

type RootEventSelection struct {
	Offset, Limit int
	Base          []ContentHash
	HashesOnly    bool
}

type verifiedRootEvent struct {
}

type VerifiedRootEvents struct {
	events                   []verifiedRootEvent
	total, offset, inherited int
}

func (verifier *CanonicalDocVerifier) VerifyConversationEvents(ctx context.Context, want ContentHash, manifest ConversationManifest, load func(context.Context, ContentHash) ([]byte, error), selection RootEventSelection) (VerifiedRootEvents, error) {
	if err := ctx.Err(); err != nil {
		return VerifiedRootEvents{}, err
	}
	if selection.Offset < -1 || selection.Limit < 0 || selection.Limit > 100 {
		return VerifiedRootEvents{}, fmt.Errorf("%w: invalid event range", ErrValidation)
	}
	if len(manifest.Envelope) > MaxConversationManifestBytes || len(manifest.Chunks) > MaxConversationManifestChunks {
		return VerifiedRootEvents{}, ErrIntegrity
	}
	if selection.Limit == 0 {
		selection.Limit = 50
	}
	selection.Base = append([]ContentHash(nil), selection.Base...)
	projection := VerifiedRootEvents{}
	var pending []canonicalEventProof
	var eventHash ContentHash
	check := func(ctx context.Context, version string, body []byte) (int, error) {
		eventHash = HashContent(body)
		return verifier.eventWithKey(ctx, canonicalEventKey{hash: eventHash, version: version}, body, &pending)
	}
	pageBytes, closed := 0, false
	observe := func(position int64, body []byte, sequence int) error {
		index := int(position)
		projection.total++
		if selection.HashesOnly {
			return nil
		}
		if index == projection.inherited && index < len(selection.Base) && eventHash == selection.Base[index] {
			projection.inherited++
		}
		start := selection.Offset
		if start == -1 {
			start = projection.inherited
		}
		if index < start || closed {
			return nil
		}
		if len(projection.events) == selection.Limit || (len(projection.events) > 0 && pageBytes+len(body) > 512<<10) {
			closed = true
			return nil
		}
		projection.events = append(projection.events, verifiedRootEvent{})
		pageBytes += len(body)
		return nil
	}
	var loadErr error
	var reader func(context.Context, ContentHash) ([]byte, error)
	if load != nil {
		reader = func(ctx context.Context, hash ContentHash) ([]byte, error) {
			body, err := load(ctx, hash)
			loadErr = err
			return body, err
		}
	}
	err := verifyConversationManifest(ctx, want, manifest, reader, check, observe)
	if loadErr != nil {
		return VerifiedRootEvents{}, loadErr
	}
	if err != nil {
		if errors.Is(err, ErrConversationManifest) && !errors.Is(err, ErrUnsupportedCIRVersion) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w: %w", ErrIntegrity, err)
		}
		return VerifiedRootEvents{}, err
	}
	projection.offset = selection.Offset
	if projection.offset == -1 {
		projection.offset = projection.inherited
	}
	if !selection.HashesOnly && projection.offset > projection.total {
		return VerifiedRootEvents{}, fmt.Errorf("%w: event offset exceeds document", ErrValidation)
	}
	if err := ctx.Err(); err != nil {
		return VerifiedRootEvents{}, err
	}
	verifier.remember(pending)
	return projection, nil
}
