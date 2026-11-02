package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDeviceLoginTransportWireAndCredentialScope(test *testing.T) {
	var mutex sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		requests = append(requests, request.Method+" "+request.URL.Path)
		mutex.Unlock()
		switch request.URL.Path {
		case "/api/v1/auth/device/start":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["label"] != "fixture-device" || request.Header.Get("Authorization") != "" || request.Header.Get("Content-Type") != "application/json" {
				test.Error("start changed JSON or credential scope", err)
			}
			_, _ = writer.Write([]byte(`{"code":"PAIR","poll_token":"poll-secret","expires_in":30,"interval":2,"verification_uri_complete":"https://app.example.test/login/device?code=PAIR"}`))
		case "/api/v1/auth/device/poll":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["code"] != "PAIR" || body["poll_token"] != "poll-secret" || request.Header.Get("Authorization") != "" {
				test.Error("poll changed credential scope", err)
			}
			_, _ = writer.Write([]byte(`{"status":"approved","token":"issued-secret"}`))
		case "/api/v1/me":
			if request.Header.Get("Authorization") != "Bearer issued-secret" {
				test.Error("identity verification lost explicit token")
			}
			_, _ = writer.Write([]byte(`{"username":"fixture"}`))
		default:
			test.Error("unexpected login endpoint")
		}
	}))
	defer server.Close()
	client := NewDeviceLoginClient(server.URL + "/api/v1")
	start, err := client.Start(context.Background(), "fixture-device")
	if err != nil || start.Code != "PAIR" || start.PollToken != "poll-secret" || start.Interval != 2 || start.ExpiresIn != 30 {
		test.Fatal("start response changed", err)
	}
	poll, err := client.Poll(context.Background(), start.Code, start.PollToken)
	if err != nil || poll.Status != "approved" || poll.Token != "issued-secret" {
		test.Fatal("poll response changed", err)
	}
	username, err := client.VerifyToken(context.Background(), poll.Token)
	mutex.Lock()
	defer mutex.Unlock()
	if err != nil || username != "fixture" || strings.Join(requests, ",") != "POST /api/v1/auth/device/start,POST /api/v1/auth/device/poll,GET /api/v1/me" {
		test.Fatal("login exchange changed", err, username, requests)
	}
}

func TestDeviceLoginTransportRejectsRedirects(test *testing.T) {
	var escaped atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { escaped.Add(1) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL+request.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := NewDeviceLoginClient(server.URL + "/api/v1")
	if _, err := client.Start(context.Background(), "private-label"); err == nil {
		test.Fatal("redirected start was accepted")
	}
	if _, err := client.Poll(context.Background(), "PAIR", "poll-secret"); err == nil {
		test.Fatal("redirected poll was accepted")
	}
	if _, err := client.VerifyToken(context.Background(), "issued-secret"); err == nil {
		test.Fatal("redirected verification was accepted")
	}
	if escaped.Load() != 0 {
		test.Fatal("login credentials followed a redirect")
	}
}

func TestDeviceLoginTransportPreservesCancellation(test *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	client := NewDeviceLoginClient(server.URL + "/api/v1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Start(ctx, "fixture"); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
	if _, err := client.Poll(ctx, "PAIR", "poll-secret"); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
	if _, err := client.VerifyToken(ctx, "issued-secret"); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
	if requests.Load() != 0 {
		test.Fatal("canceled login reached server")
	}
}
