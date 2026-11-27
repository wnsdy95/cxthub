package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

type capturePushRoundTrip func(*http.Request) (*http.Response, error)

func (f capturePushRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func capturePushJSON(t *testing.T, r *http.Request, status int, value any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw)), Request: r}
}
