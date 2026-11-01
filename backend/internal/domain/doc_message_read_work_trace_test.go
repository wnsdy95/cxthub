package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func messageReadWorkFixture(test *testing.T, count int) messageDisplayFixture {
	test.Helper()
	var raw strings.Builder
	raw.WriteString(`{"kind":"message","blocks":[`)
	for index := range count {
		if index > 0 {
			raw.WriteByte(',')
		}
		if index%7 == 0 {
			fmt.Fprintf(&raw, `{"type":"image","name":"asset %d","opaque":"private"}`, index)
		} else {
			fmt.Fprintf(&raw, `{"type":"text","text":"**block %d 한🙂**","opaque":"private"}`, index)
		}
	}
	raw.WriteString(`]}`)
	return buildMessageDisplayFixture(test, []byte(raw.String()))
}

func TestDocMessageReadWorkExactTrace(test *testing.T) {
	fixture := messageReadWorkFixture(test, 2*DocMessageInputPageEntries+1)
	input, err := NewDocMessageDisplayAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	trace := struct {
		Pages      []DocDisplayPage
		Cursors    []json.RawMessage
		Loads      [][]ContentHash
		Generation ContentHash
		Steps      int
	}{}
	cursor := bytes.Clone(input.initial)
	for position := 0; position < input.source.TextBytes; {
		var calls []ContentHash
		before := bytes.Clone(cursor)
		page, next, err := input.read(context.Background(), cursor, DocDisplayPageTextBytes, DocDisplayPageRuns,
			func(_ context.Context, hash ContentHash) ([]byte, error) {
				calls = append(calls, hash)
				body, found := fixture.objects[hash]
				if !found {
					return nil, ErrNotFound
				}
				return body, nil
			})
		if err != nil || len(page.Text) == 0 || page.From != position || len(calls) > DocDisplayMappedReplayObjects || !bytes.Equal(cursor, before) {
			test.Fatal("message replay changed its bounded progress or input cursor", position, len(calls), err)
		}
		position += len(page.Text)
		if got, err := input.cursor(next); err != nil || got != position {
			test.Fatal("message replay lost its pinned cursor", got, position, err)
		}
		trace.Pages = append(trace.Pages, page)
		trace.Cursors = append(trace.Cursors, bytes.Clone(next))
		trace.Loads = append(trace.Loads, calls)
		cursor = next
	}
	assembled := assembleDisplayFixture(test, input, fixture.objects)
	descriptor, err := assembled.generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	trace.Generation, trace.Steps = HashContent(descriptor), assembled.steps
	raw, err := json.Marshal(trace)
	if err != nil {
		test.Fatal(err)
	}
	fingerprint := HashContent(raw)
	const expected ContentHash = "sha256:fe0626d0503f3008a66a78b779ce254992d976d7e8739f3dddc16f9d87251544"
	if fingerprint != expected {
		test.Fatal("message replay changed exact pages, cursors, physical load order or derived generation", fingerprint, expected)
	}
	test.Logf("fingerprint=%s replay_fragments=%d assembly_steps=%d generation=%s", fingerprint, len(trace.Pages), trace.Steps, trace.Generation)
}

func TestDocMessageReadWorkCost(test *testing.T) {
	if os.Getenv("CXT_MESSAGE_READ_COST") != "1" {
		test.Skip("CXT_MESSAGE_READ_COST unset")
	}
	fixture := messageReadWorkFixture(test, 1200)
	input, err := NewDocMessageDisplayAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	loads := 0
	result := testing.Benchmark(func(benchmark *testing.B) {
		benchmark.ReportAllocs()
		for range benchmark.N {
			cursor := bytes.Clone(input.initial)
			for position := 0; position < input.source.TextBytes; {
				page, next, err := input.read(context.Background(), cursor, DocDisplayPageTextBytes, DocDisplayPageRuns,
					func(_ context.Context, hash ContentHash) ([]byte, error) { loads++; return fixture.objects[hash], nil })
				if err != nil || page.From != position || page.Text == "" {
					test.Fatal("cost replay failed its actual factory", position, err)
				}
				position += len(page.Text)
				cursor = next
			}
		}
	})
	test.Logf("blocks=1200 ns_per_replay=%d bytes_per_replay=%d allocations_per_replay=%d iterations=%d callback_observations=%d", result.NsPerOp(), result.AllocedBytesPerOp(), result.AllocsPerOp(), result.N, loads)
}
