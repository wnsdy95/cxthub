package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
)

type graphCacheReadState struct {
	*store.FSStore
	readOnly bool
}

func (state *graphCacheReadState) InReadOnlyTransaction(context.Context) bool {
	return state.readOnly
}
