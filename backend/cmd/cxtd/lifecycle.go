package main

import (
	"context"
	"sync"
)

type apiLifecycle struct {
	ctx          context.Context
	cancel       context.CancelFunc
	workers      sync.WaitGroup
	closeRuntime func()
	closeOnce    sync.Once
}

func newAPILifecycle(parent context.Context, closeRuntime func()) *apiLifecycle {
	ctx, cancel := context.WithCancel(parent)
	return &apiLifecycle{ctx: ctx, cancel: cancel, closeRuntime: closeRuntime}
}

func (lifecycle *apiLifecycle) start(run func(context.Context)) {
	lifecycle.workers.Add(1)
	go func() {
		defer lifecycle.workers.Done()
		run(lifecycle.ctx)
	}()
}

func (lifecycle *apiLifecycle) close() {
	lifecycle.closeOnce.Do(func() {
		lifecycle.cancel()
		lifecycle.workers.Wait()
		lifecycle.closeRuntime()
	})
}
