package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func warmInspectionProof(t *testing.T, store *FileStore, hash domain.ContentHash, r *inspectionEventReuse) {
	t.Helper()
	for i := 0; i < 2; i++ {
		if err := store.verifyDoc(context.Background(), hash, r); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.proofs.values) == 0 {
		t.Fatal("shared current events were not validated")
	}
}

func TestInspectionEventProofDoesNotMaskCurrentDamage(t *testing.T) {
	for _, damage := range []string{"missing", "changed", "decompression", "invalid-type", "duplicate-field"} {
		t.Run(damage, func(t *testing.T) {
			store, hash, manifest := verificationV2Fixture(t, `{}`, `{"seq":0}`)
			var r inspectionEventReuse
			warmInspectionProof(t, store, hash, &r)
			want := domain.ErrHashMismatch
			switch damage {
			case "missing":
				if err := os.Remove(store.objectPath("chunks", manifest.Chunks[1])); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrNotFound
			case "changed":
				if err := writeAtomic(store.objectPath("chunks", manifest.Chunks[0]), docCompress([]byte(`{"seq":1`))); err != nil {
					t.Fatal(err)
				}
			case "decompression":
				if err := writeAtomic(store.objectPath("chunks", manifest.Chunks[1]), []byte{0x28, 0xb5, 0x2f, 0xfd, 0xff}); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrInvalidCIR
			case "invalid-type", "duplicate-field":
				event := `{"seq":"bad"}`
				if damage == "duplicate-field" {
					event = `{"seq":"bad","seq":0}`
				}
				store, hash, manifest = verificationV2Fixture(t, `{}`, event)
				// Eligibility labels must not become proof of current event validity.
				r.observe(manifest.Chunks)
				want = domain.ErrInvalidCIR
			}
			_, ordinaryErr := store.GetDoc(context.Background(), hash)
			if !errors.Is(ordinaryErr, want) {
				t.Fatalf("ordinary validation = %v, want %v", ordinaryErr, want)
			}
			if err := store.verifyDoc(context.Background(), hash, &r); !errors.Is(err, want) {
				t.Fatalf("proof validation = %v, want %v", err, want)
			}
		})
	}
}

func TestInspectionEventProofKeepsDocumentDepthAndCancellation(t *testing.T) {
	event := `{"kind":"tool_result","output":` + strings.Repeat("[", 9997) + "0" + strings.Repeat("]", 9997) + `}`
	store, hash, _ := verificationV2Fixture(t, `{}`, event)
	var reuse inspectionEventReuse
	warmInspectionProof(t, store, hash, &reuse)
	store, hash, manifest := verificationV2Fixture(t, `{}`, event+`],"unknown":{"value":`+strings.Repeat("[", 9999)+"0"+strings.Repeat("]", 9999)+`},"events":[{}`)
	reuse.observe(manifest.Chunks)
	if err := store.verifyDoc(context.Background(), hash, &reuse); !errors.Is(err, domain.ErrInvalidCIR) {
		t.Fatalf("global depth changed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.verifyDoc(ctx, hash, &reuse); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation changed: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	reader := &cancelVerificationReader{input: strings.NewReader(`{"envelope":{},"events":[` + strings.Repeat(`{"seq":1},`, 200) + `{}]}`), cancel: cancel}
	proofs := &eventProofDecoder{cache: &reuse.proofs}
	reuse.proofs.remember(sha256.Sum256([]byte(`{"seq":1}`)))
	if err := verifyCIRStreamWithProofs(ctx, reader, proofs); !errors.Is(err, context.Canceled) || reader.input.Len() == 0 {
		t.Fatalf("warm proof stream did not preserve cooperative cancellation: %v", err)
	}
}

func TestInspectionEventProofOwnershipAndEligibility(t *testing.T) {
	store, hash, manifest := verificationV2Fixture(t, `{}`, `{"seq":1}`)
	var first, next inspectionEventReuse
	if err := store.verifyDoc(context.Background(), hash, &first); err != nil {
		t.Fatal(err)
	}
	if len(first.proofs.values) != 0 || first.forDocument(manifest.Chunks) == nil {
		t.Fatal("first document should use the direct decoder and register successful labels")
	}
	if next.forDocument(manifest.Chunks) != nil || len(next.proofs.values) != 0 {
		t.Fatal("proof state escaped its inspection")
	}
	labels := []domain.ContentHash{domain.HashContent([]byte("a")), domain.HashContent([]byte("b")), domain.HashContent([]byte("c"))}
	first.observe(labels[:1])
	if first.forDocument(labels) != nil {
		t.Fatal("one shared prefix enabled reuse for mostly unique chunks")
	}
	first.observe(labels[1:2])
	if first.forDocument(labels) == nil {
		t.Fatal("majority shared labels did not enable bounded decoding")
	}
	badStore, badHash, badManifest := verificationV2Fixture(t, `{}`, `{"seq":"invalid"}`)
	if err := badStore.verifyDoc(context.Background(), badHash, &next); !errors.Is(err, domain.ErrInvalidCIR) {
		t.Fatal(err)
	}
	if next.forDocument(badManifest.Chunks) != nil {
		t.Fatal("damaged document registered predictor labels")
	}
}

func TestInspectionEventProofEvictionAndThrashingFallback(t *testing.T) {
	var reuse inspectionEventReuse
	first := sha256.Sum256([]byte("first"))
	reuse.proofs.remember(first)
	for i := 0; i < inspectionEventProofLimit; i++ {
		reuse.proofs.remember(sha256.Sum256([]byte(fmt.Sprintf(`{"seq":%d}`, i))))
	}
	if reuse.proofs.contains(first) || len(reuse.proofs.values) != inspectionEventProofLimit || len(reuse.proofs.order) != inspectionEventProofLimit {
		t.Fatal("proof bound or eviction changed")
	}
	for i := 0; i <= inspectionChunkLabelLimit; i++ {
		reuse.observe([]domain.ContentHash{domain.HashContent([]byte(fmt.Sprint(i)))})
	}
	if len(reuse.chunks) != inspectionChunkLabelLimit || len(reuse.chunkOrder) != inspectionChunkLabelLimit {
		t.Fatal("predictor label bound changed")
	}
	// Ordered FIFO scans larger than the cache can produce no hits at all.
	proofs := &eventProofDecoder{cache: &reuse.proofs}
	for i := 0; i < eventProofMissLimit; i++ {
		decoder := json.NewDecoder(strings.NewReader(fmt.Sprintf(`{"seq":%d}`, i+inspectionEventProofLimit)))
		if err := proofs.decode(decoder); err != nil {
			t.Fatal(err)
		}
	}
	if !proofs.direct {
		t.Fatal("repeated labels with no event hits did not fall back")
	}
	if err := proofs.decode(json.NewDecoder(strings.NewReader(`{"seq":"bad"}`))); err == nil {
		t.Fatal("direct fallback skipped typed validation")
	}
	largeMiss := &eventProofDecoder{cache: &reuse.proofs}
	raw := `{"input":{"padding":"` + strings.Repeat("p", eventProofMissByteLimit) + `"}}`
	if err := largeMiss.decode(json.NewDecoder(strings.NewReader(raw))); err != nil || !largeMiss.direct {
		t.Fatalf("large unique event did not bound further proof work: %v", err)
	}
}

func TestInspectReplicaReusedEventsRereadCurrentFiles(t *testing.T) {
	store, _, manifest := verificationV2Fixture(t, `{}`, `{"seq":1}`)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		source, hash, current := verificationV2Fixture(t, fmt.Sprintf(`{"source_model":"fixture-%d"}`, i), `{"seq":1}`)
		for _, chunk := range current.Chunks {
			raw, err := readCxtFile(source.objectPath("chunks", chunk))
			if err != nil {
				t.Fatal(err)
			}
			inspectionWrite(t, store.objectPath("chunks", chunk), raw)
		}
		raw, err := readCxtFile(source.objectPath("docs", hash))
		if err != nil {
			t.Fatal(err)
		}
		inspectionWrite(t, store.objectPath("docs", hash), raw)
		if err := store.PutSnapshot(ctx, domain.Snapshot{ID: hash, DocHash: hash, RepoID: string(domain.HashContent([]byte("inspection fixture"))), CreatedAt: time.Unix(int64(i), 0).UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	before := inspectionDiskState(t, store.repoRoot)
	if report := store.InspectReplica(ctx); !report.Completed || report.DocumentsChecked != 3 || len(report.Issues) != 0 {
		t.Fatalf("shared healthy inspection: %+v", report)
	}
	assertInspectionUnchanged(t, store, before)
	reports := make(chan ReplicaInspection, 2)
	for i := 0; i < 2; i++ {
		go func() { reports <- store.InspectReplica(ctx) }()
	}
	for i := 0; i < 2; i++ {
		if concurrent := <-reports; !concurrent.Completed || concurrent.DocumentsChecked != 3 || len(concurrent.Issues) != 0 {
			t.Fatalf("concurrent inspections interfered: %+v", concurrent)
		}
	}
	assertInspectionUnchanged(t, store, before)
	reads := 0
	report := store.inspectReplica(ctx, func(label string) {
		if strings.HasPrefix(label, "document ") {
			reads++
			if reads == 3 {
				inspectionWrite(t, store.objectPath("chunks", manifest.Chunks[0]), docCompress([]byte(`{"seq":2`)))
			}
		}
	})
	if !report.Completed || report.DocumentsChecked != 3 || len(report.Issues) != 1 || !strings.Contains(report.Issues[0], domain.ErrHashMismatch.Error()) {
		t.Fatalf("warmed inspection missed current corruption: %+v", report)
	}
	if again := store.InspectReplica(ctx); !again.Completed || len(again.Issues) != 3 {
		t.Fatalf("next call retained stale evidence: %+v", again)
	}
}

// Compare the complete validation paths, including current disk reads. The
// fixture distinguishes repeated append-only event bytes from disjoint events.
func BenchmarkInspectionEventReuse(b *testing.B) {
	for _, workload := range []string{"shared", "unique"} {
		for _, mode := range []string{"direct", "inspection"} {
			b.Run(workload+"/"+mode, func(b *testing.B) {
				var stores []*FileStore
				var hashes []domain.ContentHash
				for d := 0; d < 8; d++ {
					var events strings.Builder
					for i := 0; i < 1000; i++ {
						if i > 0 {
							events.WriteByte(',')
						}
						identity := 0
						if workload == "unique" {
							identity = d
						}
						fmt.Fprintf(&events, `{"kind":"tool_call","seq":%d,"call_id":"doc-%d-%d","input":{"a":[1,2,{"text":"typed event fixture"}],"c":{"x":1,"z":["one","two"]}}}`, i, identity, i)
					}
					store, hash, _ := verificationV2Fixture(b, `{}`, events.String())
					stores = append(stores, store)
					hashes = append(hashes, hash)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for round := 0; round < b.N; round++ {
					var reuse inspectionEventReuse
					for i, store := range stores {
						var err error
						if mode == "direct" {
							err = store.VerifyDoc(context.Background(), hashes[i])
						} else {
							err = store.verifyDoc(context.Background(), hashes[i], &reuse)
						}
						if err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}

func seedValidatedEventProof(t *testing.T, reuse *inspectionEventReuse, raw string) {
	t.Helper()
	var event domain.Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatal(err)
	}
	reuse.proofs.remember(sha256.Sum256([]byte(raw)))
}

func TestInspectionEventReuseBreakerBoundaries(t *testing.T) {
	t.Run("threshold-current-invalid", func(t *testing.T) {
		var reuse inspectionEventReuse
		seedValidatedEventProof(t, &reuse, `{}`)
		p := &eventProofDecoder{cache: &reuse.proofs}
		for i := 0; i < 127; i++ {
			if err := p.decode(json.NewDecoder(strings.NewReader(fmt.Sprintf(`{"seq":%d}`, 1000+i)))); err != nil {
				t.Fatal(err)
			}
		}
		if p.direct || p.consecutiveMiss != 127 {
			t.Fatal("premature fallback")
		}
		count := len(reuse.proofs.values)
		if err := p.decode(json.NewDecoder(strings.NewReader(`{"seq":"bad"}`))); err == nil {
			t.Fatal("triggering invalid event skipped")
		}
		if p.direct || count != len(reuse.proofs.values) || p.consecutiveMiss != 127 {
			t.Fatal("failed proof admitted or counted")
		}
		current := `{"seq":1127}`
		if err := p.decode(json.NewDecoder(strings.NewReader(current))); err != nil {
			t.Fatal(err)
		}
		if !p.direct || !reuse.proofs.contains(sha256.Sum256([]byte(current))) {
			t.Fatal("128th valid event not validated before fallback")
		}
		if err := p.decode(json.NewDecoder(strings.NewReader(`{"replacement":[{"seq":"bad"}]}`))); err == nil {
			t.Fatal("direct remainder skipped typed replacement")
		}
	})
	t.Run("hits-reset-count-not-bytes", func(t *testing.T) {
		var reuse inspectionEventReuse
		seedValidatedEventProof(t, &reuse, `{}`)
		p := &eventProofDecoder{cache: &reuse.proofs}
		first := `{"id":"a` + strings.Repeat("x", 600<<10) + `"}`
		second := `{"id":"b` + strings.Repeat("x", 600<<10) + `"}`
		for _, raw := range []string{first, `{}`} {
			if err := p.decode(json.NewDecoder(strings.NewReader(raw))); err != nil {
				t.Fatal(err)
			}
		}
		if p.direct || p.consecutiveMiss != 0 || p.missBytes != len(first) {
			t.Fatal("hit reset cumulative bytes")
		}
		if err := p.decode(json.NewDecoder(strings.NewReader(second))); err != nil {
			t.Fatal(err)
		}
		if !p.direct || p.consecutiveMiss != 1 || p.missBytes != eventProofMissByteLimit {
			t.Fatal("cumulative byte limit not saturated")
		}
	})
	t.Run("duplicate-events-no-reset", func(t *testing.T) {
		var reuse inspectionEventReuse
		seedValidatedEventProof(t, &reuse, `{}`)
		p := &eventProofDecoder{cache: &reuse.proofs}
		var events strings.Builder
		for i := 0; i < 127; i++ {
			if i > 0 {
				events.WriteByte(',')
			}
			fmt.Fprintf(&events, `{"seq":%d}`, i+2000)
		}
		doc := `{"events":[` + events.String() + `],"events":[{"seq":2127},{"seq":"bad"}]}`
		if err := verifyCIRStreamWithProofs(context.Background(), strings.NewReader(doc), p); !errors.Is(err, domain.ErrInvalidCIR) {
			t.Fatal(err)
		}
		if !p.direct || p.consecutiveMiss != 128 {
			t.Fatal("duplicate events reset breaker")
		}
	})
	t.Run("bounded-warmer-then-next-document", func(t *testing.T) {
		var reuse inspectionEventReuse
		label := domain.HashContent([]byte("synthetic label"))
		reuse.observe([]domain.ContentHash{label})
		p := reuse.forDocument([]domain.ContentHash{label})
		if p == nil || !p.warming {
			t.Fatal("missing initial warmer")
		}
		for i := 0; i < inspectionEventProofLimit; i++ {
			raw := fmt.Sprintf(`{"seq":%d,"id":"%s"}`, i, strings.Repeat("x", 20))
			if err := p.decode(json.NewDecoder(strings.NewReader(raw))); err != nil {
				t.Fatal(err)
			}
			if i == 127 && p.direct {
				t.Fatal("warmup ended before configured proof capacity")
			}
		}
		if !p.direct || len(reuse.proofs.values) != inspectionEventProofLimit || p.missBytes != eventProofMissByteLimit {
			t.Fatal("full warmer did not latch fallback")
		}
		next := reuse.forDocument([]domain.ContentHash{label})
		if next == nil || next.warming || next.direct || next.consecutiveMiss != 0 || next.missBytes != 0 {
			t.Fatal("per-document breaker state escaped")
		}
	})
}

func TestInspectionEventReuseFailedDocProofAndAdmission(t *testing.T) {
	st := NewFileStore(t.TempDir())
	prefix := []byte(`{"seq":7},`)
	shared := domain.HashContent(prefix)
	inspectionWrite(t, st.objectPath("chunks", shared), docCompress(prefix))
	writeDoc := func(tail string) (domain.ContentHash, domain.ContentHash) {
		body := []byte(tail)
		label := domain.HashContent(body)
		inspectionWrite(t, st.objectPath("chunks", label), docCompress(body))
		man := chunkcas.Manifest{Format: chunkcas.FormatV2, Envelope: json.RawMessage(`{}`), Chunks: []domain.ContentHash{shared, label}}
		encoded, err := json.Marshal(man)
		if err != nil {
			t.Fatal(err)
		}
		hash := domain.HashContent([]byte(`{"envelope":{},"events":[` + string(prefix) + tail + `]}`))
		inspectionWrite(t, st.objectPath("docs", hash), docCompress(encoded))
		return hash, label
	}
	good, _ := writeDoc(`{"seq":8}`)
	var reuse inspectionEventReuse
	if err := st.verifyDoc(context.Background(), good, &reuse); err != nil {
		t.Fatal(err)
	}
	bad, badLabel := writeDoc(`{"seq":9},{"seq":"bad"}`)
	if err := st.verifyDoc(context.Background(), bad, &reuse); !errors.Is(err, domain.ErrInvalidCIR) {
		t.Fatal(err)
	}
	if _, ok := reuse.chunks[badLabel]; ok {
		t.Fatal("failed document label admitted")
	}
	if !reuse.proofs.contains(sha256.Sum256([]byte(`{"seq":9}`))) {
		t.Fatal("independent successful local proof not retained")
	}
	follow, _ := writeDoc(`{"seq":9}`)
	if err := st.verifyDoc(context.Background(), follow, &reuse); err != nil {
		t.Fatal(err)
	}
	// Locally valid bytes can be cached even when document-wide depth must reject them.
	raw := `{"output":` + strings.Repeat("[", 9998) + `0` + strings.Repeat("]", 9998) + `}`
	deep, deepLabel := writeDoc(raw)
	if err := st.verifyDoc(context.Background(), deep, &reuse); !errors.Is(err, domain.ErrInvalidCIR) {
		t.Fatal(err)
	}
	if !reuse.proofs.contains(sha256.Sum256([]byte(raw))) {
		t.Fatal("local typed proof was not obtained")
	}
	if _, ok := reuse.chunks[deepLabel]; ok {
		t.Fatal("global-depth failure label admitted")
	}
	if err := st.verifyDoc(context.Background(), deep, &reuse); !errors.Is(err, domain.ErrInvalidCIR) {
		t.Fatal("warm local proof masked global depth", err)
	}
}
