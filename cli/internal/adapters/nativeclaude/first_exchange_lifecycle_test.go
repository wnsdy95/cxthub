//go:build darwin || linux

package nativeclaude

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFirstExchangeAdmissionEvidenceIsAnOwnedCopy(t *testing.T) {
	f := newFirstExchangeFixture(t, "normal")
	s := f.start(t, true)
	r, err := s.Run(firstExchangeRunContext(t), "question", func(_ context.Context, e FirstQuestionEvidence) error {
		if e.Summary.AutoCompactThreshold == nil {
			t.Fatal("fixture threshold missing")
		}
		*e.Summary.AutoCompactThreshold = 1
		e.Reference.PayloadHash = "changed"
		return nil
	})
	if err != nil || !r.Completed || len(f.queries(t)) != 1 {
		t.Fatal("callback could mutate the transport's comparison evidence", err)
	}
}

func TestFirstExchangeCancellationRetiresProcessWhileAdmissionReturns(t *testing.T) {
	f := newFirstExchangeFixture(t, "normal")
	s := f.start(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		_, err := s.Run(ctx, "question", func(context.Context, FirstQuestionEvidence) error {
			close(entered)
			<-release
			return nil
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("admission did not start")
	}
	cancel()
	select {
	case <-s.s.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("native process survived canceled admission")
	}
	if len(f.queries(t)) != 0 {
		t.Fatal("canceled pending admission sent a query")
	}
	unblock()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("late admission return lost cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not finish after admission returned")
	}
}
