//go:build postgres

package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type readBlockBatchTxPG struct {
	pgx.Tx
	known     []domain.ContentHash
	execute   func(context.Context, string, ...any) (pgconn.CommandTag, error)
	locations [][]any
}

func (tx *readBlockBatchTxPG) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &readBlockBatchRowsPG{hashes: tx.known}, nil
}

func (tx *readBlockBatchTxPG) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return tx.execute(ctx, sql, args...)
}

func (tx *readBlockBatchTxPG) CopyFrom(ctx context.Context, _ pgx.Identifier, _ []string, source pgx.CopyFromSource) (int64, error) {
	for source.Next() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		values, err := source.Values()
		if err != nil {
			return 0, err
		}
		tx.locations = append(tx.locations, values)
	}
	return int64(len(tx.locations)), source.Err()
}

type readBlockBatchRowsPG struct {
	pgx.Rows
	hashes []domain.ContentHash
	index  int
}

func (rows *readBlockBatchRowsPG) Next() bool { return rows.index < len(rows.hashes) }
func (rows *readBlockBatchRowsPG) Close()     {}
func (rows *readBlockBatchRowsPG) Err() error { return nil }
func (rows *readBlockBatchRowsPG) Scan(dest ...any) error {
	*dest[0].(*domain.ContentHash) = rows.hashes[rows.index]
	rows.index++
	return nil
}

func checkReadBlockBatchPG(t *testing.T, last *string, args []any, want map[string]string) int {
	t.Helper()
	hashes, texts, lower := args[0].([]string), args[1].([]string), args[2].([]string)
	if len(hashes) == 0 || len(hashes) > readBlockSearchBatchRowsPG || len(texts) != len(hashes) || len(lower) != len(hashes) {
		t.Fatalf("invalid batch lengths: %d/%d/%d", len(hashes), len(texts), len(lower))
	}
	bytes := 0
	for index, hash := range hashes {
		if hash <= *last {
			t.Fatalf("global hash order violated: %q then %q", *last, hash)
		}
		*last = hash
		text, exists := want[hash]
		if !exists || texts[index] != text || lower[index] != strings.ToLower(text) {
			t.Fatalf("search text changed or duplicated for %s", hash)
		}
		delete(want, hash)
		bytes += len(hash) + len(texts[index]) + len(lower[index])
	}
	if bytes > readBlockSearchBatchBytesPG && len(hashes) != 1 {
		t.Fatalf("multi-entry batch exceeds byte bound: %d bytes / %d rows", bytes, len(hashes))
	}
	return bytes
}

func TestReadBlockSearchPGBatchBoundsAndGlobalOrder(t *testing.T) {
	for _, scenario := range []string{"bytes", "rows", "oversized-singleton", "empty"} {
		t.Run(scenario, func(t *testing.T) {
			var contents strings.Builder
			var entries []readBlockSearchEntryPG
			want := map[string]string{}
			count := 19
			if scenario == "rows" {
				count = readBlockSearchBatchRowsPG + 5
			}
			if scenario == "empty" {
				count = 0
			}
			for index := 0; index < count; index++ {
				text := ""
				if scenario == "bytes" {
					text = strings.Repeat("MiXeD \u023a\u0130\ud55c\\\n", 18000)
				}
				if scenario == "oversized-singleton" && index == 8 {
					text = strings.Repeat("M", readBlockSearchBatchBytesPG)
				}
				hash := string(domain.HashContent([]byte(fmt.Sprintf("entry %d", index))))
				entries = append(entries, readBlockSearchEntryPG{hash: hash, offset: int64(contents.Len()), length: int64(len(text)), checksum: sha256.Sum256([]byte(text))})
				contents.WriteString(text)
				want[hash] = text
			}
			last, calls := "", 0
			tx := &readBlockBatchTxPG{execute: func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
				if !strings.Contains(sql, "ORDER BY hash ON CONFLICT DO NOTHING") {
					t.Fatal("search insert lost SQL lock ordering")
				}
				checkReadBlockBatchPG(t, &last, args, want)
				calls++
				return pgconn.NewCommandTag("INSERT 0 1"), nil
			}}
			if err := putReadBlockSearchPG(context.Background(), tx, strings.NewReader(contents.String()), entries); err != nil {
				t.Fatal(err)
			}
			if len(want) != 0 || (count > 0 && calls < 2) || (count == 0 && calls != 0) {
				t.Fatalf("unpublished=%d batches=%d", len(want), calls)
			}
		})
	}
}

func readBlockBatchDocPG(t *testing.T, marker string, interleave bool) domain.VerifiedSessionDoc {
	t.Helper()
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: marker}}
	for index := 0; index < 260; index++ {
		text := fmt.Sprintf("%s shared %d ", marker, index) + strings.Repeat("MiXeD \u023a\ud55c 50%_\\\n", 800)
		event := domain.CIREvent{Seq: index * 2, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: text}, {Type: "text", Text: "second block"}}}
		if index%13 == 0 {
			event = domain.CIREvent{Seq: index * 2, Kind: domain.EventReasoning, RedactedSummary: text}
		}
		if index%17 == 0 {
			event = domain.CIREvent{Seq: index * 2, Kind: domain.EventToolResult, CallID: "call", Output: text}
		}
		cir.Events = append(cir.Events, event)
		if interleave {
			cir.Events = append(cir.Events, domain.CIREvent{Seq: index*2 + 1, Kind: domain.EventMessage, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: marker + " interleaved"}}})
		}
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestReadBlockEventsPGSpoolsTextPreservesMetadataAndReuse(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("TMPDIR", directory)
	doc := readBlockBatchDocPG(t, t.Name(), true)
	plan, err := doc.PlanReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	blocks := plan.Blocks()
	blocks = append(blocks, blocks[0])
	var locations [][]any
	want := map[string]string{}
	for _, block := range blocks {
		index, err := block.Build(nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range index.Events {
			want[string(event.Hash)] = event.Text
			locations = append(locations, []any{string(block.Hash), event.Index, int64(event.Offset), event.Length, string(event.Hash), event.Seq, event.Role})
		}
	}
	reused := plan.EventHashes()[23]
	delete(want, string(reused))
	last, calls := "", 0
	tx := &readBlockBatchTxPG{known: []domain.ContentHash{reused}, execute: func(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
		checkReadBlockBatchPG(t, &last, args, want)
		files, err := os.ReadDir(directory)
		if err != nil || len(files) != 1 {
			t.Fatalf("expected one spool: %v %v", files, err)
		}
		info, err := files[0].Info()
		if err != nil || info.Mode().Perm() != 0600 || info.Size() < readBlockSearchBatchBytesPG/2 {
			t.Fatalf("spool permissions or size: %v %v", info, err)
		}
		calls++
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}}
	if err := putReadBlockEventsPG(context.Background(), tx, blocks); err != nil {
		t.Fatal(err)
	}
	if len(want) != 0 || calls < 2 || !reflect.DeepEqual(tx.locations, locations) {
		t.Fatalf("publication mismatch: missing=%d batches=%d locations=%d/%d", len(want), calls, len(tx.locations), len(locations))
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 0 {
		t.Fatalf("spool survived success: %v %v", files, err)
	}
}

func TestReadBlockEventsPGCancellationAndFailureCleanSpool(t *testing.T) {
	for _, scenario := range []string{"already-canceled", "between-batches", "insert-error"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("TMPDIR", directory)
			doc := readBlockBatchDocPG(t, scenario, false)
			plan, err := doc.PlanReadIndex()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(context.Canceled)
			if scenario == "insert-error" {
				want = errors.New("insert failed")
			}
			calls := 0
			tx := &readBlockBatchTxPG{execute: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
				calls++
				if scenario == "insert-error" {
					return pgconn.CommandTag{}, want
				}
				cancel()
				return pgconn.NewCommandTag("INSERT 0 1"), nil
			}}
			if scenario == "already-canceled" {
				cancel()
			}
			if err := putReadBlockEventsPG(ctx, tx, plan.Blocks()); !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
			if calls > 1 || len(tx.locations) != 0 {
				t.Fatalf("writes continued after failure: batches=%d locations=%d", calls, len(tx.locations))
			}
			files, err := os.ReadDir(directory)
			if err != nil || len(files) != 0 {
				t.Fatalf("spool survived failure: %v %v", files, err)
			}
		})
	}
}

func TestReadBlockEventsPGRejectsSameLengthSpoolMutation(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("TMPDIR", directory)
	doc := readBlockBatchDocPG(t, t.Name(), false)
	plan, err := doc.PlanReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[domain.ContentHash]bool{}
	var target domain.ContentHash
	var offset, targetOffset int64
	for _, block := range plan.Blocks() {
		if err := block.VisitContext(context.Background(), seen, func(event domain.DocEventIndex) error {
			if seen[event.Hash] {
				return nil
			}
			if len(event.Text) > 0 && event.Hash > target {
				target, targetOffset = event.Hash, offset
			}
			offset += int64(len(event.Text))
			seen[event.Hash] = true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	tx := &readBlockBatchTxPG{execute: func(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
		for _, hash := range args[0].([]string) {
			if hash == string(target) {
				t.Fatal("mutated spool text was inserted under the original event hash")
			}
		}
		calls++
		if calls == 1 {
			files, err := os.ReadDir(directory)
			if err != nil || len(files) != 1 {
				t.Fatalf("expected one search spool: %v %v", files, err)
			}
			spool, err := os.OpenFile(filepath.Join(directory, files[0].Name()), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer spool.Close()
			var body [1]byte
			if _, err := spool.ReadAt(body[:], targetOffset); err != nil {
				t.Fatal(err)
			}
			body[0] ^= 1
			if _, err := spool.WriteAt(body[:], targetOffset); err != nil {
				t.Fatal(err)
			}
		}
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}}
	if err := putReadBlockEventsPG(context.Background(), tx, plan.Blocks()); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("same-length search spool mutation: got %v want integrity error", err)
	}
	if calls == 0 || len(tx.locations) != 0 {
		t.Fatalf("unexpected publication: batches=%d locations=%d", calls, len(tx.locations))
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 0 {
		t.Fatalf("spool survived integrity failure: %v %v", files, err)
	}
}

type readBlockChecksumReaderAtPG struct {
	io.ReaderAt
	changeAt int64
	maxRead  int
	reads    int
}

func (reader *readBlockChecksumReaderAtPG) ReadAt(body []byte, offset int64) (int, error) {
	reader.maxRead = max(reader.maxRead, len(body))
	reader.reads++
	count, err := reader.ReaderAt.ReadAt(body, offset)
	if reader.changeAt >= offset && reader.changeAt < offset+int64(count) {
		body[reader.changeAt-offset] ^= 1
	}
	return count, err
}

func TestReadBlockSearchPGStreamsChecksumVerification(t *testing.T) {
	text := strings.Repeat("Search \u023a\ud55c ", 100000)
	prefix := "outside entry"
	entry := readBlockSearchEntryPG{hash: "event", offset: int64(len(prefix)), length: int64(len(text)), checksum: sha256.Sum256([]byte(text))}
	for _, changeAt := range []int64{-1, 0, int64(len(text) / 2), int64(len(text) - 1)} {
		t.Run(fmt.Sprintf("mutation_%d", changeAt), func(t *testing.T) {
			reader := &readBlockChecksumReaderAtPG{ReaderAt: strings.NewReader(prefix + text + "outside suffix"), changeAt: -1}
			if changeAt >= 0 {
				reader.changeAt = entry.offset + changeAt
			}
			calls := 0
			tx := &readBlockBatchTxPG{execute: func(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
				calls++
				if changeAt >= 0 || !reflect.DeepEqual(args[0], []string{entry.hash}) || !reflect.DeepEqual(args[1], []string{text}) || !reflect.DeepEqual(args[2], []string{strings.ToLower(text)}) {
					t.Fatal("unverified or incorrect text inserted")
				}
				return pgconn.NewCommandTag("INSERT 0 1"), nil
			}}
			err := putReadBlockSearchPG(context.Background(), tx, reader, []readBlockSearchEntryPG{entry})
			if changeAt >= 0 {
				if !errors.Is(err, domain.ErrIntegrity) || calls != 0 {
					t.Fatalf("mutated spool: err=%v inserts=%d", err, calls)
				}
			} else if err != nil || calls != 1 {
				t.Fatalf("valid spool: err=%v inserts=%d", err, calls)
			}
			if reader.maxRead > 32<<10 || reader.reads < 2 {
				t.Fatalf("checksum read was not bounded: max=%d reads=%d", reader.maxRead, reader.reads)
			}
		})
	}
}

func TestReadBlockSearchPGRejectsTruncatedSpool(t *testing.T) {
	tx := &readBlockBatchTxPG{execute: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
		t.Fatal("truncated search content inserted")
		return pgconn.CommandTag{}, nil
	}}
	entries := []readBlockSearchEntryPG{{hash: "event", length: 10}}
	if err := putReadBlockSearchPG(context.Background(), tx, strings.NewReader("short"), entries); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated spool: %v", err)
	}
}

type readBlockCancelReaderAtPG struct {
	io.ReaderAt
	cancel context.CancelFunc
}

func (reader readBlockCancelReaderAtPG) ReadAt(body []byte, offset int64) (int, error) {
	count, err := reader.ReaderAt.ReadAt(body, offset)
	reader.cancel()
	return count, err
}

func TestReadBlockSearchPGCancelsDuringSpoolRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	text := strings.Repeat("M", 128<<10)
	reader := readBlockCancelReaderAtPG{ReaderAt: strings.NewReader(text), cancel: cancel}
	tx := &readBlockBatchTxPG{execute: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
		t.Fatal("canceled spool read inserted text")
		return pgconn.CommandTag{}, nil
	}}
	entries := []readBlockSearchEntryPG{{hash: "event", length: int64(len(text)), checksum: sha256.Sum256([]byte(text))}}
	if err := putReadBlockSearchPG(ctx, tx, reader, entries); !errors.Is(err, context.Canceled) {
		t.Fatalf("spool cancellation: %v", err)
	}
}

func TestReadBlockEventsPGReusesAllTextWithoutInserting(t *testing.T) {
	doc := readBlockDoc(t, t.Name(), "tail", 257)
	plan, err := doc.PlanReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	tx := &readBlockBatchTxPG{known: plan.EventHashes(), execute: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
		t.Fatal("reused text was inserted again")
		return pgconn.CommandTag{}, nil
	}}
	if err := putReadBlockEventsPG(context.Background(), tx, plan.Blocks()); err != nil {
		t.Fatal(err)
	}
	if len(tx.locations) != plan.EventCount() {
		t.Fatalf("reused locations=%d want=%d", len(tx.locations), plan.EventCount())
	}
}

type readBlockConcurrentBatchTxPG struct {
	pgx.Tx
	ready chan<- struct{}
	start <-chan struct{}
	last  string
	calls int
}

func (tx *readBlockConcurrentBatchTxPG) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO doc_search_events_v2") {
		if tx.calls == 0 {
			tx.ready <- struct{}{}
			select {
			case <-ctx.Done():
				return pgconn.CommandTag{}, ctx.Err()
			case <-tx.start:
			}
		}
		tx.calls++
		for _, hash := range args[0].([]string) {
			if hash <= tx.last {
				return pgconn.CommandTag{}, fmt.Errorf("global search lock order reversed: %s -> %s", tx.last, hash)
			}
			tx.last = hash
		}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestPGReadBlockSearchBatchConcurrentPublication(t *testing.T) {
	store, repo := readBlockStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var groups [][]domain.DocReadBlock
	var hashes []domain.ContentHash
	wantText := map[string]string{}
	wantLocations := map[string][]any{}
	for _, interleave := range []bool{false, true} {
		doc := readBlockBatchDocPG(t, string(repo), interleave)
		plan, err := doc.PlanReadIndex()
		if err != nil {
			t.Fatal(err)
		}
		groups = append(groups, plan.Blocks())
		for _, block := range plan.Blocks() {
			hashes = append(hashes, block.Hash)
			index, err := block.Build(nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range index.Events {
				wantText[string(event.Hash)] = event.Text
				key := fmt.Sprintf("%s:%d", block.Hash, event.Index)
				wantLocations[key] = []any{event.Offset, event.Length, string(event.Hash), event.Seq, event.Role}
			}
		}
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := store.pool.Exec(cleanup, `DELETE FROM doc_read_blocks_v3 WHERE hash=ANY($1::text[])`, hashes); err != nil {
			t.Error(err)
		}
	})
	ready, start := make(chan struct{}, 2), make(chan struct{})
	errorsOut := make(chan error, 2)
	for _, blocks := range groups {
		go func() {
			transaction, err := store.pool.Begin(ctx)
			if err != nil {
				errorsOut <- err
				return
			}
			defer transaction.Rollback(context.Background())
			observed := &readBlockConcurrentBatchTxPG{Tx: transaction, ready: ready, start: start}
			if err = putPreparedReadBlocksPG(ctx, observed, blocks); err == nil && observed.calls < 2 {
				err = fmt.Errorf("fixture did not cross search batches: %d", observed.calls)
			}
			if err == nil {
				err = transaction.Commit(ctx)
			}
			errorsOut <- err
		}()
	}
	for range groups {
		select {
		case <-ready:
		case err := <-errorsOut:
			t.Fatalf("publication failed before search barrier: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(start)
	for range groups {
		if err := <-errorsOut; err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.pool.Query(ctx, `SELECT e.block_hash,e.ordinal,e.byte_offset,e.byte_length,e.event_hash,e.seq,e.role,s.search_text,s.search_lower FROM doc_read_block_events_v3 e JOIN doc_search_events_v2 s ON s.hash=e.event_hash WHERE e.block_hash=ANY($1::text[])`, hashes)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var block, hash, role, text, lower string
		var ordinal, offset, length, seq int
		if err := rows.Scan(&block, &ordinal, &offset, &length, &hash, &seq, &role, &text, &lower); err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("%s:%d", block, ordinal)
		if !reflect.DeepEqual(wantLocations[key], []any{offset, length, hash, seq, role}) || wantText[hash] != text || strings.ToLower(text) != lower {
			t.Fatalf("stored search/location mismatch at %s", key)
		}
		delete(wantLocations, key)
	}
	if err := rows.Err(); err != nil || len(wantLocations) != 0 {
		missing := make([]string, 0, len(wantLocations))
		for key := range wantLocations {
			missing = append(missing, key)
		}
		sort.Strings(missing)
		t.Fatalf("missing locations=%v err=%v", missing, err)
	}
}

type readBlockFailingBatchTxPG struct {
	pgx.Tx
	calls int
	err   error
}

func (tx *readBlockFailingBatchTxPG) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO doc_search_events_v2") {
		tx.calls++
		if tx.calls == 2 {
			return pgconn.CommandTag{}, tx.err
		}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestPGReadBlockSearchBatchRollback(t *testing.T) {
	store, repo := readBlockStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	doc := readBlockBatchDocPG(t, string(repo), false)
	plan, err := doc.PlanReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(context.Background())
	stop := errors.New("second search batch failed")
	observed := &readBlockFailingBatchTxPG{Tx: transaction, err: stop}
	if err := putPreparedReadBlocksPG(ctx, observed, plan.Blocks()); !errors.Is(err, stop) || observed.calls != 2 {
		t.Fatalf("expected partial search insertion failure, batches=%d err=%v", observed.calls, err)
	}
	if err := transaction.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var hashes []domain.ContentHash
	for _, block := range plan.Blocks() {
		hashes = append(hashes, block.Hash)
	}
	for table, values := range map[string][]domain.ContentHash{
		"doc_read_blocks_v3":   hashes,
		"doc_search_events_v2": plan.EventHashes(),
	} {
		var count int
		if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE hash=ANY($1::text[])`, values).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial publication survived rollback in %s: rows=%d err=%v", table, count, err)
		}
	}
}
