package backendclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type ackResponseTransportFunc func(*http.Request) (*http.Response, error)

func (f ackResponseTransportFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type ackResponseBody struct {
	io.ReadCloser
	bytesRead   int
	reads       int
	closes      int
	readErr     error
	onFirstRead func()
}

func (b *ackResponseBody) Read(p []byte) (int, error) {
	if b.reads == 0 && b.onFirstRead != nil {
		b.onFirstRead()
	}
	b.reads++
	n, err := b.ReadCloser.Read(p)
	b.bytesRead += n
	b.readErr = err
	return n, err
}

func (b *ackResponseBody) Close() error {
	b.closes++
	return b.ReadCloser.Close()
}

func newACKResponseClient(t *testing.T, url string) (*BackendClient, *http.Transport) {
	t.Helper()
	c := NewBackendClient(func() string { return url }, func() string { return "" }, domain.TeamIdentity{})
	transport := &http.Transport{}
	c.httpc.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	return c, transport
}

func TestIgnoredACKReusesHTTP1Connection(t *testing.T) {
	var connections, requests atomic.Int32
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("protocol = %s, want HTTP/1", r.Proto)
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\"status\":\"ok\"}\n")
	}))
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)
	c, _ := newACKResponseClient(t, ts.URL)
	for i := 0; i < 3; i++ {
		if err := c.do(context.Background(), http.MethodPost, "/ack", map[string]string{"update": "same"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 3 || connections.Load() != 1 {
		t.Fatalf("requests=%d connections=%d, want 3 requests on 1 connection", requests.Load(), connections.Load())
	}
}

func TestIgnoredACKDrainByteLimitAndReadError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reader    io.Reader
		wantBytes int
		wantErr   error
	}{
		{"oversized", strings.NewReader(strings.Repeat("x", 64<<10)), 32 << 10, nil},
		{"truncated", io.MultiReader(strings.NewReader(`{"status":`), iotest.ErrReader(io.ErrUnexpectedEOF)), len(`{"status":`), io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &ackResponseBody{ReadCloser: io.NopCloser(tc.reader)}
			c, _ := newACKResponseClient(t, "http://ack.invalid")
			var requestContext context.Context
			c.httpc.Transport = ackResponseTransportFunc(func(req *http.Request) (*http.Response, error) {
				requestContext = req.Context()
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
			})
			if err := c.do(context.Background(), http.MethodPost, "/ack", nil, nil); err != nil {
				t.Fatalf("acknowledged write failed during cleanup: %v", err)
			}
			if body.bytesRead != tc.wantBytes || body.closes != 1 || !errors.Is(body.readErr, tc.wantErr) {
				t.Fatalf("read=%d closed=%d read error=%v; want %d/1/%v", body.bytesRead, body.closes, body.readErr, tc.wantBytes, tc.wantErr)
			}
			if requestContext.Err() == nil {
				t.Fatal("completed request retained its cancellation context")
			}
		})
	}
}

func TestIgnoredACKStalledBodyIsBounded(t *testing.T) {
	for _, userCancellation := range []bool{false, true} {
		name := "drain timeout"
		if userCancellation {
			name = "caller cancellation"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "16")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(ts.Close)
			c, transport := newACKResponseClient(t, ts.URL)
			ctx, cancelTimeout := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelTimeout()
			ctx, cancel := context.WithCancelCause(ctx)
			defer cancel(nil)
			callerCanceled := errors.New("caller canceled ACK cleanup")
			readStarted := make(chan time.Time, 1)
			var requestContext context.Context
			c.httpc.Transport = ackResponseTransportFunc(func(req *http.Request) (*http.Response, error) {
				requestContext = req.Context()
				resp, err := transport.RoundTrip(req)
				if err == nil {
					resp.Body = &ackResponseBody{ReadCloser: resp.Body, onFirstRead: func() {
						// Cancel at the body-read boundary after successful headers,
						// without depending on how soon the test goroutine runs.
						if userCancellation {
							cancel(callerCanceled)
						}
						readStarted <- time.Now()
					}}
				}
				return resp, err
			})
			done := make(chan error, 1)
			go func() { done <- c.do(ctx, http.MethodPost, "/ack", nil, nil) }()
			var started time.Time
			select {
			case started = <-readStarted:
			case <-time.After(time.Second):
				t.Fatal("successful response did not start draining")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("acknowledged write failed during cleanup: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("stalled ACK ignored the short cleanup bound")
			}
			elapsed := time.Since(started)
			if userCancellation {
				if cause := context.Cause(requestContext); !errors.Is(cause, callerCanceled) {
					t.Fatalf("request cancellation cause = %v, want caller cancellation", cause)
				}
			} else if elapsed < ackDrainTimeout/2 || elapsed > 750*time.Millisecond {
				t.Fatalf("stalled ACK cleanup took %v, want about 100ms", elapsed)
			}
		})
	}
}

func TestIgnoredACKDrainStartsAfterHeaders(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * ackDrainTimeout):
			_, _ = io.WriteString(w, "{\"status\":\"ok\"}\n")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(ts.Close)
	c, _ := newACKResponseClient(t, ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.do(ctx, http.MethodPost, "/ack", nil, nil); err != nil {
		t.Fatalf("cleanup timeout affected the write before successful headers: %v", err)
	}
}

func TestIgnoredACKLeavesOtherResponseHandlingUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		out        any
		reader     io.Reader
		wantBytes  int
		wantDomain error
		wantHTTP   bool
	}{
		{"no content ACK", http.StatusNoContent, nil, strings.NewReader("must not read"), 0, nil, false},
		{"no content asset", http.StatusNoContent, &struct{}{}, strings.NewReader("must not read"), 0, domain.ErrNotFound, false},
		{"error", http.StatusConflict, nil, strings.NewReader(`{"error":{"code":"ref_conflict","message":"changed"}}` + strings.Repeat(" ", 64<<10)), 4096, nil, true},
		{"decoded JSON", http.StatusOK, &map[string]string{}, io.MultiReader(strings.NewReader("{\"status\":\"ok\"}\n"), strings.NewReader(strings.Repeat(" ", 64<<10))), len("{\"status\":\"ok\"}\n"), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &ackResponseBody{ReadCloser: io.NopCloser(tc.reader)}
			c, _ := newACKResponseClient(t, "http://ack.invalid")
			c.httpc.Transport = ackResponseTransportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: body}, nil
			})
			err := c.do(context.Background(), http.MethodPost, "/ack", nil, tc.out)
			if tc.wantHTTP {
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) || httpErr.Status != tc.status || httpErr.Code != "ref_conflict" {
					t.Fatalf("HTTP error changed: %v", err)
				}
			} else if !errors.Is(err, tc.wantDomain) {
				t.Fatalf("error = %v, want %v", err, tc.wantDomain)
			}
			if body.bytesRead != tc.wantBytes || body.closes != 1 {
				t.Fatalf("read=%d closed=%d, want %d/1", body.bytesRead, body.closes, tc.wantBytes)
			}
			if out, ok := tc.out.(*map[string]string); ok && (*out)["status"] != "ok" {
				t.Fatalf("decoded JSON changed: %v", *out)
			}
		})
	}
}
