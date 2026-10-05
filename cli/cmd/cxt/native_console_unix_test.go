//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestNativeConsoleLineLeavesFollowingInputAndFileOpen(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := w.WriteString("question\r\nno\n"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"question", "no"} {
		got, err := nativeConsoleLine(context.Background(), r)
		if err != nil || got != want {
			t.Fatalf("got=%q err=%v", got, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := nativeConsoleLine(ctx, r); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("blocked read did not cancel", err)
	}
	if _, err := w.WriteString("next\n"); err != nil {
		t.Fatal("caller pipe closed", err)
	}
	if got, err := nativeConsoleLine(context.Background(), r); err != nil || got != "next" {
		t.Fatal("caller input lost", err)
	}
	w.Close()
	if _, err := nativeConsoleLine(context.Background(), r); !errors.Is(err, io.EOF) {
		t.Fatal("EOF", err)
	}
}

type nativeConsoleReadStartedContext struct {
	context.Context
	started chan struct{}
	once    sync.Once
}

func (c *nativeConsoleReadStartedContext) Err() error {
	c.once.Do(func() { close(c.started) })
	return c.Context.Err()
}

func TestNativeConsoleLineKeepsFileAliveWhilePolling(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reading := &nativeConsoleReadStartedContext{Context: ctx, started: make(chan struct{})}
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func(file *os.File) {
		line, err := nativeConsoleLine(reading, file)
		done <- result{line, err}
	}(r)
	// Do not retain r with a deferred Close: the active read must keep it alive.
	r = nil
	select {
	case <-reading.started:
	case <-ctx.Done():
		t.Fatal("reader did not start", ctx.Err())
	}
	for range 4 {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := w.WriteString("yes\n"); err != nil {
		t.Errorf("reader finalized while read was active: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.line != "yes" {
			t.Errorf("got=%q err=%v", got.line, got.err)
		}
	case <-ctx.Done():
		t.Fatal("reader did not finish", ctx.Err())
	}
	// The reader may now be finalized; there is deliberately no caller reference.
	runtime.GC()
}
