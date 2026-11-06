package http

import "github.com/wnsdy95/cxthub/backend/internal/domain"

type pendingStateResponse domain.PendingStateView

func newPendingStateResponse(view domain.PendingStateView) pendingStateResponse {
	response := pendingStateResponse(view)
	response.Refs = []domain.Ref{}
	for _, ref := range view.Refs {
		if ref.Kind == domain.RefBranch {
			response.Refs = append(response.Refs, ref)
		}
	}
	return response
}
