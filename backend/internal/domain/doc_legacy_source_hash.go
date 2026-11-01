package domain

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
)

func legacySourceHashAppend(ctx context.Context, state, body []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := sha256.New()
	if len(state) != 0 {
		if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
			return nil, ErrIntegrity
		}
	}
	for offset := 0; offset < len(body); offset += canonicalCheckBytes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		digest.Write(body[offset:min(offset+canonicalCheckBytes, len(body))])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return digest.(encoding.BinaryMarshaler).MarshalBinary()
}

func legacySourceHashSum(ctx context.Context, state []byte) (ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	digest := sha256.New()
	if len(state) != 0 {
		if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
			return "", ErrIntegrity
		}
	}
	return ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil))), nil
}
