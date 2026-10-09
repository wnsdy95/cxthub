package nativecodex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type generationQueueFixture struct {
	h      *Handoff
	client *websocket.Conn
	ctx    context.Context
	read   func() (websocket.MessageType, []byte, error)
}

func newGenerationQueueFixture(t *testing.T) generationQueueFixture {
	t.Helper()
	client, peer, ctx := rpcFixture(t, nil)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	hctx, hcancel := context.WithCancel(ctx)
	h := &Handoff{ctx: hctx, cancel: hcancel}
	read, stop := h.readGenerationClient(peer, nil)
	// This endpoint only sends application frames. Read control frames so Ping
	// can observe the pong without consuming the handoff's buffered messages.
	client.conn.CloseRead(ctx)
	t.Cleanup(func() {
		cancel()
		hcancel()
		stop()
	})
	return generationQueueFixture{h: h, client: client.conn, ctx: ctx, read: read}
}

func (f generationQueueFixture) burst(t *testing.T, count int) [][]byte {
	t.Helper()
	frames := make([][]byte, count)
	for i := range frames {
		frames[i] = handoffUnitRequest(t, i+1, "thread/read", map[string]any{
			"threadId": fmt.Sprintf("old-thread-%d", i), "includeTurns": false,
		})
		if err := f.client.Write(f.ctx, websocket.MessageText, frames[i]); err != nil {
			t.Fatalf("write burst frame %d: %v", i, err)
		}
	}
	// No queue consumption has started. The reader must enqueue every preceding
	// message before it can read this ping and send its pong.
	if err := f.client.Ping(f.ctx); err != nil {
		t.Fatalf("flush burst of %d frames: %v", count, err)
	}
	if err := f.h.ctx.Err(); err != nil {
		t.Fatalf("bounded burst canceled the handoff: %v", err)
	}
	return frames
}

func (f generationQueueFixture) assertCanceled(t *testing.T, want error) {
	t.Helper()
	select {
	case <-f.h.ctx.Done():
	case <-f.ctx.Done():
	}
	if err := f.ctx.Err(); err != nil {
		t.Fatalf("handoff did not cancel promptly: %v", err)
	}
	f.h.mu.Lock()
	err := f.h.err
	f.h.mu.Unlock()
	if !errors.Is(err, want) {
		t.Fatalf("handoff error = %v, want %v", err, want)
	}
	if _, data, err := f.read(); !errors.Is(err, ErrClosed) || len(data) != 0 {
		t.Fatalf("canceled reader returned queued data: bytes=%d, err=%v", len(data), err)
	}
}

func TestGenerationClientQueuePreservesDelayedBurst(t *testing.T) {
	f := newGenerationQueueFixture(t)
	// Withhold reads as first-turn preparation does. This exceeds the old
	// eight-frame queue while staying below the allowed pending-request bound.
	frames := f.burst(t, 32)
	for i, want := range frames {
		kind, got, err := f.read()
		if err != nil || kind != websocket.MessageText || !bytes.Equal(got, want) {
			t.Fatalf("read burst frame %d: kind=%v, data=%q, err=%v; want %q", i, kind, got, err, want)
		}
	}
	if err := f.h.ctx.Err(); err != nil {
		t.Fatalf("draining bounded burst canceled the handoff: %v", err)
	}
}

func TestGenerationClientQueueOverflowCancelsWithoutConsumption(t *testing.T) {
	f := newGenerationQueueFixture(t)
	f.burst(t, maxHandoffPendingRequests)
	overflow := handoffUnitRequest(t, maxHandoffPendingRequests+1, "thread/read", map[string]any{"threadId": "overflow-thread"})
	// The reader can close the socket immediately upon receiving this frame;
	// its cancellation reason, rather than the racing write result, is decisive.
	_ = f.client.Write(f.ctx, websocket.MessageText, overflow)
	f.assertCanceled(t, ErrProtocol)
}

func TestGenerationClientQueueDisconnectCancelsWithoutConsumption(t *testing.T) {
	f := newGenerationQueueFixture(t)
	f.burst(t, maxHandoffPendingRequests)
	if err := f.client.CloseNow(); err != nil {
		t.Fatal(err)
	}
	f.assertCanceled(t, ErrClosed)
}
