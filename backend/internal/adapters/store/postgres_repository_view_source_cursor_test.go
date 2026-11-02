//go:build postgres

package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestRepositoryViewSourceCursorValidation(t *testing.T) {
	valid := repositoryViewSourceCursor{
		Checkpoint: domain.RepositoryViewSourceCheckpoint{Version: 1, Scope: domain.RepositoryViewSourceScope, RepoID: domain.HashContent([]byte(t.Name())), Epoch: "12345678-1234-1234-1234-123456789abc", Sequence: 9},
		Floor:      2, Baseline: 3, Mode: "baseline", Sequence: 3, Kind: "pending", Key: "session",
	}
	for _, test := range []struct {
		name   string
		change func(*repositoryViewSourceCursor)
		want   error
	}{
		{"baseline", func(*repositoryViewSourceCursor) {}, nil},
		{"delta", func(value *repositoryViewSourceCursor) { value.Mode, value.After, value.Sequence = "delta", 3, 4 }, nil},
		{"bad_scope", func(value *repositoryViewSourceCursor) { value.Checkpoint.Scope = domain.CatalogScope }, domain.ErrCatalogResetRequired},
		{"future_floor", func(value *repositoryViewSourceCursor) { value.Floor = 10 }, domain.ErrValidation},
		{"future_baseline", func(value *repositoryViewSourceCursor) { value.Baseline = 10 }, domain.ErrValidation},
		{"future_sequence", func(value *repositoryViewSourceCursor) { value.Sequence = 10 }, domain.ErrValidation},
		{"baseline_after", func(value *repositoryViewSourceCursor) { value.After = 1 }, domain.ErrValidation},
		{"before_coverage", func(value *repositoryViewSourceCursor) { value.Mode, value.After = "delta", 2 }, domain.ErrValidation},
		{"nonprogress_delta", func(value *repositoryViewSourceCursor) { value.Mode, value.After = "delta", 3 }, domain.ErrValidation},
		{"secret_kind", func(value *repositoryViewSourceCursor) { value.Kind = "secrets" }, domain.ErrValidation},
		{"empty_key", func(value *repositoryViewSourceCursor) { value.Key = "" }, domain.ErrValidation},
	} {
		t.Run(test.name, func(t *testing.T) {
			cursor := valid
			test.change(&cursor)
			raw, err := json.Marshal(cursor)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeRepositoryViewSourceCursor(base64.RawURLEncoding.EncodeToString(raw))
			if !errors.Is(err, test.want) || (err == nil && got != cursor) {
				t.Fatalf("got %+v %v want %+v %v", got, err, cursor, test.want)
			}
		})
	}
	raw, _ := json.Marshal(valid)
	for _, malformed := range []string{
		"!", strings.Repeat("a", 16<<10+1),
		base64.RawURLEncoding.EncodeToString(append([]byte(" "), raw...)),
		base64.RawURLEncoding.EncodeToString(append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"unknown":true}`)...)),
		base64.RawURLEncoding.EncodeToString(append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"key":"session"}`)...)),
	} {
		if _, err := decodeRepositoryViewSourceCursor(malformed); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("noncanonical cursor accepted: %v", err)
		}
	}
}

func TestPGRepositoryViewSourceReadBudgets(t *testing.T) {
	store, ctx, repo := catalogPG(t)
	for index := 0; index < 7; index++ {
		key := string(rune('a' + index))
		catalogExecPG(t, ctx, store.pool, `INSERT INTO pending_contexts(repo_id,session_id,data) VALUES($1,$2,jsonb_build_object('branch',repeat('x',700000)))`, string(repo), key)
	}
	request := domain.RepositoryViewSourceRequest{Version: 1}
	seen := make(map[string]bool)
	pages := 0
	for {
		page, err := store.RepositoryViewSourceChanges(ctx, repo, request)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if pages > 10 {
			t.Fatal("source read did not progress")
		}
		bytesRead := 0
		for _, entry := range page.Entries {
			bytesRead += len(entry.Value)
			identity := entry.Kind + ":" + entry.Key
			if seen[identity] {
				t.Fatalf("repeated source key %s", identity)
			}
			seen[identity] = true
		}
		if bytesRead > domain.MaxRepositoryViewSourcePageBytes {
			t.Fatalf("page exceeded byte budget: %d", bytesRead)
		}
		if page.Checkpoint != nil {
			break
		}
		if page.NextCursor == "" || len(page.Entries) == 0 {
			t.Fatal("partial page made no progress")
		}
		request.Cursor = page.NextCursor
	}
	if pages < 2 {
		t.Fatal("byte budget failed to split metadata page")
	}
	catalogExecPG(t, ctx, store.pool, `UPDATE pending_contexts SET data=jsonb_build_object('branch',repeat('x',1100000)) WHERE repo_id=$1 AND session_id='a'`, string(repo))
	if _, err := store.RepositoryViewSourceChanges(ctx, repo, domain.RepositoryViewSourceRequest{Version: 1}); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		t.Fatalf("oversize metadata accepted: %v", err)
	}
}
