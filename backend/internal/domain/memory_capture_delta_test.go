package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func memoryCaptureDeltaFixture(test *testing.T) (MemoryCaptureHeader, []MemoryCapturePart, MemoryCaptureBundle) {
	test.Helper()
	anchorHeader := memoryCaptureTestHeader()
	anchorHeader.Generation.Graph.Sequence -= 3
	anchor := memoryCaptureTestSeal(test, anchorHeader, []MemoryCapturePart{
		{Kind: "graph:repository", Key: "repository", Value: []byte(`{}`)},
		{Kind: "graph:snapshot", Key: "shared", Sequence: anchorHeader.Generation.Graph.Sequence, Value: []byte(`{"revision":1}`)},
		{Kind: "graph:ref", Key: "shared", Sequence: anchorHeader.Generation.Graph.Sequence, Value: []byte(`{"target":"old"}`)},
	})
	header := memoryCaptureTestHeader()
	header.Version = 2
	parts := []MemoryCapturePart{
		{Kind: "graph_ref", Key: string(anchor.ID), Sequence: anchorHeader.Generation.Graph.Sequence, Value: memoryCaptureDeltaCheckpointJSON(test, anchorHeader.Generation.Graph)},
		{Kind: "graph_delta:snapshot", Key: "shared", Sequence: header.Generation.Graph.Sequence, Value: []byte(`{"revision":2}`)},
		{Kind: "graph_delta:ref", Key: "shared", Sequence: anchorHeader.Generation.Graph.Sequence + 1},
	}
	for _, part := range memoryCaptureTestParts() {
		if !strings.HasPrefix(part.Kind, "graph:") {
			parts = append(parts, part)
		}
	}
	return header, parts, anchor
}

func memoryCaptureDeltaCheckpointJSON(test *testing.T, checkpoint RepositoryViewSourceCheckpoint) []byte {
	test.Helper()
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func memoryCaptureDeltaSort(parts []MemoryCapturePart) {
	slices.SortFunc(parts, func(left, right MemoryCapturePart) int {
		if order := strings.Compare(left.Kind, right.Kind); order != 0 {
			return order
		}
		return strings.Compare(left.Key, right.Key)
	})
}

func memoryCaptureDeltaRoot(test *testing.T, header MemoryCaptureHeader, parts []MemoryCapturePart, version int) ContentHash {
	test.Helper()
	rawHeader, err := canonicalJSON(header)
	if err != nil {
		test.Fatal(err)
	}
	var transcript bytes.Buffer
	fmt.Fprintf(&transcript, "cxthub:memory-capture:v%d\x00", version)
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
	writeNumber(uint64(len(parts)))
	for _, part := range parts {
		transcript.WriteString("part\x00")
		writeField([]byte(part.Kind))
		writeField([]byte(part.Key))
		writeNumber(uint64(part.Sequence))
		digest := sha256.Sum256(part.Value)
		transcript.Write(digest[:])
	}
	return HashContent(transcript.Bytes())
}

func memoryCaptureDeltaReject(test *testing.T, header MemoryCaptureHeader, parts []MemoryCapturePart, want error) {
	test.Helper()
	original := memoryCaptureTestClone(parts)
	if bundle, err := SealMemoryCapture(context.Background(), header, parts); !errors.Is(err, want) || !reflect.DeepEqual(bundle, MemoryCaptureBundle{}) {
		test.Fatalf("seal returned bundle=%+v, error=%v; want empty bundle and %v", bundle.Header, err, want)
	}
	ordered := memoryCaptureTestClone(parts)
	memoryCaptureDeltaSort(ordered)
	bundle := MemoryCaptureBundle{Header: header, Parts: ordered}
	bundle.ID = memoryCaptureDeltaRoot(test, header, ordered, header.Version)
	if err := ValidateMemoryCapture(context.Background(), bundle); !errors.Is(err, want) {
		test.Fatalf("matching root bypassed structural validation: got %v, want %v", err, want)
	}
	if !reflect.DeepEqual(parts, original) {
		test.Fatal("rejection mutated caller-owned parts")
	}
}

func TestMemoryCaptureDeltaV1IdentityPinned(test *testing.T) {
	if MemoryCaptureVersion != 1 || MemoryCaptureDeltaVersion != 2 || MemoryCaptureRules != 1 {
		test.Fatal("capture version or rules changed")
	}
	bundle := memoryCaptureTestSeal(test, memoryCaptureTestHeader(), memoryCaptureTestParts())
	const expected ContentHash = "sha256:0666ad9341d0fa533d30d246b3295bea6294397c439f77402ad028b0c8a8675c"
	if bundle.ID != expected || memoryCaptureDeltaRoot(test, bundle.Header, bundle.Parts, 1) != expected {
		test.Fatalf("v1 fixture identity changed: got %s, want %s", bundle.ID, expected)
	}
	if base, err := MemoryCaptureGraphBase(bundle); err != nil || !reflect.DeepEqual(base, MemoryGraphBase{}) {
		test.Fatalf("v1 full capture acquired a graph base: %+v, %v", base, err)
	}
}

func TestMemoryCaptureDeltaRoundTripOwnershipAndFraming(test *testing.T) {
	header, parts, anchor := memoryCaptureDeltaFixture(test)
	original := memoryCaptureTestClone(parts)
	bundle := memoryCaptureTestSeal(test, header, parts)
	if bundle.Header != header || !reflect.DeepEqual(parts, original) {
		test.Fatal("sealing changed header or caller-owned inputs")
	}
	base, err := MemoryCaptureGraphBase(bundle)
	if err != nil || base.ID != anchor.ID || base.Checkpoint != anchor.Header.Generation.Graph {
		test.Fatalf("full v1 anchor did not round trip: %+v, %v", base, err)
	}
	if bundle.ID != memoryCaptureDeltaRoot(test, header, bundle.Parts, 2) {
		test.Fatal("v2 root does not match its length-framed transcript")
	}
	wrongDomain := bundle
	wrongDomain.ID = memoryCaptureDeltaRoot(test, header, bundle.Parts, 1)
	if wrongDomain.ID == bundle.ID || !errors.Is(ValidateMemoryCapture(context.Background(), wrongDomain), ErrIntegrity) {
		test.Fatal("v1 root domain accepted for a v2 capture")
	}
	for rotation := range len(parts) {
		permutation := append(slices.Clone(original[rotation:]), original[:rotation]...)
		slices.Reverse(permutation)
		other := memoryCaptureTestSeal(test, header, permutation)
		if other.ID != bundle.ID || !reflect.DeepEqual(other.Parts, bundle.Parts) {
			test.Fatal("input order changed delta capture identity")
		}
	}
	parts[0].Value[0] = '['
	parts[1].Value[0] = '['
	parts[2].Key = "caller mutation"
	if err := ValidateMemoryCapture(context.Background(), bundle); err != nil {
		test.Fatal("caller mutation escaped into sealed delta", err)
	}
	for index := range bundle.Parts {
		if len(bundle.Parts[index].Value) > 0 {
			bundle.Parts[index].Value[0] ^= 1
		}
	}
	memoryCaptureTestSeal(test, header, original)
}

func TestMemoryCaptureDeltaRejectsRootTampering(test *testing.T) {
	header, parts, _ := memoryCaptureDeltaFixture(test)
	original := memoryCaptureTestSeal(test, header, parts)
	changes := []struct {
		name   string
		change func(*MemoryCaptureHeader, *[]MemoryCapturePart)
	}{
		{"anchor ID", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) {
			(*parts)[0].Key = string(HashContent([]byte("another anchor")))
		}},
		{"anchor checkpoint", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) {
			checkpoint := header.Generation.Graph
			checkpoint.Sequence = (*parts)[0].Sequence - 1
			(*parts)[0].Sequence = checkpoint.Sequence
			(*parts)[0].Value = memoryCaptureDeltaCheckpointJSON(test, checkpoint)
		}},
		{"delta kind", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) {
			(*parts)[1].Kind = "graph_delta:repository"
		}},
		{"delta key", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) { (*parts)[1].Key = "another key" }},
		{"delta sequence", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) { (*parts)[1].Sequence-- }},
		{"delta object", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) {
			(*parts)[1].Value = []byte(`{"revision":3}`)
		}},
		{"object to tombstone", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) { (*parts)[1].Value = nil }},
		{"tombstone to object", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) { (*parts)[2].Value = []byte(`{}`) }},
		{"removed delta", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) {
			*parts = append((*parts)[:1], (*parts)[2:]...)
		}},
		{"extra delta", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) {
			*parts = append(*parts, MemoryCapturePart{Kind: "graph_delta:history", Key: "extra", Sequence: header.Generation.Graph.Sequence, Value: []byte(`{}`)})
		}},
		{"graph sequence", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) { header.Generation.Graph.Sequence++ }},
		{"memory sequence", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) { header.Generation.Sequence-- }},
		{"memory epoch", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) {
			header.Generation.Epoch = header.Generation.Graph.Epoch
		}},
		{"selection", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) {
			header.Selection.Branch = "feature/delta"
		}},
		{"content", func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) { header.Content = "prompt" }},
	}
	for _, kind := range []string{"memory", "absent", "pr_pin", "git_pin"} {
		changes = append(changes, struct {
			name   string
			change func(*MemoryCaptureHeader, *[]MemoryCapturePart)
		}{kind, func(header *MemoryCaptureHeader, parts *[]MemoryCapturePart) {
			for index := range *parts {
				if (*parts)[index].Kind != kind {
					continue
				}
				if kind == "absent" {
					(*parts)[index].Key = string(HashContent([]byte("another missing object")))
				} else {
					(*parts)[index].Value = []byte(`{"changed":true}`)
				}
				return
			}
		}})
	}
	for _, entry := range changes {
		test.Run(entry.name, func(test *testing.T) {
			changedHeader, changedParts := header, memoryCaptureTestClone(parts)
			entry.change(&changedHeader, &changedParts)
			resealed := memoryCaptureTestSeal(test, changedHeader, changedParts)
			if resealed.ID == original.ID {
				test.Fatal("root omitted changed delta capture content")
			}
			resealed.ID = original.ID
			if err := ValidateMemoryCapture(context.Background(), resealed); !errors.Is(err, ErrIntegrity) {
				test.Fatal("tampering passed with the original root", err)
			}
		})
	}
	for _, identifier := range []ContentHash{"", "sha256:1234", HashContent([]byte("forged root"))} {
		changed := original
		changed.ID = identifier
		if err := ValidateMemoryCapture(context.Background(), changed); !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid delta root accepted", err)
		}
	}
	slices.Reverse(original.Parts)
	original.ID = memoryCaptureDeltaRoot(test, original.Header, original.Parts, 2)
	if err := ValidateMemoryCapture(context.Background(), original); !errors.Is(err, ErrIntegrity) {
		test.Fatal("rebound root allowed unordered delta parts", err)
	}
}

func TestMemoryCaptureDeltaRejectsInvalidGraphReferences(test *testing.T) {
	header, fixture, anchor := memoryCaptureDeltaFixture(test)
	changes := []struct {
		name   string
		change func(*MemoryCapturePart)
	}{
		{"empty ID", func(part *MemoryCapturePart) { part.Key = "" }},
		{"abbreviated ID", func(part *MemoryCapturePart) { part.Key = "sha256:1234" }},
		{"bare digest", func(part *MemoryCapturePart) { part.Key = strings.TrimPrefix(part.Key, "sha256:") }},
		{"uppercase ID", func(part *MemoryCapturePart) { part.Key = strings.ToUpper(part.Key) }},
		{"nonhex ID", func(part *MemoryCapturePart) { part.Key = "sha256:" + strings.Repeat("g", 64) }},
		{"long ID", func(part *MemoryCapturePart) { part.Key += "0" }},
		{"missing checkpoint", func(part *MemoryCapturePart) { part.Value = nil }},
		{"empty checkpoint", func(part *MemoryCapturePart) { part.Value = []byte(`{}`) }},
		{"null checkpoint", func(part *MemoryCapturePart) { part.Value = []byte(`null`) }},
		{"array checkpoint", func(part *MemoryCapturePart) { part.Value = []byte(`[]`) }},
		{"malformed checkpoint", func(part *MemoryCapturePart) { part.Value = []byte(`{"version":`) }},
		{"trailing object", func(part *MemoryCapturePart) { part.Value = append(part.Value, []byte(` {}`)...) }},
		{"trailing null", func(part *MemoryCapturePart) { part.Value = append(part.Value, []byte(` null`)...) }},
		{"invalid UTF8", func(part *MemoryCapturePart) {
			part.Value = bytes.Replace(part.Value, []byte(`"scope"`), []byte{'"', 0xff, '"'}, 1)
		}},
		{"unknown field", func(part *MemoryCapturePart) {
			part.Value = append(part.Value[:len(part.Value)-1], []byte(`,"unknown":1}`)...)
		}},
		{"duplicate field", func(part *MemoryCapturePart) {
			part.Value = append(part.Value[:len(part.Value)-1], []byte(`,"version":1}`)...)
		}},
		{"escaped duplicate field", func(part *MemoryCapturePart) {
			part.Value = append(part.Value[:len(part.Value)-1], []byte(`,"\u0076ersion":1}`)...)
		}},
		{"numeric sequence", func(part *MemoryCapturePart) {
			part.Value = bytes.Replace(part.Value, []byte(fmt.Sprintf(`"sequence":"%d"`, part.Sequence)), []byte(fmt.Sprintf(`"sequence":%d`, part.Sequence)), 1)
		}},
		{"overflow sequence", func(part *MemoryCapturePart) {
			part.Value = bytes.Replace(part.Value, []byte(fmt.Sprintf(`"sequence":"%d"`, part.Sequence)), []byte(`"sequence":"9223372036854775808"`), 1)
		}},
		{"fractional sequence", func(part *MemoryCapturePart) {
			part.Value = bytes.Replace(part.Value, []byte(fmt.Sprintf(`"sequence":"%d"`, part.Sequence)), []byte(`"sequence":"1.5"`), 1)
		}},
		{"mismatched part sequence", func(part *MemoryCapturePart) { part.Sequence++ }},
		{"negative part sequence", func(part *MemoryCapturePart) { part.Sequence = -1 }},
	}
	for _, entry := range changes {
		test.Run(entry.name, func(test *testing.T) {
			parts := memoryCaptureTestClone(fixture)
			entry.change(&parts[0])
			if base, err := MemoryCaptureGraphBase(MemoryCaptureBundle{Header: header, Parts: parts}); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(base, MemoryGraphBase{}) {
				test.Fatalf("invalid graph reference exposed base=%+v, error=%v", base, err)
			}
			memoryCaptureDeltaReject(test, header, parts, ErrIntegrity)
		})
	}
	checkpointChanges := []struct {
		name   string
		change func(*RepositoryViewSourceCheckpoint)
	}{
		{"cross repository", func(checkpoint *RepositoryViewSourceCheckpoint) {
			checkpoint.RepoID = HashContent([]byte("other repository"))
		}},
		{"invalid repository", func(checkpoint *RepositoryViewSourceCheckpoint) { checkpoint.RepoID = "invalid" }},
		{"cross scope", func(checkpoint *RepositoryViewSourceCheckpoint) { checkpoint.Scope = RepositoryViewSourceScope }},
		{"invalid scope", func(checkpoint *RepositoryViewSourceCheckpoint) { checkpoint.Scope = "unknown" }},
		{"cross epoch", func(checkpoint *RepositoryViewSourceCheckpoint) { checkpoint.Epoch = header.Generation.Epoch }},
		{"invalid epoch", func(checkpoint *RepositoryViewSourceCheckpoint) { checkpoint.Epoch = "invalid" }},
		{"zero epoch", func(checkpoint *RepositoryViewSourceCheckpoint) {
			checkpoint.Epoch = "00000000-0000-0000-0000-000000000000"
		}},
		{"missing version", func(checkpoint *RepositoryViewSourceCheckpoint) { checkpoint.Version = 0 }},
		{"future version", func(checkpoint *RepositoryViewSourceCheckpoint) { checkpoint.Version++ }},
		{"negative sequence", func(checkpoint *RepositoryViewSourceCheckpoint) { checkpoint.Sequence = -1 }},
		{"future sequence", func(checkpoint *RepositoryViewSourceCheckpoint) {
			checkpoint.Sequence = header.Generation.Graph.Sequence + 1
		}},
	}
	for _, entry := range checkpointChanges {
		test.Run(entry.name, func(test *testing.T) {
			checkpoint := anchor.Header.Generation.Graph
			entry.change(&checkpoint)
			parts := memoryCaptureTestClone(fixture)
			parts[0].Sequence, parts[0].Value = checkpoint.Sequence, memoryCaptureDeltaCheckpointJSON(test, checkpoint)
			if base, err := MemoryCaptureGraphBase(MemoryCaptureBundle{Header: header, Parts: parts}); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(base, MemoryGraphBase{}) {
				test.Fatalf("incompatible checkpoint exposed base=%+v, error=%v", base, err)
			}
			memoryCaptureDeltaReject(test, header, parts, ErrIntegrity)
		})
	}
}

func TestMemoryCaptureDeltaReferenceCardinalityAndVersion(test *testing.T) {
	header, fixture, _ := memoryCaptureDeltaFixture(test)
	for _, scenario := range []string{"missing reference", "repeated reference", "different references", "v1 reference", "v1 delta", "mixed full and delta", "v2 full graph", "unsupported version"} {
		test.Run(scenario, func(test *testing.T) {
			changedHeader, parts := header, memoryCaptureTestClone(fixture[:3])
			switch scenario {
			case "missing reference":
				parts = parts[1:]
			case "repeated reference":
				parts = append(parts, parts[0])
			case "different references":
				other := parts[0]
				other.Key = string(HashContent([]byte("different anchor")))
				parts = append(parts, other)
			case "v1 reference":
				changedHeader.Version, parts = 1, parts[:1]
			case "v1 delta":
				changedHeader.Version, parts = 1, parts[1:]
			case "mixed full and delta":
				parts = append(parts, MemoryCapturePart{Kind: "graph:repository", Key: "repository", Value: []byte(`{}`)})
			case "v2 full graph":
				parts = []MemoryCapturePart{parts[0], {Kind: "graph:repository", Key: "repository", Value: []byte(`{}`)}}
			case "unsupported version":
				changedHeader.Version = 3
			}
			memoryCaptureDeltaReject(test, changedHeader, parts, ErrIntegrity)
		})
	}
	for _, sequence := range []int64{0, header.Generation.Graph.Sequence} {
		test.Run(fmt.Sprintf("base sequence %d without deltas", sequence), func(test *testing.T) {
			checkpoint := header.Generation.Graph
			checkpoint.Sequence = sequence
			anchorHeader := header
			anchorHeader.Version, anchorHeader.Generation.Graph = 1, checkpoint
			anchor := memoryCaptureTestSeal(test, anchorHeader, []MemoryCapturePart{{Kind: "graph:repository", Key: "repository", Value: []byte(`{}`)}})
			parts := []MemoryCapturePart{{Kind: "graph_ref", Key: string(anchor.ID), Sequence: sequence, Value: memoryCaptureDeltaCheckpointJSON(test, checkpoint)}}
			bundle := memoryCaptureTestSeal(test, header, parts)
			if base, err := MemoryCaptureGraphBase(bundle); err != nil || base.ID != anchor.ID || base.Checkpoint != checkpoint {
				test.Fatal("reference-only capture rejected a valid sequence boundary", base, err)
			}
		})
	}
}

func TestMemoryCaptureDeltaLatestKindKeyAndTombstones(test *testing.T) {
	header, fixture, _ := memoryCaptureDeltaFixture(test)
	for _, sequence := range []int64{fixture[0].Sequence + 1, header.Generation.Graph.Sequence} {
		for _, value := range [][]byte{nil, {}, []byte(`{}`), []byte(" \n{\"revision\":2}\t")} {
			parts := memoryCaptureTestClone(fixture[:2])
			parts[1].Sequence, parts[1].Value = sequence, value
			memoryCaptureTestSeal(test, header, parts)
		}
	}
	parts := memoryCaptureTestClone(fixture[:2])
	parts[1].Value = nil
	nilTombstone := memoryCaptureTestSeal(test, header, parts)
	parts[1].Value = []byte{}
	emptyTombstone := memoryCaptureTestSeal(test, header, parts)
	if emptyTombstone.ID != nilTombstone.ID {
		test.Fatal("empty and nil tombstones have different identities")
	}
	for _, scenario := range []string{"duplicate", "older object", "older tombstone", "latest tombstone"} {
		test.Run(scenario, func(test *testing.T) {
			parts := memoryCaptureTestClone(fixture[:2])
			other := parts[1]
			switch scenario {
			case "older object":
				other.Sequence--
			case "older tombstone":
				other.Sequence, other.Value = other.Sequence-1, nil
			case "latest tombstone":
				parts[1].Sequence--
				other.Value = nil
			}
			memoryCaptureDeltaReject(test, header, append(parts, other), ErrIntegrity)
		})
	}
	for _, sequence := range []int64{-1, 0, fixture[0].Sequence - 1, fixture[0].Sequence, header.Generation.Graph.Sequence + 1} {
		for _, value := range [][]byte{nil, []byte(`{}`)} {
			parts := memoryCaptureTestClone(fixture[:2])
			parts[1].Sequence, parts[1].Value = sequence, value
			memoryCaptureDeltaReject(test, header, parts, ErrIntegrity)
		}
	}
	for _, kind := range []string{
		"snapshot", "ref", "history", "protocol", "repository", "pending", "unsync", "session_archive", "reflog", "revision",
		"git_commit", "git_tree", "git_delta", "git_observation", "pr_checkpoint", "pr_participation", "pr_contribution",
		"git_contribution", "git_receipt", "git_admission", "git_context_head", "git_contribution_proof",
	} {
		parts := memoryCaptureTestClone(fixture[:2])
		parts[1].Kind = "graph_delta:" + kind
		parts = append(parts, MemoryCapturePart{Kind: parts[1].Kind, Key: "other key", Sequence: parts[1].Sequence, Value: []byte(`{}`)})
		memoryCaptureTestSeal(test, header, parts)
	}
	for _, value := range [][]byte{
		[]byte(`null`), []byte(`[]`), []byte(`[{}]`), []byte(`"object"`), []byte(`1`), []byte(`true`),
		[]byte(" \n\t"), []byte(`{`), []byte(`{} {}`), []byte("{\"value\":\"\xff\"}"),
	} {
		parts := memoryCaptureTestClone(fixture[:2])
		parts[1].Value = value
		memoryCaptureDeltaReject(test, header, parts, ErrIntegrity)
	}
	for _, kind := range []string{"graph_delta:", "graph_delta:unknown", "graph_delta:graph:snapshot", "graph_delta"} {
		for _, value := range [][]byte{nil, []byte(`{}`)} {
			parts := memoryCaptureTestClone(fixture[:2])
			parts[1].Kind, parts[1].Value = kind, value
			memoryCaptureDeltaReject(test, header, parts, ErrIntegrity)
		}
	}
	for _, key := range []string{"", "nul\x00key", string([]byte{0xff})} {
		parts := memoryCaptureTestClone(fixture[:2])
		parts[1].Key = key
		memoryCaptureDeltaReject(test, header, parts, ErrIntegrity)
	}
}

func TestMemoryCaptureDeltaOtherPartsRetainV1Contract(test *testing.T) {
	header, fixture, _ := memoryCaptureDeltaFixture(test)
	dependency := HashContent([]byte("current memory"))
	chunk := MemoryCapturePart{Kind: "memory", Key: string(dependency) + ":00000000", Value: []byte{0xff, 0, 7}}
	absent := MemoryCapturePart{Kind: "absent", Key: string(dependency)}
	cases := []struct {
		name  string
		parts []MemoryCapturePart
	}{
		{"empty memory", []MemoryCapturePart{{Kind: "memory", Key: chunk.Key}}},
		{"nonzero memory sequence", []MemoryCapturePart{{Kind: "memory", Key: chunk.Key, Sequence: 1, Value: chunk.Value}}},
		{"chunk gap", []MemoryCapturePart{chunk, {Kind: "memory", Key: string(dependency) + ":00000002", Value: chunk.Value}}},
		{"absent memory collision", []MemoryCapturePart{chunk, absent}},
		{"absent value", []MemoryCapturePart{{Kind: "absent", Key: absent.Key, Value: []byte(`{}`)}}},
		{"absent sequence", []MemoryCapturePart{{Kind: "absent", Key: absent.Key, Sequence: 1}}},
		{"empty PR pin", []MemoryCapturePart{{Kind: "pr_pin", Key: "pin"}}},
		{"empty git pin", []MemoryCapturePart{{Kind: "git_pin", Key: "pin"}}},
		{"nonzero PR pin sequence", []MemoryCapturePart{{Kind: "pr_pin", Key: "pin", Sequence: 1, Value: []byte(`{}`)}}},
		{"nonzero git pin sequence", []MemoryCapturePart{{Kind: "git_pin", Key: "pin", Sequence: 1, Value: []byte(`{}`)}}},
		{"malformed PR pin", []MemoryCapturePart{{Kind: "pr_pin", Key: "pin", Value: []byte(`{} {}`)}}},
	}
	for _, entry := range cases {
		test.Run(entry.name, func(test *testing.T) {
			memoryCaptureDeltaReject(test, header, append(memoryCaptureTestClone(fixture[:1]), entry.parts...), ErrIntegrity)
			legacyHeader := header
			legacyHeader.Version = 1
			memoryCaptureDeltaReject(test, legacyHeader, entry.parts, ErrIntegrity)
		})
	}
	header.Selection.Branch, header.PinsCaptured = "", false
	for _, kind := range []string{"pr_pin", "git_pin"} {
		memoryCaptureDeltaReject(test, header, append(memoryCaptureTestClone(fixture[:1]), MemoryCapturePart{Kind: kind, Key: "pin", Value: []byte(`{}`)}), ErrIntegrity)
	}
	parts := append(memoryCaptureTestClone(fixture[:1]), chunk)
	memoryCaptureTestSeal(test, header, parts)
	header.Selection.MemoryHash = dependency
	memoryCaptureTestSeal(test, header, parts)
}

func TestMemoryCaptureDeltaPartAndByteCaps(test *testing.T) {
	if MaxMemoryCaptureDeltaParts != 4096 || MaxMemoryCaptureDeltaBytes != 4<<20 {
		test.Fatal("delta capture budgets changed")
	}
	header, fixture, _ := memoryCaptureDeltaFixture(test)
	for _, tombstones := range []bool{false, true} {
		test.Run(fmt.Sprintf("4096 parts tombstones=%v", tombstones), func(test *testing.T) {
			parts := memoryCaptureTestClone(fixture[:1])
			value := []byte(`{}`)
			if tombstones {
				value = nil
			}
			for index := range 4096 {
				parts = append(parts, MemoryCapturePart{Kind: "graph_delta:snapshot", Key: fmt.Sprintf("%08d", index), Sequence: header.Generation.Graph.Sequence, Value: value})
			}
			parts = append(parts, memoryCaptureTestDependencies(4)...)
			parts = append(parts, memoryCaptureTestPins(2, 2)...)
			memoryCaptureTestSeal(test, header, parts)
			parts = append(parts, MemoryCapturePart{Kind: "graph_delta:snapshot", Key: "overflow", Sequence: header.Generation.Graph.Sequence, Value: value})
			memoryCaptureDeltaReject(test, header, parts, ErrRepositoryViewSourceLimit)
		})
	}
	test.Run("4 MiB includes delta framing only", func(test *testing.T) {
		parts := memoryCaptureTestClone(fixture[:1])
		remaining := 4 << 20
		for index := 0; remaining > 0; index++ {
			part := MemoryCapturePart{Kind: "graph_delta:snapshot", Key: fmt.Sprintf("%08d", index), Sequence: header.Generation.Graph.Sequence}
			overhead := len(part.Kind) + len(part.Key) + 128
			part.Value = memoryCaptureTestSizedJSON(min(MaxMemoryCapturePartBytes, remaining-overhead))
			remaining -= overhead + len(part.Value)
			parts = append(parts, part)
		}
		parts = append(parts, memoryCaptureTestChunks(1, MemoryCaptureChunkBytes)...)
		parts = append(parts, memoryCaptureTestPins(1, 1024)...)
		memoryCaptureTestSeal(test, header, parts)
		lastDelta := len(parts) - 3
		parts[lastDelta].Value = append(parts[lastDelta].Value, ' ')
		memoryCaptureDeltaReject(test, header, parts, ErrRepositoryViewSourceLimit)
	})
	test.Run("per-part limit retained", func(test *testing.T) {
		parts := memoryCaptureTestClone(fixture[:2])
		parts[1].Value = memoryCaptureTestSizedJSON(MaxMemoryCapturePartBytes)
		memoryCaptureTestSeal(test, header, parts)
		parts[1].Value = append(parts[1].Value, ' ')
		memoryCaptureDeltaReject(test, header, parts, ErrRepositoryViewSourceLimit)
	})
	test.Run("key limit retained", func(test *testing.T) {
		parts := memoryCaptureTestClone(fixture[:2])
		parts[1].Key = strings.Repeat("k", MaxRepositoryViewSourceKeyBytes)
		memoryCaptureTestSeal(test, header, parts)
		parts[1].Key += "k"
		memoryCaptureDeltaReject(test, header, parts, ErrRepositoryViewSourceLimit)
	})
	test.Run("checkpoint JSON limit", func(test *testing.T) {
		parts := memoryCaptureTestClone(fixture[:1])
		parts[0].Value = append(parts[0].Value, bytes.Repeat([]byte(" "), (16<<10)-len(parts[0].Value))...)
		memoryCaptureTestSeal(test, header, parts)
		parts[0].Value = append(parts[0].Value, ' ')
		memoryCaptureDeltaReject(test, header, parts, ErrIntegrity)
	})
	for _, entry := range []struct {
		name  string
		parts func() []MemoryCapturePart
	}{
		{"memory chunk bytes", func() []MemoryCapturePart { return memoryCaptureTestChunks(1, MemoryCaptureChunkBytes+1) }},
		{"dependencies", func() []MemoryCapturePart { return memoryCaptureTestDependencies(MaxMemoryCaptureObjects + 1) }},
		{"pin count", func() []MemoryCapturePart { return memoryCaptureTestPins(MaxMemoryCaptureObjects+1, 2) }},
		{"pin bytes", func() []MemoryCapturePart {
			return memoryCaptureTestPins(MaxMemoryCapturePinBytes/MaxMemoryCapturePartBytes+1, MaxMemoryCapturePartBytes)
		}},
	} {
		test.Run(entry.name, func(test *testing.T) {
			parts := append(memoryCaptureTestClone(fixture[:1]), entry.parts()...)
			memoryCaptureDeltaReject(test, header, parts, ErrMemoryProjectionLimit)
		})
	}
}

func TestMemoryCaptureDeltaCancellation(test *testing.T) {
	header, parts, _ := memoryCaptureDeltaFixture(test)
	for index := range 1024 {
		parts = append(parts, MemoryCapturePart{Kind: "graph_delta:snapshot", Key: fmt.Sprintf("%08d", 1024-index), Sequence: header.Generation.Graph.Sequence, Value: []byte(`{}`)})
	}
	original := memoryCaptureTestClone(parts)
	valid := memoryCaptureTestSeal(test, header, parts)
	for _, operation := range []string{"seal", "validate"} {
		test.Run(operation, func(test *testing.T) {
			run := func(ctx context.Context) error {
				if operation == "validate" {
					return ValidateMemoryCapture(ctx, valid)
				}
				bundle, err := SealMemoryCapture(ctx, header, parts)
				if err != nil && !reflect.DeepEqual(bundle, MemoryCaptureBundle{}) {
					test.Fatal("cancellation exposed a partial capture")
				}
				return err
			}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if err := run(canceled); !errors.Is(err, context.Canceled) {
				test.Fatal("pre-canceled operation lost cancellation", err)
			}
			expired, release := context.WithDeadline(context.Background(), time.Unix(0, 0))
			defer release()
			if err := run(expired); !errors.Is(err, context.DeadlineExceeded) {
				test.Fatal("expired operation lost deadline", err)
			}
			counter := &memoryCaptureTestContext{Context: context.Background()}
			if err := run(counter); err != nil {
				test.Fatal(err)
			}
			for _, checkpoint := range []int{2, len(parts) / 2, len(parts) + 3, counter.checks / 2, counter.checks - 1, counter.checks} {
				ctx := &memoryCaptureTestContext{Context: context.Background(), cancelAfter: checkpoint}
				if err := run(ctx); !errors.Is(err, context.Canceled) {
					test.Fatalf("missed in-flight cancellation at check %d: %v", checkpoint, err)
				}
			}
			if !reflect.DeepEqual(parts, original) {
				test.Fatal("canceled operation mutated caller-owned parts")
			}
			if err := ValidateMemoryCapture(context.Background(), valid); err != nil {
				test.Fatal("canceled operation corrupted the sealed capture", err)
			}
		})
	}
}
