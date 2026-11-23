package domain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDocMarkdownPrefetchCurrentInputFailuresRemainAtomic(test *testing.T) {
	compiled := compileMarkdownFixture(test, strings.Repeat("a", 8192), true)
	cursor, err := NewDocMarkdownPlanCursor(compiled.plan)
	if err != nil {
		test.Fatal(err)
	}
	for _, failure := range []error{ErrIntegrity, ErrNotFound, context.Canceled} {
		fragment, err := readDocMarkdownPlanFragment(context.Background(), compiled.plan, cursor,
			func(_ context.Context, hash ContentHash) ([]byte, error) { return compiled.objects[hash], nil },
			func(context.Context, int, int) ([]byte, error) { return nil, failure },
			docMarkdownReplayPolicy{MaxBytes: 4})
		if !errors.Is(err, failure) || !reflect.DeepEqual(fragment, DocMarkdownPlanFragment{}) {
			test.Fatal("current source failure exposed partial output", err, failure)
		}
	}
}
