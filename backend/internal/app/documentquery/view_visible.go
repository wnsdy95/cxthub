package documentquery

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (queries *Queries) prepareDocVisible(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	_, err := queries.prepareDocVisibleSource(ctx, repo, hash, docRangeRootOnly)
	return true, err
}

func (queries *Queries) prepareDocVisibleSource(ctx context.Context, repo, hash domain.ContentHash, requirement docRangeRequirement) (domain.DocumentIdentity, error) {
	ctx = queries.readContext(ctx)
	var needs bool
	legacy := false
	identity := domain.DocumentIdentityRootV1
	bytes, byteEnabled := queries.docBytePreparation()
	requireBytes := false
	err := queries.interactiveViewWork(ctx, func(admitted context.Context) error {
		var err error
		admitted, err = queries.authorizeVisibleRead(admitted, repo)
		if err != nil {
			return err
		}
		needs, err = repositoryReadForRepo(admitted, queries, repo, func(read context.Context) (bool, error) {
			snapshot, err := queries.metadata.GetSnapshot(read, repo, hash)
			if err != nil {
				return false, err
			}
			if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
				return false, err
			}
			identity = snapshot.DocIdentity
			legacy = requirement != docRangeRootOnly && snapshot.DocIdentity == domain.DocumentIdentityLegacy
			if legacy {
				requireBytes = requirement == docRangeLegacyBytes && byteEnabled
				if requireBytes {
					ready, err := bytes.HasDocByteRangeIndex(read, repo, hash)
					return !ready, err
				}
				storage := queries.view.RangeReadiness
				if storage == nil {
					return false, domain.ErrDocVisibleProjectionUnsupported
				}
				ready, err := storage.HasDocRangeIndex(read, repo, hash)
				return !ready, err
			}
			if err := queries.checkVisibleRootSnapshot(hash, snapshot); err != nil {
				return false, err
			}
			storage := queries.visibleReadiness()
			if storage == nil {
				return false, domain.ErrDocVisibleProjectionUnsupported
			}
			ready, err := storage.HasDocVisibleIndex(read, repo, hash)
			return !ready, err
		})
		return err
	})
	if err != nil || !needs {
		return identity, err
	}
	if outbound.DocReadOnly(ctx) {
		return identity, domain.ErrDocRangeIndexUnavailable
	}
	return identity, queries.interactiveViewWork(ctx, func(admitted context.Context) error {
		admitted, err := queries.scope.AuthorizeDocumentPreparation(admitted, repo)
		if err != nil {
			return err
		}
		var job domain.DocRangeIndexJob
		if legacy {
			storage := queries.view.RangeJobs
			if storage == nil {
				return domain.ErrDocRangeIndexUnavailable
			}
			if requireBytes {
				preparation := queries.docByteDemand()
				if preparation == nil {
					return domain.ErrDocRangeIndexUnavailable
				}
				job, err = preparation.EnqueueDocByteRangeJob(admitted, repo, hash, time.Now().UTC())
			} else {
				job, err = storage.EnqueueDocRangeJob(admitted, repo, hash, time.Now().UTC())
			}
		} else {
			storage := queries.view.VisiblePreparation
			if storage == nil {
				return domain.ErrDocRangeIndexUnavailable
			}
			job, err = storage.EnqueueDocVisibleIndex(admitted, repo, hash, time.Now().UTC())
		}
		if err != nil {
			return err
		}
		return docRangePreparationError(job)
	})
}

func visiblePageSelection(repo, hash domain.ContentHash, request domain.DocVisiblePageRequest) (domain.DocVisibleSelector, bool, error) {
	if validateHashes(repo, hash) != nil {
		return domain.DocVisibleSelector{}, false, domain.ErrValidation
	}
	if request.Cursor != "" || request.Expand != "" {
		if request.Cursor != "" && request.Expand != "" || request.Base != "" || request.View != "" || request.Limit != 0 || request.Offset != 0 && request.Offset != -1 {
			return domain.DocVisibleSelector{}, false, domain.ErrValidation
		}
		token, purpose := request.Cursor, "units"
		if request.Expand != "" {
			token, purpose = request.Expand, "fold"
		}
		selector, err := domain.DecodeDocVisibleSelector(token)
		if err != nil || selector.Repo != repo || selector.Hash != hash || selector.Purpose != purpose {
			return domain.DocVisibleSelector{}, false, domain.ErrValidation
		}
		return selector, true, nil
	}
	if domain.ValidateOptionalContentHash(request.Base) != nil || request.View != "all" && request.View != "chat" && request.View != "prompts" ||
		request.Offset < -1 || request.Offset > domain.MaxConversationDocumentBytes || request.Limit < 0 || request.Limit > 100 {
		return domain.DocVisibleSelector{}, false, domain.ErrValidation
	}
	limit := request.Limit
	if limit == 0 {
		limit = 20
	}
	return domain.DocVisibleSelector{Version: 1, Purpose: "units", Repo: repo, Hash: hash, Base: request.Base, View: request.View, Start: request.Offset, Limit: limit}, false, nil
}

func (queries *Queries) ReadDocVisiblePage(ctx context.Context, repo, hash domain.ContentHash, request domain.DocVisiblePageRequest) (domain.DocVisiblePage, error) {
	ctx = queries.readContext(ctx)
	selection, continuation, err := visiblePageSelection(repo, hash, request)
	if err != nil {
		return domain.DocVisiblePage{}, err
	}
	return preparedDocVisibleRead(ctx, queries, repo, hash, selection.Base, func() (domain.DocVisiblePage, error) {
		return queries.readDocVisiblePage(ctx, selection, continuation)
	})
}

func (queries *Queries) readDocVisiblePage(ctx context.Context, selection domain.DocVisibleSelector, continuation bool) (response domain.DocVisiblePage, resultErr error) {
	ctx, release, err := queries.acquireViewInteractive(ctx)
	if err != nil {
		return domain.DocVisiblePage{}, err
	}
	defer func() { finishDocumentResponse(ctx, release, resultErr) }()
	ctx, err = queries.authorizeVisibleRead(ctx, selection.Repo)
	if err != nil {
		return domain.DocVisiblePage{}, err
	}
	return repositoryReadForRepo(ctx, queries, selection.Repo, func(read context.Context) (domain.DocVisiblePage, error) {
		var empty domain.DocVisiblePage
		opened, err := queries.openDocVisibleSelection(read, selection, continuation)
		if err != nil {
			return empty, err
		}
		selection, source, units := opened.selection, opened.source, opened.units
		generation := units.Descriptor()
		binding, inherited := selection.Generation, selection.Inherited
		if !continuation {
			selection.Next, selection.Unit = selection.Start, 0
			if selection.Start < source.total {
				_, selection.Unit, err = units.Unit(read, selection.View, selection.Start, true)
				if err != nil {
					return empty, err
				}
			}
		}
		replayCursor, err := selection.Encode()
		if err != nil {
			return empty, err
		}
		page := domain.DocVisiblePage{Projection: domain.DocVisibleProjection, Kind: selection.Purpose,
			Hash: selection.Hash, Base: selection.Base, Generation: binding, Rules: generation.Algorithm, View: selection.View,
			Envelope: source.envelope, Total: source.total, Inherited: inherited, Start: selection.Start,
			From: selection.Next, To: selection.Next, Units: []domain.DocVisiblePageUnit{}, Events: []domain.DocVisibleEvent{}, ReplayCursor: replayCursor}
		if selection.Start == source.total {
			return finishVisiblePage(read, page)
		}
		if selection.Unit >= generation.Views[selection.View].Units {
			return empty, domain.ErrValidation
		}
		header, err := json.Marshal(page)
		if err != nil {
			return empty, err
		}
		budget := docViewBytes - len(header) - 4096
		if budget <= 4096 {
			return empty, domain.ErrDocumentResourceLimit
		}
		if selection.Purpose == "fold" {
			err = fillVisibleFold(read, &page, selection, units, source, budget)
		} else {
			err = fillVisibleUnits(read, &page, selection, units, source, budget)
		}
		if err != nil {
			return empty, err
		}
		return finishVisiblePage(read, page)
	})
}

func finishVisiblePage(ctx context.Context, page domain.DocVisiblePage) (domain.DocVisiblePage, error) {
	if err := ctx.Err(); err != nil {
		return domain.DocVisiblePage{}, err
	}
	raw, err := json.Marshal(page)
	if err != nil {
		return domain.DocVisiblePage{}, err
	}
	if len(raw) > docViewBytes {
		return domain.DocVisiblePage{}, domain.ErrDocumentResourceLimit
	}
	if err := ctx.Err(); err != nil {
		return domain.DocVisiblePage{}, err
	}
	return page, nil
}

func clippedVisibleUnit(ctx context.Context, units outbound.DocVisibleGenerationRead, source docViewSource, selection domain.DocVisibleSelector) (domain.DocVisibleUnit, error) {
	unit, ordinal, err := units.Unit(ctx, selection.View, selection.Unit, false)
	if err != nil {
		return domain.DocVisibleUnit{}, err
	}
	if ordinal != selection.Unit || unit.Validate(source.total, selection.View) != nil {
		return domain.DocVisibleUnit{}, domain.ErrIntegrity
	}
	if unit.To <= selection.Start {
		return domain.DocVisibleUnit{}, domain.ErrValidation
	}
	if unit.From < selection.Start {
		entry, err := source.entry(selection.Start)
		if err != nil {
			return domain.DocVisibleUnit{}, err
		}
		return unit.Clip(selection.Start, entry.RenderedBefore, source.total, selection.View)
	}
	return unit, nil
}

func visibleFoldRange(unit domain.DocVisibleUnit) domain.DocVisibleFoldRange {
	span := domain.DocVisibleFoldRange{From: unit.From, To: unit.To}
	if unit.EventIndex == span.From {
		span.From++
	} else if unit.EventIndex == span.To-1 {
		span.To--
	}
	return span
}

func visibleEvent(source docViewSource, position, budget int) (domain.DocVisibleEvent, error) {
	entry, err := source.entry(position)
	if err != nil {
		return domain.DocVisibleEvent{}, err
	}
	deferred := entry.Length > min(docViewBytes, max(0, budget-2048)) || entry.Kind == domain.EventToolCall || entry.Kind == domain.EventToolResult ||
		entry.Kind == domain.EventReasoning || entry.CompactSummary || entry.SeedSummary
	event := domain.CIREvent{Kind: entry.Kind, Role: domain.Role(entry.Role), Seq: entry.Seq, ToolName: entry.ToolName, CompactSummary: entry.CompactSummary}
	if !deferred {
		raw, err := source.read(entry.Offset, entry.Length)
		if err != nil {
			return domain.DocVisibleEvent{}, err
		}
		event, err = domain.DecodeIndexedEvent(raw, entry.DocEventIndex)
		if err != nil {
			return domain.DocVisibleEvent{}, err
		}
	}
	return domain.DocVisibleEvent{DocViewEvent: domain.DocViewEvent{Event: event, Index: position, Bytes: entry.Length, Deferred: deferred}, SeedSummary: entry.SeedSummary, EmptyReasoning: entry.EmptyReasoning}, nil
}

func fillVisibleUnits(ctx context.Context, page *domain.DocVisiblePage, selection domain.DocVisibleSelector, units outbound.DocVisibleGenerationRead, source docViewSource, budget int) error {
	count := units.Descriptor().Views[selection.View].Units
	for selection.Unit < count && len(page.Units) < selection.Limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		unit, err := clippedVisibleUnit(ctx, units, source, selection)
		if err != nil {
			return err
		}
		if unit.From != page.To {
			return domain.ErrValidation
		}
		shown := domain.DocVisiblePageUnit{From: unit.From, To: unit.To, TurnIndex: unit.TurnIndex}
		if unit.EventIndex >= 0 {
			event, err := visibleEvent(source, unit.EventIndex, budget-2048)
			if err != nil {
				return err
			}
			prompt := event.Event.Kind == domain.EventMessage && event.Event.Role == domain.RoleUser && !event.Event.CompactSummary && !event.SeedSummary
			if event.EmptyReasoning || selection.View == "chat" && event.Event.Kind != domain.EventMessage || selection.View == "prompts" && !prompt || unit.TurnIndex == unit.From && !prompt {
				return domain.ErrIntegrity
			}
			shown.Event = &event
		}
		fold := visibleFoldRange(unit)
		if fold.From < fold.To {
			expand := selection
			expand.Purpose, expand.Next = "fold", fold.From
			cursor, err := expand.Encode()
			if err != nil {
				return err
			}
			shown.Fold = &domain.DocVisibleFold{From: fold.From, To: fold.To, Rows: unit.FoldRows, Cursor: cursor}
		}
		raw, err := json.Marshal(shown)
		if err != nil {
			return err
		}
		if len(raw)+1 > budget {
			if len(page.Units) == 0 {
				return domain.ErrDocumentResourceLimit
			}
			break
		}
		page.Units = append(page.Units, shown)
		budget -= len(raw) + 1
		page.To, selection.Next = unit.To, unit.To
		selection.Unit++
	}
	if selection.Unit < count {
		cursor, err := selection.Encode()
		if err != nil {
			return err
		}
		page.NextCursor = &cursor
	} else if page.To != source.total {
		return domain.ErrIntegrity
	}
	return nil
}

func fillVisibleFold(ctx context.Context, page *domain.DocVisiblePage, selection domain.DocVisibleSelector, units outbound.DocVisibleGenerationRead, source docViewSource, budget int) error {
	unit, err := clippedVisibleUnit(ctx, units, source, selection)
	if err != nil {
		return err
	}
	fold := visibleFoldRange(unit)
	if selection.Next < fold.From || selection.Next >= fold.To {
		return domain.ErrValidation
	}
	page.Fold = &fold
	for page.To < fold.To && len(page.Events) < domain.DocVisiblePageUnits {
		if err := ctx.Err(); err != nil {
			return err
		}
		event, err := visibleEvent(source, page.To, budget)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if len(raw)+1 > budget {
			if len(page.Events) == 0 {
				return domain.ErrDocumentResourceLimit
			}
			break
		}
		page.Events = append(page.Events, event)
		budget -= len(raw) + 1
		page.To++
	}
	if page.To < fold.To {
		selection.Next = page.To
		cursor, err := selection.Encode()
		if err != nil {
			return err
		}
		page.NextCursor = &cursor
	}
	return nil
}
