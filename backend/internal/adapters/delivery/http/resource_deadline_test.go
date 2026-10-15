package http

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
)

type admissionDeadlineWriter struct {
	*httptest.ResponseRecorder
	read, write time.Time
}

func (writer *admissionDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	writer.read = deadline
	return nil
}

func (writer *admissionDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	writer.write = deadline
	return nil
}

func TestResourceAdmissionHTTPDeadline(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		ctx := context.Background()
		if earlier {
			bounded, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			ctx = bounded
		}
		writer := &admissionDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		start := time.Now()
		request, release, ok := (&Server{}).admitResourceRequest(writer, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), app.ResourceBulk)
		if !ok {
			t.Fatalf("admission failed: %s", writer.Body)
		}
		deadline, set := request.Context().Deadline()
		release()
		release()
		if !set || !deadline.Equal(writer.read) || !deadline.Equal(writer.write) || !errors.Is(request.Context().Err(), context.Canceled) {
			t.Fatalf("incomplete deadline lifecycle: %v %v %v %v", deadline, writer.read, writer.write, request.Context().Err())
		}
		if earlier {
			original, _ := ctx.Deadline()
			if !deadline.Equal(original) || ctx.Err() != nil {
				t.Fatal("parent deadline extended or parent canceled")
			}
		} else if deadline.Before(start.Add(resourceRequestLimit)) || deadline.After(time.Now().Add(resourceRequestLimit)) {
			t.Fatal("request limit not applied")
		}
	}
}

func TestResourceAdmissionHTTPStalledIOReleasesPermit(t *testing.T) {
	for _, protocol := range []string{"http1", "http2"} {
		for _, direction := range []string{"upload", "download"} {
			t.Run(protocol+"/"+direction, func(t *testing.T) {
				finished := make(chan error, 1)
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					ctx, cancel := context.WithTimeout(request.Context(), 300*time.Millisecond)
					defer cancel()
					request, release, ok := (&Server{}).admitResourceRequest(writer, request.WithContext(ctx), app.ResourceBulk)
					if !ok {
						finished <- errors.New("admission failed")
						return
					}
					var result error
					defer func() { release(); finished <- result }()
					if direction == "upload" {
						_, result = io.Copy(io.Discard, request.Body)
						return
					}
					if result = http.NewResponseController(writer).Flush(); result != nil {
						return
					}
					block := make([]byte, 64<<10)
					for total := 0; total < 64<<20; total += len(block) {
						if _, result = writer.Write(block); result != nil {
							return
						}
					}
					result = errors.New("stalled download did not block")
				}))
				server.EnableHTTP2 = protocol == "http2"
				if server.EnableHTTP2 {
					server.StartTLS()
				} else {
					server.Start()
				}
				defer server.Close()
				client := server.Client()
				client.Timeout = 5 * time.Second
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				var body io.Reader
				method := http.MethodGet
				if direction == "upload" {
					body, method = reader, http.MethodPost
				}
				request, err := http.NewRequest(method, server.URL, body)
				if err != nil {
					t.Fatal(err)
				}
				clientDone := make(chan struct{})
				drain := make(chan struct{})
				defer close(drain)
				go func() {
					defer close(clientDone)
					response, err := client.Do(request)
					if err == nil {
						<-drain
						response.Body.Close()
					}
				}()
				if direction == "upload" {
					if _, err := writer.Write([]byte("partial")); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case err := <-finished:
					var timeout net.Error
					timedOut := errors.As(err, &timeout) && timeout.Timeout()
					resetAfterTimeout := protocol == "http2" && err != nil && strings.HasPrefix(err.Error(), "stream error:") && strings.HasSuffix(err.Error(), "; i/o timeout")
					if !timedOut && !resetAfterTimeout {
						t.Fatalf("expected I/O timeout, got %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("stalled I/O retained permit")
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
				if err != nil {
					t.Fatalf("completed timeout leaked permit: %v", err)
				}
				release()
				t.Cleanup(func() {
					select {
					case <-clientDone:
					case <-time.After(5 * time.Second):
						t.Error("client did not exit")
					}
				})
			})
		}
	}
}

func TestResourceAdmissionHTTPDeadlineDoesNotPoisonKeepAlive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), 100*time.Millisecond)
		defer cancel()
		_, release, ok := (&Server{}).admitResourceRequest(writer, request.WithContext(ctx), app.ResourceBulk)
		if ok {
			defer release()
			_, _ = io.WriteString(writer, "ok")
		}
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	for index := 0; index < 2; index++ {
		reused := make(chan bool, 1)
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused <- info.Reused }}
		request, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, server.URL, nil)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || string(raw) != "ok" {
			t.Fatalf("keep-alive response: %d %q %v", response.StatusCode, raw, err)
		}
		if wasReused := <-reused; index == 1 && !wasReused {
			t.Fatal("deadline closed reusable connection")
		}
		time.Sleep(150 * time.Millisecond)
	}
}
