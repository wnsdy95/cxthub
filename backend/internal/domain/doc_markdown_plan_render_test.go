package domain

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestDocMarkdownPlanFragmentRejectsBadCursorBeforeIO(test *testing.T) {
	compiled := compileMarkdownFixture(test, "**alpha** beta", true)
	initial, err := NewDocMarkdownPlanCursor(compiled.plan)
	if err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*DocMarkdownPlanCursor){
		func(cursor *DocMarkdownPlanCursor) { cursor.Version = 2 },
		func(cursor *DocMarkdownPlanCursor) { cursor.Plan = HashContent([]byte("another plan")) },
		func(cursor *DocMarkdownPlanCursor) { cursor.Page = -1 },
		func(cursor *DocMarkdownPlanCursor) { cursor.Operation = DocMarkdownPlanOperations },
		func(cursor *DocMarkdownPlanCursor) { cursor.Byte = -1 },
		func(cursor *DocMarkdownPlanCursor) { cursor.TextBytes = -1 },
		func(cursor *DocMarkdownPlanCursor) { cursor.Page = compiled.plan.Pages.Count },
	} {
		cursor := initial
		mutate(&cursor)
		called := false
		fragment, err := ReadDocMarkdownPlanFragment(context.Background(), compiled.plan, cursor,
			func(context.Context, ContentHash) ([]byte, error) { called = true; return nil, ErrNotFound },
			func(context.Context, int, int) ([]byte, error) { called = true; return nil, ErrNotFound })
		if !errors.Is(err, ErrIntegrity) || called || !reflect.DeepEqual(fragment, DocMarkdownPlanFragment{}) {
			test.Fatal("invalid cursor read source", cursor, called, err)
		}
	}
	for _, mutate := range []func(*DocMarkdownPlanCursor){
		func(cursor *DocMarkdownPlanCursor) { cursor.Byte = 1 },
		func(cursor *DocMarkdownPlanCursor) { cursor.Operation = 1 },
		func(cursor *DocMarkdownPlanCursor) { cursor.TextBytes = 1 },
	} {
		cursor := initial
		mutate(&cursor)
		textRead := false
		fragment, err := ReadDocMarkdownPlanFragment(context.Background(), compiled.plan, cursor,
			func(_ context.Context, hash ContentHash) ([]byte, error) { return compiled.objects[hash], nil },
			func(context.Context, int, int) ([]byte, error) { textRead = true; return nil, ErrNotFound })
		if !errors.Is(err, ErrIntegrity) || textRead || !reflect.DeepEqual(fragment, DocMarkdownPlanFragment{}) {
			test.Fatal("inconsistent cursor read body", cursor, textRead, err)
		}
	}
}

func TestDocMarkdownPlanFragmentCancellationAndCorruptionReturnNoOutput(test *testing.T) {
	source := "**body**"
	compiled := compileMarkdownFixture(test, source, true)
	cursor, err := NewDocMarkdownPlanCursor(compiled.plan)
	if err != nil {
		test.Fatal(err)
	}
	for _, cancelAt := range []string{"metadata", "text"} {
		ctx, cancel := context.WithCancel(context.Background())
		fragment, err := ReadDocMarkdownPlanFragment(ctx, compiled.plan, cursor,
			func(_ context.Context, hash ContentHash) ([]byte, error) {
				if cancelAt == "metadata" {
					cancel()
				}
				return compiled.objects[hash], nil
			},
			func(_ context.Context, from, length int) ([]byte, error) {
				cancel()
				return []byte(source[from : from+length]), nil
			})
		cancel()
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(fragment, DocMarkdownPlanFragment{}) {
			test.Fatal("canceled replay exposed output", cancelAt, err)
		}
	}
	textRead := false
	fragment, err := ReadDocMarkdownPlanFragment(context.Background(), compiled.plan, cursor,
		func(context.Context, ContentHash) ([]byte, error) { return []byte(`{"corrupt":true}`), nil },
		func(context.Context, int, int) ([]byte, error) { textRead = true; return nil, ErrNotFound })
	if !errors.Is(err, ErrIntegrity) || textRead || !reflect.DeepEqual(fragment, DocMarkdownPlanFragment{}) {
		test.Fatal("corrupt metadata read body", textRead, err)
	}
}
