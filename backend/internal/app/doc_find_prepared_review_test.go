package app

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func preparedFindReviewSource(test *testing.T, text string) docViewSource {
	test.Helper()
	raw, err := json.Marshal(map[string]any{"kind": "tool_result", "output": text})
	if err != nil || len(raw) <= docViewBytes {
		test.Fatal("review fixture must use oversized path", err)
	}
	ctx := context.Background()
	plan, err := domain.BuildDocDisplayStringSourceReader(ctx, domain.HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal(err)
	}
	state, err := domain.NewDocDisplayStringState(plan)
	if err != nil {
		test.Fatal(err)
	}
	objects := map[domain.ContentHash][]byte{}
	for !state.Done {
		step, err := domain.StepDocDisplayString(ctx, plan, state, func(_ context.Context, offset, length int) ([]byte, error) { return raw[offset : offset+length], nil })
		if err != nil {
			test.Fatal(err)
		}
		for _, object := range step.Objects {
			objects[object.Hash] = object.Bytes
		}
		state = step.State
	}
	generation, tail, err := domain.FinishDocDisplayString(ctx, plan, state)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range tail {
		objects[object.Hash] = object.Bytes
	}
	entry := domain.DocRangeEntry{DocEventIndex: domain.DocEventIndex{Hash: plan.EventHash, Length: len(raw)}, Kind: domain.EventToolResult}
	return docViewSource{total: 1,
		entry: func(int) (domain.DocRangeEntry, error) { return entry, nil },
		read: func(int, int) ([]byte, error) {
			test.Error("prepared scan read the original source")
			return nil, domain.ErrIntegrity
		},
		displayGeneration: func(domain.DocEventIndex) (domain.DocDisplayStringGeneration, error) { return generation, nil },
		displayPage: func(_ domain.DocEventIndex, page int) (domain.DocDisplayPage, error) {
			return domain.ReadDocDisplayPage(ctx, generation, page, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
				return objects[hash], nil
			})
		},
	}
}
