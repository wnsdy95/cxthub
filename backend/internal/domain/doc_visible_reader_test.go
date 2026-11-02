package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func docVisibleReaderFixture(test *testing.T, events []CIREvent) (DocVisibleGeneration, map[ContentHash][]byte) {
	test.Helper()
	generation := DocVisibleGeneration{
		Version: 1, Algorithm: DocVisibleAlgorithm, Hash: HashContent([]byte("reader document")),
		RangeGeneration: HashContent([]byte("reader ranges")), Events: len(events), Views: make(map[string]DocVisibleRoot, 3),
	}
	for _, event := range events {
		if docVisibleTestRendered(event) {
			generation.Rendered++
		}
	}
	objects := make(map[ContentHash][]byte)
	for _, mode := range []string{"all", "chat", "prompts"} {
		builder := docVisibleBuilder{ctx: context.Background(), mode: mode, total: len(events), emit: func(_ context.Context, object DocRangeObject) error {
			objects[object.Hash] = bytes.Clone(object.Bytes)
			return nil
		}}
		for _, unit := range docVisibleReference(events, mode) {
			if err := builder.append(unit); err != nil {
				test.Fatal(err)
			}
		}
		root, err := builder.finish()
		if err != nil {
			test.Fatal(err)
		}
		generation.Views[mode] = root
	}
	if err := generation.Validate(generation.Hash, generation.RangeGeneration); err != nil {
		test.Fatal(err)
	}
	return generation, objects
}

func TestDocVisibleReaderReuseAndPageBoundaries(test *testing.T) {
	events := make([]CIREvent, 4*DocVisiblePageUnits+5)
	for position := range events {
		switch position % 4 {
		case 0:
			events[position] = docVisibleTestMessage(RoleUser, "prompt")
		case 1:
			events[position] = CIREvent{Kind: EventToolResult, Output: "work"}
		case 2:
			events[position] = docVisibleTestMessage(RoleAssistant, "answer")
		case 3:
			events[position] = CIREvent{Kind: EventReasoning}
		}
	}
	generation, objects := docVisibleReaderFixture(test, events)
	loads := 0
	read, err := NewDocVisibleReader(generation, func(ctx context.Context, hash ContentHash) ([]byte, error) {
		loads++
		return docRangeGenerationLoad(objects)(ctx, hash)
	})
	if err != nil {
		test.Fatal(err)
	}
	for _, mode := range []string{"all", "chat", "prompts"} {
		for ordinal, want := range docVisibleReference(events, mode) {
			before := loads
			got, selected, err := read.Unit(context.Background(), mode, ordinal, false)
			if err != nil || got != want || selected != ordinal || loads-before != 2 {
				test.Fatalf("%s/%d: %+v/%d, %v, %d loads", mode, ordinal, got, selected, err, loads-before)
			}
			cached := read.cache.objects
			next := read.cache.next
			for _, position := range []int{want.From, want.To - 1} {
				got, selected, err = read.Unit(context.Background(), mode, position, true)
				if err != nil || got != want || selected != ordinal || read.cache.next != next || read.cache.objects != cached {
					test.Fatalf("cached %s/%d by event %d: %+v/%d, %v", mode, ordinal, position, got, selected, err)
				}
			}
			if ordinal == DocVisiblePageUnits-1 || ordinal == DocVisiblePageUnits || ordinal == generation.Views[mode].Units-1 {
				stateless, statelessOrdinal, err := ReadDocVisibleUnit(context.Background(), generation, mode, ordinal, false, docRangeGenerationLoad(objects))
				if err != nil || stateless != want || statelessOrdinal != ordinal {
					test.Fatalf("stateless boundary %s/%d: %+v/%d, %v", mode, ordinal, stateless, statelessOrdinal, err)
				}
			}
		}
	}
}

func TestDocVisibleReaderOwnsGenerationAndBoundsDecodedCache(test *testing.T) {
	events := make([]CIREvent, (docVisibleDecodedCacheObjects+2)*DocVisiblePageUnits+1)
	for position := range events {
		events[position] = docVisibleTestMessage(RoleUser, "prompt")
	}
	generation, objects := docVisibleReaderFixture(test, events)
	read, err := NewDocVisibleReader(generation, docRangeGenerationLoad(objects))
	if err != nil {
		test.Fatal(err)
	}
	generation.Views["all"] = DocVisibleRoot{}
	delete(generation.Views, "chat")
	got, _, err := read.Unit(context.Background(), "all", 0, false)
	if err != nil || got.EventIndex != 0 {
		test.Fatalf("caller changed reader generation: %+v, %v", got, err)
	}
	var firstPage *docVisiblePage
	for _, object := range read.cache.objects {
		if object.page != nil {
			firstPage = object.page
		}
	}
	if firstPage == nil {
		test.Fatal("leaf was not retained")
	}
	got.EventIndex = -1
	for position := 0; position < len(events); position += DocVisiblePageUnits {
		unit, ordinal, err := read.Unit(context.Background(), "all", position, false)
		if err != nil || unit.EventIndex != position || ordinal != position {
			test.Fatalf("bounded seek %d: %+v/%d, %v", position, unit, ordinal, err)
		}
		for _, object := range read.cache.objects {
			if object.directory != nil && len(object.directory.Children) > DocRangeDirectoryFanout || object.page != nil && len(object.page.Units) > DocVisiblePageUnits {
				test.Fatal("decoded cache retained an oversized object")
			}
		}
	}
	for _, object := range read.cache.objects {
		if object.page == firstPage {
			test.Fatal("decoded cache failed to evict old leaf")
		}
	}
	unit, ordinal, err := read.Unit(context.Background(), "all", 0, false)
	if err != nil || unit.EventIndex != 0 || ordinal != 0 {
		test.Fatalf("evicted leaf reload: %+v/%d, %v", unit, ordinal, err)
	}
}

func TestDocVisibleReaderAuthenticatesFreshObjectsOnCacheHits(test *testing.T) {
	for _, target := range []string{"directory", "page"} {
		for _, damage := range []string{"same-allocation", "replacement", "oversized", "missing", "load-error"} {
			test.Run(target+"/"+damage, func(test *testing.T) {
				generation, objects := docVisibleReaderFixture(test, docVisibleRendererEvents())
				targetHash := generation.Views["chat"].Hash
				if target == "page" {
					var directory docVisibleDirectory
					if err := json.Unmarshal(objects[targetHash], &directory); err != nil {
						test.Fatal(err)
					}
					targetHash = directory.Children[0].Hash
				}
				original := bytes.Clone(objects[targetHash])
				var loadError error
				read, err := NewDocVisibleReader(generation, func(ctx context.Context, hash ContentHash) ([]byte, error) {
					if hash == targetHash && loadError != nil {
						return nil, loadError
					}
					return docRangeGenerationLoad(objects)(ctx, hash)
				})
				if err != nil {
					test.Fatal(err)
				}
				want, wantOrdinal, err := read.Unit(context.Background(), "chat", 1, false)
				if err != nil {
					test.Fatal(err)
				}
				wantError := ErrIntegrity
				switch damage {
				case "same-allocation":
					objects[targetHash][0] = '['
				case "replacement":
					objects[targetHash] = []byte("{}")
				case "oversized":
					objects[targetHash] = bytes.Repeat([]byte(" "), MaxDocVisibleObjectBytes+1)
				case "missing":
					delete(objects, targetHash)
					wantError = ErrNotFound
				case "load-error":
					loadError, wantError = ErrConflict, ErrConflict
				}
				unit, ordinal, err := read.Unit(context.Background(), "chat", 1, false)
				if !errors.Is(err, wantError) || unit != (DocVisibleUnit{}) || ordinal != 0 {
					test.Fatalf("warm cache hid changed object: %+v/%d, %v", unit, ordinal, err)
				}
				objects[targetHash], loadError = original, nil
				unit, ordinal, err = read.Unit(context.Background(), "chat", 1, false)
				if err != nil || unit != want || ordinal != wantOrdinal {
					test.Fatalf("restored source: %+v/%d, %v", unit, ordinal, err)
				}
			})
		}
	}
}

func TestDocVisibleReaderRejectsAuthenticatedNoncanonicalObjects(test *testing.T) {
	for _, target := range []string{"directory", "page"} {
		for _, damage := range []string{"whitespace", "unknown-field", "duplicate-field"} {
			test.Run(target+"/"+damage, func(test *testing.T) {
				generation, objects := docVisibleReaderFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "first"), docVisibleTestMessage(RoleUser, "second")})
				var directory docVisibleDirectory
				if err := json.Unmarshal(objects[generation.Views["chat"].Hash], &directory); err != nil {
					test.Fatal(err)
				}
				targetHash := generation.Views["chat"].Hash
				field := `,"level":0}`
				if target == "page" {
					targetHash = directory.Children[0].Hash
					field = `,"start":0}`
				}
				changed := bytes.Clone(objects[targetHash])
				switch damage {
				case "whitespace":
					changed = append([]byte(" "), changed...)
				case "unknown-field":
					changed = append(changed[:len(changed)-1], []byte(`,"unknown":0}`)...)
				case "duplicate-field":
					changed = append(changed[:len(changed)-1], []byte(field)...)
				}
				replacementHash := HashContent(changed)
				objects[replacementHash] = changed
				if target == "page" {
					directory.Children[0].Hash = replacementHash
					raw := docVisibleTestJSON(test, directory)
					replacementHash = HashContent(raw)
					objects[replacementHash] = raw
				}
				root := generation.Views["chat"]
				root.Hash = replacementHash
				generation.Views["chat"] = root
				read, err := NewDocVisibleReader(generation, docRangeGenerationLoad(objects))
				if err != nil {
					test.Fatal(err)
				}
				if _, _, err := read.Unit(context.Background(), "all", 0, false); err != nil {
					test.Fatal(err)
				}
				for range 2 {
					unit, ordinal, err := read.Unit(context.Background(), "chat", 0, false)
					if !errors.Is(err, ErrIntegrity) || unit != (DocVisibleUnit{}) || ordinal != 0 {
						test.Fatalf("authenticated noncanonical object accepted: %+v/%d, %v", unit, ordinal, err)
					}
				}
			})
		}
	}
}

func TestDocVisibleReaderValidatesModesAndSelectionsOnCacheHits(test *testing.T) {
	generation, objects := docVisibleReaderFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "prompt"), docVisibleTestMessage(RoleAssistant, "answer")})
	generation.Views["prompts"] = generation.Views["all"]
	loads := 0
	read, err := NewDocVisibleReader(generation, func(ctx context.Context, hash ContentHash) ([]byte, error) {
		loads++
		return docRangeGenerationLoad(objects)(ctx, hash)
	})
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := read.Unit(context.Background(), "all", 0, false); err != nil {
		test.Fatal(err)
	}
	for _, byEvent := range []bool{false, true} {
		for _, position := range []int{-1, generation.Events} {
			before := loads
			unit, ordinal, err := read.Unit(context.Background(), "all", position, byEvent)
			if !errors.Is(err, ErrValidation) || unit != (DocVisibleUnit{}) || ordinal != 0 || loads != before {
				test.Fatalf("invalid position loaded objects: %+v/%d, %v", unit, ordinal, err)
			}
		}
		before := loads
		unit, ordinal, err := read.Unit(context.Background(), "invalid", 0, byEvent)
		if !errors.Is(err, ErrValidation) || unit != (DocVisibleUnit{}) || ordinal != 0 || loads != before {
			test.Fatalf("invalid mode loaded objects: %+v/%d, %v", unit, ordinal, err)
		}
		unit, ordinal, err = read.Unit(context.Background(), "prompts", 0, byEvent)
		if !errors.Is(err, ErrIntegrity) || unit != (DocVisibleUnit{}) || ordinal != 0 {
			test.Fatalf("cached all leaf bypassed prompts validation: %+v/%d, %v", unit, ordinal, err)
		}
	}
	if read, err := NewDocVisibleReader(generation, nil); !errors.Is(err, ErrValidation) || read != nil {
		test.Fatalf("nil loader accepted: %v", err)
	}
	if read, err := NewDocVisibleReader(DocVisibleGeneration{}, docRangeGenerationLoad(objects)); !errors.Is(err, ErrIntegrity) || read != nil {
		test.Fatalf("invalid generation accepted: %v", err)
	}
}

func TestDocVisibleReaderWarmCancellationAndFreshContext(test *testing.T) {
	generation, objects := docVisibleReaderFixture(test, docVisibleRendererEvents())
	type bindingKey struct{}
	bound := context.WithValue(context.Background(), bindingKey{}, true)
	read, err := NewDocVisibleReader(generation, func(ctx context.Context, hash ContentHash) ([]byte, error) {
		if ctx.Value(bindingKey{}) != true {
			return nil, ErrConflict
		}
		return docRangeGenerationLoad(objects)(ctx, hash)
	})
	if err != nil {
		test.Fatal(err)
	}
	for _, mode := range []string{"all", "chat", "prompts"} {
		for _, byEvent := range []bool{false, true} {
			position := generation.Views[mode].Units - 1
			if byEvent {
				position = generation.Events - 1
			}
			if _, _, err := read.Unit(bound, mode, position, byEvent); err != nil {
				test.Fatal(err)
			}
			checks := 0
			if _, _, err := read.Unit(canonicalAdmissionContext{Context: bound, check: func() { checks++ }}, mode, position, byEvent); err != nil || checks < 2 {
				test.Fatalf("warm baseline: %d checkpoints, %v", checks, err)
			}
			for cancelAt := 1; cancelAt <= checks; cancelAt++ {
				ctx, cancel := context.WithCancel(bound)
				seen := 0
				unit, ordinal, err := read.Unit(canonicalAdmissionContext{Context: ctx, check: func() {
					seen++
					if seen == cancelAt {
						cancel()
					}
				}}, mode, position, byEvent)
				cancel()
				if !errors.Is(err, context.Canceled) || unit != (DocVisibleUnit{}) || ordinal != 0 {
					test.Fatalf("%s/byEvent=%t/checkpoint=%d: %+v/%d, %v", mode, byEvent, cancelAt, unit, ordinal, err)
				}
			}
			unit, ordinal, err := read.Unit(context.Background(), mode, position, byEvent)
			if !errors.Is(err, ErrConflict) || unit != (DocVisibleUnit{}) || ordinal != 0 {
				test.Fatalf("cache ignored fresh context binding: %+v/%d, %v", unit, ordinal, err)
			}
		}
	}
}

type docVisibleReaderWaitingContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *docVisibleReaderWaitingContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestDocVisibleReaderCancellationWhileWaiting(test *testing.T) {
	generation, objects := docVisibleReaderFixture(test, docVisibleRendererEvents())
	entered := make(chan struct{})
	release := make(chan struct{})
	var block sync.Once
	read, err := NewDocVisibleReader(generation, func(ctx context.Context, hash ContentHash) ([]byte, error) {
		block.Do(func() {
			close(entered)
			<-release
		})
		return docRangeGenerationLoad(objects)(ctx, hash)
	})
	if err != nil {
		test.Fatal(err)
	}
	active := make(chan error, 1)
	defer func() {
		close(release)
		select {
		case err := <-active:
			if err != nil {
				test.Error(err)
			}
		case <-time.After(5 * time.Second):
			test.Error("active read did not finish after release")
		}
	}()
	go func() {
		_, _, err := read.Unit(context.Background(), "chat", 0, false)
		active <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		test.Fatal("active read did not enter loader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := &docVisibleReaderWaitingContext{Context: ctx, waiting: make(chan struct{})}
	completed := make(chan error, 1)
	go func() {
		unit, ordinal, err := read.Unit(waiting, "chat", 0, false)
		if !errors.Is(err, context.Canceled) || unit != (DocVisibleUnit{}) || ordinal != 0 {
			completed <- fmt.Errorf("waiting cancellation returned %+v/%d, %v", unit, ordinal, err)
			return
		}
		completed <- nil
	}()
	select {
	case <-waiting.waiting:
	case <-time.After(5 * time.Second):
		test.Fatal("waiting read did not observe its context")
	}
	cancel()
	select {
	case err := <-completed:
		if err != nil {
			test.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("cancellation waited for the active read")
	}
	select {
	case err := <-active:
		test.Fatalf("active read escaped blocked loader: %v", err)
	default:
	}
}

func TestDocVisibleReaderConcurrentReads(test *testing.T) {
	events := docVisibleRendererEvents()
	generation, objects := docVisibleReaderFixture(test, events)
	read, err := NewDocVisibleReader(generation, docRangeGenerationLoad(objects))
	if err != nil {
		test.Fatal(err)
	}
	var workers sync.WaitGroup
	failures := make(chan error, 8)
	for worker := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := range 12 {
				mode := []string{"all", "chat", "prompts"}[(worker+iteration)%3]
				expected := docVisibleReference(events, mode)
				selected := (worker + iteration) % len(expected)
				want := expected[selected]
				byEvent := iteration%2 == 0
				position := selected
				if byEvent {
					position = want.To - 1
				}
				unit, ordinal, err := read.Unit(context.Background(), mode, position, byEvent)
				if err != nil || unit != want || ordinal != selected {
					failures <- fmt.Errorf("%s/%d: %+v/%d, %v", mode, position, unit, ordinal, err)
					return
				}
			}
		}()
	}
	workers.Wait()
	close(failures)
	for failure := range failures {
		test.Error(failure)
	}
}
