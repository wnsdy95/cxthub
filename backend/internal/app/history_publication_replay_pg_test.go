//go:build postgres

package app

import (
	"context"
)

type historyPublicationReplayProbePG struct {
	*historyDocumentLockProbe
	afterRead func() error
}

func (probe *historyPublicationReplayProbePG) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if err := probe.PostgresStore.WithinReadSnapshot(ctx, read); err != nil {
		return err
	}
	if probe.afterRead != nil {
		return probe.afterRead()
	}
	return nil
}
