package backendclient

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type uploadJSON []byte

func (c *BackendClient) uploadBatch(ctx context.Context, path string, body any) error {
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
