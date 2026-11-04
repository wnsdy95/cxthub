package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type legacyNativeFloat64FenceToken struct {
	name     string
	raw      string
	bits     uint64
	rejected bool
}

func legacyNativeFloat64FenceTokens() []legacyNativeFloat64FenceToken {
	tail := strings.Repeat("9", 2*ConversationManifestChunkBytes+73)
	return []legacyNativeFloat64FenceToken{
		{name: "negative-zero", raw: "-0e+" + tail, bits: 0x8000000000000000},
		{name: "overflow", raw: "1e+" + tail, rejected: true},
	}
}

type legacyNativeFloat64FenceSource struct {
	parser *legacyNativeFloat64
	buffer *legacySourceProofBuffer
	source LegacyByteSource
	from   int
	to     int
}

func legacyNativeFloat64FenceStart(test *testing.T, raw string) *legacyNativeFloat64FenceSource {
	test.Helper()
	if ConversationManifestChunkBytes != 512<<10 || canonicalCheckBytes != 64<<10 {
		test.Fatal("numeric fences require the original 512 KiB parts and 64 KiB scan quantum")
	}
	const prefix = `{"number":`
	buffer := &legacySourceProofBuffer{body: []byte(prefix + raw + `,"after":true}`)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	from, to := len(prefix), len(prefix)+len(raw)
	if to-from <= 2*ConversationManifestChunkBytes || len(source.Parts) < 3 {
		test.Fatal("numeric fence fixture does not cross more than two actual source parts")
	}
	parser, err := newLegacyNativeFloat64(from, to)
	if err != nil || parser == nil || len(buffer.reads) != 0 {
		test.Fatal("numeric constructor failed or eagerly read source payloads", err, len(buffer.reads))
	}
	if parser.position != from {
		test.Fatal("numeric constructor did not preserve its raw source span", parser.position, from, to)
	}
	return &legacyNativeFloat64FenceSource{parser: parser, buffer: buffer, source: source, from: from, to: to}
}

func (fixture *legacyNativeFloat64FenceSource) retained(test *testing.T) int64 {
	test.Helper()
	var estimate docRangeHeaderHeapEstimate
	fixture.parser.retained(&estimate)
	if estimate.err != nil || estimate.bytes <= 0 || estimate.bytes >= 64<<10 {
		test.Fatal("numeric retained estimate omitted fixed state or included source-page-sized storage", estimate.bytes, estimate.err)
	}
	return estimate.bytes
}

func (fixture *legacyNativeFloat64FenceSource) step(test *testing.T, ctx context.Context, input *legacyNormalizationInput) (uint64, bool, bool, error) {
	test.Helper()
	if input == nil {
		input = &legacyNormalizationInput{ctx: ctx, source: fixture.source}
	}
	beforePosition, beforeReads := fixture.parser.position, len(fixture.buffer.reads)
	beforeWork, beforeLoads := input.workBytes, input.loads
	bits, complete, rejected, err := fixture.parser.step(ctx, input)
	if fixture.parser.position < beforePosition || fixture.parser.position > fixture.to || fixture.parser.position-beforePosition > canonicalCheckBytes {
		test.Fatal("numeric step escaped its raw span or 64 KiB scan quantum", beforePosition, fixture.parser.position, fixture.to)
	}
	if input.unbounded || input.loads < beforeLoads || input.loads > 2 || input.workBytes < beforeWork || input.workBytes > ConversationManifestChunkBytes {
		test.Fatal("numeric step escaped the enclosing shared work or load budget", input.unbounded, beforeLoads, input.loads, beforeWork, input.workBytes)
	}
	reads := fixture.buffer.reads[beforeReads:]
	if len(reads) > 2-beforeLoads {
		test.Fatal("numeric step added a private source-load allowance", beforeLoads, reads)
	}
	for _, read := range reads {
		if read[0] < 0 || read[0] >= fixture.source.Bytes || read[0]%ConversationManifestChunkBytes != 0 ||
			read[1] != min(ConversationManifestChunkBytes, fixture.source.Bytes-read[0]) {
			test.Fatal("numeric step bypassed authenticated full-part reads", read)
		}
	}
	if err == nil && input.loads-beforeLoads != len(reads) {
		test.Fatal("numeric source loads were not charged to the enclosing input", input.loads-beforeLoads, len(reads))
	}
	if (err != nil || !complete || rejected) && bits != 0 || (err != nil || !complete) && rejected || err != nil && complete {
		test.Fatal("numeric pending, rejected, or failed step exposed a result", bits, complete, rejected, err)
	}
	if complete && fixture.parser.position != fixture.to {
		test.Fatal("numeric result preceded complete raw token consumption", fixture.parser.position, fixture.to)
	}
	fixture.retained(test)
	for _, page := range input.pages {
		clear(page)
	}
	clear(input.window)
	return bits, complete, rejected, err
}

func (fixture *legacyNativeFloat64FenceSource) result(test *testing.T, token legacyNativeFloat64FenceToken, bits uint64, rejected bool) {
	test.Helper()
	if rejected != token.rejected || bits != token.bits || fixture.parser.position != fixture.to {
		test.Fatalf("numeric outcome changed: bits=%016x rejected=%t position=%d, want bits=%016x rejected=%t position=%d", bits, rejected, fixture.parser.position, token.bits, token.rejected, fixture.to)
	}
}

func (fixture *legacyNativeFloat64FenceSource) drain(test *testing.T, token legacyNativeFloat64FenceToken) {
	test.Helper()
	scanLimit := 64 + 32*len(fixture.source.Parts)
	scanSteps, conversionSteps := 0, 0
	for stepIndex := 0; stepIndex < scanLimit+512; stepIndex++ {
		if fixture.parser.position < fixture.to {
			scanSteps++
		} else {
			conversionSteps++
		}
		if scanSteps > scanLimit || conversionSteps > 512 {
			test.Fatal("numeric fixture exhausted its separate finite scan or conversion allowance", scanSteps, conversionSteps)
		}
		ctx, cancel := context.WithCancel(context.Background())
		bits, complete, rejected, err := fixture.step(test, ctx, nil)
		cancel()
		if err != nil {
			test.Fatal("authenticated numeric fixture failed", err)
		}
		if complete {
			fixture.result(test, token, bits, rejected)
			return
		}
	}
	test.Fatal("numeric fixture exhausted its fixed per-part progress limit and bounded conversion allowance")
}

func TestLegacyNativeFloat64FencesPreCanceledStepResumes(test *testing.T) {
	for _, token := range legacyNativeFloat64FenceTokens() {
		test.Run(token.name, func(test *testing.T) {
			fixture := legacyNativeFloat64FenceStart(test, token.raw)
			for _, started := range []bool{false, true} {
				if started {
					_, complete, _, err := fixture.step(test, context.Background(), nil)
					if err != nil || complete || fixture.parser.position <= fixture.from {
						test.Fatal("numeric cancellation fixture did not expose partial scan work", err, complete, fixture.parser.position)
					}
				}
				beforePosition, beforeReads := fixture.parser.position, len(fixture.buffer.reads)
				beforeRetained := fixture.retained(test)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				bits, complete, rejected, err := fixture.step(test, ctx, nil)
				if !errors.Is(err, context.Canceled) || errors.Is(err, ErrPreparedDocRangeObject) || bits != 0 || complete || rejected ||
					fixture.parser.position != beforePosition || len(fixture.buffer.reads) != beforeReads || fixture.retained(test) != beforeRetained {
					test.Fatal("pre-canceled numeric step read, advanced, changed retained state, or lost cancellation priority", err)
				}
			}
			fixture.drain(test, token)
		})
	}
}

func TestLegacyNativeFloat64FencesSourceFailuresStayAtomic(test *testing.T) {
	for _, token := range legacyNativeFloat64FenceTokens() {
		for _, stage := range []string{"first-part", "current-part-next-call", "late-suffix"} {
			for _, failure := range []string{"part-hash", "short-part", "private-eof", "cancel", "cancel-part-hash", "cancel-short-part", "cancel-private-eof"} {
				test.Run(token.name+"/"+stage+"/"+failure, func(test *testing.T) {
					fixture := legacyNativeFloat64FenceStart(test, token.raw)
					if stage == "current-part-next-call" {
						_, complete, _, err := fixture.step(test, context.Background(), nil)
						if err != nil || complete || fixture.parser.position <= fixture.from || fixture.parser.position >= ConversationManifestChunkBytes {
							test.Fatal("numeric reauthentication fixture did not pause within its first source part", err, complete, fixture.parser.position)
						}
					}
					faultOffset := 0
					if stage == "late-suffix" {
						faultOffset = (fixture.to - 1) / ConversationManifestChunkBytes * ConversationManifestChunkBytes
					}
					readSource := fixture.source.Read
					triggered, failedAt := false, -1
					var cancelStep context.CancelFunc
					fixture.source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
						body, err := readSource(ctx, offset, length)
						if triggered || offset != faultOffset {
							return body, err
						}
						triggered, failedAt = true, len(fixture.buffer.reads)
						if strings.HasPrefix(failure, "cancel") {
							cancelStep()
						}
						switch failure {
						case "part-hash", "cancel-part-hash":
							corrupt := bytes.Clone(body)
							position := min(fixture.to-1-offset, len(corrupt)-1)
							corrupt[position] = '8'
							return corrupt, err
						case "short-part", "cancel-short-part":
							return body[:len(body)-1], err
						case "private-eof", "cancel-private-eof":
							return nil, errors.Join(ErrPreparedDocRangeObject, err, io.EOF)
						default:
							return body, err
						}
					}
					for stepIndex := 0; stepIndex < 64+32*len(fixture.source.Parts); stepIndex++ {
						ctx, cancel := context.WithCancel(context.Background())
						cancelStep = cancel
						beforePosition := fixture.parser.position
						_, complete, rejected, err := fixture.step(test, ctx, nil)
						cancel()
						if err == nil {
							if complete || rejected || triggered {
								test.Fatal("numeric source fault was ignored or bypassed by an early zero/overflow outcome", stage, failure, triggered)
							}
							continue
						}
						want := error(ErrPreparedDocRangeObject)
						if strings.HasPrefix(failure, "cancel") {
							want = context.Canceled
						}
						if !triggered || !errors.Is(err, want) || want == context.Canceled && errors.Is(err, ErrPreparedDocRangeObject) || len(fixture.buffer.reads) != failedAt {
							test.Fatal("numeric source failure lost its priority or continued source reads", triggered, err, want, failedAt, len(fixture.buffer.reads))
						}
						if stage == "late-suffix" && (faultOffset < 2*ConversationManifestChunkBytes || beforePosition < faultOffset-canonicalCheckBytes) {
							test.Fatal("numeric suffix fault was not genuinely beyond two source parts", faultOffset, beforePosition)
						}
						return
					}
					test.Fatal("numeric source fault exhausted its original finite per-part scan allowance")
				})
			}
		}
	}
}

func TestLegacyNativeFloat64FencesCurrentContextAndReleasedSourcePages(test *testing.T) {
	type stepContextKey struct{}
	tokens := legacyNativeFloat64FenceTokens()
	tokens = append(tokens, legacyNativeFloat64FenceToken{
		name: "distant-sticky-rounding",
		raw:  "1.00000000000000011102230246251565404236316680908203125" + strings.Repeat("0", 2*ConversationManifestChunkBytes+73) + "1",
		bits: 0x3ff0000000000001,
	})
	for _, token := range tokens {
		test.Run(token.name, func(test *testing.T) {
			fixture := legacyNativeFloat64FenceStart(test, token.raw)
			readSource := fixture.source.Read
			activeStep, previousOffset := 0, -1
			reauthenticated := false
			var borrowed []byte
			fixture.source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				if ctx.Err() != nil || ctx.Value(stepContextKey{}) != activeStep {
					test.Fatal("numeric read retained an earlier step context", offset, length, ctx.Err())
				}
				body, err := readSource(ctx, offset, length)
				borrowed = body
				return body, err
			}
			scanLimit := 64 + 32*len(fixture.source.Parts)
			scanSteps, conversionSteps := 0, 0
			for stepIndex := 0; stepIndex < scanLimit+512; stepIndex++ {
				beforePosition, beforeReads := fixture.parser.position, len(fixture.buffer.reads)
				if beforePosition < fixture.to {
					scanSteps++
				} else {
					conversionSteps++
				}
				if scanSteps > scanLimit || conversionSteps > 512 {
					test.Fatal("numeric context fixture exhausted its separate finite scan or conversion allowance", scanSteps, conversionSteps)
				}
				activeStep++
				ctx, cancel := context.WithCancel(context.WithValue(context.Background(), stepContextKey{}, activeStep))
				bits, complete, rejected, err := fixture.step(test, ctx, nil)
				cancel()
				clear(borrowed)
				if err != nil {
					test.Fatal("fresh context or poisoned prior source page broke numeric work", err)
				}
				reads := fixture.buffer.reads[beforeReads:]
				if fixture.parser.position > beforePosition {
					wantOffset := beforePosition / ConversationManifestChunkBytes * ConversationManifestChunkBytes
					if len(reads) == 0 || reads[0][0] != wantOffset {
						test.Fatal("numeric progress reused a prior call's authenticated source page", beforePosition, fixture.parser.position, reads)
					}
					reauthenticated = reauthenticated || reads[0][0] == previousOffset
					previousOffset = reads[0][0]
				}
				if complete {
					if !reauthenticated || scanSteps < 17 {
						test.Fatal("numeric context fixture lost current-part reauthentication or actual 64 KiB scan seams", reauthenticated, scanSteps)
					}
					fixture.result(test, token, bits, rejected)
					return
				}
			}
			test.Fatal("numeric context fixture exhausted its fixed per-part progress limit and bounded conversion allowance")
		})
	}
}

func TestLegacyNativeFloat64FencesSharedLoadAndWorkBudgets(test *testing.T) {
	for _, token := range legacyNativeFloat64FenceTokens() {
		for _, spentLoads := range []int{1, 2} {
			test.Run(fmt.Sprintf("%s/spent-loads-%d", token.name, spentLoads), func(test *testing.T) {
				fixture := legacyNativeFloat64FenceStart(test, token.raw)
				ctx, cancel := context.WithCancel(context.Background())
				input := &legacyNormalizationInput{ctx: ctx, source: fixture.source}
				for ordinal := 1; ordinal <= spentLoads; ordinal++ {
					if _, err := input.part(ordinal); err != nil {
						test.Fatal("numeric shared-load fixture could not authenticate framing parts", err)
					}
				}
				beforeReads := len(fixture.buffer.reads)
				_, complete, rejected, err := fixture.step(test, ctx, input)
				cancel()
				if err != nil || complete || rejected || len(fixture.buffer.reads)-beforeReads != 2-spentLoads ||
					spentLoads == 2 && fixture.parser.position != fixture.from || spentLoads == 1 && fixture.parser.position <= fixture.from {
					test.Fatal("numeric parser reset, extended, or could not use the shared two-load budget", spentLoads, fixture.parser.position, len(fixture.buffer.reads)-beforeReads, err)
				}
				fixture.drain(test, token)
			})
		}
		for _, availableWork := range []int{0, canonicalCheckBytes / 2} {
			test.Run(fmt.Sprintf("%s/available-work-%d", token.name, availableWork), func(test *testing.T) {
				fixture := legacyNativeFloat64FenceStart(test, token.raw)
				ctx, cancel := context.WithCancel(context.Background())
				input := &legacyNormalizationInput{ctx: ctx, source: fixture.source, workBytes: ConversationManifestChunkBytes - availableWork}
				_, complete, rejected, err := fixture.step(test, ctx, input)
				cancel()
				advanced := fixture.parser.position - fixture.from
				if err != nil || complete || rejected || advanced > availableWork || availableWork == 0 && len(fixture.buffer.reads) != 0 {
					test.Fatal("numeric parser ignored work already spent by its enclosing scanner", availableWork, advanced, len(fixture.buffer.reads), err)
				}
				fixture.drain(test, token)
			})
		}
	}
}
