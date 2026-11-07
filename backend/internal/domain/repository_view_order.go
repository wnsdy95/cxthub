package domain

import (
	"context"
	"fmt"
	"slices"
	"sort"
)

func OrderRepositoryViewMetadata(ctx context.Context, view RepositoryView) (RepositoryView, error) {
	if err := ctx.Err(); err != nil {
		return RepositoryView{}, err
	}
	view.Refs = slices.Clone(view.Refs)
	sort.Slice(view.Refs, func(left, right int) bool {
		if view.Refs[left].Kind == view.Refs[right].Kind {
			return view.Refs[left].Name < view.Refs[right].Name
		}
		return view.Refs[left].Kind < view.Refs[right].Kind
	})
	var err error
	view.History, err = OrderHistoryEvents(view.History)
	if err != nil {
		return RepositoryView{}, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	for index := range view.History {
		if err := ctx.Err(); err != nil {
			return RepositoryView{}, err
		}
		view.History[index].CreatedAt = view.History[index].CreatedAt.UTC()
	}
	view.Pending = slices.Clone(view.Pending)
	for index := range view.Pending {
		if err := ctx.Err(); err != nil {
			return RepositoryView{}, err
		}
		pending := &view.Pending[index]
		pending.UpdatedAt = pending.UpdatedAt.UTC()
		if pending.ActivityAt != nil {
			activity := pending.ActivityAt.UTC()
			pending.ActivityAt = &activity
		}
	}
	sort.Slice(view.Pending, func(left, right int) bool {
		return view.Pending[left].SessionID < view.Pending[right].SessionID
	})
	view.Unsync = slices.Clone(view.Unsync)
	for index := range view.Unsync {
		if err := ctx.Err(); err != nil {
			return RepositoryView{}, err
		}
		view.Unsync[index].UpdatedAt = view.Unsync[index].UpdatedAt.UTC()
	}
	sort.Slice(view.Unsync, func(left, right int) bool {
		if !view.Unsync[left].UpdatedAt.Equal(view.Unsync[right].UpdatedAt) {
			return view.Unsync[left].UpdatedAt.After(view.Unsync[right].UpdatedAt)
		}
		if view.Unsync[left].User != view.Unsync[right].User {
			return view.Unsync[left].User < view.Unsync[right].User
		}
		return view.Unsync[left].Branch < view.Unsync[right].Branch
	})
	view.Reflog = slices.Clone(view.Reflog)
	for index := range view.Reflog {
		if err := ctx.Err(); err != nil {
			return RepositoryView{}, err
		}
		view.Reflog[index].CreatedAt = view.Reflog[index].CreatedAt.UTC()
	}
	return view, ctx.Err()
}
