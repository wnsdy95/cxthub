package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type historyCommandsHTTPProbe struct {
	calls   []domain.HistoryEvent
	err     error
	observe func(context.Context)
}

func (commands *historyCommandsHTTPProbe) RecordHistory(ctx context.Context, event domain.HistoryEvent) error {
	commands.calls = append(commands.calls, event)
	if commands.observe != nil {
		commands.observe(ctx)
	}
	return commands.err
}

type historyCommandsHTTPBackend struct{ *repositoryRefHTTPBackend }

func (*historyCommandsHTTPBackend) RecordHistory(context.Context, domain.HistoryEvent) error {
	panic("history command reached the broad HTTP backend")
}
