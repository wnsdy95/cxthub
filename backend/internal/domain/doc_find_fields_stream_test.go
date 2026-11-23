package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type docFindFieldsTrackingReader struct {
	source     io.Reader
	chunk      int
	maxRequest int
	readCalls  int
	onRead     func()
}

func (reader *docFindFieldsTrackingReader) Read(buffer []byte) (int, error) {
	reader.readCalls++
	reader.maxRequest = max(reader.maxRequest, len(buffer))
	if reader.chunk > 0 {
		buffer = buffer[:min(len(buffer), reader.chunk)]
	}
	count, err := reader.source.Read(buffer)
	if reader.onRead != nil {
		reader.onRead()
	}
	return count, err
}

type docFindFieldsErrorReader struct {
	err error
}

func (reader docFindFieldsErrorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

type docFindFieldsDataErrorReader struct {
	data string
	err  error
}

func (reader *docFindFieldsDataErrorReader) Read(buffer []byte) (int, error) {
	count := copy(buffer, reader.data)
	reader.data = reader.data[count:]
	return count, reader.err
}

type docFindFieldsRepeatedReader struct {
	remaining int
	character byte
}

func (reader *docFindFieldsRepeatedReader) Read(buffer []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	count := min(reader.remaining, len(buffer))
	for position := range count {
		buffer[position] = reader.character
	}
	reader.remaining -= count
	return count, nil
}

func TestDocFindFieldsReaderPreservesReaderErrors(testContext *testing.T) {
	sentinel := errors.New("source read failed")
	for _, prefix := range []string{
		"",
		`{"kind":"reas`,
		`{"kind":"reasoning","locked":"opaque`,
		`{"kind":"reasoning","unknown":`,
		`{"kind":"reasoning","unknown":-`,
		`{"kind":"reasoning","unknown":1.`,
		`{"kind":"reasoning","unknown":1e+`,
		`{"kind":"reasoning"}`,
	} {
		testContext.Run(prefix, func(testContext *testing.T) {
			reader := io.MultiReader(strings.NewReader(prefix), docFindFieldsErrorReader{err: sentinel})
			fields, err := BuildDocFindFieldsReader(context.Background(), HashContent([]byte(prefix)), len(prefix)+100, reader)
			if !errors.Is(err, sentinel) || !reflect.DeepEqual(fields, DocFindFields{}) {
				testContext.Fatal("reader error was lost or produced partial metadata", fields, err)
			}
		})
	}
	const complete = `{"kind":"reasoning","redacted_summary":"visible"}`
	for _, failure := range []error{sentinel, context.DeadlineExceeded, io.ErrNoProgress} {
		reader := &docFindFieldsDataErrorReader{data: complete, err: failure}
		fields, err := BuildDocFindFieldsReader(context.Background(), HashContent([]byte(complete)), len(complete), reader)
		if !errors.Is(err, failure) || !reflect.DeepEqual(fields, DocFindFields{}) {
			testContext.Fatal("reader returning data with an error produced metadata", fields, err)
		}
	}
}

func TestDocFindFieldsReaderCancellation(testContext *testing.T) {
	const body = `{"kind":"reasoning","redacted_summary":"visible"}`
	eventHash := HashContent([]byte(body))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := &docFindFieldsTrackingReader{source: strings.NewReader(body)}
	if fields, err := BuildDocFindFieldsReader(ctx, eventHash, len(body), reader); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(fields, DocFindFields{}) || reader.readCalls != 0 {
		testContext.Fatal("pre-cancellation read source or returned metadata", fields, err, reader.readCalls)
	}
	for _, chunk := range []int{1, 64 << 10} {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &docFindFieldsTrackingReader{source: strings.NewReader(body), chunk: chunk, onRead: cancel}
		fields, err := BuildDocFindFieldsReader(ctx, eventHash, len(body), reader)
		cancel()
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(fields, DocFindFields{}) || reader.maxRequest > 64<<10 {
			testContext.Fatal("mid-read cancellation returned metadata or oversized read", fields, err, reader.maxRequest)
		}
	}
	ctx, cancel = context.WithCancel(context.Background())
	const prefix = `{"kind":"reasoning","locked":{"blob":"`
	const suffix = `"}}`
	reader = &docFindFieldsTrackingReader{source: io.MultiReader(strings.NewReader(prefix), &docFindFieldsRepeatedReader{remaining: 1 << 20, character: 'x'}, strings.NewReader(suffix))}
	reader.onRead = func() {
		if reader.readCalls == 2 {
			cancel()
		}
	}
	fields, err := BuildDocFindFieldsReader(ctx, eventHash, len(prefix)+(1<<20)+len(suffix), reader)
	cancel()
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(fields, DocFindFields{}) || reader.readCalls != 2 {
		testContext.Fatal("cancellation after streaming started was not honored", fields, err, reader.readCalls)
	}
}

func TestDocFindFieldsReaderValidatesBeyondCaptureLimit(testContext *testing.T) {
	for _, fixture := range []struct {
		prefix string
		suffix string
	}{
		{prefix: `{"`, suffix: `\q":null,"kind":"reasoning"}`},
		{prefix: `{"kind":"reasoning","locked":{"blob":"`, suffix: `\q"}}`},
		{prefix: `{"kind":"reasoning","redacted_summary":"`, suffix: `\u00x0"}`},
		{prefix: `{"kind":"reasoning","unknown":"`, suffix: "\xff\"}"},
	} {
		newSource := func() io.Reader {
			return io.MultiReader(strings.NewReader(fixture.prefix), &docFindFieldsRepeatedReader{remaining: 1 << 20, character: 'x'}, strings.NewReader(fixture.suffix))
		}
		digest := sha256.New()
		length, err := io.Copy(digest, newSource())
		if err != nil {
			testContext.Fatal(err)
		}
		eventHash := ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil)))
		fields, err := BuildDocFindFieldsReader(context.Background(), eventHash, int(length), newSource())
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(fields, DocFindFields{}) {
			testContext.Fatal("malformed data beyond capture limit produced metadata", fields, err)
		}
	}
}

func TestDocFindFieldsReaderChecksLengthAndHash(testContext *testing.T) {
	const body = `{"kind":"reasoning","redacted_summary":"visible"}`
	eventHash := HashContent([]byte(body))
	for _, input := range []struct {
		hash   ContentHash
		length int
		source io.Reader
	}{
		{hash: "", length: len(body), source: strings.NewReader(body)},
		{hash: "sha256:invalid", length: len(body), source: strings.NewReader(body)},
		{hash: HashContent([]byte("other event")), length: len(body), source: strings.NewReader(body)},
		{hash: eventHash, length: 0, source: strings.NewReader(body)},
		{hash: eventHash, length: -1, source: strings.NewReader(body)},
		{hash: eventHash, length: math.MaxInt, source: strings.NewReader(body)},
		{hash: eventHash, length: len(body) - 1, source: strings.NewReader(body)},
		{hash: eventHash, length: len(body) + 1, source: strings.NewReader(body)},
		{hash: eventHash, length: len(body), source: strings.NewReader(body + " ")},
		{hash: eventHash, length: len(body), source: strings.NewReader(body + "x")},
		{hash: eventHash, length: len(body), source: strings.NewReader(body[:len(body)-1])},
		{hash: eventHash, length: len(body), source: nil},
	} {
		fields, err := BuildDocFindFieldsReader(context.Background(), input.hash, input.length, input.source)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(fields, DocFindFields{}) {
			testContext.Fatal("source binding failure returned metadata", input.hash, input.length, fields, err)
		}
	}
}

func TestDocFindFieldsReaderHugeSourceBoundedAllocations(testContext *testing.T) {
	for _, fixture := range []struct {
		name      string
		prefix    string
		suffix    string
		character byte
		span      func(int) DocFindSpan
		blocks    bool
	}{
		{
			name: "locked blob", prefix: `{"kind":"reasoning","locked":{"blob":"`, suffix: `"},"redacted_summary":"tail"}`, character: 'x',
			span: func(repeatedBytes int) DocFindSpan {
				return DocFindSpan{From: len(`{"kind":"reasoning","locked":{"blob":"`) + repeatedBytes + len(`"},"redacted_summary":`), To: len(`{"kind":"reasoning","locked":{"blob":"`) + repeatedBytes + len(`"},"redacted_summary":"tail"`)}
			},
		},
		{
			name: "summary", prefix: `{"kind":"reasoning","redacted_summary":"`, suffix: `"}`, character: ' ',
			span: func(repeatedBytes int) DocFindSpan {
				return DocFindSpan{From: len(`{"kind":"reasoning","redacted_summary":`), To: len(`{"kind":"reasoning","redacted_summary":"`) + repeatedBytes + 1}
			},
		},
		{
			name: "unknown key", prefix: `{"`, suffix: `":null,"kind":"reasoning","redacted_summary":"tail"}`, character: 'k',
			span: func(repeatedBytes int) DocFindSpan {
				return DocFindSpan{From: len(`{"`) + repeatedBytes + len(`":null,"kind":"reasoning","redacted_summary":`), To: len(`{"`) + repeatedBytes + len(`":null,"kind":"reasoning","redacted_summary":"tail"`)}
			},
		},
		{
			name: "message locked blob", prefix: `{"kind":"message","agent_message":true,"locked":{"blob":"`, suffix: `"},"blocks":[{"type":"text","text":"tail"}]}`, character: 'x', blocks: true,
			span: func(repeatedBytes int) DocFindSpan {
				return DocFindSpan{From: len(`{"kind":"message","agent_message":true,"locked":{"blob":"`) + repeatedBytes + len(`"},"blocks":`), To: len(`{"kind":"message","agent_message":true,"locked":{"blob":"`) + repeatedBytes + len(`"},"blocks":[{"type":"text","text":"tail"}]`)}
			},
		},
		{
			name: "message blocks", prefix: `{"kind":"message","agent_message":true,"blocks":[{"type":"text","text":"`, suffix: `"}],"locked":{"blob":"opaque secret"}}`, character: ' ', blocks: true,
			span: func(repeatedBytes int) DocFindSpan {
				return DocFindSpan{From: len(`{"kind":"message","agent_message":true,"blocks":`), To: len(`{"kind":"message","agent_message":true,"blocks":[{"type":"text","text":"`) + repeatedBytes + len(`"}]`)}
			},
		},
	} {
		testContext.Run(fixture.name, func(testContext *testing.T) {
			var smallAllocation uint64
			for _, repeatedBytes := range []int{1 << 20, 16 << 20} {
				newSource := func() io.Reader {
					return io.MultiReader(strings.NewReader(fixture.prefix), &docFindFieldsRepeatedReader{remaining: repeatedBytes, character: fixture.character}, strings.NewReader(fixture.suffix))
				}
				digest := sha256.New()
				length, err := io.Copy(digest, newSource())
				if err != nil {
					testContext.Fatal(err)
				}
				eventHash := ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil)))
				reader := &docFindFieldsTrackingReader{source: newSource()}
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				fields, err := BuildDocFindFieldsReader(context.Background(), eventHash, int(length), reader)
				runtime.ReadMemStats(&after)
				allocated := after.TotalAlloc - before.TotalAlloc
				span := fields.Summary
				if fixture.blocks {
					span = fields.Blocks
					if fields.Summary != nil {
						testContext.Fatal("message retained reasoning locator", fields)
					}
				} else if fields.Blocks != nil {
					testContext.Fatal("reasoning retained blocks locator", fields)
				}
				if err != nil || span == nil || *span != fixture.span(repeatedBytes) || reader.maxRequest > 64<<10 {
					testContext.Fatal("large streaming source violated span or read bounds", fields, err, reader.maxRequest)
				}
				if allocated > 256<<10 {
					testContext.Fatalf("streaming allocated %d bytes for %d source bytes", allocated, length)
				}
				if repeatedBytes == 1<<20 {
					smallAllocation = allocated
				} else if allocated > smallAllocation+32<<10 {
					testContext.Fatalf("allocation scaled with source: 1 MiB=%d bytes, 16 MiB=%d bytes", smallAllocation, allocated)
				}
				testContext.Logf("source=%d bytes, TotalAlloc delta=%d bytes, max read=%d bytes", length, allocated, reader.maxRequest)
			}
		})
	}
}
