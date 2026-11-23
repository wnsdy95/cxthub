//go:build postgres

package app

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docViewPGStore struct {
	*store.PostgresStore
	opens, pages, fallbackPages int
}

type docViewPGRead struct {
	outbound.DocRangeGenerationRead
	store *docViewPGStore
}

func (storage *docViewPGStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	storage.opens++
	read, err := storage.PostgresStore.OpenDocRangeGeneration(ctx, repo, hash)
	return &docViewPGRead{DocRangeGenerationRead: read, store: storage}, err
}

func (read *docViewPGRead) ReadPage(ctx context.Context, position int) ([]domain.DocRangeEntry, error) {
	read.store.pages++
	return read.DocRangeGenerationRead.ReadPage(ctx, position)
}

func (storage *docViewPGStore) DocRangePage(ctx context.Context, repo domain.ContentHash, header domain.DocRangeIndex, position int) ([]domain.DocRangeEntry, error) {
	storage.fallbackPages++
	return storage.PostgresStore.DocRangePage(ctx, repo, header, position)
}

type docViewPGFixture struct {
	service  *Service
	storage  *store.PostgresStore
	pool     *pgxpool.Pool
	ctx      context.Context
	repo     domain.ContentHash
	hash     domain.ContentHash
	cir      domain.CIRDocument
	manifest domain.ConversationManifest
}
