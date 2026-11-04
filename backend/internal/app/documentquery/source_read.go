package documentquery

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type Source struct {
	ctx    context.Context
	source docViewSource
}

func (queries *Queries) OpenSource(ctx context.Context, repo, hash domain.ContentHash) (Source, error) {
	return queries.openSource(ctx, repo, hash, true)
}

func (queries *Queries) OpenViewSource(ctx context.Context, repo, hash domain.ContentHash) (Source, error) {
	return queries.openSource(ctx, repo, hash, false)
}

func (queries *Queries) openSource(ctx context.Context, repo, hash domain.ContentHash, legacyFirst bool) (Source, error) {
	if ctx == nil {
		return Source{}, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return Source{}, err
	}
	if queries == nil || absent(queries.scope) || absent(queries.metadata) || absent(queries.legacy) {
		return Source{}, domain.ErrValidation
	}
	if err := validateHashes(repo, hash); err != nil {
		return Source{}, err
	}
	caller := ctx
	bound, err := queries.scope.AuthorizeDocumentRead(queries.readContext(ctx), repo)
	if err != nil {
		return Source{}, err
	}
	if bound == nil {
		return Source{}, domain.ErrValidation
	}
	if authorize := inbound.DocumentSnapshotReadAuthorizer(bound); authorize != nil {
		bound, err = authorize(bound, repo)
		if err != nil {
			return Source{}, err
		}
		if bound == nil {
			return Source{}, domain.ErrValidation
		}
	}
	bound = queries.readContext(bound)
	if err := bound.Err(); err != nil {
		return Source{}, err
	}
	snapshot, err := queries.metadata.GetSnapshot(bound, repo, hash)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return Source{}, err
	}
	var observed *domain.Snapshot
	if err == nil {
		observed = &snapshot
	}
	var source docViewSource
	if !legacyFirst {
		source, err = queries.docViewSnapshotSource(bound, repo, hash, observed, false)
	} else {
		var legacy bool
		source, legacy, err = queries.legacySnapshotGenerationSource(bound, repo, hash, observed)
		if err == nil && !legacy {
			source, err = queries.docViewSnapshotSource(bound, repo, hash, observed, false)
		}
	}
	if err != nil {
		return Source{}, err
	}
	if err := caller.Err(); err != nil {
		return Source{}, err
	}
	return Source{ctx: caller, source: source}, nil
}

func (source Source) Binding() domain.DocReadGenerationBinding {
	return source.source.binding
}

func (source Source) Envelope() domain.CIREnvelope {
	return source.source.envelope
}

func (source Source) Total() int {
	return source.source.total
}

func (source Source) Root() bool {
	return source.source.root
}

func (source Source) StreamBytes() int64 {
	return source.source.streamBytes
}

func (source Source) Entry(position int) (domain.DocRangeEntry, error) {
	if err := source.check(); err != nil {
		return domain.DocRangeEntry{}, err
	}
	if source.source.entry == nil || position < 0 || position >= source.source.total {
		return domain.DocRangeEntry{}, domain.ErrValidation
	}
	return source.source.entry(position)
}

func (source Source) Location(position int) (domain.DocEventIndex, error) {
	if err := source.check(); err != nil {
		return domain.DocEventIndex{}, err
	}
	if source.source.location == nil || position < 0 || position >= source.source.total {
		return domain.DocEventIndex{}, domain.ErrValidation
	}
	return source.source.location(position)
}

func (source Source) Read(offset, length int) ([]byte, error) {
	if err := source.check(); err != nil {
		return nil, err
	}
	if source.source.read == nil || offset < 0 || length <= 0 {
		return nil, domain.ErrValidation
	}
	return source.source.read(offset, length)
}

func (source Source) check() error {
	if source.ctx == nil {
		return domain.ErrValidation
	}
	return source.ctx.Err()
}
