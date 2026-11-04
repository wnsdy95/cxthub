//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryCaptureTestKey struct {
	repo domain.ContentHash
	kind string
	hash domain.ContentHash
}

type memoryCaptureTestTransaction struct {
	pgx.Tx
	test      *testing.T
	objects   map[memoryCaptureTestKey][]byte
	queries   int
	copied    int
	beforeRow func()
	err       error
}

func (transaction *memoryCaptureTestTransaction) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	transaction.test.Helper()
	transaction.queries++
	if !strings.Contains(query, "octet_length(body.bytes)") || !strings.Contains(query, "CASE WHEN octet_length(body.bytes)<=$4 THEN body.bytes END") ||
		!strings.Contains(query, "owned.repo_id=$1 AND owned.kind=$2 AND owned.hash=$3") {
		transaction.test.Fatal("missing SQL byte/ownership fence", query)
	}
	if transaction.beforeRow != nil {
		transaction.beforeRow()
	}
	if err := ctx.Err(); err != nil {
		return memoryCaptureTestRow{err: err}
	}
	if transaction.err != nil {
		return memoryCaptureTestRow{err: transaction.err}
	}
	key := memoryCaptureTestKey{domain.ContentHash(arguments[0].(string)), arguments[1].(string), domain.ContentHash(arguments[2].(string))}
	body, exists := transaction.objects[key]
	if !exists {
		return memoryCaptureTestRow{err: pgx.ErrNoRows}
	}
	row := memoryCaptureTestRow{size: len(body)}
	if len(body) <= arguments[3].(int) {
		row.body = bytes.Clone(body)
		transaction.copied += len(body)
	}
	return row
}

type memoryCaptureTestRow struct {
	size int
	body []byte
	err  error
}

func (row memoryCaptureTestRow) Scan(destinations ...any) error {
	if row.err != nil {
		return row.err
	}
	*destinations[0].(*int) = row.size
	*destinations[1].(*[]byte) = row.body
	if len(destinations) == 4 {
		incarnation := "11111111-2222-3333-4444-555555555555"
		*destinations[2].(**string) = &incarnation
		*destinations[3].(*int64) = 0
	}
	return nil
}

func memoryCaptureTestJSON(test *testing.T, value any) []byte {
	test.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func memoryCaptureTestDigest() domain.MemoryDigest {
	return domain.MemoryDigest{
		SnapshotID: domain.HashContent([]byte("capture snapshot")),
		Summary:    "bounded <memory> & \"summary\"\n",
		KeyFacts:   []string{"fact"},
		OpenTasks:  []string{},
		Provider:   domain.ProviderCodex,
	}
}

func memoryCaptureTestFixture(test *testing.T, digest domain.MemoryDigest, chunked, compressed bool) (*PostgresStore, context.Context, *memoryCaptureTestTransaction, memoryCaptureTestKey) {
	test.Helper()
	storage := &PostgresStore{}
	transaction := &memoryCaptureTestTransaction{test: test, objects: make(map[memoryCaptureTestKey][]byte)}
	ctx := context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, readOnly: true})
	hash, err := domain.MemoryDigestHash(digest)
	if err != nil {
		test.Fatal(err)
	}
	key := memoryCaptureTestKey{domain.HashContent([]byte("capture repo")), "memory", hash}
	root := memoryCaptureTestJSON(test, digest)
	if chunked {
		plan, planned, err := domain.PlanMemoryChunks(digest)
		if err != nil || !planned {
			test.Fatalf("chunk fixture: planned=%v err=%v", planned, err)
		}
		root = memoryCaptureTestJSON(test, plan.Manifest)
		for component, body := range plan.Bodies {
			if compressed {
				body = docCompress(body)
			}
			transaction.objects[memoryCaptureTestKey{key.repo, "memory_chunk", component}] = body
		}
	}
	if compressed {
		root = docCompress(root)
	}
	transaction.objects[key] = root
	return storage, ctx, transaction, key
}

func memoryCaptureTestRead(test *testing.T, storage *PostgresStore, ctx context.Context, key memoryCaptureTestKey, limit int, expected error) domain.MemoryDigest {
	test.Helper()
	digest, err := storage.GetMemoryForCapture(ctx, key.repo, key.hash, limit)
	if !errors.Is(err, expected) {
		test.Fatalf("capture error=%v want=%v", err, expected)
	}
	if err != nil && !reflect.DeepEqual(digest, domain.MemoryDigest{}) {
		test.Fatalf("failed capture exposed partial digest: %+v", digest)
	}
	return digest
}

func TestMemoryCaptureRoundTrip(test *testing.T) {
	for _, format := range []string{"inline", "v1", "v2", "v3", "v4"} {
		for _, compressed := range []bool{false, true} {
			name := format + "/plain"
			if compressed {
				name = format + "/compressed"
			}
			test.Run(name, func(test *testing.T) {
				digest := memoryCaptureTestDigest()
				if format != "inline" {
					digest.Summary = strings.Repeat("summary-", 20<<10)
					digest.Fragments = []domain.MemoryFragment{{SourceSnapshot: digest.SnapshotID, Summary: strings.Repeat("fragment-", 10<<10)}}
				}
				if format == "v2" {
					digest.GraftCoverage = &domain.MemoryGraftCoverage{ProjectionVersion: domain.MemoryProjectionVersion, GraftParents: []domain.ContentHash{digest.SnapshotID}}
				}
				if format == "v3" {
					digest.PreviousMemoryHash = domain.HashContent([]byte("previous memory"))
				}
				if format == "v4" {
					digest.ClaimsVersion = domain.MemoryClaimsVersion
					digest.Fragments[0].Claims = []domain.MemoryClaim{{Kind: "decision", Text: "keep memory bounded"}}
				}
				storage, ctx, transaction, key := memoryCaptureTestFixture(test, digest, format != "inline", compressed)
				limit := 1 << 20
				if format == "inline" {
					limit = max(len(memoryCaptureTestJSON(test, digest)), len(transaction.objects[key]))
				}
				actual := memoryCaptureTestRead(test, storage, ctx, key, limit, nil)
				if !reflect.DeepEqual(actual, digest) || transaction.queries != len(transaction.objects) {
					test.Fatalf("roundtrip/unique reads: equal=%v queries=%d objects=%d", reflect.DeepEqual(actual, digest), transaction.queries, len(transaction.objects))
				}
			})
		}
	}
}

func memoryCapturePaddedComponents(test *testing.T) (domain.MemoryDigest, map[memoryCaptureTestKey][]byte, memoryCaptureTestKey, int) {
	test.Helper()
	digest := memoryCaptureTestDigest()
	digest.Summary = "ab"
	_, _, transaction, key := memoryCaptureTestFixture(test, digest, false, false)
	manifest := domain.MemoryChunkManifest{Format: domain.MemoryChunkFormatV1, SnapshotID: digest.SnapshotID, Provider: digest.Provider, KeyFacts: digest.KeyFacts, OpenTasks: digest.OpenTasks}
	maxStored := 0
	for _, summary := range []string{"a", "b"} {
		body := []byte(summary)
		component := domain.HashContent(body)
		padding := make([]byte, 608)
		binary.LittleEndian.PutUint32(padding, 0x184d2a50)
		binary.LittleEndian.PutUint32(padding[4:], uint32(len(padding)-8))
		stored := append(docCompress(body), padding...)
		decoded, err := boundedMemoryCaptureObject(context.Background(), stored, 1)
		if err != nil || !bytes.Equal(decoded, body) {
			test.Fatal("padded frame fixture is not a valid compressed component", err)
		}
		transaction.objects[memoryCaptureTestKey{key.repo, "memory_chunk", component}] = stored
		manifest.SummaryChunks = append(manifest.SummaryChunks, component)
		maxStored = max(maxStored, len(stored))
	}
	transaction.objects[key] = memoryCaptureTestJSON(test, manifest)
	return digest, transaction.objects, key, len(transaction.objects[key]) + maxStored + 1
}

func TestMemoryCaptureCumulativeStoredByteLimit(test *testing.T) {
	digest, objects, key, limit := memoryCapturePaddedComponents(test)
	storage := &PostgresStore{}
	transaction := &memoryCaptureTestTransaction{test: test, objects: objects}
	ctx := context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, readOnly: true})
	memoryCaptureTestRead(test, storage, ctx, key, limit, domain.ErrMemoryProjectionLimit)
	if transaction.queries != 3 || transaction.copied > limit {
		test.Fatalf("cumulative SQL payload limit was not enforced: queries=%d bytes=%d limit=%d", transaction.queries, transaction.copied, limit)
	}
	transaction.copied, transaction.queries = 0, 0
	if actual := memoryCaptureTestRead(test, storage, ctx, key, limit*2, nil); !reflect.DeepEqual(actual, digest) {
		test.Fatal("larger stored-byte budget changed logical memory")
	}
}

func TestMemoryCaptureRequiresOwnedReadOnlyTransaction(test *testing.T) {
	storage, ctx, transaction, key := memoryCaptureTestFixture(test, memoryCaptureTestDigest(), false, false)
	for name, bound := range map[string]context.Context{
		"absent":   context.Background(),
		"nil":      context.WithValue(ctx, repositoryTxKey{}, (*repositoryTx)(nil)),
		"write":    context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage}),
		"foreign":  context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: &PostgresStore{}, readOnly: true}),
		"unowned":  context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, readOnly: true}),
		"unbacked": context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{owner: storage, readOnly: true}),
	} {
		test.Run(name, func(test *testing.T) {
			memoryCaptureTestRead(test, storage, bound, key, 4096, domain.ErrConflict)
		})
	}
	for _, limit := range []int{-1, 0, maxMemoryCaptureBytes + 1} {
		memoryCaptureTestRead(test, storage, ctx, key, limit, domain.ErrValidation)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	memoryCaptureTestRead(test, storage, canceled, key, 4096, context.Canceled)
	if transaction.queries != 0 {
		test.Fatal("rejected capture reached database", transaction.queries)
	}
	memoryCaptureTestRead(test, storage, ctx, key, maxMemoryCaptureBytes, nil)
}

func TestMemoryCaptureMissingAndCorruptObjects(test *testing.T) {
	for _, scenario := range []string{"root-missing", "root-unowned", "root-wrong-kind", "component-missing", "component-unowned", "component-wrong-kind", "root-corrupt", "component-corrupt", "component-invalid-hash", "unsupported-manifest", "invalid-reference", "invalid-claims", "malformed-fragments"} {
		test.Run(scenario, func(test *testing.T) {
			digest := memoryCaptureTestDigest()
			digest.Summary = strings.Repeat("memory", 12<<10)
			storage, ctx, transaction, key := memoryCaptureTestFixture(test, digest, true, false)
			manifest, _, err := domain.ParseMemoryChunkManifest(transaction.objects[key])
			if err != nil {
				test.Fatal(err)
			}
			component := memoryCaptureTestKey{key.repo, "memory_chunk", manifest.SummaryChunks[0]}
			expected := domain.ErrIntegrity
			switch scenario {
			case "root-missing", "root-unowned", "root-wrong-kind":
				foreign := key
				if scenario == "root-unowned" {
					foreign.repo = domain.HashContent([]byte("foreign repository"))
				} else {
					foreign.kind = "doc"
				}
				transaction.objects[foreign] = transaction.objects[key]
				delete(transaction.objects, key)
				expected = domain.ErrNotFound
			case "component-missing", "component-unowned", "component-wrong-kind":
				foreign := component
				if scenario == "component-unowned" {
					foreign.repo = domain.HashContent([]byte("foreign repository"))
				} else {
					foreign.kind = "doc"
				}
				transaction.objects[foreign] = transaction.objects[component]
				delete(transaction.objects, component)
			case "root-corrupt":
				transaction.objects[key] = []byte(`{"summary":`)
			case "component-corrupt":
				transaction.objects[component] = []byte("corrupt")
			case "component-invalid-hash":
				manifest.SummaryChunks[0] = "invalid"
				transaction.objects[key] = memoryCaptureTestJSON(test, manifest)
			case "unsupported-manifest":
				manifest.Format = "cxt-memory-chunks-v99"
				transaction.objects[key] = memoryCaptureTestJSON(test, manifest)
			case "invalid-reference":
				manifest.SnapshotID = "invalid"
				transaction.objects[key] = memoryCaptureTestJSON(test, manifest)
			case "invalid-claims":
				manifest.Format = domain.MemoryChunkFormatV4
				manifest.ClaimsVersion = domain.MemoryClaimsVersion + 1
				transaction.objects[key] = memoryCaptureTestJSON(test, manifest)
			case "malformed-fragments":
				manifest.FragmentChunks = manifest.SummaryChunks
				transaction.objects[key] = memoryCaptureTestJSON(test, manifest)
			}
			memoryCaptureTestRead(test, storage, ctx, key, 1<<20, expected)
		})
	}
}

func TestMemoryCaptureByteLimits(test *testing.T) {
	for _, scenario := range []string{"stored-root", "stored-component", "decoded-root", "decoded-component", "repeated-summary", "repeated-fragments", "aggregate", "escaped-summary", "escaped-inline"} {
		test.Run(scenario, func(test *testing.T) {
			digest := memoryCaptureTestDigest()
			digest.Summary = strings.Repeat("memory", 12<<10)
			storage, ctx, transaction, key := memoryCaptureTestFixture(test, digest, true, false)
			manifest, _, err := domain.ParseMemoryChunkManifest(transaction.objects[key])
			if err != nil {
				test.Fatal(err)
			}
			component := memoryCaptureTestKey{key.repo, "memory_chunk", manifest.SummaryChunks[0]}
			limit := 4096
			switch scenario {
			case "stored-root":
				transaction.objects[key] = bytes.Repeat([]byte("x"), limit+1)
			case "stored-component":
				transaction.objects[component] = bytes.Repeat([]byte("x"), limit+1)
			case "decoded-root":
				transaction.objects[key] = docCompress(bytes.Repeat([]byte("x"), 4<<20))
			case "decoded-component":
				transaction.objects[component] = docCompress(bytes.Repeat([]byte("x"), 4<<20))
			case "repeated-summary", "repeated-fragments":
				component.hash = domain.HashContent(bytes.Repeat([]byte("x"), 2048))
				transaction.objects[component] = docCompress(bytes.Repeat([]byte("x"), 2048))
				manifest.SummaryChunks = []domain.ContentHash{component.hash, component.hash}
				if scenario == "repeated-fragments" {
					manifest.SummaryChunks = nil
					manifest.FragmentChunks = []domain.ContentHash{component.hash, component.hash}
				}
				transaction.objects[key] = memoryCaptureTestJSON(test, manifest)
			case "aggregate":
				limit = len(transaction.objects[key]) + len(digest.Summary) - 1
			case "escaped-summary":
				body := bytes.Repeat([]byte("<"), 2048)
				component.hash = domain.HashContent(body)
				transaction.objects[component] = body
				manifest.SummaryChunks = []domain.ContentHash{component.hash}
				transaction.objects[key] = memoryCaptureTestJSON(test, manifest)
			case "escaped-inline":
				digest.Summary = strings.Repeat("<", 2048)
				transaction.objects[key] = bytes.ReplaceAll(memoryCaptureTestJSON(test, digest), []byte(`\u003c`), []byte("<"))
			}
			memoryCaptureTestRead(test, storage, ctx, key, limit, domain.ErrMemoryProjectionLimit)
			if scenario == "stored-root" && transaction.copied != 0 {
				test.Fatal("oversized root copied before rejection", transaction.copied)
			}
			if scenario == "stored-component" && transaction.copied != len(transaction.objects[key]) {
				test.Fatal("oversized component copied before rejection", transaction.copied)
			}
		})
	}
}

func TestMemoryCaptureRecordLimits(test *testing.T) {
	for _, scenario := range []string{"inline", "manifest", "fragments"} {
		test.Run(scenario, func(test *testing.T) {
			digest := memoryCaptureTestDigest()
			storage, ctx, transaction, key := memoryCaptureTestFixture(test, digest, false, false)
			switch scenario {
			case "inline":
				digest.KeyFacts = make([]string, maxMemoryCaptureRecords+1)
				transaction.objects[key] = memoryCaptureTestJSON(test, digest)
			case "manifest", "fragments":
				body := []byte("x")
				if scenario == "fragments" {
					body = memoryCaptureTestJSON(test, make([]domain.MemoryFragment, maxMemoryCaptureRecords+1))
				}
				component := domain.HashContent(body)
				manifest := domain.MemoryChunkManifest{Format: domain.MemoryChunkFormatV1, SummaryChunks: []domain.ContentHash{component}}
				if scenario == "manifest" {
					manifest.SummaryChunks = make([]domain.ContentHash, maxMemoryCaptureRecords+1)
					for position := range manifest.SummaryChunks {
						manifest.SummaryChunks[position] = component
					}
				} else {
					manifest.SummaryChunks = nil
					manifest.FragmentChunks = []domain.ContentHash{component}
				}
				transaction.objects[key] = memoryCaptureTestJSON(test, manifest)
				transaction.objects[memoryCaptureTestKey{key.repo, "memory_chunk", component}] = body
			}
			memoryCaptureTestRead(test, storage, ctx, key, 1<<20, domain.ErrMemoryProjectionLimit)
			if scenario == "manifest" && transaction.queries != 1 {
				test.Fatal("oversized manifest fetched components", transaction.queries)
			}
		})
	}
}

func TestMemoryCaptureCancellationAndDatabaseErrors(test *testing.T) {
	digest := memoryCaptureTestDigest()
	digest.Summary = strings.Repeat("summary", 12<<10)
	storage, ctx, transaction, key := memoryCaptureTestFixture(test, digest, true, false)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	transaction.beforeRow = func() {
		if transaction.queries == 2 {
			cancel()
		}
	}
	memoryCaptureTestRead(test, storage, ctx, key, 1<<20, context.Canceled)
	storage, ctx, transaction, key = memoryCaptureTestFixture(test, digest, true, false)
	transaction.err = errors.New("database unavailable")
	memoryCaptureTestRead(test, storage, ctx, key, 1<<20, transaction.err)
}

func TestMemoryCaptureBoundedDecompression(test *testing.T) {
	for _, limit := range []int{-1, 0} {
		expected := domain.ErrValidation
		if limit == 0 {
			expected = domain.ErrMemoryProjectionLimit
		}
		for _, raw := range [][]byte{[]byte("a"), docCompress([]byte("a"))} {
			if _, err := boundedMemoryCaptureObject(context.Background(), raw, limit); !errors.Is(err, expected) {
				test.Fatal("nonpositive decode limit", limit, err)
			}
		}
	}
	for _, raw := range [][]byte{nil, docCompress(nil)} {
		if body, err := boundedMemoryCaptureObject(context.Background(), raw, 0); err != nil || len(body) != 0 {
			test.Fatal("zero-byte exact decode", err)
		}
	}
	for _, compressed := range []bool{false, true} {
		body := bytes.Repeat([]byte("a"), 64<<10)
		raw := body
		if compressed {
			raw = docCompress(body)
		}
		actual, err := boundedMemoryCaptureObject(context.Background(), raw, len(body))
		if err != nil || !bytes.Equal(actual, body) {
			test.Fatalf("exact decoded limit: compressed=%v err=%v", compressed, err)
		}
		if _, err := boundedMemoryCaptureObject(context.Background(), raw, len(body)-1); !errors.Is(err, domain.ErrMemoryProjectionLimit) {
			test.Fatal("decoded overflow", err)
		}
	}
	broken := docCompress(bytes.Repeat([]byte("memory"), 1024))
	broken[len(broken)-1] ^= 1
	if _, err := boundedMemoryCaptureObject(context.Background(), broken, 64<<10); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("corrupt compression was downgraded", err)
	}
	combined := append(docCompress([]byte("first")), docCompress([]byte("second"))...)
	if _, err := boundedMemoryCaptureObject(context.Background(), combined, 10); !errors.Is(err, domain.ErrMemoryProjectionLimit) {
		test.Fatal("concatenated frame expansion escaped budget", err)
	}
}

func TestMemoryCaptureDecoderWindowBound(test *testing.T) {
	body := []byte("small body with explicit decoder window")
	header := len(body)<<3 | 1
	for _, exponent := range []int{23, 29} {
		raw := append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0, byte((exponent - 10) << 3), byte(header), byte(header >> 8), byte(header >> 16)}, body...)
		actual, err := boundedMemoryCaptureObject(context.Background(), raw, maxMemoryCaptureBytes)
		if exponent == 23 {
			if err != nil || !bytes.Equal(actual, body) {
				test.Fatal("8 MiB decoder window rejected", err)
			}
		} else if !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("512 MiB decoder window accepted", err)
		}
	}
}

func TestMemoryCaptureRepeatedComponentZeroBudget(test *testing.T) {
	digest := memoryCaptureTestDigest()
	digest.Summary = "bodybody"
	storage, ctx, transaction, key := memoryCaptureTestFixture(test, digest, false, false)
	component := domain.HashContent([]byte("body"))
	manifest := domain.MemoryChunkManifest{
		Format: domain.MemoryChunkFormatV1, SnapshotID: digest.SnapshotID, Provider: digest.Provider,
		KeyFacts: digest.KeyFacts, OpenTasks: digest.OpenTasks, SummaryChunks: []domain.ContentHash{component, component},
	}
	transaction.objects[key] = memoryCaptureTestJSON(test, manifest)
	transaction.objects[memoryCaptureTestKey{key.repo, "memory_chunk", component}] = docCompress([]byte("body"))
	memoryCaptureTestRead(test, storage, ctx, key, len(transaction.objects[key])+1, domain.ErrMemoryProjectionLimit)
	if transaction.queries != 2 {
		test.Fatal("fixture did not exercise zero decoded component budget", transaction.queries)
	}
}

func TestMemoryCaptureRecordPreflight(test *testing.T) {
	for _, raw := range []string{`{"fragments":[`, `[{]`, `{"key_facts":["unfinished"`, `]`} {
		records := 0
		if err := domain.CountMemoryCaptureRecords(context.Background(), strings.NewReader(raw), &records); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatalf("invalid records %q: %v", raw, err)
		}
	}
	for _, count := range []int{maxMemoryCaptureRecords, maxMemoryCaptureRecords + 1} {
		records := 0
		err := domain.CountMemoryCaptureRecords(context.Background(), bytes.NewReader(memoryCaptureTestJSON(test, make([]string, count))), &records)
		if (err == nil) != (count == maxMemoryCaptureRecords) {
			test.Fatal("record boundary", count, err)
		}
	}
}

func TestMemoryCaptureExactEscapedWireLimit(test *testing.T) {
	digest := memoryCaptureTestDigest()
	digest.Summary = strings.Repeat("<>&\u2028\u2029\n\t\"\\", 200)
	storage, ctx, transaction, key := memoryCaptureTestFixture(test, digest, false, false)
	component := domain.HashContent([]byte(digest.Summary))
	manifest := domain.MemoryChunkManifest{
		Format: domain.MemoryChunkFormatV1, SnapshotID: digest.SnapshotID, Provider: digest.Provider,
		KeyFacts: digest.KeyFacts, OpenTasks: digest.OpenTasks, SummaryChunks: []domain.ContentHash{component},
	}
	transaction.objects[key] = memoryCaptureTestJSON(test, manifest)
	transaction.objects[memoryCaptureTestKey{key.repo, "memory_chunk", component}] = docCompress([]byte(digest.Summary))
	limit := len(memoryCaptureTestJSON(test, digest))
	if len(transaction.objects[key])+len(digest.Summary) >= limit {
		test.Fatal("fixture must isolate escaped wire expansion")
	}
	memoryCaptureTestRead(test, storage, ctx, key, limit-1, domain.ErrMemoryProjectionLimit)
	actual := memoryCaptureTestRead(test, storage, ctx, key, limit, nil)
	if !reflect.DeepEqual(actual, digest) {
		test.Fatal("exact wire boundary changed digest")
	}
}

func TestMemoryCapturePinsRequireOwnedReadOnlyTransaction(test *testing.T) {
	storage, ctx, transaction, key := memoryCaptureTestFixture(test, memoryCaptureTestDigest(), false, false)
	for _, bound := range []context.Context{
		context.Background(),
		context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage}),
		context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: &PostgresStore{}, readOnly: true}),
	} {
		prPins, gitPins, err := storage.ReadMemoryPinsForCapture(bound, key.repo)
		if !errors.Is(err, domain.ErrConflict) || prPins != nil || gitPins != nil {
			test.Fatal("pin capture escaped transaction", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := storage.ReadMemoryPinsForCapture(canceled, key.repo); !errors.Is(err, context.Canceled) {
		test.Fatal("pin capture ignored cancellation", err)
	}
}

func TestMemoryCapturePinWireSize(test *testing.T) {
	for _, includeSource := range []bool{false, true} {
		for _, includeMemory := range []bool{false, true} {
			if includeMemory && !includeSource {
				continue
			}
			pin := domain.PRContributionMemory{
				RepoID: domain.HashContent([]byte("repo")), ContributionID: strings.Repeat("a", 64),
				PublicationID: strings.Repeat("b", 32), MemoryPinned: true,
			}
			if includeSource {
				pin.MemorySource = domain.HashContent([]byte("source"))
			}
			if includeMemory {
				pin.MemoryHash = domain.HashContent([]byte("memory"))
			}
			pin = pin.WithID()
			if pin.Validate() != nil || domain.GitContributionMemory(pin).Validate() != nil {
				test.Fatal("invalid wire-size fixture")
			}
			if memoryCapturePinWireBytes(pin) != len(memoryCaptureTestJSON(test, pin)) || memoryCapturePinWireBytes(pin) != len(memoryCaptureTestJSON(test, domain.GitContributionMemory(pin))) {
				test.Fatal("pin wire-size estimator disagrees with canonical JSON")
			}
		}
	}
}

func TestMemoryCaptureRejectsEscapedExpansionBeforeAllocation(test *testing.T) {
	const summaryBytes = 1 << 20
	const wireLimit = 2 << 20
	const allocationLimit = 8 << 20
	digest := memoryCaptureTestDigest()
	digest.Summary = strings.Repeat("<", summaryBytes)
	storage, ctx, transaction, key := memoryCaptureTestFixture(test, digest, true, true)
	if len(transaction.objects) != 2 {
		test.Fatal("allocation fixture must deduplicate repeated components", len(transaction.objects))
	}
	measure := func(expand bool) testing.BenchmarkResult {
		return testing.Benchmark(func(bench *testing.B) {
			bench.ReportAllocs()
			for bench.Loop() {
				transaction.queries = 0
				transaction.copied = 0
				if expand {
					wire, err := json.Marshal(digest)
					if err != nil || len(wire) < 6*summaryBytes {
						bench.Fatalf("negative control did not materialize escaped JSON: bytes=%d err=%v", len(wire), err)
					}
				}
				actual, err := storage.GetMemoryForCapture(ctx, key.repo, key.hash, wireLimit)
				if !errors.Is(err, domain.ErrMemoryProjectionLimit) || actual.Summary != "" || transaction.queries != 2 {
					bench.Fatalf("escaped expansion rejection: error=%v queries=%d", err, transaction.queries)
				}
			}
		})
	}
	bounded := measure(false).AllocedBytesPerOp()
	expanded := measure(true).AllocedBytesPerOp()
	if bounded >= allocationLimit {
		test.Fatalf("adapter allocated expanded JSON: %d bytes/op, limit %d", bounded, allocationLimit)
	}
	if expanded <= allocationLimit || expanded-bounded < 6*summaryBytes {
		test.Fatalf("allocation ceiling does not distinguish full-marshal negative control: bounded=%d expanded=%d ceiling=%d", bounded, expanded, allocationLimit)
	}
	test.Logf("fake-pgx adapter allocation regression: bounded=%d bytes/op < ceiling=%d < full-marshal negative control=%d; 1 MiB decoded summary, 2 MiB wire budget", bounded, allocationLimit, expanded)
}
