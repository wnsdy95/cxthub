package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type catalogReadKey struct{}
type catalogTestStore struct {
	outbound.MetadataStore
	calls, reads int
	got          domain.CatalogRequest
	repo         domain.ContentHash
	page         domain.CatalogPage
	readErr      error
}

func (s *catalogTestStore) WithinReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	s.reads++
	if err := fn(context.WithValue(ctx, catalogReadKey{}, true)); err != nil {
		return err
	}
	return s.readErr
}
func (s *catalogTestStore) WithinRepository(context.Context, domain.ContentHash, func(context.Context) error) error {
	panic("catalog must not open a repository write transaction")
}
func (s *catalogTestStore) CatalogChanges(ctx context.Context, repo domain.ContentHash, r domain.CatalogRequest) (domain.CatalogPage, error) {
	if ctx.Value(catalogReadKey{}) != true {
		panic("catalog lost read snapshot context")
	}
	s.calls++
	s.repo, s.got = repo, r
	return s.page, nil
}

func TestCatalogServiceDelegatesInsideReadSnapshot(t *testing.T) {
	repo := domain.HashContent([]byte("catalog app"))
	page := domain.CatalogPage{Version: 1, RepoID: repo, Through: 8, NextCursor: "opaque", Entries: []domain.CatalogEntry{{Sequence: 8, Kind: "ref", Key: `["tag","raw"]`}}}
	st := &catalogTestStore{page: page}
	s := NewService(st, nil, nil, nil, nil)
	for _, limit := range []int{0, 1, 1000} {
		r := domain.CatalogRequest{Version: 1, Cursor: "adapter-owned", Limit: limit}
		got, err := s.CatalogChanges(context.Background(), repo, r)
		if err != nil || !reflect.DeepEqual(got, page) {
			t.Fatalf("page=%+v err=%v", got, err)
		}
		wantLimit := limit
		if limit == 0 {
			wantLimit = 256
		}
		if st.got.Cursor != r.Cursor || st.got.Limit != wantLimit || st.repo != repo {
			t.Fatalf("request changed: %+v", st.got)
		}
	}
	if st.calls != 3 || st.reads != 3 {
		t.Fatalf("calls=%d reads=%d", st.calls, st.reads)
	}
	st.readErr = errors.New("read transaction failed")
	got, err := s.CatalogChanges(context.Background(), repo, domain.CatalogRequest{Version: 1})
	if !errors.Is(err, st.readErr) || !reflect.DeepEqual(got, domain.CatalogPage{}) {
		t.Fatalf("failed transaction leaked progress: %+v %v", got, err)
	}
}

func TestCatalogServiceValidationCancellationAndUnsupported(t *testing.T) {
	st := &catalogTestStore{}
	s := NewService(st, nil, nil, nil, nil)
	repo := domain.HashContent([]byte("catalog app"))
	if _, err := s.CatalogChanges(context.Background(), "bad", domain.CatalogRequest{Version: 1}); !errors.Is(err, domain.ErrValidation) {
		t.Fatal(err)
	}
	if _, err := s.CatalogChanges(context.Background(), repo, domain.CatalogRequest{Version: 2}); !errors.Is(err, domain.ErrValidation) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.CatalogChanges(ctx, repo, domain.CatalogRequest{Version: 1}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if st.calls != 0 || st.reads != 0 {
		t.Fatal("invalid/canceled request reached store")
	}
	fs := store.NewFSStore(t.TempDir())
	s = NewService(fs, fs, nil, nil, nil)
	if _, err := s.CatalogChanges(context.Background(), repo, domain.CatalogRequest{Version: 1}); !errors.Is(err, domain.ErrCatalogUnsupported) {
		t.Fatalf("FS=%v", err)
	}
}

func (s *catalogTestStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	if ctx.Value(catalogReadKey{}) != true {
		panic("compatibility read escaped snapshot")
	}
	return domain.Repo{ID: repo}, nil
}
