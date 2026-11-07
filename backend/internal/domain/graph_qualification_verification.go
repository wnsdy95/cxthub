package domain

import (
	"context"
	"slices"
)

type GraphProjectionPartDigest struct {
	Kind   string
	Key    string
	Digest ContentHash
}

type graphQualificationVerification struct {
	key      GraphQualificationKey
	expected ContentHash
	actual   ContentHash
	parts    []GraphProjectionPartDigest
}

func VerifyGraphQualificationParts(ctx context.Context, key GraphQualificationKey, expected ContentHash, parts []GraphProjectionPart) (GraphQualificationResult, error) {
	if err := ctx.Err(); err != nil {
		return GraphQualificationResult{}, err
	}
	if key.Validate() != nil || ValidateContentHash(expected) != nil {
		return GraphQualificationResult{}, ErrValidation
	}
	if _, err := DecodeGraphViewProjection(ctx, key.RepoID, parts); err != nil {
		return GraphQualificationResult{}, err
	}
	actual, err := GraphViewProjectionManifestDigest(ctx, key.RepoID, parts)
	if err != nil {
		return GraphQualificationResult{}, err
	}
	proof := &graphQualificationVerification{key: key, expected: expected, actual: actual, parts: make([]GraphProjectionPartDigest, len(parts))}
	for index, part := range parts {
		if err := ctx.Err(); err != nil {
			return GraphQualificationResult{}, err
		}
		proof.parts[index] = GraphProjectionPartDigest{Kind: part.Kind, Key: part.Key, Digest: HashContent(part.Value)}
	}
	if err := ctx.Err(); err != nil {
		return GraphQualificationResult{}, err
	}
	return GraphQualificationResult{GraphQualificationKey: key, Expected: expected, Actual: actual, verification: proof}, nil
}

func (result GraphQualificationResult) VerifiedParts() ([]GraphProjectionPartDigest, bool, error) {
	if err := result.Validate(); err != nil {
		return nil, false, err
	}
	if result.verification == nil {
		return nil, false, nil
	}
	return slices.Clone(result.verification.parts), true, nil
}
