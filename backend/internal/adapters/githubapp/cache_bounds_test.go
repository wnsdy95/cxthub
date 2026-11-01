package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type cacheBoundsTransport func(*http.Request) (*http.Response, error)

func (transport cacheBoundsTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func cacheBoundsResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func cacheBoundsTokenResponse(token string) *http.Response {
	return cacheBoundsResponse(http.StatusCreated, fmt.Sprintf(`{"token":%q,"expires_at":%q}`, token, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
}

func cacheBoundsClient(testingContext *testing.T, transport cacheBoundsTransport) *Client {
	testingContext.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		testingContext.Fatal(err)
	}
	return &Client{
		cfg:    Config{ClientID: "cache-bounds"},
		key:    privateKey,
		http:   &http.Client{Transport: transport},
		api:    "https://github.invalid",
		tokens: make(map[int64]*tokenSlot),
		limits: make(map[string]time.Time),
	}
}

func assertCacheBounds(testingContext *testing.T, client *Client) {
	testingContext.Helper()
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.tokens) > optionalCacheTokenSlots || len(client.limits) > optionalCacheCooldownEntries || client.retainedCacheBytesLocked() > optionalCacheRetainedBytes {
		testingContext.Fatalf("cache policy exceeded: tokens=%d cooldowns=%d bytes=%d", len(client.tokens), len(client.limits), client.retainedCacheBytesLocked())
	}
}

func TestCacheBoundsTokenSlotCeilingEvictsOnlyIdle(testingContext *testing.T) {
	var calls atomic.Int32
	client := cacheBoundsClient(testingContext, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return cacheBoundsTokenResponse("new-token"), nil
	})
	active := client.acquireSlot(1, true)
	defer client.releaseSlot(1, active)
	previous := make(map[int64]*tokenSlot)
	for installationID := int64(2); installationID <= optionalCacheTokenSlots; installationID++ {
		cached := client.slot(installationID)
		cached.token = "idle-token"
		cached.expires = time.Now().Add(time.Hour)
		previous[installationID] = cached
	}
	if token, err := client.InstallationToken(context.Background(), optionalCacheTokenSlots+1); err != nil || token != "new-token" {
		testingContext.Fatalf("idle eviction rejected valid authentication: %v", err)
	}
	if client.tokens[1] != active || calls.Load() != 1 || len(client.tokens) != optionalCacheTokenSlots {
		testingContext.Fatal("slot ceiling or active ownership changed")
	}
	evicted := 0
	for installationID, cached := range previous {
		if client.tokens[installationID] != cached {
			evicted++
			if cached.token != "" || !cached.expires.IsZero() {
				testingContext.Fatal("evicted slot still retains credentials")
			}
		}
	}
	if evicted != 1 {
		testingContext.Fatalf("unexpected eviction count: %d", evicted)
	}
	assertCacheBounds(testingContext, client)
}

func TestCacheBoundsInFlightInvalidationAndSaturation(testingContext *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	releaseRequest := func() { resumeOnce.Do(func() { close(resume) }) }
	testingContext.Cleanup(releaseRequest)
	client := cacheBoundsClient(testingContext, func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-resume
		}
		return cacheBoundsTokenResponse("fresh-token"), nil
	})
	results := make(chan error, 2)
	go func() {
		_, err := client.InstallationToken(context.Background(), 9)
		results <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		testingContext.Fatal("refresh did not start")
	}
	go func() {
		_, err := client.InstallationToken(context.Background(), 9)
		results <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		client.mu.Lock()
		waiting := client.tokens[9].users == 2
		client.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			testingContext.Fatal("second caller did not reserve the same slot")
		}
		runtime.Gosched()
	}
	client.mu.Lock()
	active := client.tokens[9]
	client.mu.Unlock()
	client.Invalidate(9)
	if client.slot(9) != active {
		testingContext.Fatal("invalidation detached an in-flight slot")
	}
	reserved := make(map[int64]*tokenSlot)
	for installationID := int64(100); len(reserved) < optionalCacheTokenSlots-1; installationID++ {
		cached := client.acquireSlot(installationID, true)
		if cached == nil {
			testingContext.Fatal("unexpected early slot saturation")
		}
		reserved[installationID] = cached
	}
	if token, err := client.InstallationToken(context.Background(), 1); err == nil || token != "" || calls.Load() != 1 {
		testingContext.Fatal("saturated in-flight slots admitted provider traffic")
	}
	assertCacheBounds(testingContext, client)
	for installationID, cached := range reserved {
		client.releaseSlot(installationID, cached)
	}
	releaseRequest()
	for completed := 0; completed < 2; completed++ {
		select {
		case err := <-results:
			if !errors.Is(err, domain.ErrForbidden) {
				testingContext.Fatalf("in-flight invalidation resurrected a credential: %v", err)
			}
		case <-time.After(5 * time.Second):
			testingContext.Fatal("serialized callers did not complete")
		}
	}
	if calls.Load() != 1 || len(client.tokens) != 0 || active.token != "" {
		testingContext.Fatal("invalidated slot survived or refresh serialization broke")
	}
	if token, err := client.InstallationToken(context.Background(), 9); err != nil || token != "fresh-token" || calls.Load() != 2 {
		testingContext.Fatalf("post-invalidation refresh failed: %v", err)
	}
}

func TestCacheBoundsExpiredAndFailedSlotsAreReleased(testingContext *testing.T) {
	client := cacheBoundsClient(testingContext, func(request *http.Request) (*http.Response, error) {
		return cacheBoundsResponse(http.StatusUnauthorized, "private-provider-response"), nil
	})
	expired := client.slot(1)
	expired.token = "expired-secret"
	expired.expires = time.Now().Add(-time.Second)
	empty := client.slot(2)
	for installationID := int64(3); installationID < 12; installationID++ {
		if token, err := client.InstallationToken(context.Background(), installationID); !errors.Is(err, domain.ErrForbidden) || token != "" {
			testingContext.Fatalf("failed refresh classification changed: %v", err)
		}
	}
	if len(client.tokens) != 0 || expired.token != "" || !expired.expires.IsZero() || empty.token != "" {
		testingContext.Fatal("expired or failed token slots accumulated")
	}
}

func TestCacheBoundsOversizedTokenAuthenticatesWithoutRetention(testingContext *testing.T) {
	oversized := strings.Repeat("s", optionalCacheRetainedBytes+1)
	var calls atomic.Int32
	client := cacheBoundsClient(testingContext, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return cacheBoundsTokenResponse(oversized), nil
	})
	for attempt := 0; attempt < 2; attempt++ {
		token, err := client.InstallationToken(context.Background(), 9)
		if err != nil || token != oversized {
			testingContext.Fatalf("optional-cache guard rejected successful authentication: %v", err)
		}
		if len(client.tokens) != 0 {
			testingContext.Fatal("oversized token remained cached")
		}
		assertCacheBounds(testingContext, client)
	}
	if calls.Load() != 2 {
		testingContext.Fatal("oversized credential was reused from cache")
	}
}

func TestCacheBoundsCombinedTokenAndKeyEnvelope(testingContext *testing.T) {
	for _, active := range []bool{false, true} {
		testingContext.Run(strconv.FormatBool(active), func(testingContext *testing.T) {
			bucket := "existing-cooldown"
			token := strings.Repeat("s", optionalCacheRetainedBytes-8-len(bucket))
			client := cacheBoundsClient(testingContext, func(request *http.Request) (*http.Response, error) {
				return cacheBoundsTokenResponse(token), nil
			})
			client.deferRequests(bucket, http.Header{"Retry-After": {"60"}})
			if returned, err := client.InstallationToken(context.Background(), 9); err != nil || returned != token {
				testingContext.Fatalf("exact-envelope credential failed: %v", err)
			}
			cached := client.tokens[9]
			if cached == nil || client.retainedCacheBytesLocked() != optionalCacheRetainedBytes {
				testingContext.Fatal("token and cooldown keys were not jointly accounted")
			}
			if active {
				if client.acquireSlot(9, true) != cached {
					testingContext.Fatal("active slot changed")
				}
				defer client.releaseSlot(9, cached)
			}
			until := client.limits[bucket]
			client.deferRequests("new-cooldown", http.Header{"Retry-After": {"60"}})
			if client.limits[bucket] != until {
				testingContext.Fatal("active cooldown was evicted")
			}
			if active {
				if client.tokens[9] != cached || cached.token == "" || !client.limitOverflowUntil.After(time.Now()) || len(client.limits) != 1 {
					testingContext.Fatal("byte saturation evicted an in-flight token or lost cooldown")
				}
			} else if client.tokens[9] != nil || cached.token != "" || len(client.limits) != 2 {
				testingContext.Fatal("idle token did not yield optional retained bytes")
			}
			assertCacheBounds(testingContext, client)
		})
	}
}

func TestCacheBoundsCooldownOverflowFailsClosedUntilExpiry(testingContext *testing.T) {
	var calls atomic.Int32
	client := cacheBoundsClient(testingContext, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return cacheBoundsResponse(http.StatusOK, `{}`), nil
	})
	until := time.Now().Add(time.Hour)
	for index := 0; index < optionalCacheCooldownEntries; index++ {
		client.limits["installation:"+strconv.Itoa(index)] = until
	}
	client.deferRequests("overflow", http.Header{"Retry-After": {"120"}})
	blockedUntil := client.limitOverflowUntil
	client.deferRequests("another-overflow", http.Header{"Retry-After": {"60"}})
	client.deferRequests("installation:0", http.Header{"Retry-After": {"7200"}})
	if client.limitOverflowUntil != blockedUntil || len(client.limits) != optionalCacheCooldownEntries || !client.limits["installation:0"].After(until) {
		testingContext.Fatal("overflow shortened cooldowns, lost an entry, or exceeded cardinality")
	}
	if err := client.request(context.Background(), http.MethodGet, client.api+"/ok", "sensitive", "unrelated", nil, &struct{}{}); err == nil || !strings.Contains(err.Error(), "rate limited") || strings.Contains(err.Error(), "sensitive") || calls.Load() != 0 {
		testingContext.Fatal("cooldown overflow allowed traffic or exposed credentials")
	}
	assertCacheBounds(testingContext, client)
	for bucket := range client.limits {
		client.limits[bucket] = time.Now().Add(-time.Second)
	}
	client.limitOverflowUntil = time.Now().Add(-time.Second)
	if err := client.request(context.Background(), http.MethodGet, client.api+"/ok", "", "unrelated", nil, &struct{}{}); err != nil || calls.Load() != 1 || len(client.limits) != 0 || !client.limitOverflowUntil.IsZero() {
		testingContext.Fatalf("expired cooldowns did not release admission: %v", err)
	}
}

func TestCacheBoundsOversizedCooldownKeyIsNotRetained(testingContext *testing.T) {
	var calls atomic.Int32
	client := cacheBoundsClient(testingContext, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return cacheBoundsResponse(http.StatusOK, `{}`), nil
	})
	client.deferRequests(strings.Repeat("k", optionalCacheRetainedBytes+1), http.Header{"Retry-After": {"60"}})
	if len(client.limits) != 0 || !client.limitOverflowUntil.After(time.Now()) {
		testingContext.Fatal("oversized cooldown key was retained or forgotten")
	}
	if err := client.request(context.Background(), http.MethodGet, client.api+"/ok", "", "unrelated", nil, nil); err == nil || calls.Load() != 0 {
		testingContext.Fatal("oversized cooldown key bypassed fail-closed admission")
	}
	assertCacheBounds(testingContext, client)
}

func TestCacheBoundsCooldownKeepsNormalInstallationAndResourceScope(testingContext *testing.T) {
	var calls atomic.Int32
	client := cacheBoundsClient(testingContext, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return cacheBoundsResponse(http.StatusOK, `{}`), nil
	})
	client.deferRequests("installation:9:graphql", http.Header{"Retry-After": {"60"}})
	if err := client.request(context.Background(), http.MethodPost, client.api+"/graphql", "", "installation:9", nil, &struct{}{}); err == nil {
		testingContext.Fatal("GraphQL cooldown was bypassed")
	}
	if err := client.request(context.Background(), http.MethodGet, client.api+"/ok", "", "installation:9", nil, &struct{}{}); err != nil {
		testingContext.Fatalf("GraphQL cooldown blocked REST: %v", err)
	}
	client.deferRequests("installation:9", http.Header{"Retry-After": {"60"}})
	if err := client.request(context.Background(), http.MethodGet, client.api+"/ok", "", "installation:10", nil, &struct{}{}); err != nil || calls.Load() != 2 {
		testingContext.Fatalf("normal-capacity cooldown crossed installations: %v", err)
	}
}
