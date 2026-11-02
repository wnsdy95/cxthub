package inbound

import "context"

type DocumentPublicationCommands interface {
	Commit(context.Context, CommitInput) (CommitOutput, error)
	StoreChunks(context.Context, StoreChunksInput) (StoreChunksOutput, error)
}
