package domain

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

func legacyInputGeometryWidth(partBytes int) int {
	if partBytes == 0 {
		return ConversationManifestChunkBytes
	}
	return partBytes
}

func legacyInputGeometrySource(body []byte, partBytes int) (*legacySourceProofBuffer, LegacyByteSource) {
	buffer := &legacySourceProofBuffer{body: body}
	source := LegacyByteSource{Bytes: len(body), PartBytes: partBytes, Read: buffer.Read}
	width := legacyInputGeometryWidth(partBytes)
	for offset := 0; offset < len(body); offset += width {
		part := body[offset:min(offset+width, len(body))]
		source.Parts = append(source.Parts, ConversationManifestChunk{Hash: HashContent(part), Bytes: int64(len(part))})
	}
	return buffer, source
}

func legacyInputGeometryReads(test *testing.T, reads [][2]int, width, extent int) {
	test.Helper()
	for _, read := range reads {
		if read[0] < 0 || read[0] >= extent || read[0]%width != 0 || read[1] != min(width, extent-read[0]) {
			test.Fatal("input did not read a complete authenticated geometry part", read, width, extent)
		}
	}
}

func legacyInputGeometryZeroStep(test *testing.T, step LegacyNormalizationStep) {
	test.Helper()
	if step.Reset || step.Events != nil || step.Complete || step.Envelope != nil || step.Count != 0 {
		test.Fatal("failed or partial input exposed a normalization result", step)
	}
}

func TestLegacyNormalizationInputGeometrySameStepSeamAndClone(test *testing.T) {
	if LegacyNormalizationPartBytes != 64<<10 || ConversationManifestChunkBytes != 512<<10 {
		test.Fatal("input geometry changed the normalization leaf or logical work quantum")
	}
	for _, partBytes := range []int{0, ConversationManifestChunkBytes, LegacyNormalizationPartBytes} {
		test.Run(strconv.Itoa(partBytes), func(test *testing.T) {
			width := legacyInputGeometryWidth(partBytes)
			body := append(bytes.Repeat([]byte("a"), width), bytes.Repeat([]byte("b"), 19)...)
			buffer, source := legacyInputGeometrySource(body, partBytes)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			borrowed := make([]byte, width)
			sourceRead := source.Read
			source.Read = func(current context.Context, offset, length int) ([]byte, error) {
				if current != ctx {
					test.Fatal("input used a context other than its current Step context")
				}
				part, err := sourceRead(current, offset, length)
				if err != nil {
					return nil, err
				}
				copy(borrowed, part)
				return borrowed[:length], nil
			}
			input := &legacyNormalizationInput{ctx: ctx, source: source, fields: 7, values: 11, semanticBytes: 13}
			positions := []int{0, 0, canonicalCheckBytes, width - 1, width, len(body) - 1, width - 2, width + 1}
			for index, position := range positions {
				current, err := input.current(position)
				if err != nil || current != body[position] || input.workBytes != index+1 {
					test.Fatal("geometry byte lookup changed bytes or charged physical part bytes", position, current, input.workBytes, err)
				}
				part, err := input.scalarPart(position, len(body), canonicalCheckBytes)
				length := min(canonicalCheckBytes, len(body)-position, width-position%width)
				if err != nil || !bytes.Equal(part, body[position:position+length]) || input.workBytes != index+1 {
					test.Fatal("scalar crossed its authenticated leaf seam or charged reserved bytes", position, len(part), input.workBytes, err)
				}
				if input.windowStart != position-position%width || input.loads > 2 {
					test.Fatal("byte and scalar access did not share the geometry window", input.windowStart, input.loads)
				}
			}
			clear(borrowed)
			for _, position := range []int{0, width} {
				part, err := input.scalarPart(position, len(body), 1)
				if err != nil || len(part) != 1 || part[0] != body[position] {
					test.Fatal("authenticated page retained the source reader's borrowed alias", position, part, err)
				}
				current, err := input.current(position)
				if err != nil || current != body[position] {
					test.Fatal("same-Step scalar-to-byte cache reuse changed source bytes", position, current, err)
				}
				reader := &legacyNormalizationValueReader{input: input, position: position, end: position + 1}
				output := []byte{0xff}
				if read, err := reader.Read(output); err != nil || read != 1 || output[0] != body[position] {
					test.Fatal("value reader retained the source reader's borrowed alias", position, read, err)
				}
			}
			if input.loads != 2 || len(buffer.reads) != 2 || buffer.reads[0] != [2]int{0, width} || buffer.reads[1] != [2]int{width, 19} ||
				input.workBytes != len(positions)+4 || input.semanticBytes != 13 || input.fields != 7 || input.values != 11 || input.failure != nil {
				test.Fatal("same-Step geometry cache changed source loads or independent guards", input.loads, buffer.reads, input.workBytes)
			}
			for _, position := range []int{-1, len(body), len(body) + 1} {
				if _, err := input.current(position); !errors.Is(err, ErrIntegrity) {
					test.Fatal("cached geometry window bypassed source bounds", position, err)
				}
			}
			if input.workBytes != len(positions)+4 || len(buffer.reads) != 2 {
				test.Fatal("invalid byte positions consumed work or source reads")
			}
		})
	}
}

func TestLegacyNormalizationInputGeometrySharedTwoLoadLimit(test *testing.T) {
	for _, partBytes := range []int{0, ConversationManifestChunkBytes, LegacyNormalizationPartBytes} {
		test.Run(strconv.Itoa(partBytes), func(test *testing.T) {
			width := legacyInputGeometryWidth(partBytes)
			buffer, source := legacyInputGeometrySource(bytes.Repeat([]byte("x"), 2*width+17), partBytes)
			input := &legacyNormalizationInput{ctx: context.Background(), source: source, fields: 7, values: 11}
			if _, err := input.current(width - 1); err != nil {
				test.Fatal(err)
			}
			if part, err := input.scalarPart(width, source.Bytes, 1); err != nil || len(part) != 1 {
				test.Fatal("scalar failed to share the byte input's second load", len(part), err)
			}
			if !input.canDecode(width-1, width+1) || input.canDecode(2*width, 2*width+1) || input.canDecode(width-1, 2*width+1) {
				test.Fatal("decode admission ignored the shared two-page geometry cache")
			}
			if current, err := input.current(2 * width); current != 0 || !errors.Is(err, errLegacyNormalizationYield) {
				test.Fatal("byte lookup allowed a third geometry load", current, err)
			}
			if part, err := input.scalarPart(2*width, source.Bytes, 1); part != nil || !errors.Is(err, errLegacyNormalizationYield) {
				test.Fatal("scalar lookup allowed a third geometry load", len(part), err)
			}
			reader := &legacyNormalizationValueReader{input: input, position: 2 * width, end: source.Bytes}
			output := []byte{0xff}
			if read, err := reader.Read(output); read != 0 || !errors.Is(err, errLegacyNormalizationYield) || reader.position != 2*width || output[0] != 0xff {
				test.Fatal("value reader consumed or exposed a third source part", read, reader.position, err)
			}
			if input.loads != 2 || len(buffer.reads) != 2 || input.workBytes != 1 || input.failure != nil {
				test.Fatal("load-budget yield read source, charged work, or became a source failure", input.loads, buffer.reads, input.workBytes, input.failure)
			}
			if current, err := input.current(0); err != nil || current != 'x' {
				test.Fatal("third-load yield invalidated an authenticated cached leaf", err)
			}
			reader = &legacyNormalizationValueReader{input: input, position: width, end: width + 3}
			output = make([]byte, 3)
			if read, err := reader.Read(output); err != nil || read != 3 || string(output) != "xxx" {
				test.Fatal("reader could not reuse a scalar-authenticated leaf", read, err)
			}
			if input.loads != 2 || len(buffer.reads) != 2 || input.workBytes != 5 || input.semanticBytes != 0 || input.fields != 7 || input.values != 11 {
				test.Fatal("cached access bypassed work accounting or changed field/value guards", input.workBytes, input.loads)
			}
			legacyInputGeometryReads(test, buffer.reads, width, source.Bytes)
		})
	}
}

func TestLegacyNormalizationInputGeometryScalarBoundsAndWork(test *testing.T) {
	for _, partBytes := range []int{0, ConversationManifestChunkBytes, LegacyNormalizationPartBytes} {
		width := legacyInputGeometryWidth(partBytes)
		body := bytes.Repeat([]byte("x"), width+19)
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
			{name: "remaining-logical-work", end: len(body), maximum: 100, work: ConversationManifestChunkBytes - 6, length: 3},
			{name: "exhausted-logical-work", end: len(body), maximum: 100, work: ConversationManifestChunkBytes, want: errLegacyNormalizationYield},
			{name: "one-logical-work-byte", end: len(body), maximum: 100, work: ConversationManifestChunkBytes - 1, want: errLegacyNormalizationYield},
			{name: "zero-maximum", end: len(body), want: errLegacyNormalizationYield},
			{name: "negative-maximum", end: len(body), maximum: -1, want: errLegacyNormalizationYield},
			{name: "leaf-seam", position: width - 2, end: len(body), maximum: 100, length: 2},
			{name: "partial-final-leaf", position: width + 3, end: len(body), maximum: 100, length: 16},
			{name: "selected-end", end: 7, maximum: 100, length: 7},
			{name: "negative-position", position: -1, end: len(body), maximum: 1, want: ErrIntegrity},
			{name: "empty-span", position: 7, end: 7, maximum: 1, want: ErrIntegrity},
			{name: "source-end", position: len(body), end: len(body), maximum: 1, want: ErrIntegrity},
			{name: "end-past-source", end: len(body) + 1, maximum: 1, want: ErrIntegrity},
		} {
			test.Run(strconv.Itoa(partBytes)+"/"+fixture.name, func(test *testing.T) {
				buffer, source := legacyInputGeometrySource(body, partBytes)
				input := &legacyNormalizationInput{ctx: context.Background(), source: source, workBytes: fixture.work, fields: 7, values: 11}
				part, err := input.scalarPart(fixture.position, fixture.end, fixture.maximum)
				if !errors.Is(err, fixture.want) || len(part) != fixture.length || input.workBytes != fixture.work || input.failure != nil || input.fields != 7 || input.values != 11 {
					test.Fatal("geometry scalar changed bounds or reserved-work semantics", len(part), input.workBytes, err, input.failure)
				}
				if fixture.want != nil {
					if part != nil || input.loads != 0 || len(buffer.reads) != 0 {
						test.Fatal("invalid or exhausted scalar loaded source", input.loads, buffer.reads)
					}
					return
				}
				if input.loads != 1 || len(buffer.reads) != 1 || !bytes.Equal(part, body[fixture.position:fixture.position+fixture.length]) {
					test.Fatal("scalar did not authenticate the selected geometry part")
				}
				input.workBytes += 2 * len(part)
				if input.workBytes > ConversationManifestChunkBytes {
					test.Fatal("scalar read/write changed the original logical work ceiling")
				}
				if input.workBytes == ConversationManifestChunkBytes {
					if part, err := input.scalarPart(fixture.position, fixture.end, 1); part != nil || !errors.Is(err, errLegacyNormalizationYield) {
						test.Fatal("hot scalar cache bypassed exhausted logical work", err)
					}
					if len(buffer.reads) != 1 {
						test.Fatal("exhausted cached scalar reread its source part")
					}
				}
				legacyInputGeometryReads(test, buffer.reads, width, source.Bytes)
			})
		}
	}
}

func TestLegacyNormalizationInputGeometryCanDecodeAdmission(test *testing.T) {
	for _, partBytes := range []int{0, ConversationManifestChunkBytes, LegacyNormalizationPartBytes} {
		width := legacyInputGeometryWidth(partBytes)
		for _, fixture := range []struct {
			name    string
			from    int
			to      int
			work    int
			preload []int
			want    bool
		}{
			{name: "negative", from: -1, to: 1},
			{name: "empty", from: 1, to: 1},
			{name: "reversed", from: 2, to: 1},
			{name: "past-source", to: 2*width + 18},
			{name: "one-leaf", to: width, want: true},
			{name: "two-leaf-seam", from: width - 1, to: width + 1, want: true},
			{name: "three-leaves-or-original-large-fallback", to: 2*width + 1, want: width == ConversationManifestChunkBytes},
			{name: "remaining-work-exact", to: 2, work: ConversationManifestChunkBytes - 2, want: true},
			{name: "remaining-work-short", to: 3, work: ConversationManifestChunkBytes - 2},
			{name: "exhausted-work", to: 1, work: ConversationManifestChunkBytes},
			{name: "cached-plus-one-new-leaf", from: width - 1, to: width + 1, preload: []int{0}, want: true},
			{name: "one-new-leaf-after-one-load", from: 2 * width, to: 2*width + 1, preload: []int{0}, want: true},
			{name: "two-new-leaves-after-one-load", from: width, to: 2*width + 1, preload: []int{0}},
			{name: "both-cached", from: width - 1, to: width + 1, preload: []int{0, width}, want: true},
			{name: "third-leaf", from: 2 * width, to: 2*width + 1, preload: []int{0, width}},
			{name: "cached-work-exact", from: width - 1, to: width + 1, work: ConversationManifestChunkBytes - 4, preload: []int{0, width}, want: true},
			{name: "cached-work-exhausted", to: 1, work: ConversationManifestChunkBytes - 2, preload: []int{0, width}},
		} {
			test.Run(strconv.Itoa(partBytes)+"/"+fixture.name, func(test *testing.T) {
				buffer, source := legacyInputGeometrySource(bytes.Repeat([]byte("x"), 2*width+17), partBytes)
				input := &legacyNormalizationInput{ctx: context.Background(), source: source, workBytes: fixture.work, fields: 7, values: 11, semanticBytes: 13}
				for _, position := range fixture.preload {
					if _, err := input.current(position); err != nil {
						test.Fatal(err)
					}
				}
				before, reads := *input, len(buffer.reads)
				if got := input.canDecode(fixture.from, fixture.to); got != fixture.want {
					test.Fatal("decode admission changed geometry, remaining work, or the two-load guard", got, fixture.want, input.loads, input.workBytes)
				}
				if len(buffer.reads) != reads || input.loads != before.loads || input.workBytes != before.workBytes || input.semanticBytes != before.semanticBytes ||
					input.fields != before.fields || input.values != before.values || input.failure != nil || input.next != before.next || input.windowStart != before.windowStart || input.ordinals != before.ordinals {
					test.Fatal("decode admission consumed source/work or rewrote its authenticated cache")
				}
				legacyInputGeometryReads(test, buffer.reads, width, source.Bytes)
			})
		}
	}
}

func TestLegacyNormalizationInputGeometryValueReaderSeamAndEOF(test *testing.T) {
	for _, partBytes := range []int{0, ConversationManifestChunkBytes, LegacyNormalizationPartBytes} {
		for _, tail := range []int{11, 19} {
			test.Run(strconv.Itoa(partBytes)+"/"+strconv.Itoa(tail), func(test *testing.T) {
				width := legacyInputGeometryWidth(partBytes)
				body := append(bytes.Repeat([]byte("a"), width), bytes.Repeat([]byte("b"), 19)...)
				buffer, source := legacyInputGeometrySource(body, partBytes)
				input := &legacyNormalizationInput{ctx: context.Background(), source: source, fields: 7, values: 11}
				reader := &legacyNormalizationValueReader{input: input, position: width - 3, end: width + tail}
				output := bytes.Repeat([]byte{0xff}, 32)
				if read, err := reader.Read(output); err != nil || read != 3 || string(output[:3]) != "aaa" || output[3] != 0xff || reader.position != width {
					test.Fatal("value reader crossed a geometry leaf in a single read", read, reader.position, err)
				}
				if read, err := reader.Read(output[3:]); err != nil || read != tail || !bytes.Equal(output[:3+tail], body[width-3:width+tail]) || output[3+tail] != 0xff || reader.position != reader.end {
					test.Fatal("value reader changed selected-end or partial-final-leaf bytes", read, reader.position, err)
				}
				if read, err := reader.Read(output); read != 0 || !errors.Is(err, io.EOF) {
					test.Fatal("value reader did not stop at its exact selected end", read, err)
				}
				if read, err := reader.Read(nil); read != 0 || err != nil {
					test.Fatal("zero-length value read changed EOF semantics", read, err)
				}
				if input.loads != 2 || len(buffer.reads) != 2 || input.workBytes != 3+tail || input.semanticBytes != 0 || input.fields != 7 || input.values != 11 {
					test.Fatal("value reader charged geometry bytes rather than consumed bytes", input.loads, input.workBytes, buffer.reads)
				}
				if part, err := input.scalarPart(width, source.Bytes, 1); err != nil || len(part) != 1 || part[0] != 'b' || len(buffer.reads) != 2 || input.windowStart != width {
					test.Fatal("value reader did not share its authenticated window with scalar input", err)
				}
				legacyInputGeometryReads(test, buffer.reads, width, source.Bytes)
			})
		}
	}
}

func TestLegacyNormalizationInputGeometrySourceReadFailure(test *testing.T) {
	for _, partBytes := range []int{0, ConversationManifestChunkBytes, LegacyNormalizationPartBytes} {
		for _, access := range []string{"current", "scalar", "reader"} {
			for _, fixture := range []struct {
				name string
				want error
			}{
				{name: "late-io", want: io.ErrClosedPipe},
				{name: "joined-eof", want: io.EOF},
				{name: "bytes-and-eof", want: io.EOF},
				{name: "short-read", want: ErrPreparedDocRangeObject},
				{name: "long-read", want: ErrPreparedDocRangeObject},
				{name: "mutated-unrequested-byte", want: ErrPreparedDocRangeObject},
				{name: "canceled-read", want: context.Canceled},
			} {
				test.Run(strconv.Itoa(partBytes)+"/"+access+"/"+fixture.name, func(test *testing.T) {
					width := legacyInputGeometryWidth(partBytes)
					buffer, source := legacyInputGeometrySource(bytes.Repeat([]byte("x"), 2*width+17), partBytes)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					sourceRead := source.Read
					source.Read = func(current context.Context, offset, length int) ([]byte, error) {
						if current != ctx {
							test.Fatal("faulting input did not use the current context")
						}
						part, err := sourceRead(current, offset, length)
						if err != nil || offset == 0 {
							return part, err
						}
						switch fixture.name {
						case "late-io":
							return part, io.ErrClosedPipe
						case "joined-eof":
							return nil, errors.Join(ErrPreparedDocRangeObject, io.EOF)
						case "bytes-and-eof":
							return part, io.EOF
						case "short-read":
							return part[:len(part)-1], nil
						case "long-read":
							return append(bytes.Clone(part), 'x'), nil
						case "mutated-unrequested-byte":
							buffer.body[offset+length-1] ^= 1
						case "canceled-read":
							cancel()
						}
						return part, nil
					}
					input := &legacyNormalizationInput{ctx: ctx, source: source, fields: 7, values: 11}
					if _, err := input.current(0); err != nil {
						test.Fatal(err)
					}
					var failure error
					switch access {
					case "current":
						current, err := input.current(width)
						if current != 0 {
							test.Fatal("failed byte load exposed unauthenticated data", current)
						}
						failure = err
					case "scalar":
						part, err := input.scalarPart(width, source.Bytes, 1)
						if part != nil {
							test.Fatal("failed scalar load exposed unauthenticated data")
						}
						failure = err
					case "reader":
						reader := &legacyNormalizationValueReader{input: input, position: width, end: source.Bytes}
						output := []byte{0xff}
						read, err := reader.Read(output)
						if read != 0 || output[0] != 0xff || reader.position != width {
							test.Fatal("failed value load exposed bytes or advanced its cursor", read, reader.position)
						}
						failure = err
					}
					if !errors.Is(failure, fixture.want) || !errors.Is(failure, ErrPreparedDocRangeObject) || input.failure != failure {
						test.Fatal("geometry input lost the actual source failure", failure, input.failure)
					}
					if input.loads != 1 || len(buffer.reads) != 2 || input.workBytes != 1 || input.windowStart != 0 || !bytes.Equal(input.window, buffer.body[:width]) ||
						input.semanticBytes != 0 || input.fields != 7 || input.values != 11 {
						test.Fatal("failed authentication replaced a good page or charged unread bytes", input.loads, buffer.reads, input.workBytes)
					}
					legacyInputGeometryReads(test, buffer.reads, width, source.Bytes)
				})
			}
		}
	}
}

func TestLegacyNormalizationInputGeometryNextStepFreshRead(test *testing.T) {
	for _, partBytes := range []int{0, ConversationManifestChunkBytes, LegacyNormalizationPartBytes} {
		for _, fault := range []string{"fresh-context", "source-mutation", "late-io", "canceled-read"} {
			test.Run(strconv.Itoa(partBytes)+"/"+fault, func(test *testing.T) {
				width := legacyInputGeometryWidth(partBytes)
				body := []byte(`{"ignored":"` + strings.Repeat("x", width+1) + `"}`)
				buffer, source := legacyInputGeometrySource(body, partBytes)
				first, cancelFirst := context.WithCancel(context.Background())
				defer cancelFirst()
				current := context.Context(first)
				var contexts []context.Context
				inject := false
				var cancelRead context.CancelFunc
				sourceRead := source.Read
				source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
					contexts = append(contexts, ctx)
					if ctx != current {
						test.Fatal("normalizer reused the previous Step context", offset, length)
					}
					part, err := sourceRead(ctx, offset, length)
					if err != nil || !inject {
						return part, err
					}
					inject = false
					switch fault {
					case "source-mutation":
						buffer.body[0] ^= 1
					case "late-io":
						return part, io.ErrClosedPipe
					case "canceled-read":
						cancelRead()
					}
					return part, nil
				}
				normalizer, err := NewLegacyNormalizationDecoder(first, source)
				if err != nil || len(buffer.reads) != 0 {
					test.Fatal("normalizer failed construction or eagerly loaded source bytes", err, buffer.reads)
				}
				step, err := normalizer.Step(first)
				if err != nil || normalizer.position != 1 || len(buffer.reads) != 1 || buffer.reads[0] != [2]int{0, width} {
					test.Fatal("root opening did not stop within its first authenticated geometry leaf", err, normalizer.position, buffer.reads)
				}
				legacyInputGeometryZeroStep(test, step)
				cancelFirst()
				skipped, cancelSkipped := context.WithCancel(context.Background())
				cancelSkipped()
				step, err = normalizer.Step(skipped)
				if !errors.Is(err, context.Canceled) || len(buffer.reads) != 1 || normalizer.position != 1 {
					test.Fatal("pre-canceled next Step read source or advanced its cursor", err, buffer.reads)
				}
				legacyInputGeometryZeroStep(test, step)
				next, cancelNext := context.WithCancel(context.Background())
				defer cancelNext()
				current, cancelRead, inject = next, cancelNext, true
				before := len(buffer.reads)
				step, err = normalizer.Step(next)
				if len(buffer.reads) <= before || len(buffer.reads)-before > 2 || buffer.reads[before] != [2]int{0, width} {
					test.Fatal("next Step reused a hash/page proof or exceeded the two-load guard", buffer.reads[before:])
				}
				for _, observed := range contexts[before:] {
					if observed != next {
						test.Fatal("a source read kept the previous Step context")
					}
				}
				legacyInputGeometryReads(test, buffer.reads, width, source.Bytes)
				if fault == "fresh-context" {
					if err != nil || step.Complete || normalizer.position <= 1 {
						test.Fatal("fresh context did not resume the actual source cursor", err, normalizer.position)
					}
					if retained, err := normalizer.RetainedBytes(); err != nil || retained <= 0 || retained >= canonicalCheckBytes {
						test.Fatal("normalizer retained its Step-local authenticated source pages", retained, err)
					}
					return
				}
				legacyInputGeometryZeroStep(test, step)
				want := ErrPreparedDocRangeObject
				if fault == "late-io" {
					want = io.ErrClosedPipe
				} else if fault == "canceled-read" {
					want = context.Canceled
				}
				if !errors.Is(err, want) || fault != "canceled-read" && !errors.Is(err, ErrPreparedDocRangeObject) || fault != "canceled-read" && errors.Is(err, context.Canceled) {
					test.Fatal("next Step accepted stale source bytes or lost the current read failure", fault, err)
				}
				if len(buffer.reads) != before+1 {
					test.Fatal("failed first authentication continued reading other source leaves", buffer.reads[before:])
				}
			})
		}
	}
}
