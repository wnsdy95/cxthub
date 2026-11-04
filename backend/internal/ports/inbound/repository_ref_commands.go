package inbound

import "context"

type RepositoryRefCommands interface {
	UpdateRef(context.Context, UpdateRefInput) (UpdateRefOutput, error)
	UpdateRefs(context.Context, UpdateRefsInput) (UpdateRefsOutput, error)
}
