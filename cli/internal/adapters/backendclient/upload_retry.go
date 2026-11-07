package backendclient

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type uploadJSON []byte

const (
	// uploadPatientMaxBackoff caps one wait between patient retries.
	uploadPatientMaxBackoff = 30 * time.Second
	uploadPaceMin           = 250 * time.Millisecond
	uploadPaceMax           = 5 * time.Second
)

// uploadPacer is shared by every patient batch of one client. Backpressure
// widens the gap before the next batch; accepted batches narrow it again, so a
// small server is never fed back-to-back bulk bodies while it is shedding.
type uploadPacer struct {
	mu    sync.Mutex
	delay time.Duration
}

func (p *uploadPacer) next() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.delay
}

func (p *uploadPacer) observe(overloaded bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case overloaded:
		p.delay = min(max(2*p.delay, uploadPaceMin), uploadPaceMax)
	case p.delay <= uploadPaceMin:
		p.delay = 0
	default:
		p.delay /= 2
	}
}

func uploadSleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func uploadOverloaded(err error) (*HTTPError, bool) {
	rejection, ok := err.(*HTTPError)
	return rejection, ok && rejection.Status == http.StatusServiceUnavailable && rejection.Code == "resource_overloaded" && rejection.retryAfterValid
}

func (c *BackendClient) uploadBatch(ctx context.Context, path string, body any) error {
	if policy, ok := outbound.UploadPolicyFromContext(ctx); ok {
		return c.uploadBatchPatient(ctx, path, body, policy)
	}
	budget := 30 * time.Second
	if c.httpc.Timeout > 0 && c.httpc.Timeout < budget {
		budget = c.httpc.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	endpoint, token, err := c.transport()
	if err != nil {
		return err
	}
	frozen, err := json.Marshal(body)
	if err != nil {
		return err
	}
	peer := c.frozenRequestClient(endpoint, token)
	for retries := 0; ; retries++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := peer.do(ctx, http.MethodPost, path, uploadJSON(frozen), nil)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rejection, ok := err.(*HTTPError)
		if retries == 4 || !ok || rejection.Status != http.StatusServiceUnavailable || rejection.Code != "resource_overloaded" || !rejection.retryAfterValid {
			return err
		}
		delay := max(time.Second<<retries, rejection.retryAfter) + time.Duration(1+rand.IntN(250))*time.Millisecond
		deadline, _ := ctx.Deadline()
		if delay >= time.Until(deadline) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// uploadBatchPatient treats resource_overloaded as backpressure. Each attempt
// keeps the bounded per-request budget, but consecutive rejections may be
// waited out for the policy's stall budget. The frozen body and destination
// never change across retries, exactly as in the bounded path.
func (c *BackendClient) uploadBatchPatient(ctx context.Context, path string, body any, policy outbound.UploadPolicy) error {
	perAttempt := 30 * time.Second
	if c.httpc.Timeout > 0 && c.httpc.Timeout < perAttempt {
		perAttempt = c.httpc.Timeout
	}
	endpoint, token, err := c.transport()
	if err != nil {
		return err
	}
	frozen, err := json.Marshal(body)
	if err != nil {
		return err
	}
	peer := c.frozenRequestClient(endpoint, token)
	gate := policy.Gate
	if gate == nil {
		gate = func(_ context.Context, request func() error) error { return request() }
	}
	// Pacing spaces batches; retries of one batch already wait out backoff.
	if err := uploadSleep(ctx, c.pace.next()); err != nil {
		return err
	}
	stalled := time.Now()
	for retries := 0; ; retries++ {
		err := gate(ctx, func() error {
			attempt, cancel := context.WithTimeout(ctx, perAttempt)
			defer cancel()
			return peer.do(attempt, http.MethodPost, path, uploadJSON(frozen), nil)
		})
		if err == nil {
			c.pace.observe(false)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rejection, ok := uploadOverloaded(err)
		if !ok {
			return err
		}
		c.pace.observe(true)
		delay := max(min(time.Second<<min(retries, 5), uploadPatientMaxBackoff), rejection.retryAfter) + time.Duration(1+rand.IntN(250))*time.Millisecond
		if time.Since(stalled)+delay > policy.StallBudget {
			return err
		}
		if deadline, ok := ctx.Deadline(); ok && delay >= time.Until(deadline) {
			return err
		}
		if err := uploadSleep(ctx, delay); err != nil {
			return err
		}
	}
}

func uploadRetryAfter(values []string, now time.Time) (time.Duration, bool) {
	if len(values) == 0 {
		return 0, true
	}
	if len(values) != 1 || len(values[0]) > 128 {
		return 0, false
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return 0, false
	}
	digits := true
	for _, char := range value {
		if char < '0' || char > '9' {
			digits = false
			break
		}
	}
	if digits {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > 10 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := max(0, date.Sub(now))
	return delay, delay <= 10*time.Second
}
