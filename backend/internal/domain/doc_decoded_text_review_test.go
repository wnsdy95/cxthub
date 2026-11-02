package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func decodedTextReviewFirstStep(test *testing.T, fixture decodedTextFixture) DocDecodedTextStep {
	test.Helper()
	initial, err := NewDocDecodedTextState(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	step, err := StepDocDecodedText(context.Background(), fixture.source, initial,
		func(_ context.Context, from, length int) ([]byte, error) { return fixture.raw[from : from+length], nil },
		func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil })
	if err != nil || step.Page == nil || step.State.Done {
		test.Fatal("review needs an unfinished first page", err)
	}
	return step
}

func TestDocDecodedTextReviewAlignedCheckpointCannotSkipOrReplayRawRune(test *testing.T) {
	fixture := buildDecodedTextFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"aaaa`+strings.Repeat(`\uD83D\uDE42`, 1024)+`"}`))
	first := decodedTextReviewFirstStep(test, fixture)
	if first.State.HashNext != first.State.TextBytes {
		test.Fatal("review needs an exactly block-aligned checkpoint")
	}
	for _, shift := range []int{-12, 12} {
		test.Run(fmt.Sprint(shift), func(test *testing.T) {
			prior := first.State
			prior.Next += shift
			encoded, err := prior.Encode(fixture.source)
			if err != nil {
				test.Fatal("forgery must pass checkpoint shape checks", err)
			}
			prior, err = DecodeDocDecodedTextState(fixture.source, HashContent(encoded), encoded)
			if err != nil {
				test.Fatal(err)
			}
			objects := make(map[ContentHash][]byte, len(fixture.objects))
			for hash, raw := range fixture.objects {
				objects[hash] = raw
			}
			load := func(_ context.Context, hash ContentHash) ([]byte, error) { return objects[hash], nil }
			for !prior.Done {
				step, err := StepDocDecodedText(context.Background(), fixture.source, prior,
					func(_ context.Context, from, length int) ([]byte, error) { return fixture.raw[from : from+length], nil }, load)
				if errors.Is(err, ErrIntegrity) {
					if !reflect.DeepEqual(step, DocDecodedTextStep{}) {
						test.Fatal("rejected checkpoint leaked partial results")
					}
					return
				}
				if err != nil {
					test.Fatal(err)
				}
				for _, object := range step.Objects {
					objects[object.Hash] = object.Bytes
				}
				prior = step.State
			}
			generation, _, err := FinishDocDecodedText(context.Background(), fixture.source, prior)
			if errors.Is(err, ErrIntegrity) {
				return
			}
			test.Fatalf("raw cursor forgery published a false decoded digest without changing SHA state: shift=%d original=%s published=%s bytes=%d/%d error=%v", shift, fixture.generation.Hash, generation.Hash, generation.TextBytes, fixture.generation.TextBytes, err)
		})
	}
}

func TestDocDecodedTextReviewEverySHA256TailRemainder(test *testing.T) {
	seen := make(map[int]bool)
	for prefix := 1; prefix <= sha256.BlockSize; prefix++ {
		test.Run(fmt.Sprint(prefix), func(test *testing.T) {
			text := strings.Repeat("a", prefix) + strings.Repeat("🙂", 1024)
			raw := []byte(`{"kind":"reasoning","redacted_summary":"` + strings.Repeat("a", prefix) + strings.Repeat(`\uD83D\uDE42`, 1024) + `"}`)
			fixture := buildDecodedTextFixture(test, raw)
			first := decodedTextReviewFirstStep(test, fixture)
			pending := first.State.TextBytes - first.State.HashNext
			seen[pending] = true
			hashState := first.State.HashState
			if pending < 0 || pending >= sha256.BlockSize || binary.BigEndian.Uint64(hashState[len(hashState)-8:]) != uint64(first.State.HashNext) || !bytes.Equal(hashState[4+sha256.Size:len(hashState)-8], make([]byte, sha256.BlockSize)) {
				test.Fatal("checkpoint retained plaintext or lost its decoded hash count", pending)
			}
			if fixture.generation.Hash != HashContent([]byte(text)) || fixture.generation.TextBytes != len(text) {
				test.Fatal("staged decoded tail changed the whole-input SHA256", pending, fixture.generation.Hash)
			}
		})
	}
	if len(seen) != sha256.BlockSize {
		test.Fatal("did not cover all staged SHA256 tail lengths", len(seen))
	}
}

func TestDocDecodedTextReviewRejectsMalformedCheckpointBeforeIO(test *testing.T) {
	fixture := buildDecodedTextFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"a`+strings.Repeat(`\uD83D\uDE42`, 1024)+`"}`))
	first := decodedTextReviewFirstStep(test, fixture)
	for name, mutate := range map[string]func(*DocDecodedTextState){
		"version":            func(state *DocDecodedTextState) { state.Version = 3 },
		"source":             func(state *DocDecodedTextState) { state.Source = HashContent([]byte("different source")) },
		"next-overflow":      func(state *DocDecodedTextState) { state.Next = math.MaxInt },
		"text-overflow":      func(state *DocDecodedTextState) { state.TextBytes = math.MaxInt },
		"hash-next-overflow": func(state *DocDecodedTextState) { state.HashNext = math.MaxInt },
		"hash-next-negative": func(state *DocDecodedTextState) { state.HashNext = -1 },
		"hash-next-restart":  func(state *DocDecodedTextState) { state.HashNext = 0 },
		"hash-count-restart": func(state *DocDecodedTextState) {
			initial, err := NewDocDecodedTextState(fixture.source)
			if err != nil {
				test.Fatal(err)
			}
			state.HashState = initial.HashState
		},
		"hash-count-overflow": func(state *DocDecodedTextState) {
			binary.BigEndian.PutUint64(state.HashState[len(state.HashState)-8:], math.MaxUint64)
		},
		"hash-buffer-tail":   func(state *DocDecodedTextState) { state.HashState[4+sha256.Size] = 'x' },
		"hash-header":        func(state *DocDecodedTextState) { state.HashState[0] ^= 1 },
		"hash-truncated":     func(state *DocDecodedTextState) { state.HashState = state.HashState[:len(state.HashState)-1] },
		"premature-digest":   func(state *DocDecodedTextState) { state.Hash = fixture.generation.Hash },
		"premature-done":     func(state *DocDecodedTextState) { state.Done = true },
		"directory-overflow": func(state *DocDecodedTextState) { state.Directory.Count = math.MaxInt },
		"lookup-overflow":    func(state *DocDecodedTextState) { state.LookupDirectory.Count = math.MaxInt },
		"lookup-empty":       func(state *DocDecodedTextState) { state.LookupEntries = nil },
		"lookup-unflushed":   func(state *DocDecodedTextState) { state.LookupEntries = make([]int, DocDecodedTextLookupEntries) },
		"lookup-ordinal":     func(state *DocDecodedTextState) { state.LookupEntries[0] = state.Directory.Count },
		"lookup-negative":    func(state *DocDecodedTextState) { state.LookupEntries[0] = -1 },
	} {
		test.Run(name, func(test *testing.T) {
			state := first.State
			state.HashState = bytes.Clone(first.State.HashState)
			state.LookupEntries = append([]int(nil), first.State.LookupEntries...)
			mutate(&state)
			raw, err := canonicalJSON(state)
			if err != nil {
				test.Fatal(err)
			}
			decoded, err := DecodeDocDecodedTextState(fixture.source, HashContent(raw), raw)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocDecodedTextState{}) {
				test.Fatal("malformed canonical checkpoint accepted", err)
			}
			step, err := StepDocDecodedText(context.Background(), fixture.source, state,
				func(context.Context, int, int) ([]byte, error) {
					test.Fatal("invalid checkpoint read raw bytes")
					return nil, nil
				},
				func(context.Context, ContentHash) ([]byte, error) {
					test.Fatal("invalid checkpoint read objects")
					return nil, nil
				})
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocDecodedTextStep{}) {
				test.Fatal("malformed checkpoint exposed a partial step", err)
			}
		})
	}
}

func TestDocDecodedTextReviewStrictPageVersionsAndProfiles(test *testing.T) {
	fixture := buildDecodedTextFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"ab"}`))
	literal := DocDisplayStringSource{Version: 1, Profile: DocDisplayStringProfile, EventHash: fixture.source.EventHash, EventBytes: fixture.source.EventBytes, Token: fixture.source.Token}
	projection := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: fixture.source.EventHash, EventBytes: fixture.source.EventBytes, Plan: fixture.generation.Hash, TextBytes: 2}
	literalHash, err := literal.Hash()
	if err != nil {
		test.Fatal(err)
	}
	projectionHash, err := projection.Hash()
	if err != nil {
		test.Fatal(err)
	}
	decodedHash, err := fixture.source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	for name, decoder := range map[string]struct {
		version int
		source  ContentHash
		decode  func(ContentHash, []byte) (DocDisplayPage, error)
	}{
		"literal-v1": {1, literalHash, func(hash ContentHash, raw []byte) (DocDisplayPage, error) {
			return DecodeDocDisplayPage(literal, hash, raw)
		}},
		"projection-v2": {2, projectionHash, func(hash ContentHash, raw []byte) (DocDisplayPage, error) {
			return DecodeDocDisplayProjectionPage(projection, hash, raw)
		}},
		"decoded-v3": {3, decodedHash, func(hash ContentHash, raw []byte) (DocDisplayPage, error) {
			return DecodeDocDecodedTextPage(fixture.source, hash, raw)
		}},
	} {
		for _, version := range []int{0, 1, 2, 3, 4} {
			for _, source := range []ContentHash{literalHash, projectionHash, decodedHash} {
				test.Run(fmt.Sprintf("%s/version-%d/source-%s", name, version, source[7:15]), func(test *testing.T) {
					page := fixture.pages[0]
					page.Version, page.Source = version, source
					raw, err := canonicalJSON(page)
					if err != nil {
						test.Fatal(err)
					}
					actual, err := decoder.decode(HashContent(raw), raw)
					if version == decoder.version && source == decoder.source {
						if err != nil || !reflect.DeepEqual(actual, page) {
							test.Fatal("valid legacy/current page changed", err)
						}
					} else if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(actual, DocDisplayPage{}) {
						test.Fatal("page crossed version or source profile", err)
					}
				})
			}
		}
	}
}

func TestDocDecodedTextReviewCanonicalMetadataRejectsTampering(test *testing.T) {
	fixture := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"**safe**"}`))
	decoded := fixture.decoded
	sourceHash, err := decoded.source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	lookup := DocDecodedTextLookupPage{Version: 1, Source: sourceHash, Pages: []int{0}}
	for name, codec := range map[string]struct {
		value  any
		decode func(ContentHash, []byte) (bool, error)
	}{
		"source": {decoded.source, func(hash ContentHash, raw []byte) (bool, error) {
			value, err := DecodeDocDecodedTextSource(hash, raw)
			return value == (DocDecodedTextSource{}), err
		}},
		"state": {decoded.state, func(hash ContentHash, raw []byte) (bool, error) {
			value, err := DecodeDocDecodedTextState(decoded.source, hash, raw)
			return reflect.DeepEqual(value, DocDecodedTextState{}), err
		}},
		"generation": {decoded.generation, func(hash ContentHash, raw []byte) (bool, error) {
			value, err := DecodeDocDecodedTextGeneration(hash, raw)
			return value == (DocDecodedTextGeneration{}), err
		}},
		"page": {decoded.pages[0], func(hash ContentHash, raw []byte) (bool, error) {
			value, err := DecodeDocDecodedTextPage(decoded.source, hash, raw)
			return reflect.DeepEqual(value, DocDisplayPage{}), err
		}},
		"lookup": {lookup, func(hash ContentHash, raw []byte) (bool, error) {
			value, err := DecodeDocDecodedTextLookupPage(decoded.source, hash, raw)
			return reflect.DeepEqual(value, DocDecodedTextLookupPage{}), err
		}},
		"composite-plan": {fixture.plan, func(hash ContentHash, raw []byte) (bool, error) {
			value, err := DecodeDocDecodedMarkdownPlan(hash, raw)
			return value == (DocDecodedMarkdownPlan{}), err
		}},
	} {
		test.Run(name, func(test *testing.T) {
			raw, err := canonicalJSON(codec.value)
			if err != nil {
				test.Fatal(err)
			}
			if zero, err := codec.decode(HashContent(raw), raw); zero || err != nil {
				test.Fatal("valid canonical metadata rejected", err)
			}
			for mutation, body := range map[string][]byte{
				"leading-whitespace": append([]byte(" "), raw...),
				"unknown-member":     append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"unrecognized":true}`)...),
				"duplicate-version":  append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"version":1}`)...),
				"fractional-version": bytes.ReplaceAll(bytes.ReplaceAll(raw, []byte(`"version":1`), []byte(`"version":1.0`)), []byte(`"version":3`), []byte(`"version":3.0`)),
				"truncated":          raw[:len(raw)-1],
				"trailing-value":     append(bytes.Clone(raw), []byte(`{}`)...),
			} {
				test.Run(mutation, func(test *testing.T) {
					zero, err := codec.decode(HashContent(body), body)
					if !errors.Is(err, ErrIntegrity) || !zero {
						test.Fatal("hash-consistent but noncanonical metadata accepted", err)
					}
				})
			}
			if zero, err := codec.decode(HashContent([]byte("wrong object")), raw); !zero || !errors.Is(err, ErrIntegrity) {
				test.Fatal("object hash mismatch accepted", err)
			}
		})
	}
}

func TestDocDecodedTextReviewTailReadsValidateRawEndAndDiscardFailures(test *testing.T) {
	fixture := buildDecodedTextFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"a`+strings.Repeat(`\uD83D\uDE42`, 1024)+`"}`))
	first := decodedTextReviewFirstStep(test, fixture)
	if first.State.TextBytes-first.State.HashNext != 61 {
		test.Fatal("review needs the 61-byte staged tail")
	}
	for _, shift := range []int{-12, 12} {
		prior := first.State
		prior.Next += shift
		if err := prior.Validate(fixture.source); err != nil {
			test.Fatal("raw cursor forgery must be shape-valid", err)
		}
		step, err := StepDocDecodedText(context.Background(), fixture.source, prior,
			func(_ context.Context, from, length int) ([]byte, error) { return fixture.raw[from : from+length], nil },
			func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil })
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocDecodedTextStep{}) {
			test.Fatal("staged tail was not bound to the prior raw endpoint", shift, err)
		}
	}
	before, err := first.State.Encode(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	for _, target := range []string{"raw", "object"} {
		for _, mode := range []string{"error", "corrupt", "cancel", "cancel-error"} {
			test.Run(target+"/"+mode, func(test *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				failure := errors.New("review loader failed")
				calls := 0
				alter := func(raw []byte) ([]byte, error) {
					calls++
					switch mode {
					case "error":
						return nil, failure
					case "corrupt":
						return []byte(`{}`), nil
					case "cancel":
						cancel()
					case "cancel-error":
						cancel()
						return nil, failure
					}
					return raw, nil
				}
				step, err := StepDocDecodedText(ctx, fixture.source, first.State,
					func(_ context.Context, from, length int) ([]byte, error) {
						raw := fixture.raw[from : from+length]
						if target == "raw" {
							return alter(raw)
						}
						return raw, nil
					},
					func(_ context.Context, hash ContentHash) ([]byte, error) {
						if target == "object" {
							return alter(fixture.objects[hash])
						}
						return fixture.objects[hash], nil
					})
				want := failure
				if mode == "corrupt" {
					want = ErrIntegrity
				} else if strings.HasPrefix(mode, "cancel") {
					want = context.Canceled
				}
				after, encodeErr := first.State.Encode(fixture.source)
				if calls == 0 || !errors.Is(err, want) || !reflect.DeepEqual(step, DocDecodedTextStep{}) || encodeErr != nil || !bytes.Equal(before, after) {
					test.Fatal("failed step leaked effects or changed error precedence", calls, err, encodeErr)
				}
			})
		}
	}
}

func TestDocDecodedTextReviewGenerationNumericLimitsBeforeIO(test *testing.T) {
	fixture := buildDecodedTextFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"safe"}`))
	for name, mutate := range map[string]func(*DocDecodedTextGeneration){
		"source-size":       func(value *DocDecodedTextGeneration) { value.Source.EventBytes = math.MaxInt },
		"source-from":       func(value *DocDecodedTextGeneration) { value.Source.Token.From = math.MinInt },
		"source-to":         func(value *DocDecodedTextGeneration) { value.Source.Token.To = math.MaxInt },
		"decoded-size":      func(value *DocDecodedTextGeneration) { value.TextBytes = math.MaxInt },
		"decoded-negative":  func(value *DocDecodedTextGeneration) { value.TextBytes = math.MinInt },
		"page-count":        func(value *DocDecodedTextGeneration) { value.Pages.Count = math.MaxInt },
		"page-negative":     func(value *DocDecodedTextGeneration) { value.Pages.Count = math.MinInt },
		"lookup-count":      func(value *DocDecodedTextGeneration) { value.Lookup.Count = math.MaxInt },
		"lookup-negative":   func(value *DocDecodedTextGeneration) { value.Lookup.Count = math.MinInt },
		"empty-directory":   func(value *DocDecodedTextGeneration) { value.Pages = DocRangeDirectoryRoot{} },
		"lookup-missing":    func(value *DocDecodedTextGeneration) { value.Lookup = DocRangeDirectoryRoot{} },
		"profile-downgrade": func(value *DocDecodedTextGeneration) { value.Source.Profile = DocDisplayStringProfile },
	} {
		test.Run(name, func(test *testing.T) {
			generation := fixture.generation
			mutate(&generation)
			load := func(context.Context, ContentHash) ([]byte, error) {
				test.Fatal("invalid generation performed IO")
				return nil, nil
			}
			if data, err := ReadDocDecodedTextBytes(context.Background(), generation, 0, 1, load); !errors.Is(err, ErrIntegrity) || data != nil {
				test.Fatal("invalid generation returned data", err)
			}
			if span, err := LocateDocDecodedTextSource(context.Background(), generation, 0, 1, load); !errors.Is(err, ErrIntegrity) || span != (DocFindSpan{}) {
				test.Fatal("invalid generation returned a source span", span, err)
			}
		})
	}
}

func TestDocDecodedMarkdownReviewCompositeCursorBindingBeforeIO(test *testing.T) {
	fixture := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"**safe**"}`))
	cursor, err := NewDocDecodedMarkdownCursor(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	for name, mutate := range map[string]func(*DocDecodedMarkdownPlan, *DocDecodedMarkdownCursor){
		"origin-hash": func(plan *DocDecodedMarkdownPlan, _ *DocDecodedMarkdownCursor) {
			plan.Input.Source.EventHash = HashContent([]byte("other origin"))
		},
		"origin-size":  func(plan *DocDecodedMarkdownPlan, _ *DocDecodedMarkdownCursor) { plan.Input.Source.EventBytes++ },
		"origin-token": func(plan *DocDecodedMarkdownPlan, _ *DocDecodedMarkdownCursor) { plan.Input.Source.Token.From-- },
		"origin-profile": func(plan *DocDecodedMarkdownPlan, _ *DocDecodedMarkdownCursor) {
			plan.Input.Source.Profile = DocDisplayStringProfile
		},
		"data-directory": func(plan *DocDecodedMarkdownPlan, _ *DocDecodedMarkdownCursor) {
			plan.Input.Pages.Hash = HashContent([]byte("other pages"))
		},
		"lookup-directory": func(plan *DocDecodedMarkdownPlan, _ *DocDecodedMarkdownCursor) {
			plan.Input.Lookup.Hash = HashContent([]byte("other lookup"))
		},
		"decoded-digest": func(plan *DocDecodedMarkdownPlan, _ *DocDecodedMarkdownCursor) {
			plan.Input.Hash = HashContent([]byte("other decoded text"))
		},
		"decoded-size":  func(plan *DocDecodedMarkdownPlan, _ *DocDecodedMarkdownCursor) { plan.Input.TextBytes++ },
		"outer-version": func(_ *DocDecodedMarkdownPlan, cursor *DocDecodedMarkdownCursor) { cursor.Version = 2 },
		"outer-binding": func(_ *DocDecodedMarkdownPlan, cursor *DocDecodedMarkdownCursor) { cursor.Plan = cursor.Render.Plan },
		"inner-version": func(_ *DocDecodedMarkdownPlan, cursor *DocDecodedMarkdownCursor) { cursor.Render.Version = 2 },
		"inner-binding": func(_ *DocDecodedMarkdownPlan, cursor *DocDecodedMarkdownCursor) { cursor.Render.Plan = cursor.Plan },
		"inner-page":    func(_ *DocDecodedMarkdownPlan, cursor *DocDecodedMarkdownCursor) { cursor.Render.Page = math.MaxInt },
		"inner-byte":    func(_ *DocDecodedMarkdownPlan, cursor *DocDecodedMarkdownCursor) { cursor.Render.Byte = math.MaxInt },
		"inner-operation": func(_ *DocDecodedMarkdownPlan, cursor *DocDecodedMarkdownCursor) {
			cursor.Render.Operation = math.MaxInt
		},
	} {
		test.Run(name, func(test *testing.T) {
			plan, changed := fixture.plan, cursor
			mutate(&plan, &changed)
			fragment, err := ReadDocDecodedMarkdownFragment(context.Background(), plan, changed,
				func(context.Context, ContentHash) ([]byte, error) {
					test.Fatal("mixed cursor performed IO")
					return nil, nil
				})
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(fragment, DocDecodedMarkdownFragment{}) {
				test.Fatal("composite cursor crossed source/plan bindings", err)
			}
		})
	}
}

func TestDocDecodedMarkdownReviewCancellationDiscardsFragmentAndSpan(test *testing.T) {
	fixture := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"**a\uD83D\uDE42** [label](https://hidden)"}`))
	cursor, err := NewDocDecodedMarkdownCursor(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.decoded.objects[hash], nil }
	fragment, err := ReadDocDecodedMarkdownFragment(context.Background(), fixture.plan, cursor, load)
	if err != nil {
		test.Fatal(err)
	}
	operations := map[string]func(context.Context, func(context.Context, ContentHash) ([]byte, error)) (bool, error){
		"fragment": func(ctx context.Context, load func(context.Context, ContentHash) ([]byte, error)) (bool, error) {
			actual, err := ReadDocDecodedMarkdownFragment(ctx, fixture.plan, cursor, load)
			return reflect.DeepEqual(actual, DocDecodedMarkdownFragment{}), err
		},
		"span": func(ctx context.Context, load func(context.Context, ContentHash) ([]byte, error)) (bool, error) {
			actual, err := fragment.LocateSource(ctx, 0, len(fragment.Text()), load)
			return actual == (DocFindSpan{}), err
		},
	}
	for name, operation := range operations {
		test.Run(name, func(test *testing.T) {
			calls := 0
			if _, err := operation(context.Background(), func(ctx context.Context, hash ContentHash) ([]byte, error) {
				calls++
				return load(ctx, hash)
			}); err != nil || calls == 0 {
				test.Fatal("review operation did not exercise prepared objects", calls, err)
			}
			for failAt := 1; failAt <= calls; failAt++ {
				ctx, cancel := context.WithCancel(context.Background())
				count := 0
				zero, err := operation(ctx, func(ctx context.Context, hash ContentHash) ([]byte, error) {
					count++
					if count == failAt {
						cancel()
					}
					return load(ctx, hash)
				})
				cancel()
				if !zero || !errors.Is(err, context.Canceled) {
					test.Fatal("canceled composite operation leaked output", failAt, zero, err)
				}
			}
		})
	}
	encoded, err := json.Marshal(cursor)
	if err != nil {
		test.Fatal(err)
	}
	var restored DocDecodedMarkdownCursor
	if err := json.Unmarshal(encoded, &restored); err != nil || restored != cursor {
		test.Fatal("composite cursor JSON round trip changed its bindings", err)
	}
}

func decodedTextReviewReanchorPage(test *testing.T, fixture decodedTextFixture, index, sourceFrom int) (DocDecodedTextGeneration, map[ContentHash][]byte) {
	test.Helper()
	objects := make(map[ContentHash][]byte, len(fixture.objects))
	for hash, raw := range fixture.objects {
		objects[hash] = raw
	}
	hashes := make([]ContentHash, len(fixture.pages))
	for ordinal, original := range fixture.pages {
		page := original
		if ordinal == index {
			page.Runs = append([]DocDisplayRun(nil), original.Runs...)
			shift := sourceFrom - page.Runs[0].SourceFrom
			for position := range page.Runs {
				page.Runs[position].SourceFrom += shift
				page.Runs[position].SourceTo += shift
			}
		}
		raw, err := canonicalJSON(page)
		if err != nil {
			test.Fatal(err)
		}
		hashes[ordinal] = HashContent(raw)
		objects[hashes[ordinal]] = raw
	}
	generation := fixture.generation
	var produced []DocRangeObject
	var err error
	generation.Pages, err = buildDocRangeDirectory(context.Background(), hashes, &produced)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range produced {
		objects[object.Hash] = object.Bytes
	}
	if err := generation.Validate(); err != nil {
		test.Fatal("reanchored generation must retain valid directory counts", err)
	}
	return generation, objects
}

func TestDocDecodedTextReviewRejectsImpossibleSelectedRawPrefix(test *testing.T) {
	fixture := buildDecodedTextFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"`+strings.Repeat(`\u0061`, 4*DocDisplayPageRuns)+`"}`))
	page := fixture.pages[1]
	generation, objects := decodedTextReviewReanchorPage(test, fixture, 1, fixture.source.Token.From+1+6*page.From+1)
	load := func(_ context.Context, hash ContentHash) ([]byte, error) { return objects[hash], nil }
	actual, err := ReadDocDecodedTextPage(context.Background(), generation, 1, load)
	if !errors.Is(err, ErrIntegrity) {
		test.Fatalf("accepted impossible raw prefix: decoded_prefix=%d raw_prefix=%d page=%d error=%v", page.From, 6*page.From+1, actual.Index, err)
	}
}

func TestDocDecodedTextReviewRejectsImpossibleSkippedRawGap(test *testing.T) {
	fixture := buildDecodedTextFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"`+strings.Repeat(`\u0061`, 6*DocDisplayPageRuns)+`"}`))
	first, last := fixture.pages[0], fixture.pages[2]
	decodedGap := last.From - first.From - len(first.Text)
	blocked := make(map[ContentHash]bool)
	for index, page := range fixture.pages {
		if index != 0 && index != 2 {
			raw, err := canonicalJSON(page)
			if err != nil {
				test.Fatal(err)
			}
			blocked[HashContent(raw)] = true
		}
	}
	for name, rawGap := range map[string]int{"shorter-than-decoded-gap": 1, "longer-than-sixfold-gap": 6*decodedGap + 1} {
		test.Run(name, func(test *testing.T) {
			generation, objects := decodedTextReviewReanchorPage(test, fixture, 2, first.Runs[len(first.Runs)-1].SourceTo+rawGap)
			loads := 0
			load := func(_ context.Context, hash ContentHash) ([]byte, error) {
				if blocked[hash] {
					test.Fatal("source lookup loaded an intermediate or unrelated page", hash)
				}
				loads++
				return objects[hash], nil
			}
			span, err := LocateDocDecodedTextSource(context.Background(), generation, 0, last.From+1, load)
			if !errors.Is(err, ErrIntegrity) || span != (DocFindSpan{}) {
				test.Fatalf("accepted impossible skipped cover without reading its interior: decoded_gap=%d raw_gap=%d span=%+v loads=%d error=%v", decodedGap, rawGap, span, loads, err)
			}
		})
	}
}
