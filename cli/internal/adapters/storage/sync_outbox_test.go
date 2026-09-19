package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestSyncOutboxPromotionAcknowledgementPreservesConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a, b := NewSyncOutbox(), NewSyncOutbox()
	first, second := domain.HashContent([]byte("first")), domain.HashContent([]byte("second"))
	if err := a.EnqueuePromotion(ctx, root, first, "old"); err != nil {
		t.Fatal(err)
	}
	sent, err := a.ListPromotions(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.EnqueuePromotion(ctx, root, first, "new"); err != nil {
		t.Fatal(err)
	}
	if err := b.EnqueuePromotion(ctx, root, second, "unrelated"); err != nil {
		t.Fatal(err)
	}
	if err := a.AcknowledgePromotion(ctx, root, first, sent[first]); err != nil {
		t.Fatal(err)
	}
	got, err := a.ListPromotions(ctx, root)
	if err != nil || len(got) != 2 || got[first] != "new" || got[second] != "unrelated" {
		t.Fatalf("lost concurrent promotion: %v %v", got, err)
	}
	if err := a.AcknowledgePromotion(ctx, root, first, "new"); err != nil {
		t.Fatal(err)
	}
	got, err = a.ListPromotions(ctx, root)
	if err != nil || len(got) != 1 || got[second] != "unrelated" {
		t.Fatalf("ack removed wrong work: %v %v", got, err)
	}
}
func TestSyncOutboxConcurrentPromotionWriters(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- NewSyncOutbox().EnqueuePromotion(ctx, root, domain.HashContent([]byte{byte(i)}), "message")
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	got, err := NewSyncOutbox().ListPromotions(ctx, root)
	if err != nil || len(got) != 24 {
		t.Fatalf("lost writes: %d %v", len(got), err)
	}
}
func TestSyncOutboxCancelledWaitAndAccessLifetime(t *testing.T) {
	root := t.TempDir()
	a, b := NewSyncOutbox(), NewSyncOutbox()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	var escaped outbound.GraftQueueAccess
	go func() {
		done <- a.WithGrafts(context.Background(), root, func(q outbound.GraftQueueAccess) error { escaped = q; close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := b.WithGrafts(ctx, root, func(outbound.GraftQueueAccess) error {
		t.Error("cancelled contender entered critical section")
		return nil
	})
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait: %v", err)
	}
	if _, err := escaped.Load(); err == nil {
		t.Fatal("access escaped lock lifetime")
	}
	if err := escaped.Store(nil); err == nil {
		t.Fatal("write escaped lock lifetime")
	}
	if err := b.WithGrafts(context.Background(), root, func(outbound.GraftQueueAccess) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestSyncOutboxCorruptionDoesNotDiscardRetryWork(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	s := NewSyncOutbox()
	for _, name := range []string{"grafts.json", "promotions.json"} {
		if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, ".cxt", name)
		original := []byte("{broken queue bytes")
		if err := os.WriteFile(target, original, 0600); err != nil {
			t.Fatal(err)
		}
		var err error
		if name == "grafts.json" {
			err = s.WithGrafts(ctx, root, func(q outbound.GraftQueueAccess) error { _, e := q.Load(); return e })
		} else {
			err = s.EnqueuePromotion(ctx, root, domain.HashContent([]byte("x")), "new")
		}
		if err == nil {
			t.Fatalf("accepted corrupt %s", name)
		}
		got, e := os.ReadFile(target)
		if e != nil || string(got) != string(original) {
			t.Fatalf("changed corrupt source %s", name)
		}
	}
}
