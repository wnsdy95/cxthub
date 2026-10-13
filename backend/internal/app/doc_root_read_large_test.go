package app

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rootReadLargeFixture struct {
	Repo, Base, Target domain.ContentHash
	Bytes              int64
	Events, Chunks     int
	BaseEvents         int
}

func writeRootReadLargeFixture(test *testing.T, dir string) rootReadLargeFixture {
	test.Helper()
	const streamBytes, eventCount = 309851925, 87449
	repo := h('1')
	storage := store.NewFSStore(dir)
	envelope := historyEnvelope()
	empty, err := domain.CanonicalBytes(domain.CIRDocument{Envelope: envelope, Events: []domain.CIREvent{}})
	if err != nil {
		test.Fatal(err)
	}
	var outer struct{ Envelope json.RawMessage }
	if err := json.Unmarshal(empty, &outer); err != nil {
		test.Fatal(err)
	}
	eventBytes := func(sequence int, text string) []byte {
		event := historyMessage(domain.RoleUser, text)
		event.Seq = sequence
		raw, err := domain.CanonicalBytes(domain.CIRDocument{Envelope: envelope, Events: []domain.CIREvent{event}})
		if err != nil {
			test.Fatal(err)
		}
		return raw[len(empty)-2 : len(raw)-2]
	}
	tail := eventBytes(eventCount-1, "new answer")
	fixed := len(eventBytes(0, "x")) - 1
	overhead := len(tail) + eventCount - 1
	for sequence := 0; sequence < eventCount-1; sequence++ {
		overhead += fixed + len(strconv.Itoa(sequence)) - 1
	}
	payload := streamBytes - overhead
	text := strings.Repeat("s", payload/(eventCount-1))
	longText := text + "s"
	var chunks []domain.ConversationManifestChunk
	buffer := make([]byte, 0, domain.ConversationManifestChunkBytes)
	storeChunk := func(body []byte) domain.ConversationManifestChunk {
		hash := domain.HashContent(body)
		if _, _, err := storage.PutChunks(systemTestContext(), repo, map[domain.ContentHash][]byte{hash: body}); err != nil {
			test.Fatal(err)
		}
		return domain.ConversationManifestChunk{Hash: hash, Bytes: int64(len(body))}
	}
	appendBytes := func(body []byte) {
		for len(body) > 0 {
			count := min(domain.ConversationManifestChunkBytes-len(buffer), len(body))
			buffer = append(buffer, body[:count]...)
			body = body[count:]
			if len(buffer) == domain.ConversationManifestChunkBytes {
				chunks = append(chunks, storeChunk(buffer))
				buffer = buffer[:0]
			}
		}
	}
	publish := func(count int) (domain.ContentHash, domain.ConversationManifest) {
		order := append([]domain.ConversationManifestChunk{}, chunks...)
		if len(buffer) > 0 {
			order = append(order, storeChunk(buffer))
		}
		manifest, err := domain.NewConversationManifest(outer.Envelope, order, int64(count))
		if err != nil {
			test.Fatal(err)
		}
		hash, err := domain.ConversationManifestHash(manifest)
		if err != nil {
			test.Fatal(err)
		}
		raw, err := domain.CanonicalConversationManifest(manifest)
		if err != nil {
			test.Fatal(err)
		}
		path := rootReadObjectPath(dir, repo, "docs", hash)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			test.Fatal(err)
		}
		if err := storage.PutSnapshot(systemTestContext(), domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1, Branch: "main"}); err != nil {
			test.Fatal(err)
		}
		return hash, manifest
	}
	for sequence := 0; sequence < eventCount-1; sequence++ {
		if sequence > 0 {
			appendBytes([]byte{','})
		}
		value := text
		if sequence < payload%(eventCount-1) {
			value = longText
		}
		appendBytes(eventBytes(sequence, value))
	}
	base, _ := publish(eventCount - 1)
	appendBytes([]byte{','})
	appendBytes(tail)
	target, manifest := publish(eventCount)
	if manifest.StreamBytes != streamBytes || len(manifest.Chunks) != 591 {
		test.Fatalf("synthetic fixture size: bytes=%d chunks=%d", manifest.StreamBytes, len(manifest.Chunks))
	}
	return rootReadLargeFixture{Repo: repo, Base: base, Target: target, Bytes: manifest.StreamBytes, Events: eventCount, Chunks: len(manifest.Chunks), BaseEvents: eventCount - 1}
}

func TestRootEventPageLargeWorkingSet(test *testing.T) {
	if dir := os.Getenv("CXT_TEST_ROOT_READ_FIXTURE"); dir != "" {
		measureRootReadLargeFixture(test, dir)
		return
	}
	if os.Getenv("CXT_TEST_ROOT_READ_LARGE") != "1" {
		test.Skip("opt-in 310MB synthetic read-cost measurement")
	}
	dir := test.TempDir()
	var fixture rootReadLargeFixture
	modes := []string{"no-base", "no-base-proof-gc", "normal", "proof-gc"}
	if source := os.Getenv("CXT_TEST_ROOT_READ_SOURCE"); source != "" {
		fixture = copyRootReadLocalFixture(test, source, dir)
		modes = []string{"no-base", "no-base-proof-gc", "shape"}
		if fixture.Base != "" {
			modes = append(modes, "normal", "proof-gc")
		}
	} else {
		fixture = writeRootReadLargeFixture(test, dir)
	}
	raw, err := json.Marshal(fixture)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), raw, 0600); err != nil {
		test.Fatal(err)
	}
	for _, mode := range modes {
		if selected := os.Getenv("CXT_TEST_ROOT_READ_CASE"); selected != "" && mode != selected {
			continue
		}
		arguments := []string{os.Args[0], "-test.run=^TestRootEventPageLargeWorkingSet$", "-test.v", "-test.timeout=120s"}
		profile := filepath.Join(dir, mode+".cpu")
		if os.Getenv("CXT_TEST_ROOT_READ_CPU") == "1" {
			arguments = append(arguments, "-test.cpuprofile="+profile)
		}
		if runtime.GOOS == "darwin" {
			arguments = append([]string{"/usr/bin/time", "-l"}, arguments...)
		}
		command := exec.Command(arguments[0], arguments[1:]...)
		for _, variable := range os.Environ() {
			if !strings.HasPrefix(variable, "GOMEMLIMIT=") {
				command.Env = append(command.Env, variable)
			}
		}
		command.Env = append(command.Env, "GOMEMLIMIT=384MiB", "CXT_TEST_ROOT_READ_FIXTURE="+dir, "CXT_TEST_ROOT_READ_MODE="+mode)
		output, err := command.CombinedOutput()
		test.Logf("%s worker:\n%s", mode, output)
		if err != nil {
			test.Fatal(err)
		}
		if os.Getenv("CXT_TEST_ROOT_READ_CPU") == "1" {
			output, err := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "tool", "pprof", "-top", "-nodecount=12", os.Args[0], profile).CombinedOutput()
			test.Logf("%s CPU profile:\n%s", mode, output)
			if err != nil {
				test.Fatal(err)
			}
		}
	}
}

func measureRootReadLargeFixture(test *testing.T, dir string) {
	test.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "fixture.json"))
	if err != nil {
		test.Fatal(err)
	}
	var fixture rootReadLargeFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		test.Fatal(err)
	}
	storage := store.NewFSStore(dir)
	mode := os.Getenv("CXT_TEST_ROOT_READ_MODE")
	if mode == "shape" {
		measureRootReadLocalShape(test, storage, fixture)
		return
	}
	reader := &rootReadCostStore{FSStore: storage, measure: strings.HasSuffix(mode, "proof-gc")}
	service := NewService(storage, reader, nil, nil, nil)
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	stop, sampled := make(chan struct{}), make(chan uint64, 1)
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		var peak uint64
		for {
			select {
			case <-stop:
				sampled <- peak
				return
			case <-ticker.C:
				var stats runtime.MemStats
				runtime.ReadMemStats(&stats)
				peak = max(peak, stats.HeapAlloc)
			}
		}
	}()
	base, offset, inherited, next := fixture.Base, -1, fixture.BaseEvents, -1
	if fixture.BaseEvents+1 < fixture.Events {
		next = fixture.BaseEvents + 1
	}
	if strings.HasPrefix(mode, "no-base") {
		base, offset, inherited, next = "", 0, 0, 1
	}
	started := time.Now()
	page, err := service.ReadDocEvents(systemTestContext(), fixture.Repo, fixture.Target, base, offset, 1)
	elapsed := time.Since(started)
	close(stop)
	peak := <-sampled
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	test.Logf("mode=%s stream=%d events=%d chunks=%d elapsed=%s allocated=%d sampled_heap=%d proof_live=%d baseline_heap=%d gc_cycles=%d", mode, fixture.Bytes, fixture.Events, fixture.Chunks, elapsed, after.TotalAlloc-baseline.TotalAlloc, max(peak, after.HeapAlloc), reader.peakLive, baseline.HeapAlloc, after.NumGC-baseline.NumGC)
	if err != nil || len(page.Events) != 1 || page.Inherited != inherited || page.Next != next {
		test.Fatalf("large root page: inherited=%d events=%d next=%d error=%v", page.Inherited, len(page.Events), page.Next, err)
	}
}
