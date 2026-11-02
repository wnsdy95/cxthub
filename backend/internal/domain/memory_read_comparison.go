package domain

import (
	"context"
	"encoding/json"
	"errors"
)

const MemoryReadComparisonVersion = 1
const MaxMemoryReadComparisonBytes = 4096

var ErrMemoryReadComparisonMismatch = errors.New("memory read comparison requires attention")

type MemoryReadComparison struct {
	Version   int                              `json:"version"`
	ReadID    ContentHash                      `json:"read_id"`
	CaptureID ContentHash                      `json:"capture_id"`
	TargetID  ContentHash                      `json:"target_id"`
	Source    RepositoryMemorySourceGeneration `json:"source"`
	Expected  ContentHash                      `json:"expected"`
	Actual    ContentHash                      `json:"actual"`
}

func (comparison MemoryReadComparison) Matched() bool {
	return ValidateContentHash(comparison.Expected) == nil && comparison.Expected == comparison.Actual
}

func MemoryReadComparisonStateHash(ctx context.Context, repo ContentHash, origin string, page EffectiveMemoryPage) (ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if ValidateContentHash(repo) != nil || page.Selection.Validate() != nil ||
		ValidateContentHash(page.DeliveryStateHash) != nil || ValidateContentHash(page.StateHash) != nil ||
		ValidateContentHash(page.LineageHash) != nil || page.Total < 0 || page.Total > MaxMemoryReadItems {
		return "", ErrIntegrity
	}
	page.Items, page.NextCursor = nil, ""
	raw, err := json.Marshal(struct {
		Version int                 `json:"version"`
		RepoID  ContentHash         `json:"repo_id"`
		Origin  string              `json:"origin"`
		View    EffectiveMemoryPage `json:"view"`
	}{MemoryReadComparisonVersion, repo, origin, page})
	if err != nil {
		return "", err
	}
	if len(raw) > MaxMemoryReadHeaderBytes {
		return "", ErrMemoryProjectionLimit
	}
	return HashContent(append([]byte("cxthub:memory-read-comparison:v1\x00"), raw...)), ctx.Err()
}

func MemoryReadComparisonBytes(ctx context.Context, comparison MemoryReadComparison) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if comparison.Version != MemoryReadComparisonVersion || comparison.Source.Validate() != nil ||
		ValidateContentHash(comparison.ReadID) != nil || ValidateContentHash(comparison.CaptureID) != nil ||
		ValidateContentHash(comparison.TargetID) != nil || ValidateContentHash(comparison.Expected) != nil ||
		ValidateContentHash(comparison.Actual) != nil {
		return nil, ErrIntegrity
	}
	raw, err := json.Marshal(comparison)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxMemoryReadComparisonBytes {
		return nil, ErrMemoryProjectionLimit
	}
	return raw, ctx.Err()
}

func ValidateMemoryReadComparison(ctx context.Context, comparison MemoryReadComparison, header MemoryReadHeader) error {
	if _, err := MemoryReadComparisonBytes(ctx, comparison); err != nil {
		return err
	}
	raw, err := MemoryReadHeaderBytes(ctx, header)
	if err != nil {
		return err
	}
	target, err := MemoryReadTargetID(ctx, MemoryReadTarget{Version: MemoryReadWorkVersion, Rules: header.Rules,
		RepoID: header.Source.Graph.RepoID, Selection: header.View.Selection, Content: header.View.Content})
	if err != nil {
		return err
	}
	actual, err := MemoryReadComparisonStateHash(ctx, header.Source.Graph.RepoID, header.Origin, header.View)
	if err != nil {
		return err
	}
	if comparison.ReadID != HashContent(raw) || comparison.CaptureID != header.CaptureID ||
		comparison.Source != header.Source || comparison.TargetID != target || comparison.Actual != actual {
		return ErrIntegrity
	}
	return ctx.Err()
}
