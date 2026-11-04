package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func messageInputReaderFixture(test *testing.T) messageInputFixture {
	test.Helper()
	return buildMessageInputFixture(test, []byte(`{"kind":"message","blocks":[`+strings.Repeat(`{"type":"text","text":"body"},`, DocMessageInputPageEntries)+`{"type":"image","name":"asset"}]}`))
}

func TestDocMessageInputReaderParityAndSinglePageProof(test *testing.T) {
	fixture := messageInputReaderFixture(test)
	want := messageFixtureInputs(test, fixture)
	if len(want) != DocMessageInputPageEntries+1 || fixture.generation.Pages.Count != 2 {
		test.Fatal("fixture must cross exactly one message input page boundary")
	}
	reader := docMessageInputReader{generation: fixture.generation}
	var previous, first *DocFindSpan
	previousIndex := -1
	for _, index := range []int{0, 0, 1, 1, 0} {
		var publicLoads, privateLoads []ContentHash
		public, publicErr := ReadDocMessageInputs(context.Background(), fixture.generation, index, func(_ context.Context, hash ContentHash) ([]byte, error) {
			publicLoads = append(publicLoads, hash)
			return fixture.objects[hash], nil
		})
		inputs, err := reader.read(context.Background(), index, func(_ context.Context, hash ContentHash) ([]byte, error) {
			privateLoads = append(privateLoads, hash)
			return fixture.objects[hash], nil
		})
		from := index * DocMessageInputPageEntries
		if err != nil || publicErr != nil || !reflect.DeepEqual(inputs, public) || !reflect.DeepEqual(inputs, want[from:min(from+DocMessageInputPageEntries, len(want))]) || !reflect.DeepEqual(privateLoads, publicLoads) || len(privateLoads) != 2 {
			test.Fatal("public parity or physical loads changed", index, privateLoads, publicLoads, err, publicErr)
		}
		if reader.cachedGeneration != fixture.generation || reader.index != index || reader.hash != privateLoads[1] || !reflect.DeepEqual(reader.inputs, inputs) {
			test.Fatal("proof is not bound to generation, index and resolved hash")
		}
		proof := reader.inputs[0].entry.Type
		if proof == nil || proof == public[0].entry.Type || (index == previousIndex && proof != previous) || (index != previousIndex && proof == previous) || (index == 0 && previousIndex == 1 && proof == first) {
			test.Fatal("parsed proof not reused, evicted, or request-local", index)
		}
		if first == nil {
			first = proof
		}
		previous, previousIndex = proof, index
	}
}

func TestDocMessageInputReaderReloadsAndOwnsParsedProof(test *testing.T) {
	fixture := messageInputReaderFixture(test)
	want := messageFixtureInputs(test, fixture)[:DocMessageInputPageEntries]
	load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil }
	reader := docMessageInputReader{generation: fixture.generation}
	if _, err := reader.read(context.Background(), 0, load); err != nil {
		test.Fatal(err)
	}
	proof := reader.inputs[0].entry.Type
	raw := fixture.objects[reader.hash]
	original := bytes.Clone(raw)
	raw[0] ^= 1
	if !reflect.DeepEqual(reader.inputs, want) {
		test.Fatal("parsed proof aliases loader-owned raw bytes")
	}
	for _, malformed := range [][]byte{raw, nil, bytes.Repeat([]byte(" "), MaxDocMessageInputPageBytes+1), append(bytes.Clone(original), ' ')} {
		fixture.objects[reader.hash] = malformed
		loads := 0
		inputs, err := reader.read(context.Background(), 0, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			loads++
			return load(ctx, hash)
		})
		if !errors.Is(err, ErrIntegrity) || inputs != nil || loads != 2 || len(reader.inputs) != len(want) || reader.inputs[0].entry.Type != proof || !reflect.DeepEqual(reader.inputs, want) {
			test.Fatal("cache hit bypassed raw checks or replaced valid proof", len(malformed), loads, err)
		}
	}
	copy(raw, original)
	fixture.objects[reader.hash] = raw
	if inputs, err := reader.read(context.Background(), 0, load); err != nil || !reflect.DeepEqual(inputs, want) || reader.inputs[0].entry.Type != proof {
		test.Fatal("restored raw did not recover the prior valid proof", err)
	}
}

func TestDocMessageInputReaderGenerationAndInstanceBinding(test *testing.T) {
	fixture := messageInputReaderFixture(test)
	load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil }
	reader := docMessageInputReader{generation: fixture.generation}
	independent := docMessageInputReader{generation: fixture.generation}
	for _, candidate := range []*docMessageInputReader{&reader, &independent} {
		if _, err := candidate.read(context.Background(), 0, load); err != nil {
			test.Fatal(err)
		}
	}
	proof := reader.inputs[0].entry.Type
	if proof == independent.inputs[0].entry.Type || &reader.inputs[0] == &independent.inputs[0] {
		test.Fatal("independent readers shared pointer or slice cache state")
	}
	reader.generation.Count++
	if inputs, err := reader.read(context.Background(), 0, load); err != nil || len(inputs) != DocMessageInputPageEntries || reader.cachedGeneration != reader.generation || reader.inputs[0].entry.Type == proof {
		test.Fatal("same hash and source reused a different generation's proof", err)
	}
	proof = reader.inputs[0].entry.Type
	for _, scenario := range []struct {
		name   string
		mutate func(*DocMessageInputGeneration)
		loads  int
	}{
		{"foreign-source", func(generation *DocMessageInputGeneration) { generation.Source.Summary = !generation.Source.Summary }, 2},
		{"invalid-generation", func(generation *DocMessageInputGeneration) { generation.Version++ }, 0},
	} {
		generation := reader.generation
		scenario.mutate(&generation)
		reader.generation = generation
		loads := 0
		inputs, err := reader.read(context.Background(), 0, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			loads++
			return load(ctx, hash)
		})
		if !errors.Is(err, ErrIntegrity) || inputs != nil || loads != scenario.loads || reader.inputs[0].entry.Type != proof {
			test.Fatal("foreign or invalid generation admitted", scenario.name, loads, err)
		}
		reader.generation = reader.cachedGeneration
	}
	if inputs, err := reader.read(context.Background(), 0, nil); !errors.Is(err, ErrIntegrity) || inputs != nil {
		test.Fatal("warm proof bypassed loader validation", err)
	}
}

func TestDocMessageInputReaderFailuresAndCancellationKeepPriorProof(test *testing.T) {
	fixture := messageInputReaderFixture(test)
	load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil }
	loadFailure := errors.New("message input load failed")
	for _, scenario := range []struct {
		name      string
		index     int
		cancelAt  int
		failAt    int
		corruptAt int
		want      error
		loads     int
	}{
		{"negative-index", -1, 0, 0, 0, ErrIntegrity, 0},
		{"past-last-index", 2, 0, 0, 0, ErrIntegrity, 0},
		{"directory-load-error", 0, 0, 1, 0, loadFailure, 1},
		{"page-load-error", 0, 0, 2, 0, loadFailure, 2},
		{"directory-integrity", 0, 0, 0, 1, ErrIntegrity, 1},
		{"pre-cancelled", 0, -1, 0, 0, context.Canceled, 0},
		{"cancel-directory", 0, 1, 0, 0, context.Canceled, 1},
		{"cancel-directory-error", 0, 1, 1, 0, context.Canceled, 1},
		{"cancel-directory-integrity", 0, 1, 0, 1, context.Canceled, 1},
		{"cancel-page", 0, 2, 0, 0, context.Canceled, 2},
		{"cancel-page-error", 0, 2, 2, 0, context.Canceled, 2},
		{"cancel-page-integrity", 0, 2, 0, 2, context.Canceled, 2},
		{"cancel-second-page", 1, 2, 0, 0, context.Canceled, 2},
		{"second-page-load-error", 1, 0, 2, 0, loadFailure, 2},
		{"second-page-integrity", 1, 0, 0, 2, ErrIntegrity, 2},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			reader := docMessageInputReader{generation: fixture.generation}
			if _, err := reader.read(context.Background(), 0, load); err != nil {
				test.Fatal(err)
			}
			prior := reader
			proof := reader.inputs[0].entry.Type
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario.cancelAt < 0 {
				cancel()
			}
			loads := 0
			inputs, err := reader.read(ctx, scenario.index, func(_ context.Context, hash ContentHash) ([]byte, error) {
				loads++
				if loads == scenario.cancelAt {
					cancel()
				}
				if loads == scenario.failAt {
					return fixture.objects[hash], loadFailure
				}
				if loads == scenario.corruptAt {
					return []byte("corrupt"), nil
				}
				return fixture.objects[hash], nil
			})
			if !errors.Is(err, scenario.want) || inputs != nil || loads != scenario.loads {
				test.Fatal("failure precedence, load count, or partial exposure changed", loads, err)
			}
			if reader.cachedGeneration != prior.cachedGeneration || reader.index != prior.index || reader.hash != prior.hash || !reflect.DeepEqual(reader.inputs, prior.inputs) {
				test.Fatal("failed read replaced prior valid proof")
			}
			if recovered, err := reader.read(context.Background(), 0, load); err != nil || !reflect.DeepEqual(recovered, prior.inputs) || reader.inputs[0].entry.Type != proof {
				test.Fatal("failed read evicted the prior parsed proof", err)
			}
		})
	}
}
