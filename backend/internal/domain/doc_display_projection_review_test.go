package domain_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func projectionReviewSource(textBytes int) domain.DocDisplayProjectionSource {
	return domain.DocDisplayProjectionSource{Version: 1, Profile: domain.DocDisplayProjectionProfile,
		EventHash: domain.HashContent([]byte("review-event")), EventBytes: 1024,
		Plan: domain.HashContent([]byte("review-plan")), TextBytes: textBytes}
}

func projectionReviewPage(test *testing.T, source domain.DocDisplayProjectionSource, index, from int, text string) domain.DocDisplayPage {
	test.Helper()
	hash, err := source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	return domain.DocDisplayPage{Version: 2, Source: hash, Index: index, From: from, Text: text,
		Runs: []domain.DocDisplayRun{{To: len(text), SourceFrom: 0, SourceTo: 1}}}
}

func projectionReviewJSON(test *testing.T, value any) []byte {
	test.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	var generic any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&generic); err != nil {
		test.Fatal(err)
	}
	raw, err = json.Marshal(generic)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func projectionReviewSelected(test *testing.T, source domain.DocDisplayProjectionSource, count, index int, pageRaw []byte) (domain.DocDisplayProjectionGeneration, map[domain.ContentHash][]byte, []domain.ContentHash) {
	test.Helper()
	selected := domain.HashContent(pageRaw)
	bodies := map[domain.ContentHash][]byte{selected: bytes.Clone(pageRaw)}
	path := []domain.ContentHash{selected}
	for level, childSpan := 0, 1; ; level, childSpan = level+1, childSpan*domain.DocRangeDirectoryFanout {
		span := childSpan * domain.DocRangeDirectoryFanout
		start := index / span * span
		hashes := make([]domain.ContentHash, min(domain.DocRangeDirectoryFanout, (count-start+childSpan-1)/childSpan))
		for offset := range hashes {
			hashes[offset] = domain.HashContent([]byte(fmt.Sprintf("unselected/%d/%d", level, start+offset*childSpan)))
		}
		hashes[(index-start)/childSpan] = selected
		raw := projectionReviewJSON(test, map[string]any{"level": level, "start": start, "hashes": hashes})
		selected = domain.HashContent(raw)
		bodies[selected] = raw
		path = append(path, selected)
		if span >= count {
			break
		}
	}
	for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
		path[left], path[right] = path[right], path[left]
	}
	return domain.DocDisplayProjectionGeneration{Version: 1, Source: source,
		Pages: domain.DocRangeDirectoryRoot{Hash: selected, Count: count}}, bodies, path
}

func projectionReviewLoad(bodies map[domain.ContentHash][]byte) func(context.Context, domain.ContentHash) ([]byte, error) {
	return func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		raw, found := bodies[hash]
		if !found {
			return nil, domain.ErrNotFound
		}
		return bytes.Clone(raw), nil
	}
}

func TestDocDisplayProjectionReviewCommitmentAndEventBounds(test *testing.T) {
	source := projectionReviewSource(2)
	page := projectionReviewPage(test, source, 0, 0, "ab")
	page.Runs = []domain.DocDisplayRun{{To: 2, SourceFrom: 1022, SourceTo: 1024, Linear: true}}
	raw, err := page.EncodeProjection(source)
	if err != nil {
		test.Fatal(err)
	}
	state, err := domain.NewDocDisplayProjectionState(source)
	if err != nil {
		test.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.DocDisplayProjectionSource){
		"plan": func(value *domain.DocDisplayProjectionSource) { value.Plan = domain.HashContent([]byte("forged-plan")) },
		"event": func(value *domain.DocDisplayProjectionSource) {
			value.EventHash = domain.HashContent([]byte("forged-event"))
		},
		"event-bound": func(value *domain.DocDisplayProjectionSource) { value.EventBytes-- },
		"text-length": func(value *domain.DocDisplayProjectionSource) { value.TextBytes++ },
	} {
		test.Run(name, func(test *testing.T) {
			changed := source
			mutate(&changed)
			if _, err := domain.DecodeDocDisplayProjectionPage(changed, domain.HashContent(raw), raw); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("old page accepted a different commitment", err)
			}
			if err := state.Validate(changed); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("old frontier accepted a different commitment", err)
			}
		})
	}
	for _, bound := range []int{-1, 0, 1025, math.MinInt, math.MaxInt} {
		changed := page
		changed.Runs = []domain.DocDisplayRun{{To: 2, SourceFrom: 0, SourceTo: bound}}
		if err := changed.ValidateProjection(source); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("out-of-event cover accepted", bound, err)
		}
	}
}

func TestDocDisplayProjectionReviewCanonicalDecoders(test *testing.T) {
	source := projectionReviewSource(2)
	page := projectionReviewPage(test, source, 0, 0, "ab")
	state, err := domain.NewDocDisplayProjectionState(source)
	if err != nil {
		test.Fatal(err)
	}
	state, _, err = domain.AppendDocDisplayProjectionPage(context.Background(), source, state, page)
	if err != nil {
		test.Fatal(err)
	}
	generation, _, err := domain.FinishDocDisplayProjection(context.Background(), source, state)
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		name    string
		value   any
		version string
		decode  func(domain.ContentHash, []byte) error
	}{
		{"source", source, "1", func(hash domain.ContentHash, raw []byte) error {
			_, err := domain.DecodeDocDisplayProjectionSource(hash, raw)
			return err
		}},
		{"page", page, "2", func(hash domain.ContentHash, raw []byte) error {
			_, err := domain.DecodeDocDisplayProjectionPage(source, hash, raw)
			return err
		}},
		{"state", state, "1", func(hash domain.ContentHash, raw []byte) error {
			_, err := domain.DecodeDocDisplayProjectionState(source, hash, raw)
			return err
		}},
		{"generation", generation, "1", func(hash domain.ContentHash, raw []byte) error {
			_, err := domain.DecodeDocDisplayProjectionGeneration(hash, raw)
			return err
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw := projectionReviewJSON(test, scenario.value)
			if err := scenario.decode(domain.HashContent(raw), raw); err != nil {
				test.Fatal("canonical fixture rejected", err)
			}
			for _, altered := range [][]byte{
				append(bytes.Clone(raw), ' '),
				append([]byte(`{"unknown":true,`), raw[1:]...),
				append([]byte(`{"version":`+scenario.version+`,`), raw[1:]...),
				bytes.Replace(raw, []byte(`"version"`), []byte(`"\u0076ersion"`), 1),
			} {
				if err := scenario.decode(domain.HashContent(altered), altered); !errors.Is(err, domain.ErrIntegrity) {
					test.Fatal("noncanonical metadata survived a recomputed hash", string(altered), err)
				}
			}
			if err := scenario.decode(domain.HashContent([]byte("unrelated")), raw); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("wrong expected hash accepted", err)
			}
		})
	}
}

func TestDocDisplayProjectionReviewV1BytesAndVersionSeparation(test *testing.T) {
	source := domain.DocDisplayStringSource{Version: 1, Profile: domain.DocDisplayStringProfile,
		EventHash: domain.HashContent([]byte(`"ab"`)), EventBytes: 4, Token: domain.DocFindSpan{From: 0, To: 4}}
	raw, err := source.Encode()
	want := fmt.Sprintf(`{"empty":false,"event_bytes":4,"event_hash":%q,"profile":"tool-result-string-v1","token":{"from":0,"to":4},"version":1}`, source.EventHash)
	if err != nil || string(raw) != want {
		test.Fatal("v1 source byte contract changed", string(raw), err)
	}
	hash := domain.HashContent(raw)
	page := domain.DocDisplayPage{Version: 1, Source: hash, Text: "ab",
		Runs: []domain.DocDisplayRun{{To: 1, SourceFrom: 1, SourceTo: 2, Linear: true}, {To: 2, SourceFrom: 2, SourceTo: 3, Linear: true}}}
	raw, err = page.Encode(source)
	want = fmt.Sprintf(`{"from":0,"index":0,"runs":[{"linear":true,"source_from":1,"source_to":2,"to":1},{"linear":true,"source_from":2,"source_to":3,"to":2}],"source":%q,"text":"ab","version":1}`, hash)
	if err != nil || string(raw) != want {
		test.Fatal("v1 page byte contract changed", string(raw), err)
	}
	projection := projectionReviewSource(2)
	if _, err := domain.DecodeDocDisplayProjectionPage(projection, domain.HashContent(raw), raw); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("v1 page upgraded without a new binding", err)
	}
	page.Version = 2
	if _, err := page.Encode(source); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("v2 page accepted by literal encoder", err)
	}
	page = projectionReviewPage(test, projection, 0, 0, "ab")
	page.Version = 1
	if _, err := page.EncodeProjection(projection); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("projection accepted downgraded version", err)
	}
	for _, runs := range [][]domain.DocDisplayRun{
		{{To: 1, SourceFrom: 1, SourceTo: 2, Linear: true}, {To: 2, SourceFrom: 1, SourceTo: 2, Linear: true}},
		{{To: 2, SourceFrom: 1, SourceTo: 3}},
	} {
		page = domain.DocDisplayPage{Version: 1, Source: hash, Text: "ab", Runs: runs}
		if _, err := page.Encode(source); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("v2 run semantics leaked into v1 encoding", err)
		}
		if _, err := page.SourceRange(0, 2); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("v2 run semantics leaked into v1 coordinates", err)
		}
	}
}

func TestDocDisplayProjectionReviewFrontierIsolation(test *testing.T) {
	source := projectionReviewSource(128 * domain.DocDisplayPageRuns)
	state, err := domain.NewDocDisplayProjectionState(source)
	if err != nil {
		test.Fatal(err)
	}
	state.TextBytes = 127 * domain.DocDisplayPageRuns
	state.Directory.Count = 127
	state.Directory.Levels = [][]domain.ContentHash{make([]domain.ContentHash, 127)}
	for index := range state.Directory.Levels[0] {
		state.Directory.Levels[0][index] = domain.HashContent([]byte(fmt.Sprintf("prior/%d", index)))
	}
	before, err := state.Encode(source)
	if err != nil {
		test.Fatal(err)
	}
	page := projectionReviewPage(test, source, 127, state.TextBytes, strings.Repeat("a", domain.DocDisplayPageRuns))
	next, objects, err := domain.AppendDocDisplayProjectionPage(context.Background(), source, state, page)
	if err != nil || !next.Done || len(objects) != 2 {
		test.Fatal("frontier flush failed", len(objects), err)
	}
	page.Runs[0].SourceTo = math.MaxInt
	for _, object := range objects {
		if domain.HashContent(object.Bytes) != object.Hash {
			test.Fatal("page mutation changed emitted objects")
		}
	}
	_, _, err = domain.FinishDocDisplayProjection(context.Background(), source, next)
	if err != nil {
		test.Fatal(err)
	}
	next.Directory.Levels[1][0] = domain.HashContent([]byte("mutated-next-frontier"))
	objects[0].Bytes[0] ^= 1
	after, err := state.Encode(source)
	if err != nil || !bytes.Equal(before, after) {
		test.Fatal("append/finish or returned-object mutation changed the prior frontier", err)
	}
}

func TestDocDisplayProjectionReviewNumericAndGenerationLimits(test *testing.T) {
	for _, value := range []int{math.MinInt, -1, domain.MaxConversationDocumentBytes + 1, math.MaxInt} {
		for _, field := range []string{"event", "text"} {
			source := projectionReviewSource(1)
			if field == "event" {
				source.EventBytes = value
			} else {
				source.TextBytes = value
			}
			if _, err := source.Encode(); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("out-of-capacity source accepted", field, value, err)
			}
		}
	}
	source := projectionReviewSource(domain.MaxConversationDocumentBytes)
	source.EventBytes = domain.MaxConversationDocumentBytes
	state, err := domain.NewDocDisplayProjectionState(source)
	if err != nil {
		test.Fatal(err)
	}
	state.TextBytes, state.Done = source.TextBytes, true
	state.Directory.Count = source.TextBytes / domain.DocDisplayPageRuns
	for remaining := state.Directory.Count; remaining > 0; remaining /= domain.DocRangeDirectoryFanout {
		hashes := make([]domain.ContentHash, remaining%domain.DocRangeDirectoryFanout)
		for index := range hashes {
			hashes[index] = domain.HashContent([]byte("bounded-frontier"))
		}
		state.Directory.Levels = append(state.Directory.Levels, hashes)
	}
	raw, err := state.Encode(source)
	if err != nil || len(raw) > domain.MaxDocDisplayStateBytes {
		test.Fatal("maximum valid frontier rejected or unbounded", len(raw), err)
	}
	decoded, err := domain.DecodeDocDisplayProjectionState(source, domain.HashContent(raw), raw)
	if err != nil || !reflect.DeepEqual(decoded, state) {
		test.Fatal("maximum frontier did not roundtrip", err)
	}
	for name, mutate := range map[string]func(*domain.DocDisplayProjectionState){
		"text-min":  func(value *domain.DocDisplayProjectionState) { value.TextBytes = math.MinInt },
		"text-max":  func(value *domain.DocDisplayProjectionState) { value.TextBytes = math.MaxInt },
		"count-min": func(value *domain.DocDisplayProjectionState) { value.Directory.Count = math.MinInt },
		"count-max": func(value *domain.DocDisplayProjectionState) { value.Directory.Count = math.MaxInt },
		"partial-done": func(value *domain.DocDisplayProjectionState) {
			value.TextBytes--
			value.Directory.Count = 0
			value.Directory.Levels = nil
		},
	} {
		test.Run(name, func(test *testing.T) {
			changed := state
			mutate(&changed)
			if err := changed.Validate(source); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("invalid numeric checkpoint accepted", err)
			}
		})
	}
	for _, generation := range []domain.DocDisplayProjectionGeneration{
		{Version: 1, Source: projectionReviewSource(0), Pages: domain.DocRangeDirectoryRoot{Hash: source.Plan}},
		{Version: 1, Source: projectionReviewSource(1), Pages: domain.DocRangeDirectoryRoot{Count: 1}},
		{Version: 1, Source: projectionReviewSource(1024), Pages: domain.DocRangeDirectoryRoot{Hash: source.Plan, Count: 3}},
		{Version: 1, Source: projectionReviewSource(domain.DocDisplayPageTextBytes + 1), Pages: domain.DocRangeDirectoryRoot{Hash: source.Plan, Count: 1}},
	} {
		if _, err := generation.Encode(); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("impossible generation accepted", generation, err)
		}
	}
}

func TestDocDisplayProjectionReviewSelectedPathsAndCancellation(test *testing.T) {
	for _, count := range []int{129, domain.MaxConversationDocumentBytes / domain.DocDisplayPageRuns} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			source := projectionReviewSource(count * domain.DocDisplayPageRuns)
			page := projectionReviewPage(test, source, count-1, (count-1)*domain.DocDisplayPageRuns, strings.Repeat("a", domain.DocDisplayPageRuns))
			raw, err := page.EncodeProjection(source)
			if err != nil {
				test.Fatal(err)
			}
			generation, bodies, path := projectionReviewSelected(test, source, count, count-1, raw)
			var loaded []domain.ContentHash
			found, err := domain.ReadDocDisplayProjectionPage(context.Background(), generation, count-1, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
				loaded = append(loaded, hash)
				return projectionReviewLoad(bodies)(ctx, hash)
			})
			if err != nil || !reflect.DeepEqual(found, page) || !reflect.DeepEqual(loaded, path) {
				test.Fatal("selected read touched siblings or returned wrong page", loaded, err)
			}
			for _, target := range path {
				ctx, cancel := context.WithCancel(context.Background())
				found, err := domain.ReadDocDisplayProjectionPage(ctx, generation, count-1, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
					if hash == target {
						cancel()
					}
					return projectionReviewLoad(bodies)(ctx, hash)
				})
				cancel()
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(found, domain.DocDisplayPage{}) {
					test.Fatal("canceled selected path returned output", err)
				}
				original := bodies[target]
				bodies[target] = append(bytes.Clone(original), ' ')
				found, err = domain.ReadDocDisplayProjectionPage(context.Background(), generation, count-1, projectionReviewLoad(bodies))
				bodies[target] = original
				if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(found, domain.DocDisplayPage{}) {
					test.Fatal("corrupt selected object returned output", err)
				}
			}
		})
	}
}

func TestDocDisplayProjectionReviewHashConsistentSelectedCorruption(test *testing.T) {
	source := projectionReviewSource(129 * domain.DocDisplayPageRuns)
	page := projectionReviewPage(test, source, 128, 128*domain.DocDisplayPageRuns, strings.Repeat("a", domain.DocDisplayPageRuns))
	raw, err := page.EncodeProjection(source)
	if err != nil {
		test.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]json.RawMessage){
		"wrong-level": func(fields map[string]json.RawMessage) { fields["level"] = json.RawMessage(`0`) },
		"wrong-start": func(fields map[string]json.RawMessage) { fields["start"] = json.RawMessage(`1`) },
		"bad-hash":    func(fields map[string]json.RawMessage) { fields["hashes"] = json.RawMessage(`["bad","bad"]`) },
		"short-node":  func(fields map[string]json.RawMessage) { fields["hashes"] = json.RawMessage(`[]`) },
	} {
		test.Run(name, func(test *testing.T) {
			generation, bodies, _ := projectionReviewSelected(test, source, 129, 128, raw)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(bodies[generation.Pages.Hash], &fields); err != nil {
				test.Fatal(err)
			}
			mutate(fields)
			altered := projectionReviewJSON(test, fields)
			generation.Pages.Hash = domain.HashContent(altered)
			bodies[generation.Pages.Hash] = altered
			found, err := domain.ReadDocDisplayProjectionPage(context.Background(), generation, 128, projectionReviewLoad(bodies))
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(found, domain.DocDisplayPage{}) {
				test.Fatal("structurally invalid directory survived rehashing", err)
			}
		})
	}
	for name, mutate := range map[string]func(*domain.DocDisplayPage){
		"wrong-ordinal": func(value *domain.DocDisplayPage) { value.Index-- },
		"wrong-source":  func(value *domain.DocDisplayPage) { value.Source = source.Plan },
		"downgrade":     func(value *domain.DocDisplayPage) { value.Version = 1 },
		"short-final": func(value *domain.DocDisplayPage) {
			value.Text = value.Text[:len(value.Text)-1]
			value.Runs = []domain.DocDisplayRun{{To: len(value.Text), SourceFrom: 0, SourceTo: 1}}
		},
	} {
		test.Run(name, func(test *testing.T) {
			changed := page
			mutate(&changed)
			generation, bodies, _ := projectionReviewSelected(test, source, 129, 128, projectionReviewJSON(test, changed))
			found, err := domain.ReadDocDisplayProjectionPage(context.Background(), generation, 128, projectionReviewLoad(bodies))
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(found, domain.DocDisplayPage{}) {
				test.Fatal("invalid selected page survived rehashing", err)
			}
		})
	}
}

type projectionReviewCancelContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (ctx *projectionReviewCancelContext) Err() error {
	ctx.remaining--
	if ctx.remaining <= 0 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestDocDisplayProjectionReviewCancellationDuringFrontierOperations(test *testing.T) {
	source := projectionReviewSource(2 * domain.DocDisplayPageRuns)
	state, err := domain.NewDocDisplayProjectionState(source)
	if err != nil {
		test.Fatal(err)
	}
	first := projectionReviewPage(test, source, 0, 0, strings.Repeat("a", domain.DocDisplayPageRuns))
	state, _, err = domain.AppendDocDisplayProjectionPage(context.Background(), source, state, first)
	if err != nil {
		test.Fatal(err)
	}
	before, err := state.Encode(source)
	if err != nil {
		test.Fatal(err)
	}
	last := projectionReviewPage(test, source, 1, state.TextBytes, strings.Repeat("b", domain.DocDisplayPageRuns))
	finished, _, err := domain.AppendDocDisplayProjectionPage(context.Background(), source, state, last)
	if err != nil {
		test.Fatal(err)
	}
	var canceledAppend, canceledFinish int
	for budget := 1; budget <= 16; budget++ {
		base, cancel := context.WithCancel(context.Background())
		ctx := &projectionReviewCancelContext{Context: base, cancel: cancel, remaining: budget}
		found, objects, err := domain.AppendDocDisplayProjectionPage(ctx, source, state, last)
		cancel()
		if errors.Is(err, context.Canceled) {
			canceledAppend++
			if !reflect.DeepEqual(found, domain.DocDisplayProjectionState{}) || objects != nil {
				test.Fatal("canceled append leaked progress", budget)
			}
		} else if err != nil || found.Validate(source) != nil {
			test.Fatal("noncanceled append failed", budget, err)
		}
		base, cancel = context.WithCancel(context.Background())
		ctx = &projectionReviewCancelContext{Context: base, cancel: cancel, remaining: budget}
		generation, objects, err := domain.FinishDocDisplayProjection(ctx, source, finished)
		cancel()
		if errors.Is(err, context.Canceled) {
			canceledFinish++
			if generation != (domain.DocDisplayProjectionGeneration{}) || objects != nil {
				test.Fatal("canceled finish leaked a generation", budget)
			}
		} else if err != nil || generation.Validate() != nil {
			test.Fatal("noncanceled finish failed", budget, err)
		}
		after, err := state.Encode(source)
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("cancellation changed caller frontier", budget, err)
		}
	}
	if canceledAppend < 3 || canceledFinish < 3 {
		test.Fatal("did not exercise mid-operation cancellation", canceledAppend, canceledFinish)
	}
}

func TestDocDisplayProjectionReviewRejectsImpossibleSelectedOrdinals(test *testing.T) {
	for _, scenario := range []struct {
		name                      string
		count, index, total, from int
		text                      string
	}{
		{"prefix-exceeds-page-capacity", 2, 1, 2 * domain.DocDisplayPageTextBytes, 2*domain.DocDisplayPageTextBytes - 1, "x"},
		{"suffix-exceeds-page-capacity", 2, 0, 2 * domain.DocDisplayPageTextBytes, 0, strings.Repeat("x", domain.DocDisplayPageRuns)},
		{"suffix-cannot-fill-nonterminal-page", 3, 0, 2*domain.DocDisplayPageRuns + 1, 0, strings.Repeat("x", 2*domain.DocDisplayPageRuns)},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			source := projectionReviewSource(scenario.total)
			page := projectionReviewPage(test, source, scenario.index, scenario.from, scenario.text)
			raw := projectionReviewJSON(test, page)
			generation, bodies, _ := projectionReviewSelected(test, source, scenario.count, scenario.index, raw)
			found, err := domain.ReadDocDisplayProjectionPage(context.Background(), generation, scenario.index, projectionReviewLoad(bodies))
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(found, domain.DocDisplayPage{}) {
				test.Errorf("selected page cannot belong to any contiguous generation: count=%d index=%d total=%d from=%d bytes=%d; returned=%d error=%v", scenario.count, scenario.index, scenario.total, scenario.from, len(scenario.text), len(found.Text), err)
			}
		})
	}
}

func BenchmarkDocDisplayProjectionReviewLiteralCoordinateLookup(benchmark *testing.B) {
	for _, size := range []int{16, 512, domain.DocDisplayPageTextBytes} {
		benchmark.Run(fmt.Sprint(size), func(benchmark *testing.B) {
			page := domain.DocDisplayPage{Version: 1, Text: strings.Repeat("a", size),
				Runs: []domain.DocDisplayRun{{To: size, SourceFrom: 1, SourceTo: size + 1, Linear: true}}}
			benchmark.ReportAllocs()
			for benchmark.Loop() {
				span, err := page.SourceRange(0, 1)
				if err != nil || span != (domain.DocFindSpan{From: 1, To: 2}) {
					benchmark.Fatal(span, err)
				}
			}
		})
	}
}
