package domain

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

const GraphQualificationRules = 1

type GraphQualificationKey struct {
	RepoID          ContentHash
	Dataset         string
	Applied         RepositoryViewSourceCheckpoint
	Rules           int
	ComparisonRules int
}

type GraphQualificationLease struct {
	GraphQualificationKey
	Fence             int64
	LeaseUntil        time.Time
	ReferenceExpected ContentHash
}

type GraphProjectionReference struct {
	GraphQualificationKey
	Expected ContentHash
}

type GraphQualificationResult struct {
	GraphQualificationKey
	Expected ContentHash
	Actual   ContentHash
}

func (key GraphQualificationKey) Validate() error {
	if key.Applied.Validate() != nil || key.Applied.Scope != RepositoryGraphSourceScope ||
		key.Applied.RepoID != key.RepoID || key.Rules != GraphViewProjectionRules || key.ComparisonRules != GraphQualificationRules {
		return ErrValidation
	}
	dataset := key.Applied
	dataset.Epoch = key.Dataset
	return dataset.Validate()
}

func (lease GraphQualificationLease) Validate() error {
	if lease.Fence <= 0 || lease.LeaseUntil.IsZero() || ValidateOptionalContentHash(lease.ReferenceExpected) != nil {
		return ErrValidation
	}
	return lease.GraphQualificationKey.Validate()
}

func (reference GraphProjectionReference) Validate() error {
	if ValidateContentHash(reference.Expected) != nil {
		return ErrValidation
	}
	return reference.GraphQualificationKey.Validate()
}

func (result GraphQualificationResult) Validate() error {
	if ValidateContentHash(result.Expected) != nil || ValidateContentHash(result.Actual) != nil {
		return ErrValidation
	}
	return result.GraphQualificationKey.Validate()
}

func (result GraphQualificationResult) Matched() bool {
	return result.Validate() == nil && result.Expected == result.Actual
}

func (projection GraphProjectionRead) QualificationKey() GraphQualificationKey {
	return GraphQualificationKey{RepoID: projection.Applied.RepoID, Dataset: projection.Dataset,
		Applied: projection.Applied, Rules: projection.Rules, ComparisonRules: GraphQualificationRules}
}

func GraphViewDigest(ctx context.Context, repo ContentHash, view RepositoryView) (ContentHash, error) {
	parts, err := EncodeGraphViewProjection(ctx, repo, view)
	if err != nil {
		return "", err
	}
	return GraphViewProjectionManifestDigest(ctx, repo, parts)
}

func GraphViewProjectionManifestDigest(ctx context.Context, repo ContentHash, parts []GraphProjectionPart) (ContentHash, error) {
	view := RepositoryView{Graph: &GraphState{}}
	fields := graphViewProjectionFields(&view, &graphProjectionOperations{}, &graphViewProjectionRows{})
	if len(parts) != len(fields)+1 {
		return "", ErrIntegrity
	}
	var manifest *graphProjectionHeader
	for _, part := range parts {
		if part.Kind != "header" || part.Key != "view" {
			continue
		}
		if manifest != nil {
			return "", ErrIntegrity
		}
		header, err := graphProjectionDecodeFieldHeader(ctx, part, GraphViewProjectionRules, "graph-view-v2", fields)
		if err != nil {
			return "", err
		}
		manifest = &header
	}
	if manifest == nil || manifest.RepoID != repo {
		return "", ErrIntegrity
	}
	slices.SortFunc(manifest.Parts, func(left, right graphProjectionPartIdentity) int { return strings.Compare(left.Key, right.Key) })
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return HashContent(encoded), nil
}
