package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func memoryReadVerifierTestNew(test *testing.T, bundle MemoryReadBundle) *MemoryReadChunkVerifier {
	test.Helper()
	verifier, err := NewMemoryReadChunkVerifier(context.Background(), bundle.Header.Source.Graph.RepoID, bundle.ID, bundle.Header)
	if err != nil || verifier == nil {
		test.Fatal("valid header rejected", err)
	}
	return verifier
}

func memoryReadVerifierTestItems(count int) []EffectiveMemoryItem {
	items := memoryReadTestItems(count)
	items[0].Kind = "code"
	items[0].Text = "preserve <scope> & \ud55c\uae00 😀"
	items[0].Code = &MemoryCodeScope{Commit: strings.Repeat("b", 40), Paths: []string{"src/main.go"}}
	items[0].PublicationIDs = []string{"publication-id"}
	items[0].Paths = []CodePathState{{Path: "src/main.go", State: "unknown", Reason: "incomplete_ancestry"}}
	return items
}

func TestMemoryReadChunkVerifierIdentities(test *testing.T) {
	items := memoryReadTestItems(130)
	bundle := memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	verifier := memoryReadVerifierTestNew(test, bundle)
	for ordinal, chunk := range bundle.Chunks {
		decoded, err := verifier.Decode(context.Background(), ordinal, chunk.Bytes)
		if err != nil {
			test.Fatal("valid chunk rejected", ordinal, err)
		}
		reference := bundle.Header.Chunks[ordinal]
		if !reflect.DeepEqual(decoded, items[reference.Start:reference.Start+reference.Count]) {
			test.Fatal("chunk lost item identity or order", ordinal)
		}
		audited, err := DecodeMemoryReadChunk(context.Background(), bundle.Header, ordinal, chunk.Bytes)
		if err != nil || !reflect.DeepEqual(decoded, audited) {
			test.Fatal("verifier disagrees with audit decoder", ordinal, err)
		}
	}
	for _, entry := range []struct {
		name       string
		repo       ContentHash
		identifier ContentHash
	}{
		{"wrong repository", HashContent([]byte("other repository")), bundle.ID},
		{"empty repository", "", bundle.ID},
		{"malformed repository", "invalid", bundle.ID},
		{"wrong identifier", bundle.Header.Source.Graph.RepoID, HashContent([]byte("other header"))},
		{"empty identifier", bundle.Header.Source.Graph.RepoID, ""},
		{"malformed identifier", bundle.Header.Source.Graph.RepoID, "invalid"},
	} {
		test.Run(entry.name, func(test *testing.T) {
			if verifier, err := NewMemoryReadChunkVerifier(context.Background(), entry.repo, entry.identifier, bundle.Header); !errors.Is(err, ErrIntegrity) || verifier != nil {
				test.Fatal("unbound identity admitted", err)
			}
		})
	}
	for _, entry := range []struct {
		name   string
		mutate func(*MemoryReadHeader)
	}{
		{"capture", func(header *MemoryReadHeader) { header.CaptureID = HashContent([]byte("other capture")) }},
		{"origin", func(header *MemoryReadHeader) { header.Origin += "/changed" }},
		{"generation", func(header *MemoryReadHeader) { header.Source.Sequence++ }},
		{"revision", func(header *MemoryReadHeader) { header.View.Revision.Graph++ }},
		{"selection", func(header *MemoryReadHeader) { header.View.Selection.SnapshotID = HashContent([]byte("other snapshot")) }},
		{"chunk", func(header *MemoryReadHeader) { header.Chunks[0].Hash = HashContent([]byte("other chunk")) }},
	} {
		test.Run("stale "+entry.name, func(test *testing.T) {
			header := memoryReadTestCopy(test, bundle.Header)
			entry.mutate(&header)
			if _, err := MemoryReadHeaderBytes(context.Background(), header); err != nil {
				test.Fatal("identity fixture is not a valid header", err)
			}
			if verifier, err := NewMemoryReadChunkVerifier(context.Background(), header.Source.Graph.RepoID, bundle.ID, header); !errors.Is(err, ErrIntegrity) || verifier != nil {
				test.Fatal("stale header identity admitted", err)
			}
		})
	}
}

func TestMemoryReadChunkVerifierHeaderValidation(test *testing.T) {
	items := memoryReadTestItems(65)
	bundle := memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	for _, entry := range []struct {
		name   string
		mutate func(*MemoryReadHeader)
		want   error
	}{
		{"version", func(header *MemoryReadHeader) { header.Version++ }, ErrIntegrity},
		{"rules", func(header *MemoryReadHeader) { header.Rules++ }, ErrIntegrity},
		{"source", func(header *MemoryReadHeader) { header.Source.Graph.RepoID = "invalid" }, ErrIntegrity},
		{"dependency", func(header *MemoryReadHeader) { header.Dependencies = []MemoryReadDependency{{Hash: "invalid"}} }, ErrIntegrity},
		{"items", func(header *MemoryReadHeader) { header.View.Items = items }, ErrIntegrity},
		{"total", func(header *MemoryReadHeader) { header.View.Total++ }, ErrIntegrity},
		{"later chunk gap", func(header *MemoryReadHeader) { header.Chunks[1].Start++ }, ErrIntegrity},
		{"later chunk hash", func(header *MemoryReadHeader) { header.Chunks[1].Hash = "invalid" }, ErrIntegrity},
		{"chunk bytes", func(header *MemoryReadHeader) { header.Chunks[1].Bytes = MaxMemoryReadChunkBytes + 1 }, ErrMemoryProjectionLimit},
		{"chunk count", func(header *MemoryReadHeader) { header.Chunks = make([]MemoryReadChunkRef, MaxMemoryReadChunks+1) }, ErrMemoryProjectionLimit},
		{"header size", func(header *MemoryReadHeader) { header.Origin = strings.Repeat("x", MaxMemoryReadHeaderBytes+1) }, ErrMemoryProjectionLimit},
	} {
		test.Run(entry.name, func(test *testing.T) {
			header := memoryReadTestCopy(test, bundle.Header)
			entry.mutate(&header)
			rawHeader, err := canonicalJSON(header)
			if err != nil {
				test.Fatal(err)
			}
			if verifier, err := NewMemoryReadChunkVerifier(context.Background(), header.Source.Graph.RepoID, HashContent(rawHeader), header); !errors.Is(err, entry.want) || verifier != nil {
				test.Fatal("matching identity bypassed header validation", err)
			}
		})
	}
}

func TestMemoryReadChunkVerifierOwnsReferences(test *testing.T) {
	items := memoryReadTestItems(65)
	header := memoryReadTestHeader(test, items)
	header.Dependencies = []MemoryReadDependency{{Hash: HashContent([]byte("dependency")), Available: true}}
	header.View.Inclusion = &BranchContext{Roots: []ContentHash{items[0].SourceSnapshot}}
	memoryReadTestDelivery(test, &header, items)
	bundle := memoryReadTestBuild(test, header, items)
	before := memoryReadTestCopy(test, bundle.Header)
	verifier := memoryReadVerifierTestNew(test, bundle)
	if !reflect.DeepEqual(bundle.Header, before) {
		test.Fatal("constructor changed caller header")
	}
	bundle.Header.Chunks[0] = bundle.Header.Chunks[1]
	bundle.Header.Chunks[1] = MemoryReadChunkRef{}
	bundle.Header.Dependencies[0].Hash = "invalid"
	bundle.Header.View.Inclusion.Roots[0] = "invalid"
	bundle.Header.View.Items = items
	bundle.Header.Version++
	for ordinal, chunk := range bundle.Chunks {
		decoded, err := verifier.Decode(context.Background(), ordinal, chunk.Bytes)
		reference := before.Chunks[ordinal]
		if err != nil || !reflect.DeepEqual(decoded, items[reference.Start:reference.Start+reference.Count]) {
			test.Fatal("decode retained caller header or chunk references", ordinal, err)
		}
	}
	bundle.Header.Chunks = nil
	if _, err := verifier.Decode(context.Background(), 1, bundle.Chunks[1].Bytes); err != nil {
		test.Fatal("replacing caller chunk slice invalidated verifier", err)
	}
}

func TestMemoryReadChunkVerifierRawOwnership(test *testing.T) {
	items := memoryReadVerifierTestItems(1)
	bundle := memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	verifier := memoryReadVerifierTestNew(test, bundle)
	raw := slices.Clone(bundle.Chunks[0].Bytes)
	decoded, err := verifier.Decode(context.Background(), 0, raw)
	if err != nil || !reflect.DeepEqual(decoded, items) {
		test.Fatal("valid nested item rejected", err)
	}
	if !bytes.Equal(raw, bundle.Chunks[0].Bytes) {
		test.Fatal("decode mutated caller bytes")
	}
	clear(raw)
	if !reflect.DeepEqual(decoded, items) {
		test.Fatal("decoded items alias caller bytes")
	}
	separate, err := verifier.Decode(context.Background(), 0, bundle.Chunks[0].Bytes)
	if err != nil {
		test.Fatal(err)
	}
	decoded[0].Text = "changed"
	decoded[0].Code.Paths[0] = "changed"
	decoded[0].PublicationIDs[0] = "changed"
	decoded[0].Paths[0].Reason = "changed"
	if !reflect.DeepEqual(separate, items) {
		test.Fatal("separate decodes share mutable output")
	}
	again, err := verifier.Decode(context.Background(), 0, bundle.Chunks[0].Bytes)
	if err != nil || !reflect.DeepEqual(again, items) {
		test.Fatal("caller mutation changed future decodes", err)
	}
}

func TestMemoryReadChunkVerifierInvalidReceiversAndOrdinals(test *testing.T) {
	items := memoryReadTestItems(1)
	bundle := memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	verifier := memoryReadVerifierTestNew(test, bundle)
	empty := memoryReadTestBuild(test, memoryReadTestHeader(test, nil), nil)
	emptyVerifier := memoryReadVerifierTestNew(test, empty)
	for _, entry := range []struct {
		name     string
		verifier *MemoryReadChunkVerifier
		ordinal  int
	}{
		{"nil", nil, 0},
		{"zero", &MemoryReadChunkVerifier{}, 0},
		{"negative", verifier, -1},
		{"past end", verifier, len(bundle.Chunks)},
		{"empty negative", emptyVerifier, -1},
		{"empty zero", emptyVerifier, 0},
		{"empty positive", emptyVerifier, 1},
	} {
		test.Run(entry.name, func(test *testing.T) {
			if decoded, err := entry.verifier.Decode(context.Background(), entry.ordinal, bundle.Chunks[0].Bytes); !errors.Is(err, ErrIntegrity) || decoded != nil {
				test.Fatal("invalid verifier or ordinal accepted", err)
			}
		})
	}
	verifierType := reflect.TypeFor[MemoryReadChunkVerifier]()
	for index := range verifierType.NumField() {
		if verifierType.Field(index).IsExported() {
			test.Fatal("verifier exposes forgeable state", verifierType.Field(index).Name)
		}
	}
}

func TestMemoryReadChunkVerifierSelectedCorruption(test *testing.T) {
	items := memoryReadTestItems(130)
	bundle := memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	verifier := memoryReadVerifierTestNew(test, bundle)
	corrupted := slices.Clone(bundle.Chunks[0].Bytes)
	corrupted[10] ^= 1
	for _, entry := range []struct {
		name string
		raw  []byte
		want error
	}{
		{"missing", nil, ErrIntegrity},
		{"wrong ordinal", bundle.Chunks[1].Bytes, ErrIntegrity},
		{"changed bytes", corrupted, ErrIntegrity},
		{"truncated", bundle.Chunks[0].Bytes[:len(bundle.Chunks[0].Bytes)-1], ErrIntegrity},
		{"oversized", make([]byte, MaxMemoryReadChunkBytes+1), ErrMemoryProjectionLimit},
	} {
		test.Run(entry.name, func(test *testing.T) {
			if decoded, err := verifier.Decode(context.Background(), 0, entry.raw); !errors.Is(err, entry.want) || decoded != nil {
				test.Fatal("corrupt selected chunk returned items", err)
			}
		})
	}
	bundle.Chunks[0].Bytes = corrupted
	if decoded, err := verifier.Decode(context.Background(), 2, bundle.Chunks[2].Bytes); err != nil || !reflect.DeepEqual(decoded, items[128:]) {
		test.Fatal("unselected corruption affected selected chunk", err)
	}
	if err := ValidateMemoryReadBundle(context.Background(), bundle); !errors.Is(err, ErrIntegrity) {
		test.Fatal("audit failed to detect unselected corruption", err)
	}
	valid := string(bundle.Chunks[2].Bytes)
	for _, entry := range []struct {
		name string
		raw  string
	}{
		{"duplicate field", strings.Replace(valid, `"kind":`, `"kind":"decision","kind":`, 1)},
		{"invalid item", strings.Replace(valid, `"legacy_summary"`, `"instruction"`, 1)},
		{"extra item", valid[:len(valid)-1] + "," + valid[1:]},
		{"trailing object", valid + `{}`},
		{"invalid unicode", strings.Replace(valid, "historical text 128", `\ud800`, 1)},
	} {
		test.Run(entry.name, func(test *testing.T) {
			header := memoryReadTestCopy(test, bundle.Header)
			raw := []byte(entry.raw)
			header.Chunks[2].Hash, header.Chunks[2].Bytes = HashContent(raw), len(raw)
			rawHeader, err := MemoryReadHeaderBytes(context.Background(), header)
			if err != nil {
				test.Fatal("corrupt chunk fixture has invalid header", err)
			}
			verifier, err := NewMemoryReadChunkVerifier(context.Background(), header.Source.Graph.RepoID, HashContent(rawHeader), header)
			if err != nil {
				test.Fatal("constructor required unselected chunk bodies", err)
			}
			if decoded, err := verifier.Decode(context.Background(), 2, raw); !errors.Is(err, ErrIntegrity) || decoded != nil {
				test.Fatal("matching chunk hash bypassed strict decode", err)
			}
		})
	}
}

func TestMemoryReadChunkVerifierConcurrentDecodes(test *testing.T) {
	items := memoryReadVerifierTestItems(130)
	bundle := memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	header := memoryReadTestCopy(test, bundle.Header)
	verifier, err := NewMemoryReadChunkVerifier(context.Background(), header.Source.Graph.RepoID, bundle.ID, header)
	if err != nil {
		test.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Go(func() {
		for range 100 {
			for ordinal := range header.Chunks {
				header.Chunks[ordinal] = MemoryReadChunkRef{}
			}
		}
	})
	for range 8 {
		workers.Go(func() {
			for range 4 {
				for ordinal, chunk := range bundle.Chunks {
					decoded, err := verifier.Decode(context.Background(), ordinal, chunk.Bytes)
					reference := bundle.Header.Chunks[ordinal]
					if err != nil || !reflect.DeepEqual(decoded, items[reference.Start:reference.Start+reference.Count]) {
						test.Error("concurrent decode changed output", ordinal, err)
						return
					}
					decoded[0].Text = "worker-owned mutation"
					if decoded[0].Code != nil {
						decoded[0].Code.Paths[0] = "worker-owned path"
						decoded[0].PublicationIDs[0] = "worker-owned publication"
						decoded[0].Paths[0].Reason = "worker-owned assessment"
					}
				}
			}
		})
	}
	workers.Wait()
	if err := ValidateMemoryReadBundle(context.Background(), bundle); err != nil {
		test.Fatal("concurrent decode mutated original chunk bytes", err)
	}
}

type memoryReadVerifierTestContext struct {
	context.Context
	checks      int
	cancelAfter int
}

func (ctx *memoryReadVerifierTestContext) Err() error {
	ctx.checks++
	if ctx.cancelAfter > 0 && ctx.checks >= ctx.cancelAfter {
		return context.Canceled
	}
	return nil
}

func TestMemoryReadChunkVerifierCancellation(test *testing.T) {
	items := memoryReadTestItems(2)
	bundle := memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	verifier := memoryReadVerifierTestNew(test, bundle)
	for _, entry := range []struct {
		name string
		run  func(*testing.T, context.Context) error
	}{
		{"constructor", func(test *testing.T, ctx context.Context) error {
			admitted, err := NewMemoryReadChunkVerifier(ctx, bundle.Header.Source.Graph.RepoID, bundle.ID, bundle.Header)
			if err != nil && admitted != nil {
				test.Fatal("canceled constructor exposed verifier")
			}
			return err
		}},
		{"decode", func(test *testing.T, ctx context.Context) error {
			decoded, err := verifier.Decode(ctx, 0, bundle.Chunks[0].Bytes)
			if err != nil && decoded != nil {
				test.Fatal("canceled decode exposed partial items")
			}
			return err
		}},
	} {
		test.Run(entry.name, func(test *testing.T) {
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if err := entry.run(test, canceled); !errors.Is(err, context.Canceled) {
				test.Fatal("operation ignored cancellation", err)
			}
			expired, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
			defer cancel()
			if err := entry.run(test, expired); !errors.Is(err, context.DeadlineExceeded) {
				test.Fatal("operation lost deadline error", err)
			}
			counter := &memoryReadVerifierTestContext{Context: context.Background()}
			if err := entry.run(test, counter); err != nil {
				test.Fatal(err)
			}
			for cancelAfter := 1; cancelAfter <= counter.checks; cancelAfter++ {
				ctx := &memoryReadVerifierTestContext{Context: context.Background(), cancelAfter: cancelAfter}
				if err := entry.run(test, ctx); !errors.Is(err, context.Canceled) {
					test.Fatal("missed cancellation checkpoint", cancelAfter, err)
				}
			}
		})
	}
	if decoded, err := verifier.Decode(context.Background(), 0, bundle.Chunks[0].Bytes); err != nil || !reflect.DeepEqual(decoded, items) {
		test.Fatal("canceled decode poisoned verifier", err)
	}
}

func TestMemoryReadChunkVerifierDecodeSkipsHeaderValidation(test *testing.T) {
	items := memoryReadTestItems(130)
	header := memoryReadTestHeader(test, items)
	for index := range 128 {
		header.Dependencies = append(header.Dependencies, MemoryReadDependency{Hash: HashContent(fmt.Appendf(nil, "dependency %d", index))})
	}
	bundle := memoryReadTestBuild(test, header, items)
	verifier := memoryReadVerifierTestNew(test, bundle)
	directContext := &memoryReadVerifierTestContext{Context: context.Background()}
	direct, err := memoryReadDecodeChunk(directContext, bundle.Header.Chunks[2], bundle.Chunks[2].Bytes)
	if err != nil {
		test.Fatal(err)
	}
	verifiedContext := &memoryReadVerifierTestContext{Context: context.Background()}
	decoded, err := verifier.Decode(verifiedContext, 2, bundle.Chunks[2].Bytes)
	if err != nil || !reflect.DeepEqual(decoded, direct) {
		test.Fatal("selected decode differs from private chunk decoder", err)
	}
	if verifiedContext.checks != directContext.checks+1 {
		test.Fatal("decode repeated header work", verifiedContext.checks, directContext.checks)
	}
}
