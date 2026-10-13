package domain

import (
	"context"
	"errors"
	"fmt"
)

func VerifyConversationManifestReference(ctx context.Context, want ContentHash, manifest ConversationManifest, load func(context.Context, ContentHash) ([]byte, error)) (VerifiedDocReference, error) {
	var verifier CanonicalDocVerifier
	return verifier.VerifyConversationManifestReference(ctx, want, manifest, load)
}

func (verifier *CanonicalDocVerifier) VerifyConversationManifestReference(ctx context.Context, want ContentHash, manifest ConversationManifest, load func(context.Context, ContentHash) ([]byte, error)) (VerifiedDocReference, error) {
	var pending []canonicalEventProof
	check := func(ctx context.Context, version string, body []byte) (int, error) {
		return verifier.eventWithKey(ctx, canonicalEventKey{hash: HashContent(body), version: version}, body, &pending)
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
	err := verifyConversationManifest(ctx, want, manifest, reader, check, nil)
	if loadErr != nil {
		return VerifiedDocReference{}, loadErr
	}
	if err != nil {
		if errors.Is(err, ErrConversationManifest) && !errors.Is(err, ErrUnsupportedCIRVersion) &&
			!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w: %w", ErrIntegrity, err)
		}
		return VerifiedDocReference{}, err
	}
	if err := ctx.Err(); err != nil {
		return VerifiedDocReference{}, err
	}
	verifier.remember(pending)
	return VerifiedDocReference{hash: want, identity: DocumentIdentityRootV1}, nil
}
