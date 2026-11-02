package domain

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestLegacyNormalizationInputWindowSameStepBoundary(test *testing.T) {
	body := append(bytes.Repeat([]byte("a"), ConversationManifestChunkBytes), []byte("boundary")...)
	buffer := &legacySourceProofBuffer{body: body}
	source := buffer.input()
	borrowed := make([]byte, ConversationManifestChunkBytes)
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		part, err := buffer.Read(ctx, offset, length)
		if err != nil {
			return nil, err
		}
		copy(borrowed, part)
		return borrowed[:length], nil
	}
	input := &legacyNormalizationInput{ctx: context.Background(), source: source}
	positions := []int{0, 0, canonicalCheckBytes, ConversationManifestChunkBytes - 1, ConversationManifestChunkBytes, len(body) - 1, ConversationManifestChunkBytes - 2, ConversationManifestChunkBytes + 1}
	for index, position := range positions {
		current, err := input.current(position)
		if err != nil || current != body[position] || input.workBytes != index+1 {
			test.Fatal("byte window changed source bytes or actual-read accounting", position, current, input.workBytes, err)
		}
		part, err := input.scalarPart(position, len(body), canonicalCheckBytes)
		length := min(canonicalCheckBytes, len(body)-position, ConversationManifestChunkBytes-position%ConversationManifestChunkBytes)
		if err != nil || !bytes.Equal(part, body[position:position+length]) || input.workBytes != index+1 {
			test.Fatal("scalar window crossed a page or charged unconsumed bytes", position, len(part), input.workBytes, err)
		}
		if input.windowStart != position-position%ConversationManifestChunkBytes || input.loads > 2 {
			test.Fatal("current and scalar did not share their authenticated window", input.windowStart, input.loads)
		}
	}
	clear(borrowed)
	for _, position := range []int{0, ConversationManifestChunkBytes} {
		part, err := input.scalarPart(position, len(body), 1)
		if err != nil || len(part) != 1 || part[0] != body[position] {
			test.Fatal("window retained a borrowed source alias", position, part, err)
		}
		current, err := input.current(position)
		if err != nil || current != part[0] {
			test.Fatal("scalar-to-byte window reuse changed bytes", position, current, err)
		}
	}
	if input.loads != 2 || len(buffer.reads) != 2 || input.workBytes != len(positions)+2 || input.semanticBytes != 0 {
		test.Fatal("same-step window reloaded a cached page or changed accounting", input.loads, buffer.reads, input.workBytes, input.semanticBytes)
	}
	for _, position := range []int{-1, len(body), len(body) + 1} {
		if _, err := input.current(position); !errors.Is(err, ErrIntegrity) {
			test.Fatal("cached window bypassed source bounds", position, err)
		}
	}
	if input.workBytes != len(positions)+2 || len(buffer.reads) != 2 {
		test.Fatal("out-of-bounds reads consumed work or loaded source")
	}
}

func TestLegacyNormalizationInputWindowTwoLoadLimit(test *testing.T) {
	buffer := &legacySourceProofBuffer{body: bytes.Repeat([]byte("x"), 2*ConversationManifestChunkBytes+1)}
	input := &legacyNormalizationInput{ctx: context.Background(), source: buffer.input()}
	if _, err := input.current(ConversationManifestChunkBytes - 1); err != nil {
		test.Fatal(err)
	}
	if part, err := input.scalarPart(ConversationManifestChunkBytes, input.source.Bytes, 1); err != nil || len(part) != 1 {
		test.Fatal("scalar did not load the second authenticated part", len(part), err)
	}
	if _, err := input.current(2 * ConversationManifestChunkBytes); !errors.Is(err, errLegacyNormalizationYield) {
		test.Fatal("byte window bypassed the two-load limit", err)
	}
	if _, err := input.scalarPart(2*ConversationManifestChunkBytes, input.source.Bytes, 1); !errors.Is(err, errLegacyNormalizationYield) {
		test.Fatal("scalar window bypassed the two-load limit", err)
	}
	if input.loads != 2 || len(buffer.reads) != 2 || input.workBytes != 1 || input.failure != nil {
		test.Fatal("load-budget yield read source, charged bytes, or became source failure", input.loads, buffer.reads, input.workBytes, input.failure)
	}
	for _, position := range []int{0, ConversationManifestChunkBytes} {
		current, err := input.current(position)
		if err != nil || current != 'x' {
			test.Fatal("denied third load invalidated an authenticated page", position, err)
		}
	}
	if input.loads != 2 || len(buffer.reads) != 2 || input.workBytes != 3 {
		test.Fatal("cached reads bypassed work accounting after load-budget yield")
	}
}

func TestLegacyNormalizationInputWindowScalarBudget(test *testing.T) {
	body := bytes.Repeat([]byte("x"), ConversationManifestChunkBytes+19)
	for _, fixture := range []struct {
		name     string
		position int
		end      int
		maximum  int
		work     int
		length   int
		want     error
	}{
		{name: "maximum", end: len(body), maximum: 3, length: 3},
		{name: "check-quantum", end: len(body), maximum: len(body), length: canonicalCheckBytes},
		{name: "remaining-work", end: len(body), maximum: 100, work: ConversationManifestChunkBytes - 6, length: 3},
		{name: "exhausted-work", end: len(body), maximum: 100, work: ConversationManifestChunkBytes, want: errLegacyNormalizationYield},
		{name: "one-work-byte", end: len(body), maximum: 100, work: ConversationManifestChunkBytes - 1, want: errLegacyNormalizationYield},
		{name: "zero-maximum", end: len(body), want: errLegacyNormalizationYield},
		{name: "negative-maximum", end: len(body), maximum: -1, want: errLegacyNormalizationYield},
		{name: "page-boundary", position: ConversationManifestChunkBytes - 2, end: len(body), maximum: 100, length: 2},
		{name: "partial-final-page", position: ConversationManifestChunkBytes + 3, end: len(body), maximum: 100, length: 16},
		{name: "selected-end", end: 7, maximum: 100, length: 7},
		{name: "negative-position", position: -1, end: len(body), maximum: 1, want: ErrIntegrity},
		{name: "empty-span", position: 7, end: 7, maximum: 1, want: ErrIntegrity},
		{name: "source-end", position: len(body), end: len(body), maximum: 1, want: ErrIntegrity},
		{name: "end-past-source", end: len(body) + 1, maximum: 1, want: ErrIntegrity},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			buffer := &legacySourceProofBuffer{body: body}
			input := &legacyNormalizationInput{ctx: context.Background(), source: buffer.input(), workBytes: fixture.work}
			part, err := input.scalarPart(fixture.position, fixture.end, fixture.maximum)
			if !errors.Is(err, fixture.want) || len(part) != fixture.length || input.workBytes != fixture.work || input.failure != nil {
				test.Fatal("scalar window changed bounds or reserved-work semantics", len(part), input.workBytes, err, input.failure)
			}
			if fixture.want != nil {
				if part != nil || input.loads != 0 || len(buffer.reads) != 0 {
					test.Fatal("invalid or exhausted scalar span loaded source", input.loads, buffer.reads)
				}
				return
			}
			if input.loads != 1 || len(buffer.reads) != 1 || !bytes.Equal(part, body[fixture.position:fixture.position+fixture.length]) {
				test.Fatal("scalar window did not authenticate its bounded source span")
			}
			input.workBytes += 2 * len(part)
			if input.workBytes > ConversationManifestChunkBytes {
				test.Fatal("scalar read and write exceeded the shared work budget")
			}
			if _, err := input.scalarPart(fixture.position, fixture.end, 1); input.workBytes == ConversationManifestChunkBytes && !errors.Is(err, errLegacyNormalizationYield) {
				test.Fatal("hot scalar window bypassed the exhausted work budget", err)
			}
		})
	}
}

func TestLegacyNormalizationInputWindowValueReaderReplacement(test *testing.T) {
	buffer := &legacySourceProofBuffer{body: bytes.Repeat([]byte("x"), 2*ConversationManifestChunkBytes+17)}
	input := &legacyNormalizationInput{ctx: context.Background(), source: buffer.input()}
	if _, err := input.current(0); err != nil {
		test.Fatal(err)
	}
	reader := &legacyNormalizationValueReader{input: input, end: input.source.Bytes}
	decoded := make([]byte, input.source.Bytes)
	input.unbounded = true
	read, err := io.ReadFull(reader, decoded)
	input.unbounded = false
	if err != nil || read != len(decoded) || !bytes.Equal(decoded, buffer.body) {
		test.Fatal("value reader replacement changed source bytes", read, err)
	}
	if input.loads != 3 || len(buffer.reads) != 3 || input.workBytes != 1+len(buffer.body) || input.semanticBytes != 0 {
		test.Fatal("value reader changed its existing fallback budget or accounting", input.loads, input.workBytes, input.semanticBytes)
	}
	work := input.workBytes
	if _, err := input.current(0); !errors.Is(err, errLegacyNormalizationYield) {
		test.Fatal("window retained an evicted page and bypassed the restored load budget", err)
	}
	input.workBytes = 0
	if _, err := input.scalarPart(0, input.source.Bytes, 1); !errors.Is(err, errLegacyNormalizationYield) {
		test.Fatal("scalar window retained an evicted page", err)
	}
	input.workBytes = work
	current, err := input.current(input.source.Bytes - 1)
	if err != nil || current != 'x' || input.workBytes != work+1 || len(buffer.reads) != 3 || input.failure != nil {
		test.Fatal("value reader did not update the last authenticated window", current, input.workBytes, err)
	}
}

func TestLegacyNormalizationInputWindowReadFailure(test *testing.T) {
	for _, access := range []string{"current", "scalar"} {
		for _, fixture := range []struct {
			name string
			want error
		}{
			{name: "read-error", want: io.ErrClosedPipe},
			{name: "joined-eof", want: io.EOF},
			{name: "short-read", want: ErrPreparedDocRangeObject},
			{name: "hash-mismatch", want: ErrPreparedDocRangeObject},
			{name: "canceled-read", want: context.Canceled},
		} {
			test.Run(access+"/"+fixture.name, func(test *testing.T) {
				buffer := &legacySourceProofBuffer{body: bytes.Repeat([]byte("x"), ConversationManifestChunkBytes+19)}
				source := buffer.input()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
					part, err := buffer.Read(ctx, offset, length)
					if err != nil || offset == 0 {
						return part, err
					}
					switch fixture.name {
					case "read-error":
						return nil, io.ErrClosedPipe
					case "joined-eof":
						return nil, errors.Join(ErrPreparedDocRangeObject, io.EOF)
					case "short-read":
						return part[:len(part)-1], nil
					case "hash-mismatch":
						part = bytes.Clone(part)
						part[0] ^= 1
					case "canceled-read":
						cancel()
					}
					return part, nil
				}
				input := &legacyNormalizationInput{ctx: ctx, source: source}
				if _, err := input.current(0); err != nil {
					test.Fatal(err)
				}
				var failure error
				if access == "current" {
					current, err := input.current(ConversationManifestChunkBytes)
					if current != 0 {
						test.Fatal("failed byte read exposed unauthenticated data", current)
					}
					failure = err
				} else {
					part, err := input.scalarPart(ConversationManifestChunkBytes, source.Bytes, 1)
					if part != nil {
						test.Fatal("failed scalar read exposed unauthenticated data")
					}
					failure = err
				}
				if !errors.Is(failure, fixture.want) || !errors.Is(failure, ErrPreparedDocRangeObject) || input.failure != failure {
					test.Fatal("window lost the authenticated-source failure classification", failure, input.failure)
				}
				if input.loads != 1 || len(buffer.reads) != 2 || input.workBytes != 1 || input.windowStart != 0 || !bytes.Equal(input.window, buffer.body[:ConversationManifestChunkBytes]) {
					test.Fatal("failed load replaced the authenticated window or charged unread bytes", input.loads, buffer.reads, input.workBytes, input.windowStart)
				}
			})
		}
	}
}

func TestLegacyNormalizationInputWindowNextStepFreshSource(test *testing.T) {
	for _, mutate := range []bool{false, true} {
		name := "fresh-context"
		if mutate {
			name = "changed-source-behind-cursor"
		}
		test.Run(name, func(test *testing.T) {
			buffer := &legacySourceProofBuffer{body: []byte(`{"ignored":"` + strings.Repeat("x", ConversationManifestChunkBytes+1) + `"}`)}
			source := buffer.input()
			var contexts []context.Context
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				contexts = append(contexts, ctx)
				return buffer.Read(ctx, offset, length)
			}
			first, cancel := context.WithCancel(context.Background())
			defer cancel()
			normalizer, err := NewLegacyNormalizationDecoder(first, source)
			if err != nil {
				test.Fatal(err)
			}
			step, err := normalizer.Step(first)
			if err != nil || step.Complete || normalizer.position != 1 || len(buffer.reads) != 1 || contexts[0] != first {
				test.Fatal("root opening did not yield inside its first authenticated part", err, normalizer.position, buffer.reads)
			}
			cancel()
			if mutate {
				buffer.body[0] ^= 1
			}
			next, cancelNext := context.WithCancel(context.Background())
			defer cancelNext()
			before := len(buffer.reads)
			step, err = normalizer.Step(next)
			if len(buffer.reads) <= before || len(buffer.reads)-before > 2 || buffer.reads[before][0] != 0 {
				test.Fatal("next Step reused an authenticated page or exceeded two loads", buffer.reads[before:])
			}
			for _, observed := range contexts[before:] {
				if observed != next {
					test.Fatal("the previous Step context survived in the input window")
				}
			}
			if mutate {
				if !errors.Is(err, ErrPreparedDocRangeObject) || errors.Is(err, context.Canceled) || step.Complete || step.Reset || step.Events != nil || step.Envelope != nil || step.Count != 0 {
					test.Fatal("next Step accepted changed source or reused its canceled context", err)
				}
				return
			}
			if err != nil || step.Complete || normalizer.position <= 1 {
				test.Fatal("fresh context did not advance the next Step", err, normalizer.position)
			}
			retained, err := normalizer.RetainedBytes()
			if err != nil || retained >= canonicalCheckBytes {
				test.Fatal("normalizer retained the Step-local source window", retained, err)
			}
		})
	}
}
