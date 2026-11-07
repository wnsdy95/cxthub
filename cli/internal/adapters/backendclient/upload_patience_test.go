package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// patientChunkFixture uploads n distinct chunks (32 per batch) through the
// real document chunk loop and reports each request's batch size.
func patientChunkFixture(t *testing.T, n int, exchange func(request *http.Request, size int) *http.Response) func(context.Context) error {
	t.Helper()
	repo := string(domain.HashContent([]byte("patient upload repo")))
	bodies := map[domain.ContentHash][]byte{}
	var order []domain.ContentHash
	wants := map[domain.ContentHash]bool{}
	for index := range n {
		body := []byte(fmt.Sprintf("patient chunk %d", index))
		hash := domain.HashContent(body)
		bodies[hash] = body
		order = append(order, hash)
		wants[hash] = true
	}
	client := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
	client.httpc.Transport = reviewTransferRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != client.reposPath(repo)+"/push/chunks" {
			t.Fatalf("unexpected request %s", request.URL.Path)
		}
		var wire chunksReq
		if err := json.NewDecoder(request.Body).Decode(&wire); err != nil {
			t.Fatal(err)
		}
		return exchange(request, len(wire.Chunks)), nil
	})
	doc := outbound.DocumentChunks{ReadChunk: func(_ context.Context, hash domain.ContentHash) ([]byte, error) { return bodies[hash], nil }}
	return func(ctx context.Context) error { return client.pushRequestedDocChunks(ctx, repo, doc, order, wants) }
}

func patient(stall time.Duration) context.Context {
	return outbound.WithUploadPolicy(context.Background(), outbound.UploadPolicy{StallBudget: stall})
}

func TestPatientUploadOutlastsBoundedRetryLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		upload := patientChunkFixture(t, 1, func(request *http.Request, _ int) *http.Response {
			calls++
			if calls <= 8 {
				return uploadOverloadResponse(request, "3")
			}
			return reviewWireResponse(request, http.StatusOK, nil)
		})
		if err := upload(patient(15 * time.Minute)); err != nil || calls != 9 {
			t.Fatalf("patient upload gave up: err=%v calls=%d", err, calls)
		}
		// The unchanged bounded contract still stops after five attempts.
		calls = 0
		if err := upload(context.Background()); err == nil || calls != 5 {
			t.Fatalf("legacy contract changed: err=%v calls=%d", err, calls)
		}
	})
}

func TestPatientUploadStallBudgetRestartsOnProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rejected, batches := 0, 0
		upload := patientChunkFixture(t, 65, func(request *http.Request, _ int) *http.Response {
			if rejected < 4 {
				rejected++
				return uploadOverloadResponse(request, "1")
			}
			rejected = 0
			batches++
			return reviewWireResponse(request, http.StatusOK, nil)
		})
		started := time.Now()
		if err := upload(patient(30 * time.Second)); err != nil || batches != 3 {
			t.Fatalf("progressing upload stopped: err=%v batches=%d", err, batches)
		}
		if time.Since(started) <= 30*time.Second {
			t.Fatalf("fixture did not exceed one stall budget overall: %s", time.Since(started))
		}
	})
}

func TestPatientUploadStopsStuckBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		upload := patientChunkFixture(t, 1, func(request *http.Request, _ int) *http.Response {
			calls++
			return uploadOverloadResponse(request, "1")
		})
		started := time.Now()
		var rejection *HTTPError
		if err := upload(patient(30 * time.Second)); !errors.As(err, &rejection) || rejection.Code != "resource_overloaded" {
			t.Fatalf("stuck batch did not surface backpressure: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 30*time.Second || calls < 2 {
			t.Fatalf("stall budget overrun: elapsed=%s calls=%d", elapsed, calls)
		}
	})
}

func TestPatientUploadPacesBatchesAfterBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rejected := 0
		var accepted, nextStart time.Time
		upload := patientChunkFixture(t, 33, func(request *http.Request, size int) *http.Response {
			if size == 1 {
				nextStart = time.Now()
				return reviewWireResponse(request, http.StatusOK, nil)
			}
			if rejected < 3 {
				rejected++
				return uploadOverloadResponse(request, "1")
			}
			accepted = time.Now()
			return reviewWireResponse(request, http.StatusOK, nil)
		})
		if err := upload(patient(time.Minute)); err != nil {
			t.Fatal(err)
		}
		// Three rejections widen the gap to 1s; one acceptance halves it.
		if gap := nextStart.Sub(accepted); gap < 500*time.Millisecond {
			t.Fatalf("next batch was not paced after backpressure: gap=%s", gap)
		}
	})
}

func TestPatientUploadGatesEachRequestAndReportsProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inGate, gated, calls := false, 0, 0
		var progress []string
		upload := patientChunkFixture(t, 65, func(request *http.Request, _ int) *http.Response {
			calls++
			if !inGate {
				t.Error("request ran outside the repository upload slot")
			}
			if calls == 2 {
				return uploadOverloadResponse(request, "1")
			}
			return reviewWireResponse(request, http.StatusOK, nil)
		})
		ctx := outbound.WithUploadPolicy(context.Background(), outbound.UploadPolicy{
			StallBudget: time.Minute,
			Gate: func(_ context.Context, request func() error) error {
				gated++
				inGate = true
				defer func() { inGate = false }()
				return request()
			},
			Progress: func(sent, total int) { progress = append(progress, fmt.Sprintf("%d/%d", sent, total)) },
		})
		if err := upload(ctx); err != nil {
			t.Fatal(err)
		}
		if gated != calls || calls != 4 {
			t.Fatalf("gate=%d calls=%d", gated, calls)
		}
		if got := strings.Join(progress, " "); got != "0/65 32/65 64/65 65/65" {
			t.Fatalf("progress=%q", got)
		}
	})
}
