package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Err is called outside the verifier's cache lock, so these observations cannot
// reenter it. The hook runs synchronously in the goroutine calling Verify.
type canonicalAdmissionContext struct {
	context.Context
	check func()
}

func (c canonicalAdmissionContext) Err() error {
	c.check()
	return c.Context.Err()
}

func canonicalAdmissionEvents(first, count int) []CIREvent {
	events := make([]CIREvent, count)
	for i := range events {
		events[i] = CIREvent{Kind: EventMessage, Role: RoleUser, Seq: first + i}
	}
	return events
}

func canonicalAdmissionDoc(t testing.TB, events []CIREvent) []byte {
	t.Helper()
	raw, err := CanonicalBytes(CIRDocument{Envelope: CIREnvelope{CIRVersion: "1"}, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCanonicalDocVerifierDefersEvictionUntilDocumentSuccess(t *testing.T) {
	const eventCount = 67000
	if eventCount <= maxCanonicalEventProofs {
		t.Fatal("fixture must exceed the proof cache capacity")
	}
	raw := canonicalAdmissionDoc(t, canonicalAdmissionEvents(0, eventCount))
	hash := HashContent(raw)
	var v CanonicalDocVerifier
	if _, err := v.Verify(context.Background(), hash, raw); err != nil {
		t.Fatal(err)
	}
	// Each scan has both misses and hits. With immediate FIFO admission, an
	// early miss evicts a proof still needed later in this same ordered stream.
	for scan := 1; scan <= 2; scan++ {
		v.mu.Lock()
		if len(v.events) != maxCanonicalEventProofs || len(v.order) != maxCanonicalEventProofs {
			v.mu.Unlock()
			t.Fatal("priming did not fill the bounded cache")
		}
		oldest := v.order[v.next]
		seq := v.events[oldest]
		v.mu.Unlock()
		checks, firstEviction := 0, 0
		ctx := canonicalAdmissionContext{Context: context.Background(), check: func() {
			checks++
			v.mu.Lock()
			_, present := v.events[oldest]
			v.mu.Unlock()
			if !present && firstEviction == 0 {
				firstEviction = checks
			}
		}}
		verified, err := v.Verify(ctx, hash, raw)
		if err != nil || !verified.Valid() || !bytes.Equal(verified.Bytes(), raw) {
			t.Fatalf("scan %d changed document acceptance or bytes: %v", scan, err)
		}
		if firstEviction != 0 {
			t.Fatalf("scan %d evicted cached sequence %d before document success (context check %d)", scan, seq, firstEviction)
		}
		if checks < eventCount {
			t.Fatal("observer did not cover the event scan")
		}
		v.mu.Lock()
		_, present := v.events[oldest]
		v.mu.Unlock()
		if present {
			t.Fatal("successful scan did not admit its misses after verification")
		}
	}
}

func TestCanonicalDocVerifierFailedDocumentDoesNotAdmitProofs(t *testing.T) {
	raw := canonicalAdmissionDoc(t, canonicalAdmissionEvents(0, 3))
	seed := canonicalAdmissionDoc(t, canonicalAdmissionEvents(100, 1))
	// Discover the last cancellation checkpoint with the same document, instead
	// of assuming how many Err calls the scanner makes per event or byte range.
	checks := 0
	var probe CanonicalDocVerifier
	if _, err := probe.Verify(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, HashContent(raw), raw); err != nil {
		t.Fatal(err)
	}
	for _, warm := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			raw      []byte
			want     error
			cancelAt int
		}{
			{"late-noncanonical-event", bytes.Replace(raw, []byte(`"seq":2}`), []byte(`"seq":2,"unknown":true}`), 1), ErrIntegrity, 0},
			{"late-sequence-regression", bytes.Replace(raw, []byte(`"seq":2}`), []byte(`"seq":0}`), 1), ErrIntegrity, 0},
			{"late-unsupported-version-fields", bytes.Replace(raw, []byte(`"seq":2}`), []byte(`"seq":2,"agent_message":false}`), 1), ErrUnsupportedCIRVersion, 0},
			{"canceled-during-scan", raw, context.Canceled, checks - 2},
			{"canceled-at-final-check", raw, context.Canceled, checks},
		} {
			t.Run(fmt.Sprintf("%s/warm=%t", tc.name, warm), func(t *testing.T) {
				var v CanonicalDocVerifier
				if warm {
					if _, err := v.Verify(context.Background(), HashContent(seed), seed); err != nil {
						t.Fatal(err)
					}
				}
				beforeEvents, beforeOrder, beforeNext := maps.Clone(v.events), slices.Clone(v.order), v.next
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				observed := canonicalAdmissionContext{Context: ctx, check: func() {
					calls++
					if tc.cancelAt > 0 && calls == tc.cancelAt {
						cancel()
					}
				}}
				verified, err := v.Verify(observed, HashContent(tc.raw), tc.raw)
				if !errors.Is(err, tc.want) || verified.Valid() {
					t.Fatalf("got valid=%t, error=%v; want %v", verified.Valid(), err, tc.want)
				}
				if !maps.Equal(v.events, beforeEvents) || !slices.Equal(v.order, beforeOrder) || v.next != beforeNext {
					t.Fatalf("failed document changed cache: events %d -> %d, order %d -> %d, next %d -> %d", len(beforeEvents), len(v.events), len(beforeOrder), len(v.order), beforeNext, v.next)
				}
				// The discarded proofs must not prevent a later successful retry.
				if verified, err := v.Verify(context.Background(), HashContent(raw), raw); err != nil || !verified.Valid() {
					t.Fatalf("valid retry failed: %v", err)
				}
				if len(v.events) != len(beforeEvents)+3 {
					t.Fatal("successful retry did not admit all new proofs")
				}
			})
		}
	}
}

func TestCanonicalDocVerifierDuplicateEventsAdmitOnce(t *testing.T) {
	event := CIREvent{Kind: EventMessage, Role: RoleUser, Seq: 7}
	other := CIREvent{Kind: EventMessage, Role: RoleAssistant, Seq: 7}
	raw := canonicalAdmissionDoc(t, []CIREvent{event, event, other, other, event})
	var v CanonicalDocVerifier
	for scan := 0; scan < 2; scan++ {
		verified, err := v.Verify(context.Background(), HashContent(raw), raw)
		if err != nil || !verified.Valid() || !bytes.Equal(verified.Bytes(), raw) {
			t.Fatalf("duplicate events or stable equal sequences changed: %v", err)
		}
		if len(v.events) != 2 || len(v.order) != 2 || v.order[0] == v.order[1] || v.next != 0 {
			t.Fatal("duplicate proofs consumed cache slots or moved the eviction cursor")
		}
	}
}

func TestCanonicalDocVerifierConcurrentAdmissionRemainsBounded(t *testing.T) {
	const workers = 4
	const perWorker = maxCanonicalEventProofs/workers + 1
	var v CanonicalDocVerifier
	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < workers; worker++ {
		raw := canonicalAdmissionDoc(t, canonicalAdmissionEvents(worker*perWorker, perWorker))
		hash := HashContent(raw)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx := canonicalAdmissionContext{Context: context.Background(), check: func() {
				v.mu.Lock()
				bounded := len(v.events) <= maxCanonicalEventProofs && len(v.order) == len(v.events)
				v.mu.Unlock()
				if !bounded {
					t.Error("concurrent admission exceeded cache capacity or diverged from FIFO order")
				}
			}}
			for scan := 0; scan < 2; scan++ {
				if verified, err := v.Verify(ctx, hash, raw); err != nil || !verified.Valid() {
					t.Errorf("concurrent verification failed: %v", err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.events) != maxCanonicalEventProofs || len(v.order) != maxCanonicalEventProofs || v.next < 0 || v.next >= len(v.order) {
		t.Fatal("concurrent admission left an invalid bounded cache")
	}
	seen := make(map[canonicalEventKey]bool, len(v.order))
	for _, key := range v.order {
		if _, exists := v.events[key]; !exists || seen[key] {
			t.Fatal("FIFO order contains a missing or duplicate proof")
		}
		seen[key] = true
	}
}

// Use exactly equal document sizes on both sides of capacity, so throughput
// changes cannot be explained by comparing different total input lengths.
func canonicalAdmissionSizedDoc(b *testing.B, count, totalBytes int) []byte {
	b.Helper()
	events := canonicalAdmissionEvents(0, count)
	for i := range events {
		events[i].Blocks = []ContentBlock{{Type: "text", Text: "x"}}
	}
	extra := totalBytes - len(canonicalAdmissionDoc(b, events))
	if extra < 0 {
		b.Fatal("requested document size cannot hold the event framing")
	}
	short := strings.Repeat("x", 1+extra/count)
	long := short + "x"
	for i := range events {
		events[i].Blocks[0].Text = short
		if i < extra%count {
			events[i].Blocks[0].Text = long
		}
	}
	raw := canonicalAdmissionDoc(b, events)
	if len(raw) != totalBytes {
		b.Fatalf("fixture has %d bytes, want %d", len(raw), totalBytes)
	}
	return raw
}

func BenchmarkCanonicalDocVerifierAdmissionCapacity(b *testing.B) {
	for _, size := range []int{8 << 20, 16 << 20} {
		for _, count := range []int{65000, 67000} {
			b.Run(fmt.Sprintf("bytes=%d/events=%d", size, count), func(b *testing.B) {
				raw := canonicalAdmissionSizedDoc(b, count, size)
				hash := HashContent(raw)
				ctx := context.Background()
				var v CanonicalDocVerifier
				if _, err := v.Verify(ctx, hash, raw); err != nil {
					b.Fatal(err)
				}
				b.SetBytes(int64(len(raw)))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if _, err := v.Verify(ctx, hash, raw); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
