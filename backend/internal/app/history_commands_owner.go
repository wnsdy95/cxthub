package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type HistoryProfile string

const (
	HistoryCompatibility HistoryProfile = "compatibility"
	HistoryComplete      HistoryProfile = "complete"
)

type HistoryMetadata interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
	GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
}

type HistoryEventStore interface {
	ListHistoryEvents(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error)
	ApplyHistoryEvent(context.Context, domain.HistoryEvent) error
}

type HistoryMemoryReader interface {
	GetMemory(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error)
}

type HistoryDocumentSources interface {
	VerifySnapshotDocument(context.Context, domain.ContentHash, domain.Snapshot) error
}

type HistoryWriteScope interface {
	WithinHistoryWrite(context.Context, domain.ContentHash, func(context.Context) error) error
	WithinHistoryObservation(context.Context, domain.ContentHash, func(context.Context) error) error
	WithinHistoryRead(context.Context, domain.ContentHash, func(context.Context) error) error
}

type HistoryFollowups interface {
	NotifyRefUpdate(context.Context, domain.ContentHash, domain.Ref, bool, bool) error
	QueueHistoryGitScan(context.Context, domain.HistoryEvent) error
	WakePRSourceJobs(context.Context, domain.ContentHash, time.Time) error
}

type HistoryCommandsConfig struct {
	Profile             HistoryProfile
	Metadata            HistoryMetadata
	Events              HistoryEventStore
	Memories            HistoryMemoryReader
	Sources             HistoryDocumentSources
	Scope               HistoryWriteScope
	Followups           HistoryFollowups
	Preparation         outbound.HistoryDocumentProofStore
	SupportedIdentities []domain.DocumentIdentity
}

type HistoryCommands = historyRecorder

var _ inbound.HistoryCommands = (*HistoryCommands)(nil)

func NewHistoryCommands(config HistoryCommandsConfig) (*HistoryCommands, error) {
	if config.Profile != HistoryCompatibility && config.Profile != HistoryComplete {
		return nil, domain.ErrValidation
	}
	if !rootAdapterPresent(config.Metadata) || !rootAdapterPresent(config.Scope) {
		return nil, domain.ErrValidation
	}
	for _, dependency := range []any{config.Events, config.Memories, config.Sources, config.Followups, config.Preparation} {
		if dependency != nil && !rootAdapterPresent(dependency) {
			return nil, domain.ErrValidation
		}
	}
	if config.Preparation != nil && (config.Events == nil || config.Sources == nil) {
		return nil, domain.ErrCatalogUnsupported
	}
	if config.Profile == HistoryComplete {
		for _, dependency := range []any{config.Events, config.Memories, config.Sources, config.Followups} {
			if dependency == nil {
				return nil, domain.ErrCatalogUnsupported
			}
		}
	}
	return &historyRecorder{metadata: config.Metadata, events: config.Events, memories: config.Memories,
		sources: config.Sources, scope: config.Scope, followups: config.Followups, preparation: config.Preparation,
		supported: append([]domain.DocumentIdentity(nil), config.SupportedIdentities...), profile: config.Profile}, nil
}

func (commands *historyRecorder) RecordHistory(ctx context.Context, event domain.HistoryEvent) error {
	if commands == nil {
		return domain.ErrValidation
	}
	if commands.configurationError != nil {
		return commands.configurationError
	}
	return resourceWork(ctx, ResourceBulk, func(bound context.Context) error {
		return commands.recordHistoryWithDocumentPreparation(bound, event)
	})
}
