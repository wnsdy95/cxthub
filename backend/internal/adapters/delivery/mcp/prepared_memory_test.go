package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type preparedMemoryQueryFunc func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error)

func (query preparedMemoryQueryFunc) QueryPreparedMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
	return query(ctx, repo, request)
}

type scopedPreparedMemoryQuery struct {
	inbound.MemoryReadScope
	preparedMemoryQueryFunc
}

func preparedMemoryReader(scope inbound.MemoryReadScope, query preparedMemoryQueryFunc) inbound.PreparedMemoryReader {
	return scopedPreparedMemoryQuery{scope, query}
}

func requirePreparedMemoryReader(test *testing.T, server *Server, reader inbound.PreparedMemoryReader) {
	test.Helper()
	if err := server.SetPreparedMemory(reader); err != nil {
		test.Fatal(err)
	}
}

type preparedMemoryScopeFunc func(context.Context, domain.ContentHash, func(context.Context) error) error

func (scope preparedMemoryScopeFunc) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope(ctx, repo, read)
}

type preparedMemoryTestBackend struct {
	fakeContextBackend
	refs          []domain.Ref
	refReads      int
	snapshotReads int
	denyReads     bool
}

func (backend *preparedMemoryTestBackend) ListRefs(context.Context, domain.ContentHash) ([]domain.Ref, error) {
	backend.refReads++
	if backend.denyReads {
		return nil, errors.New("changed refs must not be resolved")
	}
	return backend.refs, nil
}

func (backend *preparedMemoryTestBackend) GetSnapshot(ctx context.Context, repo, snapshot domain.ContentHash) (domain.Snapshot, error) {
	backend.snapshotReads++
	if backend.denyReads {
		return domain.Snapshot{}, errors.New("changed snapshots must not be resolved")
	}
	return backend.fakeContextBackend.GetSnapshot(ctx, repo, snapshot)
}

type preparedMemoryTestPage struct {
	ReadVersion int                     `json:"read_version"`
	Format      string                  `json:"fragment_format"`
	Status      domain.MemoryReadStatus `json:"status"`
	Fragment    string                  `json:"json_fragment"`
	Next        string                  `json:"next_cursor"`
	Offset      int                     `json:"byte_offset"`
	Complete    bool                    `json:"item_complete"`
}

func preparedMemoryTestStatus(repo domain.ContentHash) domain.MemoryReadStatus {
	source := domain.RepositoryMemorySourceGeneration{
		Version: domain.RepositoryMemorySourceVersion, Epoch: "11111111-1111-4111-8111-111111111111", Sequence: 11,
		Graph: domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope,
			RepoID: repo, Epoch: "22222222-2222-4222-8222-222222222222", Sequence: 7},
	}
	return domain.MemoryReadStatus{Version: domain.MemoryReadVersion, Rules: domain.MemoryReadRules, Generation: pageHash(8),
		Minimum: source, Observed: source, Applied: source, Complete: true, Freshness: "current"}
}

func preparedMemoryTestSetup(branch string) (*preparedMemoryTestBackend, domain.Repo, toolArgs) {
	repo := domain.Repo{ID: pageHash(1), DefaultBranch: branch}
	snapshot := domain.Snapshot{ID: pageHash(2), RepoID: repo.ID, Branch: branch}
	backend := &preparedMemoryTestBackend{fakeContextBackend: fakeContextBackend{
		repos: []domain.Repo{repo}, snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {snapshot}}},
		refs: []domain.Ref{{Kind: domain.RefBranch, Name: branch, Target: snapshot.ID}}}
	version := 1
	return backend, repo, toolArgs{Repository: string(repo.ID), Mode: "effective", ReadVersion: &version, Ref: "HEAD", CodeCommit: strings.Repeat("a", 40)}
}

func preparedMemoryTestRead(test *testing.T, server *Server, repo domain.Repo, args toolArgs) preparedMemoryTestPage {
	test.Helper()
	raw, err := server.memoryPage(systemTestContext(), repo, args)
	if err != nil {
		test.Fatal(err)
	}
	if len(raw) > pageBytes || !utf8.ValidString(raw) {
		test.Fatalf("invalid transport page bytes=%d", len(raw))
	}
	var page preparedMemoryTestPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		test.Fatal(err)
	}
	if !utf8.ValidString(page.Fragment) || page.ReadVersion != 1 || page.Format != "memory-prepared-v1" {
		test.Fatal("invalid prepared page format")
	}
	return page
}

func TestPreparedMemorySchemaOptIn(test *testing.T) {
	for _, definition := range toolDefinitions() {
		schema := definition["inputSchema"].(map[string]any)
		properties := schema["properties"].(map[string]any)
		property, exists := properties["read_version"]
		if definition["name"] != "memory_load" {
			if exists {
				test.Fatal("read_version exposed on unrelated tool", definition["name"])
			}
			continue
		}
		if !exists || property.(map[string]any)["type"] != "integer" || !reflect.DeepEqual(property.(map[string]any)["enum"], []int{1}) {
			test.Fatal("prepared read schema missing explicit version")
		}
		for _, required := range schema["required"].([]string) {
			if required == "read_version" {
				test.Fatal("prepared reads must remain optional")
			}
		}
		if properties["cursor"].(map[string]any)["maxLength"] != maxPreparedMemoryTransportCursorBytes {
			test.Fatal("cursor schema does not match decoder")
		}
	}
}

func TestPreparedMemoryFragmentsReplayAndPinnedSelection(test *testing.T) {
	backend, repo, args := preparedMemoryTestSetup("main")
	status := preparedMemoryTestStatus(repo.ID)
	items := []domain.EffectiveMemoryItem{
		{ID: pageHash(3), SourceSnapshot: pageHash(2), Kind: "code", Text: strings.Repeat("한글🙂\n\"<>&", 2200)},
		{ID: pageHash(4), SourceSnapshot: pageHash(2), Kind: "rationale", Text: strings.Repeat("두 번째🙂", 1500)},
	}
	tokens := []string{"current-zero-" + strings.Repeat("a", 1800), "current-one-" + strings.Repeat("b", 1800)}
	initialReads, calls := 0, 0
	query := preparedMemoryQueryFunc(func(_ context.Context, selectedRepo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
		calls++
		if selectedRepo != repo.ID || request.Selection.Branch != "main" || request.Selection.SnapshotID != pageHash(2) || request.Limit != 1 {
			test.Fatal("selection changed", request)
		}
		index := 0
		switch request.Cursor {
		case "":
			initialReads++
		case tokens[0]:
		case tokens[1]:
			index = 1
		default:
			test.Fatal("inner cursor was not preserved")
		}
		page := domain.EffectiveMemoryPage{Selection: request.Selection, StateHash: pageHash(5), LineageHash: pageHash(6), Total: 2, Items: items[index : index+1]}
		if index == 0 {
			page.NextCursor = tokens[1]
		}
		return domain.MemoryReadPage{Cursor: tokens[index], Status: status, Page: page}, nil
	})
	var assembled strings.Builder
	var received []domain.EffectiveMemoryItem
	for ordinal := 0; ordinal < 250; ordinal++ {
		server := &Server{context: backend}
		requirePreparedMemoryReader(test, server, preparedMemoryReader(preparedMemoryScopeFunc(func(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
			return read(ctx)
		}), query))
		page := preparedMemoryTestRead(test, server, repo, args)
		if page.Status != status || page.Offset != assembled.Len() || page.Fragment == "" {
			test.Fatal("status/fragment changed", page.Offset, assembled.Len())
		}
		assembled.WriteString(page.Fragment)
		if page.Complete {
			var item domain.EffectiveMemoryItem
			if err := json.Unmarshal([]byte(assembled.String()), &item); err != nil {
				test.Fatal(err)
			}
			received = append(received, item)
			assembled.Reset()
		}
		if page.Next == "" {
			break
		}
		args.Cursor = page.Next
		cursor, err := cursorFor(string(repo.ID), "memory_load", args)
		if err != nil {
			test.Fatal(err)
		}
		if cursor.EffectiveCursor != tokens[len(received)] || cursor.ContextBranch != "main" {
			test.Fatal("fragment advanced before item completion")
		}
		backend.denyReads = true
		backend.refs = []domain.Ref{{Kind: domain.RefTag, Name: "main", Target: pageHash(7)}}
		repo.DefaultBranch = "changed-default"
		status.Observed.Sequence++
		status.Freshness = "catching_up"
	}
	if !reflect.DeepEqual(items, received) || initialReads != 1 || calls < 4 || backend.refReads != 2 || backend.snapshotReads != 1 {
		test.Fatalf("read drift or truncation: items=%d initial=%d calls=%d refs=%d snapshots=%d", len(received), initialReads, calls, backend.refReads, backend.snapshotReads)
	}
}

func TestPreparedMemoryCursorBounds(test *testing.T) {
	for _, size := range []int{800, 1500, 1800, domain.MaxPreparedMemoryCursorBytes} {
		test.Run(fmt.Sprint(size), func(test *testing.T) {
			backend, repo, args := preparedMemoryTestSetup(strings.Repeat("b", 1024))
			query := preparedMemoryQueryFunc(func(_ context.Context, _ domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
				return domain.MemoryReadPage{Cursor: strings.Repeat("a", size), Status: preparedMemoryTestStatus(repo.ID), Page: domain.EffectiveMemoryPage{
					Selection: request.Selection, StateHash: pageHash(5), LineageHash: pageHash(6), Total: 1,
					Items: []domain.EffectiveMemoryItem{{ID: pageHash(3), Text: strings.Repeat("한글\"", 5000)}}}}, nil
			})
			server := &Server{context: backend, preparedMemory: preparedMemoryReader(preparedMemoryScopeFunc(func(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
				return read(ctx)
			}), query)}
			page := preparedMemoryTestRead(test, server, repo, args)
			if page.Next == "" || len(page.Next) > maxPreparedMemoryTransportCursorBytes {
				test.Fatal("continuation exceeds decoder bound", len(page.Next))
			}
			test.Logf("inner=%d branch=1024 outer=%d limit=%d", size, len(page.Next), maxPreparedMemoryTransportCursorBytes)
			args.Cursor = page.Next
			preparedMemoryTestRead(test, server, repo, args)
			if size == domain.MaxPreparedMemoryCursorBytes && len(page.Next) <= 4096 {
				test.Fatal("worst-case nested cursor did not exercise legacy overflow")
			}
		})
	}
}

func TestPreparedMemoryRejectsInvalidArguments(test *testing.T) {
	for _, candidate := range []struct{ name, arguments string }{
		{"memory_load", `{"read_version":0,"mode":"effective"}`},
		{"memory_load", `{"read_version":2,"mode":"effective"}`},
		{"memory_load", `{"read_version":null,"mode":"effective"}`},
		{"memory_load", `{"READ_VERSION":null,"mode":"effective"}`},
		{"memory_load", `{"read_version":"1","mode":"effective"}`},
		{"memory_load", `{"read_version":1.5,"mode":"effective"}`},
		{"memory_load", `{"read_version":true,"mode":"effective"}`},
		{"memory_load", `{"read_version":1}`},
		{"memory_load", `{"read_version":1,"mode":"project"}`},
		{"memory_load", `{"read_version":1,"mode":"stored"}`},
		{"memory_load", `{"read_version":1,"mode":"effective"}`},
		{"repository_list", `{"read_version":1,"mode":"effective","repository":"repo"}`},
		{"repository_list", `{"READ_VERSION":1,"mode":"effective","repository":"repo"}`},
		{"context_fetch", `{"read_version":1,"mode":"effective","repository":"repo"}`},
		{"unknown", `{"read_version":1,"mode":"effective","repository":"repo"}`},
	} {
		test.Run(candidate.name+candidate.arguments, func(test *testing.T) {
			_, err := (&Server{}).runTool(context.Background(), domain.User{}, candidate.name, json.RawMessage(candidate.arguments))
			if !errors.Is(err, domain.ErrValidation) {
				test.Fatal("invalid opt-in accepted", err)
			}
		})
	}
}

func TestPreparedMemoryInvalidCursorsAndGenerationChanges(test *testing.T) {
	backend, repo, args := preparedMemoryTestSetup("main")
	status := preparedMemoryTestStatus(repo.ID)
	calls := 0
	query := preparedMemoryQueryFunc(func(_ context.Context, _ domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
		calls++
		return domain.MemoryReadPage{Cursor: "replay", Status: status, Page: domain.EffectiveMemoryPage{
			Selection: request.Selection, StateHash: pageHash(5), LineageHash: pageHash(6), Total: 1,
			Items: []domain.EffectiveMemoryItem{{ID: pageHash(3), Text: strings.Repeat("한글", 5000)}}}}, nil
	})
	server := &Server{context: backend, preparedMemory: preparedMemoryReader(preparedMemoryScopeFunc(func(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
		return read(ctx)
	}), query)}
	args.Cursor = preparedMemoryTestRead(test, server, repo, args).Next
	valid, err := cursorFor(string(repo.ID), "memory_load", args)
	if err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*pageCursor){
		func(cursor *pageCursor) { cursor.Version = 3 },
		func(cursor *pageCursor) { cursor.FragmentFormat = "memory-effective-v1" },
		func(cursor *pageCursor) { cursor.Snapshot = "" },
		func(cursor *pageCursor) { cursor.Projection = "" },
		func(cursor *pageCursor) { cursor.Generation = "" },
		func(cursor *pageCursor) { cursor.Index = 1 },
		func(cursor *pageCursor) { cursor.Offset = -1 },
		func(cursor *pageCursor) { cursor.EffectiveCursor = "" },
		func(cursor *pageCursor) {
			cursor.EffectiveCursor = strings.Repeat("a", domain.MaxPreparedMemoryCursorBytes+1)
		},
		func(cursor *pageCursor) { cursor.ContextBranch = "../bad" },
		func(cursor *pageCursor) { cursor.Repository = string(pageHash(7)) },
		func(cursor *pageCursor) { cursor.PreparedPending = true },
		func(cursor *pageCursor) {
			cursor.PreparedPending = true
			cursor.Projection = ""
			cursor.Generation = ""
		},
	} {
		bad := valid
		mutate(&bad)
		changed := args
		changed.Cursor = encodeCursor(bad)
		before := calls
		if _, err := server.memoryPage(systemTestContext(), repo, changed); !errors.Is(err, domain.ErrValidation) || calls != before {
			test.Fatal("invalid cursor reached query", err, bad)
		}
	}
	for _, mutate := range []func(*toolArgs){
		func(args *toolArgs) { args.CodeCommit = strings.Repeat("b", 40) },
		func(args *toolArgs) { args.MemoryHash = string(pageHash(9)) },
		func(args *toolArgs) { args.Ref = "other" },
		func(args *toolArgs) { args.Cursor = strings.Repeat("a", maxPreparedMemoryTransportCursorBytes+1) },
	} {
		changed := args
		mutate(&changed)
		before := calls
		if _, err := server.memoryPage(systemTestContext(), repo, changed); !errors.Is(err, domain.ErrValidation) || before != calls {
			test.Fatal("cursor filter mismatch accepted", err)
		}
	}
	legacy := args
	legacy.ReadVersion = nil
	if _, err := cursorFor(string(repo.ID), "memory_load", legacy); err == nil {
		test.Fatal("prepared cursor accepted without opt-in")
	}
	status.Generation = pageHash(9)
	if _, err := server.memoryPage(systemTestContext(), repo, args); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
		test.Fatal("mixed generation fragments", err)
	}
}

func TestPreparedMemoryPendingPreservesFloorAndSelection(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	version := 1
	args := toolArgs{ReadVersion: &version, Mode: "effective", CodeCommit: strings.Repeat("a", 40), Ref: "HEAD"}
	calls := 0
	var selected domain.EffectiveMemorySelection
	requirePreparedMemoryReader(test, fixture.server, preparedMemoryReader(fixture.backend, func(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
		calls++
		if actor, system := inbound.RepositoryActor(ctx); actor != fixture.reader.ID || system || inbound.DocumentReadAuthorizer(ctx) == nil {
			test.Fatal("prepared query bypassed admission")
		}
		if calls == 1 {
			selected = request.Selection
			if request.Cursor != "" {
				test.Fatal("initial request had a cursor")
			}
			return domain.MemoryReadPage{}, fmt.Errorf("private DB details: %w", &domain.MemoryReadPending{Cursor: "pinned-minimum"})
		}
		if request.Cursor != "pinned-minimum" || request.Selection != selected {
			test.Fatal("retry changed minimum/selection", request)
		}
		if calls == 2 {
			return domain.MemoryReadPage{}, &domain.MemoryReadPending{Cursor: "pinned-minimum"}
		}
		return domain.MemoryReadPage{Status: preparedMemoryTestStatus(repo), Page: domain.EffectiveMemoryPage{
			Selection: selected, StateHash: pageHash(5), LineageHash: pageHash(6)}}, nil
	}))
	for attempt := 0; attempt < 3; attempt++ {
		response := httptest.NewRecorder()
		fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), args))
		text, isError := memoryAdmissionResult(test, response)
		if attempt == 2 {
			if isError {
				test.Fatal(text)
			}
			break
		}
		var envelope struct {
			Result struct {
				Structured struct {
					Error preparedMemoryToolError `json:"error"`
				} `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			test.Fatal(err)
		}
		failure := envelope.Result.Structured.Error
		if !isError || failure.Code != "memory_read_not_ready" || !failure.Retryable || failure.RetryCursor == "" || strings.Contains(text, "private DB") {
			test.Fatal("pending error lost floor or exposed details", text)
		}
		args.Repository = string(fixture.repo.ID)
		args.Cursor = failure.RetryCursor
		cursor, err := cursorFor(string(fixture.repo.ID), "memory_load", args)
		if err != nil || !cursor.PreparedPending || cursor.Offset != 0 || cursor.Projection != "" || cursor.Generation != "" || cursor.EffectiveCursor != "pinned-minimum" {
			test.Fatal("invalid pending continuation", cursor, err)
		}
		changed := fixture.repo
		changed.DefaultBranch = "missing-default"
		fixture.backend.repo.Store(&changed)
	}
	if calls != 3 || fixture.backend.reads.Load() != 1 || fixture.observed.resolves.Load() < 6 {
		test.Fatal("retry skipped authorization or re-resolved snapshot", calls, fixture.backend.reads.Load(), fixture.observed.resolves.Load())
	}
}

func TestPreparedMemoryTypedSafeProtocolErrors(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	version := 1
	args := toolArgs{Mode: "effective", ReadVersion: &version, Ref: string(fixture.snapshot.ID), CodeCommit: strings.Repeat("a", 40)}
	for _, candidate := range []struct {
		cause     error
		code      string
		retryable bool
	}{
		{domain.ErrMemoryReadNotReady, "memory_read_not_ready", true},
		{domain.ErrMemoryReadNeedsAttention, "memory_read_needs_attention", false},
		{domain.ErrEffectiveMemoryCursorStale, "memory_cursor_stale", false},
		{domain.ErrMemoryCaptureExpired, "memory_capture_expired", false},
		{domain.ErrCatalogResetRequired, "catalog_reset_required", false},
		{domain.ErrConflict, "conflict", false},
		{domain.ErrResourceOverloaded, "resource_overloaded", true},
		{domain.ErrUnauthorized, "unauthenticated", false},
		{domain.ErrForbidden, "forbidden", false},
		{domain.ErrNotFound, "not_found", false},
		{domain.ErrBreakGlassAuditUnavailable, "audit_unavailable", false},
		{domain.ErrValidation, "validation", false},
		{domain.ErrIntegrity, "integrity_violation", false},
		{domain.ErrMemoryProjectionLimit, "memory_projection_limit", false},
		{domain.ErrCatalogUnsupported, "prepared_memory_unavailable", false},
		{context.DeadlineExceeded, "deadline_exceeded", true},
		{context.Canceled, "canceled", false},
		{errors.New("SELECT credentials FROM private_table"), "unavailable", false},
	} {
		test.Run(candidate.code+candidate.cause.Error(), func(test *testing.T) {
			requirePreparedMemoryReader(test, fixture.server, preparedMemoryReader(fixture.backend, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
				return domain.MemoryReadPage{}, fmt.Errorf("secret database DSN: %w", candidate.cause)
			}))
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), args))
			text, isError := memoryAdmissionResult(test, response)
			var payload struct {
				Error preparedMemoryToolError `json:"error"`
			}
			if err := json.Unmarshal([]byte(text), &payload); err != nil || !isError || payload.Error.Code != candidate.code || payload.Error.Retryable != candidate.retryable {
				test.Fatal("wrong protocol error", text, err)
			}
			var envelope struct {
				Result struct {
					Structured struct {
						Error preparedMemoryToolError `json:"error"`
					} `json:"structuredContent"`
				} `json:"result"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || !reflect.DeepEqual(payload.Error, envelope.Result.Structured.Error) {
				test.Fatal("structured error differs from text", err)
			}
			if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "private_table") || !strings.Contains(response.Body.String(), "structuredContent") {
				test.Fatal("unsafe or unstructured error", response.Body)
			}
			if !errors.Is(preparedMemoryFailure(candidate.cause), candidate.cause) {
				test.Fatal("typed error identity lost")
			}
		})
	}
}

func TestPreparedMemoryTerminalErrorsSuppressPending(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	version := 1
	args := toolArgs{Mode: "effective", ReadVersion: &version, Ref: string(fixture.snapshot.ID), CodeCommit: strings.Repeat("a", 40)}
	for _, candidate := range []struct {
		cause error
		code  string
	}{
		{domain.ErrUnauthorized, "unauthenticated"},
		{domain.ErrForbidden, "forbidden"},
		{domain.ErrBreakGlassAuditUnavailable, "audit_unavailable"},
		{domain.ErrIntegrity, "integrity_violation"},
		{domain.ErrMemoryReadNeedsAttention, "memory_read_needs_attention"},
		{domain.ErrEffectiveMemoryCursorStale, "memory_cursor_stale"},
		{domain.ErrMemoryCaptureExpired, "memory_capture_expired"},
		{domain.ErrCatalogResetRequired, "catalog_reset_required"},
		{domain.ErrCatalogUnsupported, "prepared_memory_unavailable"},
		{domain.ErrConflict, "conflict"},
		{context.Canceled, "canceled"},
		{domain.ErrValidation, "validation"},
		{domain.ErrNotFound, "not_found"},
		{domain.ErrMemoryProjectionLimit, "memory_projection_limit"},
	} {
		for _, classified := range []bool{false, true} {
			for _, reversed := range []bool{false, true} {
				test.Run(fmt.Sprintf("%s/classified=%t/reversed=%t", candidate.code, classified, reversed), func(test *testing.T) {
					var pending error = &domain.MemoryReadPending{Cursor: "must-not-leak"}
					if classified {
						failure := preparedMemoryFailure(pending).(*preparedMemoryToolError)
						failure.RetryCursor = "must-not-leak"
						pending = failure
					}
					joined := errors.Join(candidate.cause, pending)
					if reversed {
						joined = errors.Join(pending, candidate.cause)
					}
					requirePreparedMemoryReader(test, fixture.server, preparedMemoryReader(fixture.backend, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
						return domain.MemoryReadPage{}, fmt.Errorf("secret database details: %w", joined)
					}))
					response := httptest.NewRecorder()
					fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), args))
					text, isError := memoryAdmissionResult(test, response)
					var payload struct {
						Error preparedMemoryToolError `json:"error"`
					}
					if err := json.Unmarshal([]byte(text), &payload); err != nil || !isError || payload.Error.Code != candidate.code || payload.Error.Retryable || payload.Error.RetryCursor != "" {
						test.Fatal("terminal error became retryable or lost its safe code", text, err)
					}
					var envelope struct {
						Result struct {
							Structured struct {
								Error preparedMemoryToolError `json:"error"`
							} `json:"structuredContent"`
						} `json:"result"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || !reflect.DeepEqual(payload.Error, envelope.Result.Structured.Error) {
						test.Fatal("structured terminal error differs from text", err)
					}
					for _, forbidden := range []string{"retry_cursor", "must-not-leak", "secret", "json_fragment"} {
						if strings.Contains(response.Body.String(), forbidden) {
							test.Fatal("terminal error leaked retry state or internal details", response.Body)
						}
					}
					if !errors.Is(preparedMemoryFailure(joined), candidate.cause) {
						test.Fatal("terminal error identity lost")
					}
				})
			}
		}
	}
}

func TestPreparedMemoryExpiredPinAndDisabledOptIn(test *testing.T) {
	for _, name := range []string{"expired-pin", "disabled-opt-in"} {
		test.Run(name, func(test *testing.T) {
			fixture := newMemoryAdmissionFixture(test)
			version := 1
			args := toolArgs{Mode: "effective", ReadVersion: &version, Ref: string(fixture.snapshot.ID), CodeCommit: strings.Repeat("a", 40)}
			legacy := &effectiveQuery{scope: fixture.backend, repo: fixture.repo.ID, state: pageHash(3)}
			fixture.server.SetEffectiveMemory(legacy)
			reader, err := fixture.backend.Service.PreparedMemoryQueries()
			if err != nil {
				test.Fatal(err)
			}
			if err := fixture.server.SetPreparedMemory(reader); err != nil {
				test.Fatal(err)
			}
			wantCode := "prepared_memory_unavailable"
			if name == "expired-pin" {
				wantCode = "memory_capture_expired"
				requirePreparedMemoryReader(test, fixture.server, preparedMemoryReader(fixture.backend, func(_ context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
					if request.Cursor != "" {
						if request.Cursor != "pinned-generation" || request.Selection.SnapshotID != fixture.snapshot.ID {
							test.Fatal("expiry retry lost pinned selection", request)
						}
						return domain.MemoryReadPage{}, fmt.Errorf("secret database retention details: %w", domain.ErrMemoryCaptureExpired)
					}
					return domain.MemoryReadPage{Cursor: "pinned-generation", Status: preparedMemoryTestStatus(repo), Page: domain.EffectiveMemoryPage{
						Selection: request.Selection, StateHash: pageHash(5), LineageHash: pageHash(6), Total: 1,
						Items: []domain.EffectiveMemoryItem{{ID: pageHash(3), Text: strings.Repeat("retained memory", 2000)}}}}, nil
				}))
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), args))
				text, isError := memoryAdmissionResult(test, response)
				var page preparedMemoryTestPage
				if err := json.Unmarshal([]byte(text), &page); err != nil || isError || page.Complete || page.Next == "" {
					test.Fatal("expected pinned fragment continuation", text, err)
				}
				args.Cursor = page.Next
			}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), args))
			text, isError := memoryAdmissionResult(test, response)
			var payload struct {
				Error preparedMemoryToolError `json:"error"`
			}
			if err := json.Unmarshal([]byte(text), &payload); err != nil || !isError || payload.Error.Code != wantCode || payload.Error.Retryable || payload.Error.RetryCursor != "" {
				test.Fatal("expiry or disabled opt-in lost typed error", text, err)
			}
			if len(legacy.received) != 0 || strings.Contains(text, "secret") || strings.Contains(text, "json_fragment") {
				test.Fatal("failed prepared read leaked data or fell back", text)
			}
			if name == "expired-pin" && !strings.Contains(payload.Error.Message, "restart without cursor") {
				test.Fatal("expiry lacks safe restart guidance", text)
			}
		})
	}
}

func TestPreparedMemoryMissingNeverFallsBackAndLegacyUnchanged(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	version := 1
	args := toolArgs{Repository: string(fixture.repo.ID), Mode: "effective", ReadVersion: &version, Ref: string(fixture.snapshot.ID), CodeCommit: strings.Repeat("a", 40)}
	legacy := &effectiveQuery{scope: fixture.backend, repo: fixture.repo.ID, state: pageHash(3)}
	fixture.server.SetEffectiveMemory(legacy)
	raw, _ := json.Marshal(args)
	if _, err := fixture.server.runTool(context.Background(), fixture.reader, "memory_load", raw); !errors.Is(err, domain.ErrCatalogUnsupported) || len(legacy.received) != 0 {
		test.Fatal("missing prepared query fell back", err)
	}
	requirePreparedMemoryReader(test, fixture.server, preparedMemoryReader(fixture.backend, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
		test.Fatal("legacy read reached prepared query")
		return domain.MemoryReadPage{}, nil
	}))
	args.ReadVersion = nil
	raw, _ = json.Marshal(args)
	text, err := fixture.server.runTool(context.Background(), fixture.reader, "memory_load", raw)
	if err != nil || len(legacy.received) != 1 || strings.Contains(text, "read_version") || strings.Contains(text, "status") {
		test.Fatal("legacy effective mode changed", text, err)
	}
}

func TestPreparedMemoryRejectsInvalidServicePagesAndOffsets(test *testing.T) {
	backend, repo, args := preparedMemoryTestSetup("main")
	selection := domain.EffectiveMemorySelection{SnapshotID: pageHash(2), Branch: "main", CodeCommit: args.CodeCommit}
	valid := domain.MemoryReadPage{Cursor: "replay", Status: preparedMemoryTestStatus(repo.ID), Page: domain.EffectiveMemoryPage{
		Selection: selection, StateHash: pageHash(5), LineageHash: pageHash(6), Total: 1,
		Items: []domain.EffectiveMemoryItem{{ID: pageHash(3), Text: strings.Repeat("한글", 5000)}}}}
	for _, candidate := range []struct {
		name   string
		mutate func(*domain.MemoryReadPage)
	}{
		{"missing-replay", func(page *domain.MemoryReadPage) { page.Cursor = "" }},
		{"long-replay", func(page *domain.MemoryReadPage) {
			page.Cursor = strings.Repeat("a", domain.MaxPreparedMemoryCursorBytes+1)
		}},
		{"long-next", func(page *domain.MemoryReadPage) {
			page.Page.NextCursor = strings.Repeat("a", domain.MaxPreparedMemoryCursorBytes+1)
		}},
		{"wrong-selection", func(page *domain.MemoryReadPage) { page.Page.Selection.Branch = "other" }},
		{"invalid-status", func(page *domain.MemoryReadPage) { page.Status.Complete = false }},
		{"future-status", func(page *domain.MemoryReadPage) { page.Status.Version++ }},
		{"two-items", func(page *domain.MemoryReadPage) {
			page.Page.Items = append(page.Page.Items, domain.EffectiveMemoryItem{})
		}},
		{"empty-next", func(page *domain.MemoryReadPage) { page.Page.Items = nil; page.Page.NextCursor = "next" }},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			invalid := valid
			candidate.mutate(&invalid)
			server := &Server{context: backend, preparedMemory: preparedMemoryReader(preparedMemoryScopeFunc(func(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
				return read(ctx)
			}), func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
				return invalid, nil
			})}
			if raw, err := server.memoryPage(systemTestContext(), repo, args); !errors.Is(err, domain.ErrIntegrity) || raw != "" {
				test.Fatal("invalid service response escaped", err)
			}
		})
	}
	server := &Server{context: backend, preparedMemory: preparedMemoryReader(preparedMemoryScopeFunc(func(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
		return read(ctx)
	}), func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
		return valid, nil
	})}
	args.Cursor = preparedMemoryTestRead(test, server, repo, args).Next
	cursor, err := cursorFor(string(repo.ID), "memory_load", args)
	if err != nil {
		test.Fatal(err)
	}
	raw, _ := json.Marshal(valid.Page.Items[0])
	for _, offset := range []int{len(raw), len(raw) + 1, strings.Index(string(raw), "한글") + 1} {
		cursor.Offset = offset
		args.Cursor = encodeCursor(cursor)
		if text, err := server.memoryPage(systemTestContext(), repo, args); text != "" || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("invalid UTF-8 or oversized fragment offset accepted", offset, err)
		}
	}
}

func TestPreparedMemoryPendingCursorLimits(test *testing.T) {
	for _, size := range []int{0, domain.MaxPreparedMemoryCursorBytes, domain.MaxPreparedMemoryCursorBytes + 1} {
		test.Run(fmt.Sprint(size), func(test *testing.T) {
			backend, repo, args := preparedMemoryTestSetup("main")
			server := &Server{context: backend, preparedMemory: preparedMemoryReader(preparedMemoryScopeFunc(func(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
				return read(ctx)
			}), func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
				return domain.MemoryReadPage{}, &domain.MemoryReadPending{Cursor: strings.Repeat("a", size)}
			})}
			_, err := server.memoryPage(systemTestContext(), repo, args)
			if size == domain.MaxPreparedMemoryCursorBytes {
				var failure *preparedMemoryToolError
				if !errors.As(err, &failure) || failure.RetryCursor == "" || len(failure.RetryCursor) > maxPreparedMemoryTransportCursorBytes {
					test.Fatal("valid bounded retry cursor rejected", err)
				}
			} else if !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("invalid pending cursor emitted", err)
			}
		})
	}
}

func TestPreparedMemoryContinuationReauthorizes(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	version := 1
	args := toolArgs{Mode: "effective", ReadVersion: &version, Ref: string(fixture.snapshot.ID), CodeCommit: strings.Repeat("a", 40)}
	calls := 0
	requirePreparedMemoryReader(test, fixture.server, preparedMemoryReader(fixture.backend, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
		calls++
		return domain.MemoryReadPage{}, &domain.MemoryReadPending{Cursor: "pinned-floor"}
	}))
	response := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), args))
	text, isError := memoryAdmissionResult(test, response)
	var payload struct {
		Error preparedMemoryToolError `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil || !isError || payload.Error.RetryCursor == "" {
		test.Fatal("missing pending retry", err, text)
	}
	args.Cursor = payload.Error.RetryCursor
	if err := fixture.identity.RemoveMember(systemTestContext(), fixture.owner.ID, fixture.repository.ID, fixture.reader.ID); err != nil {
		test.Fatal(err)
	}
	response = httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), args))
	text, isError = memoryAdmissionResult(test, response)
	if !isError || calls != 1 || strings.Contains(text, "pinned-floor") {
		test.Fatal("continuation bypassed current authorization", text, calls)
	}
}
