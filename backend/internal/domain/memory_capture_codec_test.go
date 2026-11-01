package domain

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func memoryCaptureTestHeader() MemoryCaptureHeader {
	return MemoryCaptureHeader{
		Version: MemoryCaptureVersion, Rules: MemoryCaptureRules,
		Generation: RepositoryMemorySourceGeneration{
			Version: RepositoryMemorySourceVersion,
			Graph: RepositoryViewSourceCheckpoint{
				Version: RepositoryViewSourceVersion, Scope: RepositoryGraphSourceScope,
				RepoID: HashContent([]byte("capture repository")), Epoch: "12345678-1234-4321-9876-123456789abc", Sequence: 9007199254740993,
			},
			Epoch: "23456789-1234-4321-9876-123456789abc", Sequence: 9007199254740995,
		},
		Selection: EffectiveMemorySelection{
			Branch: "main", SnapshotID: HashContent([]byte("capture snapshot")), CodeCommit: strings.Repeat("1", 40),
		},
		Content: "claims", PinsCaptured: true,
	}
}

func memoryCaptureTestParts() []MemoryCapturePart {
	dependency := HashContent([]byte("opaque memory object"))
	return []MemoryCapturePart{
		{Kind: "memory", Key: fmt.Sprintf("%s:%08d", dependency, 1), Value: []byte{0xff, 0, 7}},
		{Kind: "pr_pin", Key: "pin", Value: []byte(`{"id":"pin"}`)},
		{Kind: "graph:snapshot", Key: "snapshot", Sequence: 9007199254740993, Value: []byte(`{"raw":"snapshot"}`)},
		{Kind: "absent", Key: string(HashContent([]byte("missing object")))},
		{Kind: "git_pin", Key: "pin", Value: []byte(`{"id":"pin"}`)},
		{Kind: "memory", Key: fmt.Sprintf("%s:%08d", dependency, 0), Value: []byte("arbitrary canonical memory bytes")},
		{Kind: "graph:repository", Key: "repository", Value: []byte(" { }\n")},
	}
}

func memoryCaptureTestSeal(test *testing.T, header MemoryCaptureHeader, parts []MemoryCapturePart) MemoryCaptureBundle {
	test.Helper()
	bundle, err := SealMemoryCapture(context.Background(), header, parts)
	if err != nil {
		test.Fatal(err)
	}
	if err := ValidateMemoryCapture(context.Background(), bundle); err != nil {
		test.Fatal("sealed bundle failed validation", err)
	}
	return bundle
}

func memoryCaptureTestClone(parts []MemoryCapturePart) []MemoryCapturePart {
	cloned := slices.Clone(parts)
	for index := range cloned {
		cloned[index].Value = bytes.Clone(cloned[index].Value)
	}
	return cloned
}

func TestMemoryCaptureOwnedSortedDeterministic(test *testing.T) {
	header, input := memoryCaptureTestHeader(), memoryCaptureTestParts()
	original := memoryCaptureTestClone(input)
	bundle := memoryCaptureTestSeal(test, header, input)
	if !reflect.DeepEqual(input, original) || bundle.Header != header {
		test.Fatal("sealing changed input or header")
	}
	for index := 1; index < len(bundle.Parts); index++ {
		if !memoryCapturePartLess(bundle.Parts[index-1], bundle.Parts[index]) {
			test.Fatal("parts are not ordered by kind/key")
		}
	}
	for rotation := range len(input) {
		permutation := append(slices.Clone(original[rotation:]), original[:rotation]...)
		slices.Reverse(permutation)
		other := memoryCaptureTestSeal(test, header, permutation)
		if other.ID != bundle.ID || !reflect.DeepEqual(other.Parts, bundle.Parts) {
			test.Fatal("input ordering changed sealed identity")
		}
	}
	input[0].Value[0] = 'x'
	input[1].Key = "changed input key"
	if err := ValidateMemoryCapture(context.Background(), bundle); err != nil {
		test.Fatal("input mutation changed owned bundle", err)
	}
	second := memoryCaptureTestSeal(test, header, original)
	bundle.Parts[1].Value[0] = '['
	if err := ValidateMemoryCapture(context.Background(), second); err != nil || !bytes.Equal(original[4].Value, second.Parts[1].Value) {
		test.Fatal("separate captures share owned bytes", err)
	}
	if err := ValidateMemoryCapture(context.Background(), bundle); !errors.Is(err, ErrIntegrity) {
		test.Fatal("changed owned bytes accepted", err)
	}
	for _, empty := range [][]MemoryCapturePart{nil, {}} {
		if _, err := SealMemoryCapture(context.Background(), header, empty); !errors.Is(err, ErrIntegrity) {
			test.Fatal("empty capture accepted", err)
		}
	}
}

func TestMemoryCaptureRootFraming(test *testing.T) {
	bundle := memoryCaptureTestSeal(test, memoryCaptureTestHeader(), memoryCaptureTestParts())
	rawHeader, err := canonicalJSON(bundle.Header)
	if err != nil {
		test.Fatal(err)
	}
	var transcript bytes.Buffer
	transcript.WriteString("cxthub:memory-capture:v1\x00")
	writeNumber := func(value uint64) {
		test.Helper()
		if err := binary.Write(&transcript, binary.BigEndian, value); err != nil {
			test.Fatal(err)
		}
	}
	writeField := func(value []byte) {
		writeNumber(uint64(len(value)))
		transcript.Write(value)
	}
	writeField(rawHeader)
	writeNumber(uint64(len(bundle.Parts)))
	for _, part := range bundle.Parts {
		transcript.WriteString("part\x00")
		writeField([]byte(part.Kind))
		writeField([]byte(part.Key))
		writeNumber(uint64(part.Sequence))
		digest, err := ContentHashHex(HashContent(part.Value))
		if err != nil {
			test.Fatal(err)
		}
		decoded, err := hex.DecodeString(digest)
		if err != nil {
			test.Fatal(err)
		}
		transcript.Write(decoded)
	}
	if bundle.ID != HashContent(transcript.Bytes()) {
		test.Fatal("root no longer matches domain-separated length-framed transcript")
	}
}

func TestMemoryCaptureRejectsPartTampering(test *testing.T) {
	original := memoryCaptureTestSeal(test, memoryCaptureTestHeader(), memoryCaptureTestParts())
	changes := []struct {
		name   string
		change func(*MemoryCaptureBundle)
	}{
		{"missing first", func(bundle *MemoryCaptureBundle) { bundle.Parts = bundle.Parts[1:] }},
		{"missing middle", func(bundle *MemoryCaptureBundle) { bundle.Parts = append(bundle.Parts[:2], bundle.Parts[3:]...) }},
		{"missing last", func(bundle *MemoryCaptureBundle) { bundle.Parts = bundle.Parts[:len(bundle.Parts)-1] }},
		{"reordered", func(bundle *MemoryCaptureBundle) { bundle.Parts[0], bundle.Parts[1] = bundle.Parts[1], bundle.Parts[0] }},
		{"duplicate", func(bundle *MemoryCaptureBundle) { bundle.Parts = append(bundle.Parts, bundle.Parts[0]) }},
		{"changed bytes", func(bundle *MemoryCaptureBundle) { bundle.Parts[1].Value = append(bundle.Parts[1].Value, '\n') }},
		{"changed key", func(bundle *MemoryCaptureBundle) { bundle.Parts[2].Key = "other source key" }},
		{"changed sequence", func(bundle *MemoryCaptureBundle) { bundle.Parts[2].Sequence++ }},
		{"changed kind", func(bundle *MemoryCaptureBundle) { bundle.Parts[2].Kind = "graph:ref" }},
		{"changed ID", func(bundle *MemoryCaptureBundle) { bundle.ID = HashContent([]byte("another root")) }},
		{"malformed ID", func(bundle *MemoryCaptureBundle) { bundle.ID = "not a hash" }},
		{"extra part", func(bundle *MemoryCaptureBundle) {
			bundle.Parts = append(bundle.Parts, MemoryCapturePart{Kind: "pr_pin", Key: "z", Value: []byte(`{}`)})
		}},
	}
	for _, change := range changes {
		test.Run(change.name, func(test *testing.T) {
			bundle := original
			bundle.Parts = memoryCaptureTestClone(original.Parts)
			change.change(&bundle)
			if err := ValidateMemoryCapture(context.Background(), bundle); !errors.Is(err, ErrIntegrity) {
				test.Fatal("tampered bundle accepted", err)
			}
		})
	}
}

func TestMemoryCaptureBindsGenerationAndSelection(test *testing.T) {
	original := memoryCaptureTestSeal(test, memoryCaptureTestHeader(), []MemoryCapturePart{{Kind: "graph:repository", Key: "repo", Value: []byte(`{}`)}})
	changes := []struct {
		name   string
		change func(*MemoryCaptureHeader)
	}{
		{"repository", func(header *MemoryCaptureHeader) {
			header.Generation.Graph.RepoID = HashContent([]byte("other repository"))
		}},
		{"graph epoch", func(header *MemoryCaptureHeader) { header.Generation.Graph.Epoch = header.Generation.Epoch }},
		{"graph sequence precision", func(header *MemoryCaptureHeader) { header.Generation.Graph.Sequence-- }},
		{"memory epoch", func(header *MemoryCaptureHeader) { header.Generation.Epoch = header.Generation.Graph.Epoch }},
		{"memory sequence precision", func(header *MemoryCaptureHeader) { header.Generation.Sequence-- }},
		{"branch", func(header *MemoryCaptureHeader) { header.Selection.Branch = "feature/other" }},
		{"snapshot", func(header *MemoryCaptureHeader) { header.Selection.SnapshotID = HashContent([]byte("other snapshot")) }},
		{"code commit", func(header *MemoryCaptureHeader) { header.Selection.CodeCommit = strings.Repeat("2", 64) }},
		{"saved original", func(header *MemoryCaptureHeader) {
			header.Selection.Branch = ""
			header.PinsCaptured = false
			header.Selection.MemoryHash = HashContent([]byte("saved original"))
		}},
		{"without pins", func(header *MemoryCaptureHeader) { header.Selection.Branch = ""; header.PinsCaptured = false }},
		{"prompt", func(header *MemoryCaptureHeader) { header.Content = "prompt" }},
		{"all", func(header *MemoryCaptureHeader) { header.Content = "" }},
	}
	for _, change := range changes {
		test.Run(change.name, func(test *testing.T) {
			bundle := original
			change.change(&bundle.Header)
			if err := ValidateMemoryCapture(context.Background(), bundle); !errors.Is(err, ErrIntegrity) {
				test.Fatal("header swap accepted", err)
			}
			if resealed := memoryCaptureTestSeal(test, bundle.Header, bundle.Parts); resealed.ID == original.ID {
				test.Fatal("root does not bind changed header")
			}
		})
	}
}

func TestMemoryCaptureRejectsInvalidHeaders(test *testing.T) {
	changes := []func(*MemoryCaptureHeader){
		func(header *MemoryCaptureHeader) { header.Version++ },
		func(header *MemoryCaptureHeader) { header.Rules++ },
		func(header *MemoryCaptureHeader) { header.Generation.Version++ },
		func(header *MemoryCaptureHeader) { header.Generation.Sequence = -1 },
		func(header *MemoryCaptureHeader) { header.Generation.Epoch = "" },
		func(header *MemoryCaptureHeader) { header.Generation.Epoch = "00000000-0000-0000-0000-000000000000" },
		func(header *MemoryCaptureHeader) { header.Generation.Graph.Version++ },
		func(header *MemoryCaptureHeader) { header.Generation.Graph.Scope = RepositoryViewSourceScope },
		func(header *MemoryCaptureHeader) { header.Generation.Graph.RepoID = "invalid" },
		func(header *MemoryCaptureHeader) { header.Generation.Graph.Epoch = "" },
		func(header *MemoryCaptureHeader) { header.Generation.Graph.Sequence = -1 },
		func(header *MemoryCaptureHeader) { header.Selection.SnapshotID = "" },
		func(header *MemoryCaptureHeader) { header.Selection.CodeCommit = "HEAD" },
		func(header *MemoryCaptureHeader) { header.Selection.Branch = "invalid..branch" },
		func(header *MemoryCaptureHeader) { header.Selection.Branch = strings.Repeat("a", 1025) },
		func(header *MemoryCaptureHeader) { header.Selection.Branch = string([]byte{0xff}) },
		func(header *MemoryCaptureHeader) {
			header.Selection.MemoryHash = HashContent([]byte("incompatible saved original"))
		},
		func(header *MemoryCaptureHeader) { header.Content = "all" },
		func(header *MemoryCaptureHeader) { header.PinsCaptured = false },
		func(header *MemoryCaptureHeader) { header.Selection.Branch = "" },
	}
	for index, change := range changes {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			header := memoryCaptureTestHeader()
			change(&header)
			parts := memoryCaptureTestParts()
			if bundle, err := SealMemoryCapture(context.Background(), header, parts); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(bundle, MemoryCaptureBundle{}) {
				test.Fatal("invalid header sealed", err)
			}
			if err := ValidateMemoryCapture(context.Background(), MemoryCaptureBundle{ID: HashContent(nil), Header: header, Parts: parts}); !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid header validated", err)
			}
		})
	}
}

func TestMemoryCaptureRejectsInvalidPartsEvenWithReboundRoot(test *testing.T) {
	dependency := HashContent([]byte("dependency"))
	chunk := MemoryCapturePart{Kind: "memory", Key: fmt.Sprintf("%s:%08d", dependency, 0), Value: []byte("m")}
	graph := MemoryCapturePart{Kind: "graph:snapshot", Key: "key", Value: []byte(`{}`)}
	cases := [][]MemoryCapturePart{
		{graph, graph},
		{{Kind: "graph:unknown", Key: "key", Value: []byte(`{}`)}},
		{{Kind: "unknown", Key: "key"}},
		{{Kind: "graph:snapshot", Key: "", Value: []byte(`{}`)}},
		{{Kind: "graph:snapshot", Key: string([]byte{0xff}), Value: []byte(`{}`)}},
		{{Kind: "graph:snapshot", Key: "nul\x00key", Value: []byte(`{}`)}},
		{{Kind: "graph:snapshot", Key: "key", Sequence: -1, Value: []byte(`{}`)}},
		{{Kind: "graph:snapshot", Key: "key", Sequence: memoryCaptureTestHeader().Generation.Graph.Sequence + 1, Value: []byte(`{}`)}},
		{{Kind: "graph:snapshot", Key: "key", Value: []byte(`[]`)}},
		{{Kind: "graph:snapshot", Key: "key", Value: []byte(`null`)}},
		{{Kind: "graph:snapshot", Key: "key", Value: []byte(`{`)}},
		{{Kind: "graph:snapshot", Key: "key", Value: []byte("{\"x\":\"\xff\"}")}},
		{{Kind: "pr_pin", Key: "key", Value: []byte(`{} {}`)}},
		{{Kind: "git_pin", Key: "key", Sequence: 1, Value: []byte(`{}`)}},
		{{Kind: "absent", Key: "invalid"}},
		{{Kind: "absent", Key: string(dependency), Sequence: 1}},
		{{Kind: "absent", Key: string(dependency), Value: []byte(`null`)}},
		{{Kind: "absent", Key: string(dependency)}, chunk},
		{chunk, chunk},
		{{Kind: "memory", Key: chunk.Key, Sequence: 1}},
		{{Kind: "memory", Key: string(dependency) + ":0"}},
		{{Kind: "memory", Key: string(dependency) + ":000000000"}},
		{{Kind: "memory", Key: string(dependency) + ":+0000000"}},
		{{Kind: "memory", Key: strings.ToUpper(string(dependency)) + ":00000000"}},
		{{Kind: "memory", Key: string(dependency) + ":00000001"}},
		{chunk, {Kind: "memory", Key: string(dependency) + ":00000002"}},
	}
	header := memoryCaptureTestHeader()
	rawHeader, err := canonicalJSON(header)
	if err != nil {
		test.Fatal(err)
	}
	for index, parts := range cases {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			for position := range parts {
				if parts[position].Kind == "memory" && parts[position].Value == nil {
					parts[position].Value = []byte("m")
				}
			}
			identifier, err := memoryCaptureRoot(context.Background(), header.Version, rawHeader, parts)
			if err != nil {
				test.Fatal(err)
			}
			if err := ValidateMemoryCapture(context.Background(), MemoryCaptureBundle{ID: identifier, Header: header, Parts: parts}); !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid parts accepted with matching root", err)
			}
			if bundle, err := SealMemoryCapture(context.Background(), header, parts); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(bundle, MemoryCaptureBundle{}) {
				test.Fatal("invalid parts sealed", err)
			}
		})
	}
	parts := memoryCaptureTestParts()
	identifier, err := memoryCaptureRoot(context.Background(), header.Version, rawHeader, parts)
	if err != nil {
		test.Fatal(err)
	}
	if err := ValidateMemoryCapture(context.Background(), MemoryCaptureBundle{ID: identifier, Header: header, Parts: parts}); !errors.Is(err, ErrIntegrity) {
		test.Fatal("matching root allowed noncanonical order", err)
	}
	header.Selection.Branch, header.PinsCaptured = "", false
	if _, err := SealMemoryCapture(context.Background(), header, []MemoryCapturePart{{Kind: "pr_pin", Key: "pin", Value: []byte(`{}`)}}); !errors.Is(err, ErrIntegrity) {
		test.Fatal("uncaptured pins accepted", err)
	}
}

type memoryCaptureTestContext struct {
	context.Context
	checks      int
	cancelAfter int
}

func (ctx *memoryCaptureTestContext) Err() error {
	ctx.checks++
	if ctx.cancelAfter > 0 && ctx.checks >= ctx.cancelAfter {
		return context.Canceled
	}
	return nil
}

func TestMemoryCaptureCancellation(test *testing.T) {
	header, parts := memoryCaptureTestHeader(), memoryCaptureTestParts()
	valid := memoryCaptureTestSeal(test, header, parts)
	for _, operation := range []string{"seal", "validate"} {
		test.Run(operation, func(test *testing.T) {
			run := func(ctx context.Context) error {
				if operation == "validate" {
					return ValidateMemoryCapture(ctx, valid)
				}
				bundle, err := SealMemoryCapture(ctx, header, parts)
				if err != nil && !reflect.DeepEqual(bundle, MemoryCaptureBundle{}) {
					test.Fatal("canceled seal exposed partial result")
				}
				return err
			}
			counter := &memoryCaptureTestContext{Context: context.Background()}
			if err := run(counter); err != nil {
				test.Fatal(err)
			}
			for cancelAfter := 1; cancelAfter <= counter.checks; cancelAfter++ {
				ctx := &memoryCaptureTestContext{Context: context.Background(), cancelAfter: cancelAfter}
				if err := run(ctx); !errors.Is(err, context.Canceled) {
					test.Fatal("missed cancellation checkpoint", cancelAfter, err)
				}
			}
		})
	}
}

func TestMemoryCaptureEmptyChunksAreNotAbsence(test *testing.T) {
	header := memoryCaptureTestHeader()
	for _, count := range []int{1, 3} {
		original := memoryCaptureTestSeal(test, header, memoryCaptureTestChunks(count, 1))
		for index := range original.Parts {
			for _, empty := range [][]byte{nil, {}} {
				parts := memoryCaptureTestClone(original.Parts)
				parts[index].Value = empty
				if bundle, err := SealMemoryCapture(context.Background(), header, parts); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(bundle, MemoryCaptureBundle{}) {
					test.Fatal("empty chunk resealed as observed absence", count, index, err)
				}
				rawHeader, err := canonicalJSON(header)
				if err != nil {
					test.Fatal(err)
				}
				identifier, err := memoryCaptureRoot(context.Background(), header.Version, rawHeader, parts)
				if err != nil {
					test.Fatal(err)
				}
				if err := ValidateMemoryCapture(context.Background(), MemoryCaptureBundle{ID: identifier, Header: header, Parts: parts}); !errors.Is(err, ErrIntegrity) {
					test.Fatal("matching root accepted empty chunk", count, index, err)
				}
			}
		}
	}
	memoryCaptureTestSeal(test, header, []MemoryCapturePart{{Kind: "absent", Key: string(HashContent([]byte("observed absence")))}})
}
