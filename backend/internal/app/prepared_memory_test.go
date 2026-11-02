package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func preparedCursorFixture(test *testing.T) (domain.MemoryReadTarget, preparedMemoryCursor) {
	test.Helper()
	captured, request := memoryReadFixture(test, 2, 32)
	bundle := buildMemoryReadFixture(test, captured)
	target, err := memoryReadTarget(bundle.Header.Source.Graph.RepoID, request)
	if err != nil {
		test.Fatal(err)
	}
	identifier, err := domain.MemoryReadTargetID(context.Background(), target)
	if err != nil {
		test.Fatal(err)
	}
	return target, preparedMemoryCursor{Version: 1, Requirement: domain.MemoryReadRequirement{
		Version: 1, TargetID: identifier, Minimum: bundle.Header.Source}, Generation: bundle.ID}
}

func TestPreparedMemoryCursorReplayPendingAndValidation(test *testing.T) {
	target, original := preparedCursorFixture(test)
	for _, continuation := range []string{"", "bounded-inner-cursor"} {
		cursor := original
		cursor.Continuation = continuation
		encoded, err := encodePreparedMemoryCursor(cursor)
		if err != nil || len(encoded) > domain.MaxPreparedMemoryCursorBytes {
			test.Fatal(err)
		}
		decoded, err := decodePreparedMemoryCursor(encoded, target)
		if err != nil || decoded != cursor {
			test.Fatal("replay/continuation roundtrip", err)
		}
	}
	pending := original
	pending.Generation = ""
	encoded, err := encodePreparedMemoryCursor(pending)
	if err != nil {
		test.Fatal(err)
	}
	if actual, err := decodePreparedMemoryCursor(encoded, target); err != nil || actual != pending {
		test.Fatal("pending cursor lost fixed floor", err)
	}
	for _, scenario := range []string{"version", "missing-generation", "bad-generation", "inner-limit", "wrong-target", "wrong-epoch", "unknown-field", "duplicate-field", "whitespace", "oversize", "base64", "legacy"} {
		test.Run(scenario, func(test *testing.T) {
			cursor := original
			switch scenario {
			case "version":
				cursor.Version++
			case "missing-generation":
				cursor.Generation, cursor.Continuation = "", "page"
			case "bad-generation":
				cursor.Generation = "invalid"
			case "inner-limit":
				cursor.Continuation = strings.Repeat("a", 1025)
			case "wrong-target":
				cursor.Requirement.TargetID = domain.HashContent([]byte("foreign target"))
			case "wrong-epoch":
				cursor.Requirement.Minimum.Epoch = "invalid"
			}
			raw, err := json.Marshal(cursor)
			if err != nil {
				test.Fatal(err)
			}
			switch scenario {
			case "unknown-field":
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"unknown":true}`)
			case "duplicate-field":
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"version":1}`)
			case "whitespace":
				raw = append(raw, ' ')
			case "legacy":
				raw = []byte(`{"v":1,"index":1}`)
			}
			encoded := base64.RawURLEncoding.EncodeToString(raw)
			if scenario == "oversize" {
				encoded = strings.Repeat("a", domain.MaxPreparedMemoryCursorBytes+1)
			}
			if scenario == "base64" {
				encoded = "%"
			}
			if decoded, err := decodePreparedMemoryCursor(encoded, target); err == nil || decoded != (preparedMemoryCursor{}) {
				test.Fatal("bad cursor accepted", decoded, err)
			}
		})
	}
}

type pendingPreparedMemoryStore struct {
	outbound.MetadataStore
	requirement domain.MemoryReadRequirement
	observed    int
	read        int
	failure     error
}

func (storage *pendingPreparedMemoryStore) ObserveMemoryReadRequirement(ctx context.Context, _ domain.MemoryReadTarget, authorize func(context.Context) (context.Context, error)) (domain.MemoryReadRequirement, error) {
	storage.observed++
	if _, err := authorize(ctx); err != nil {
		return domain.MemoryReadRequirement{}, err
	}
	return storage.requirement, nil
}

func (storage *pendingPreparedMemoryStore) WithMinimumMemoryReadRange(ctx context.Context, _ domain.MemoryReadTarget, requirement domain.MemoryReadRequirement, _ domain.ContentHash, authorize func(context.Context) (context.Context, error), _ func(context.Context, domain.MemoryReadStatus, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	storage.read++
	if _, err := authorize(ctx); err != nil {
		return err
	}
	if requirement != storage.requirement {
		return domain.ErrIntegrity
	}
	if storage.failure != nil {
		return storage.failure
	}
	return domain.ErrMemoryReadNotReady
}

func TestPreparedMemoryPreservesNonPendingCauses(test *testing.T) {
	target, cursor := preparedCursorFixture(test)
	storage := &pendingPreparedMemoryStore{requirement: cursor.Requirement}
	service := &Service{meta: storage}
	if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{target.RepoID}); err != nil {
		test.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{Selection: target.Selection, Content: target.Content}
	pinned, err := encodePreparedMemoryCursor(cursor)
	if err != nil {
		test.Fatal(err)
	}
	for _, cause := range []error{domain.ErrForbidden, domain.ErrUnauthorized, domain.ErrIntegrity, domain.ErrCatalogUnsupported, domain.ErrMemoryReadNeedsAttention, domain.ErrMemoryCaptureExpired, domain.ErrCatalogResetRequired, domain.ErrValidation, context.Canceled, errors.New("unexpected storage failure")} {
		for _, encoded := range []string{"", pinned} {
			for _, failure := range []error{errors.Join(cause, domain.ErrMemoryReadNotReady), errors.Join(domain.ErrMemoryReadNotReady, cause)} {
				storage.failure = fmt.Errorf("wrapped read failure: %w", failure)
				request.Cursor = encoded
				page, err := service.QueryPreparedMemory(systemTestContext(), target.RepoID, request)
				var pending *domain.MemoryReadPending
				if !errors.Is(err, cause) || errors.As(err, &pending) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
					test.Fatal("non-pending cause was discarded", cause, err)
				}
			}
		}
	}
	for _, failure := range []error{domain.ErrMemoryReadNotReady, fmt.Errorf("wrapped pending: %w", domain.ErrMemoryReadNotReady), errors.Join(domain.ErrMemoryReadNotReady, domain.ErrMemoryReadNotReady)} {
		storage.failure = failure
		request.Cursor = ""
		_, err := service.QueryPreparedMemory(systemTestContext(), target.RepoID, request)
		var pending *domain.MemoryReadPending
		if !errors.As(err, &pending) || pending.Cursor == "" {
			test.Fatal("pure pending lost fixed-floor retry", err)
		}
	}
}

func TestPreparedMemoryRetriesKeepFloorWithoutReobservation(test *testing.T) {
	target, cursor := preparedCursorFixture(test)
	storage := &pendingPreparedMemoryStore{requirement: cursor.Requirement}
	service := &Service{meta: storage}
	request := domain.EffectiveMemoryRequest{Selection: target.Selection, Content: target.Content}
	if _, err := service.QueryPreparedMemory(systemTestContext(), target.RepoID, request); !errors.Is(err, domain.ErrCatalogUnsupported) || storage.observed != 0 {
		test.Fatal("reader was not default-off", err)
	}
	if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{target.RepoID}); err != nil {
		test.Fatal(err)
	}
	for range 4 {
		page, err := service.QueryPreparedMemory(systemTestContext(), target.RepoID, request)
		var pending *domain.MemoryReadPending
		if !errors.As(err, &pending) || !errors.Is(err, domain.ErrMemoryReadNotReady) || page.Cursor != "" || page.Status.Complete {
			test.Fatal("pending result lost typed cursor or leaked partial page", page, err)
		}
		request.Cursor = pending.Cursor
	}
	if storage.observed != 1 || storage.read != 4 {
		test.Fatal("retry advanced original minimum", storage.observed, storage.read)
	}
	if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{target.RepoID, "bad"}); err == nil || !service.preparedMemoryRepos[target.RepoID] {
		test.Fatal("invalid reconfiguration partially changed scope", err)
	}
	if err := service.ConfigurePreparedMemoryReads(nil); err != nil {
		test.Fatal(err)
	}
	if _, err := service.QueryPreparedMemory(systemTestContext(), target.RepoID, request); !errors.Is(err, domain.ErrCatalogUnsupported) || storage.read != 4 {
		test.Fatal("disabled reader still accessed store", err)
	}
	if err := (&Service{}).ConfigurePreparedMemoryReads([]domain.ContentHash{target.RepoID}); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("enabled without query port", err)
	}
}
