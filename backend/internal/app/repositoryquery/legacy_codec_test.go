package repositoryquery

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphProjectionCancellationContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining atomic.Int32
}

func (ctx *graphProjectionCancellationContext) Err() error {
	if ctx.remaining.Add(-1) == 0 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestGraphProjectionEncodingCancellation(test *testing.T) {
	graph := domain.GraphState{Version: 1, ScopeLabels: map[string]string{"main": strings.Repeat("label", 1<<15)}}
	encoded, err := EncodeGraphProjection(context.Background(), graph)
	if err != nil {
		test.Fatal(err)
	}
	for _, helper := range []string{"encode", "decode"} {
		for _, check := range []int32{1, 2, 3} {
			test.Run(fmt.Sprintf("%s/cancel-at-check-%d", helper, check), func(test *testing.T) {
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx := &graphProjectionCancellationContext{Context: parent, cancel: cancel}
				ctx.remaining.Store(check)
				if helper == "encode" {
					result, err := EncodeGraphProjection(ctx, graph)
					if !errors.Is(err, context.Canceled) || len(result) != 0 {
						test.Fatalf("canceled encoder returned bytes=%d err=%v", len(result), err)
					}
				} else {
					result, err := DecodeGraphProjection(ctx, encoded)
					if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, domain.GraphState{}) {
						test.Fatalf("canceled decoder exposed a partial graph: %v", err)
					}
				}
			})
		}
	}
}

func graphProjectionTestGzip(test *testing.T, raw io.Reader) []byte {
	test.Helper()
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := io.Copy(writer, raw); err != nil {
		test.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		test.Fatal(err)
	}
	return buffer.Bytes()
}

func TestGraphProjectionEncodingRejectsCorruption(test *testing.T) {
	raw := `{"version":1,"revision":{"graph":"9007199254740993","pending":"7"}}`
	valid := graphProjectionTestGzip(test, strings.NewReader(raw))
	checksum := bytes.Clone(valid)
	checksum[len(checksum)-8] ^= 1
	size := bytes.Clone(valid)
	size[len(size)-4] ^= 1
	for _, scenario := range []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"not gzip", []byte(raw)},
		{"truncated header", valid[:5]},
		{"truncated deflate", valid[:len(valid)/2]},
		{"missing trailer", valid[:len(valid)-8]},
		{"truncated trailer", valid[:len(valid)-1]},
		{"checksum mismatch", checksum},
		{"size mismatch", size},
		{"empty JSON", graphProjectionTestGzip(test, strings.NewReader(""))},
		{"malformed JSON", graphProjectionTestGzip(test, strings.NewReader(`{"version":}`))},
		{"truncated JSON", graphProjectionTestGzip(test, strings.NewReader(`{"version":1`))},
		{"wrong JSON type", graphProjectionTestGzip(test, strings.NewReader(`{"snapshot_ids":1}`))},
		{"trailing JSON object", graphProjectionTestGzip(test, strings.NewReader(raw+` {}`))},
		{"trailing JSON null", graphProjectionTestGzip(test, strings.NewReader(raw+` null`))},
		{"trailing JSON garbage", graphProjectionTestGzip(test, strings.NewReader(raw+` !`))},
		{"trailing gzip garbage", append(bytes.Clone(valid), []byte("not gzip")...)},
		{"second gzip JSON member", append(bytes.Clone(valid), graphProjectionTestGzip(test, strings.NewReader(`{}`))...)},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			graph, err := DecodeGraphProjection(context.Background(), scenario.body)
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(graph, domain.GraphState{}) {
				test.Fatalf("corrupt cache entry was accepted or exposed a partial graph: %v", err)
			}
		})
	}
	test.Run("valid JSON whitespace", func(test *testing.T) {
		encoded := graphProjectionTestGzip(test, strings.NewReader(raw+" \t\r\n"))
		graph, err := DecodeGraphProjection(context.Background(), encoded)
		if err != nil || graph.Version != 1 || graph.Revision != (domain.RepositoryRevision{Graph: 1<<53 + 1, Pending: 7}) {
			test.Fatalf("valid gzip JSON with trailing whitespace rejected: %v", err)
		}
	})
	test.Run("valid whitespace gzip member", func(test *testing.T) {
		encoded := append(bytes.Clone(valid), graphProjectionTestGzip(test, strings.NewReader(" \t\r\n"))...)
		graph, err := DecodeGraphProjection(context.Background(), encoded)
		if err != nil || graph.Version != 1 || graph.Revision != (domain.RepositoryRevision{Graph: 1<<53 + 1, Pending: 7}) {
			test.Fatalf("valid concatenated gzip whitespace changed multistream semantics: %v", err)
		}
	})
}

func TestGraphProjectionEncodingDecodeLimits(test *testing.T) {
	test.Run("compressed input", func(test *testing.T) {
		graph, err := DecodeGraphProjection(context.Background(), make([]byte, GraphProjectionCacheBytes+1))
		if !errors.Is(err, ErrGraphProjectionCacheLimit) || !reflect.DeepEqual(graph, domain.GraphState{}) {
			test.Fatalf("oversized encoded entry bypassed the compressed budget: %v", err)
		}
	})
	for _, extra := range []int{0, 1} {
		test.Run(fmt.Sprintf("expanded-budget-plus-%d", extra), func(test *testing.T) {
			padding := strings.Repeat(" ", 32<<10)
			readers := []io.Reader{strings.NewReader(`{}`)}
			for remaining := GraphProjectionExpandedBytes + extra - 2; remaining > 0; {
				length := min(remaining, len(padding))
				readers = append(readers, strings.NewReader(padding[:length]))
				remaining -= length
			}
			encoded := graphProjectionTestGzip(test, io.MultiReader(readers...))
			if len(encoded) >= GraphProjectionCacheBytes {
				test.Fatal("expansion fixture exceeded the compressed budget")
			}
			graph, err := DecodeGraphProjection(context.Background(), encoded)
			if extra == 0 && err != nil {
				test.Fatalf("exact expansion budget rejected: %v", err)
			}
			if extra > 0 && !errors.Is(err, ErrGraphProjectionCacheLimit) {
				test.Fatalf("expanded trailing whitespace bypassed the budget: %v", err)
			}
			if !reflect.DeepEqual(graph, domain.GraphState{}) {
				test.Fatal("decode returned unexpected graph data")
			}
		})
	}
}

func TestGraphProjectionEncodingWriterCompressedLimit(test *testing.T) {
	writer := &graphProjectionWriter{ctx: context.Background(), writer: io.Discard, remaining: GraphProjectionCacheBytes}
	chunk := make([]byte, 32<<10)
	for remaining := GraphProjectionCacheBytes - 1; remaining > 0; {
		length := min(remaining, len(chunk))
		written, err := writer.Write(chunk[:length])
		if err != nil || written != length {
			test.Fatalf("write within compressed budget: written=%d err=%v", written, err)
		}
		remaining -= written
	}
	if written, err := writer.Write(chunk[:2]); written != 0 || !errors.Is(err, ErrGraphProjectionCacheLimit) || writer.remaining != 1 {
		test.Fatalf("over-budget write was partially accepted: written=%d remaining=%d err=%v", written, writer.remaining, err)
	}
	if written, err := writer.Write(chunk[:1]); written != 1 || err != nil || writer.remaining != 0 {
		test.Fatalf("exact compressed budget rejected: written=%d remaining=%d err=%v", written, writer.remaining, err)
	}
	if written, err := writer.Write(chunk[:1]); written != 0 || !errors.Is(err, ErrGraphProjectionCacheLimit) {
		test.Fatalf("exhausted compressed budget accepted bytes: written=%d err=%v", written, err)
	}
}

func TestGraphProjectionEncodingRoundTripOwnsDecodedResults(test *testing.T) {
	identity := legacyCacheHash(test.Name())
	graph := domain.GraphState{
		Version:         1,
		Revision:        domain.RepositoryRevision{Graph: 1<<53 + 1, Pending: 1<<53 + 3, Evidence: 1<<53 + 5},
		PositionEvent:   "position",
		PrimaryBranch:   "main",
		SnapshotIDs:     []domain.ContentHash{identity},
		BranchSnapshots: map[string][]domain.ContentHash{"main": {identity}},
		BranchContexts: map[string]domain.BranchContext{"main": {
			BranchID: "branch", SnapshotID: identity, SnapshotIDs: []domain.ContentHash{identity}, Roots: []domain.ContentHash{identity},
			Merges: []domain.BranchContextMerge{{Source: identity, State: "included"}},
		}},
		ScopeLabels: map[string]string{"main": strings.Repeat("projection", GraphProjectionCacheBytes/len("projection")+1)},
	}
	raw, err := json.Marshal(graph)
	if err != nil || len(raw) <= GraphProjectionCacheBytes || len(raw) >= GraphProjectionExpandedBytes {
		test.Fatalf("large codec fixture outside expected budgets: bytes=%d err=%v", len(raw), err)
	}
	encoded, err := EncodeGraphProjection(context.Background(), graph)
	if err != nil || len(encoded) == 0 || len(encoded) > GraphProjectionCacheBytes {
		test.Fatalf("large graph encoding: bytes=%d err=%v", len(encoded), err)
	}
	decoded, err := DecodeGraphProjection(context.Background(), encoded)
	if err != nil || !reflect.DeepEqual(decoded, graph) {
		test.Fatalf("large graph round trip changed projection identity or data: %v", err)
	}
	decoded.SnapshotIDs[0] = legacyCacheHash("caller snapshot")
	decoded.BranchSnapshots["main"][0] = legacyCacheHash("caller branch")
	decoded.BranchContexts["main"].Roots[0] = legacyCacheHash("caller root")
	decoded.BranchContexts["main"].Merges[0].State = "caller mutation"
	decoded.ScopeLabels["main"] = "caller label"
	again, err := DecodeGraphProjection(context.Background(), encoded)
	if err != nil || !reflect.DeepEqual(again, graph) {
		test.Fatalf("decoded graph aliases another result or its retained encoding: %v", err)
	}
}

func TestGraphProjectionEncodingExpandedLimit(test *testing.T) {
	graph := domain.GraphState{ScopeLabels: map[string]string{"main": strings.Repeat("x", GraphProjectionExpandedBytes)}}
	encoded, err := EncodeGraphProjection(context.Background(), graph)
	if !errors.Is(err, ErrGraphProjectionCacheLimit) || len(encoded) != 0 {
		test.Fatalf("encoder failed to enforce expanded budget: bytes=%d err=%v", len(encoded), err)
	}
}

func TestGraphProjectionEncodingCloseCancellation(test *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &graphProjectionCancellationContext{Context: parent, cancel: cancel}
	ctx.remaining.Store(4)
	encoded, err := EncodeGraphProjection(ctx, domain.GraphState{Version: 1})
	if !errors.Is(err, context.Canceled) || len(encoded) != 0 {
		test.Fatalf("gzip close error exposed an encoding: bytes=%d err=%v", len(encoded), err)
	}
}

type graphProjectionTestWriter struct {
	written int
	err     error
	calls   int
}

func (writer *graphProjectionTestWriter) Write(body []byte) (int, error) {
	writer.calls++
	return min(writer.written, len(body)), writer.err
}

func TestGraphProjectionEncodingWriterShortWriteAndCancellation(test *testing.T) {
	output := &graphProjectionTestWriter{written: 2, err: io.ErrShortWrite}
	writer := &graphProjectionWriter{ctx: context.Background(), writer: output, remaining: 4}
	if written, err := writer.Write([]byte("abc")); written != 2 || !errors.Is(err, io.ErrShortWrite) || writer.remaining != 2 || output.calls != 1 {
		test.Fatalf("short write changed accounting or error: written=%d remaining=%d err=%v", written, writer.remaining, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writer.ctx = ctx
	writer.remaining = 0
	if written, err := writer.Write([]byte("abc")); written != 0 || !errors.Is(err, context.Canceled) || output.calls != 1 || writer.remaining != 0 {
		test.Fatalf("canceled writer reached output or lost error precedence: written=%d err=%v", written, err)
	}
}

func TestGraphProjectionEncodingReaderCancellationAndEOF(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := graphProjectionReader{ctx: ctx, reader: strings.NewReader("ab")}
	body := make([]byte, 2)
	if read, err := reader.Read(body); read != 2 || err != nil || string(body) != "ab" {
		test.Fatalf("reader changed bytes: read=%d body=%q err=%v", read, body, err)
	}
	if read, err := reader.Read(body); read != 0 || !errors.Is(err, io.EOF) {
		test.Fatalf("reader hid EOF: read=%d err=%v", read, err)
	}
	cancel()
	reader.reader = strings.NewReader("cd")
	if read, err := reader.Read(body); read != 0 || !errors.Is(err, context.Canceled) || string(body) != "ab" {
		test.Fatalf("canceled reader reached input or exposed bytes: read=%d body=%q err=%v", read, body, err)
	}
}
