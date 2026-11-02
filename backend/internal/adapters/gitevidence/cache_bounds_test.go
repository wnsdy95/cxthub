package gitevidence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type cooldownBoundsTransport func(*http.Request) (*http.Response, error)

func (transport cooldownBoundsTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestScopedCooldownBoundsKeepActiveEntries(test *testing.T) {
	now := time.Unix(1000, 0)
	until := now.Add(time.Hour)
	for _, scenario := range []string{"entries", "key-bytes", "oversized-key"} {
		test.Run(scenario, func(test *testing.T) {
			provider := NewGitHub(nil)
			keyBytes := 0
			entries := 0
			rejectedBucket := "overflow"
			switch scenario {
			case "entries":
				for index := 0; index < scopedLimitMaxEntries; index++ {
					bucket := fmt.Sprintf("scope-%d", index)
					provider.retainRateLimit(bucket, until, now)
					keyBytes += len(bucket)
				}
				entries = scopedLimitMaxEntries
			case "key-bytes":
				bucket := strings.Repeat("x", scopedLimitMaxKeyBytes)
				provider.retainRateLimit(bucket, until, now)
				keyBytes = len(bucket)
				entries = 1
			case "oversized-key":
				rejectedBucket = strings.Repeat("x", scopedLimitMaxKeyBytes+1)
				provider.retainRateLimit(rejectedBucket, until, now)
			}
			provider.retainRateLimit(rejectedBucket, until.Add(time.Minute), now)
			provider.retainRateLimit(rejectedBucket, until, now)
			if provider.scopedOverflowAt != until.Add(time.Minute) {
				test.Fatal("overflow deadline was shortened")
			}
			if len(provider.scopedLimits) != entries || provider.scopedKeyBytes != keyBytes {
				test.Fatalf("retention changed: entries=%d bytes=%d", len(provider.scopedLimits), provider.scopedKeyBytes)
			}
			for bucket, retained := range provider.scopedLimits {
				if retained != until || !provider.rateLimited(bucket, now) {
					test.Fatal("active cooldown was replaced or bypassed")
				}
			}
			if !provider.rateLimited("overflow", now) || !provider.rateLimited("unseen", now) {
				test.Fatal("overflow permits scoped traffic")
			}
			if provider.rateLimited("", now) || !provider.resumeAt.IsZero() {
				test.Fatal("scoped overflow changed global fallback")
			}
		})
	}
}

func TestScopedCooldownSweepReclaimsExpiredKeys(test *testing.T) {
	now := time.Unix(1000, 0)
	provider := NewGitHub(nil)
	provider.retainRateLimit(strings.Repeat("x", scopedLimitMaxKeyBytes), now.Add(time.Minute), now)
	provider.retainRateLimit("overflow", now.Add(2*time.Minute), now)
	provider.retainRateLimit("fresh", now.Add(time.Hour), now.Add(time.Minute))
	if len(provider.scopedLimits) != 1 || provider.scopedKeyBytes != len("fresh") {
		test.Fatal("expired key bytes were not reclaimed before insertion")
	}
	if !provider.rateLimited("unseen", now.Add(time.Minute)) {
		test.Fatal("reclaimed capacity forgot active overflow")
	}
	if provider.rateLimited("unseen", now.Add(2*time.Minute)) || !provider.scopedOverflowAt.IsZero() {
		test.Fatal("overflow did not expire at its deadline")
	}
	if !provider.rateLimited("fresh", now.Add(2*time.Minute)) {
		test.Fatal("overflow expiry removed a later scoped cooldown")
	}
	if provider.rateLimited("fresh", now.Add(time.Hour)) || len(provider.scopedLimits) != 0 || provider.scopedKeyBytes != 0 {
		test.Fatal("read did not sweep expired cooldown")
	}
}

func TestScopedCooldownExtensionsAndIsolation(test *testing.T) {
	now := time.Unix(1000, 0)
	provider := NewGitHub(nil)
	provider.retainRateLimit("scope", now.Add(time.Hour), now)
	provider.retainRateLimit("scope", now.Add(time.Minute), now)
	provider.retainRateLimit("scope", now.Add(2*time.Hour), now)
	if provider.scopedLimits["scope"] != now.Add(2*time.Hour) || provider.scopedKeyBytes != len("scope") || len(provider.scopedLimits) != 1 {
		test.Fatal("extension shortened cooldown or double-counted key")
	}
	provider.retainRateLimit("expired", now, now)
	if len(provider.scopedLimits) != 1 || provider.rateLimited("other", now) || provider.rateLimited("", now) {
		test.Fatal("normal scoped cooldown blocked an unrelated scope")
	}
	provider.retainRateLimit("", now.Add(time.Hour), now)
	provider.retainRateLimit("", now.Add(time.Minute), now)
	if !provider.rateLimited("", now) || provider.rateLimited("other", now) || provider.resumeAt != now.Add(time.Hour) {
		test.Fatal("global fallback behavior changed")
	}
}

func TestScopedCooldownOverflowBlocksHTTPAfterCredentialLookup(test *testing.T) {
	bucket := strings.Repeat("x", scopedLimitMaxKeyBytes+1)
	lookups, requests := 0, 0
	var credentialErr error
	provider := NewAuthenticated(func(_ context.Context, path string) (string, string, string, error) {
		lookups++
		return fmt.Sprintf("token-%d", lookups), bucket, "/authorized" + path, credentialErr
	})
	provider.client = &http.Client{Transport: cooldownBoundsTransport(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Path != "/authorized/evidence" || request.Header.Get("Authorization") != "Bearer token-1" {
			test.Fatal("current credential or authorized path lost")
		}
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	var output any
	if err := provider.get(context.Background(), "/evidence", &output); err == nil || requests != 1 || provider.scopedOverflowAt.IsZero() {
		test.Fatal("provider cooldown was not retained on overflow")
	}
	bucket = "other"
	if err := provider.get(context.Background(), "/evidence", &output); err == nil || requests != 1 || lookups != 2 {
		test.Fatal("overflow bypassed cooldown or skipped current credentials")
	}
	credentialErr = errors.New("revoked")
	if err := provider.get(context.Background(), "/evidence", &output); !errors.Is(err, credentialErr) || requests != 1 || lookups != 3 {
		test.Fatal("cooldown masked current authorization failure")
	}
	if len(provider.scopedLimits) != 0 || provider.scopedKeyBytes != 0 {
		test.Fatal("oversized bucket was retained")
	}
}

func TestScopedCooldownConcurrentOverflowKeepsLongestDeadline(test *testing.T) {
	now := time.Unix(1000, 0)
	provider := NewGitHub(nil)
	provider.retainRateLimit(strings.Repeat("x", scopedLimitMaxKeyBytes), now.Add(time.Hour), now)
	var workers sync.WaitGroup
	for index := 1; index <= 32; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			provider.retainRateLimit(fmt.Sprintf("overflow-%d", index), now.Add(time.Duration(index)*time.Minute), now)
			if !provider.rateLimited("unseen", now) {
				test.Error("concurrent overflow bypassed cooldown")
			}
		}(index)
	}
	workers.Wait()
	if len(provider.scopedLimits) != 1 || provider.scopedKeyBytes != scopedLimitMaxKeyBytes || provider.scopedOverflowAt != now.Add(32*time.Minute) {
		test.Fatal("concurrent overflow lost bounds or longest deadline")
	}
	if provider.rateLimited("unseen", now.Add(32*time.Minute)) || !provider.rateLimited(strings.Repeat("x", scopedLimitMaxKeyBytes), now.Add(32*time.Minute)) {
		test.Fatal("overflow expiry did not preserve original active cooldown")
	}
}
