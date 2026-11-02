package documentworker

import (
	"context"
	"time"
)

func WithLease(ctx context.Context, lease time.Duration, renew func(context.Context) error, run func(context.Context) error) error {
	work, cancel := context.WithTimeout(ctx, WorkLimit)
	defer cancel()
	done, renewed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-work.Done():
				return
			case <-ticker.C:
				renewal, finish := context.WithTimeout(work, 5*time.Second)
				err := renew(renewal)
				finish()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	err := run(work)
	close(done)
	<-renewed
	return err
}
