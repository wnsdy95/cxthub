package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/authcfg"
)

func TestEndpointDeviceVerificationURL(t *testing.T) {
	const api = "https://api.example.test/api/v1"
	const frontend = "https://app.example.test/login/device?code=PAIR-1234"
	for _, test := range []struct{ base, host, complete, want string }{
		{api, "api.example.test", frontend, frontend},
		{api, "api.example.test", "", "https://api.example.test/login/device?code=PAIR-1234"},
		{"http://127.0.0.1:8080/api/v1", "127.0.0.1:8080", "http://localhost:5173/login/device?code=PAIR-1234", "http://localhost:5173/login/device?code=PAIR-1234"},
	} {
		got, err := deviceVerificationURL(test.base, test.host, "PAIR-1234", test.complete)
		if err != nil || got != test.want {
			t.Fatal(got, err)
		}
	}
	for _, complete := range []string{
		"http://app.example.test/login/device?code=PAIR-1234",
		"http://localhost:5173/login/device?code=PAIR-1234",
		"https://secret@app.example.test/login/device?code=PAIR-1234",
		frontend + "#", frontend + "#fragment", frontend + "&poll_token=private", frontend + "&code=PAIR-1234",
		"https://app.example.test/login/device?code=WRONG", "https://app.example.test/login/other?code=PAIR-1234",
		"https://app.example.test/login%2fdevice?code=PAIR-1234", "//app.example.test/login/device?code=PAIR-1234",
	} {
		if _, err := deviceVerificationURL(api, "api.example.test", "PAIR-1234", complete); err == nil {
			t.Fatal("unsafe browser URL accepted")
		}
	}
}

type deviceEndpointTransport func(*http.Request) (*http.Response, error)

func (transport deviceEndpointTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestEndpointDeviceLoginSeparatesFrontendFromAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CXT_NO_BROWSER", "1")
	previous := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = previous })
	var paths []string
	http.DefaultClient = &http.Client{Transport: deviceEndpointTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" || request.URL.Host != "api.example.test" {
			t.Fatal("device secrets left API host")
		}
		paths = append(paths, request.URL.Path)
		body := ""
		switch request.URL.Path {
		case "/api/v1/auth/device/start":
			body = `{"code":"PAIR-1234","poll_token":"synthetic-poll-secret","expires_in":30,"interval":1,"verification_uri_complete":"https://app.example.test/login/device?code=PAIR-1234"}`
		case "/api/v1/auth/device/poll":
			var payload map[string]string
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || payload["code"] != "PAIR-1234" || payload["poll_token"] != "synthetic-poll-secret" {
				t.Fatal("poll payload changed", err)
			}
			body = `{"status":"approved","token":"synthetic-issued-secret"}`
		case "/api/v1/me":
			if request.Header.Get("Authorization") != "Bearer synthetic-issued-secret" {
				t.Fatal("wrong token verification")
			}
			body = `{"username":"synthetic-owner"}`
		default:
			t.Fatal("unexpected API request")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	output, err := captureContractStdout(t, func() error {
		return deviceLogin(context.Background(), "https://api.example.test/api/v1", "api.example.test")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "https://app.example.test/login/device?code=PAIR-1234") || strings.Contains(output, "synthetic-poll-secret") || strings.Contains(output, "synthetic-issued-secret") {
		t.Fatal("unsafe or missing browser output")
	}
	if !reflect.DeepEqual(paths, []string{"/api/v1/auth/device/start", "/api/v1/auth/device/poll", "/api/v1/me"}) {
		t.Fatal(paths)
	}
	if authcfg.Token("api.example.test") != "synthetic-issued-secret" || authcfg.Token("app.example.test") != "" {
		t.Fatal("credential stored under browser host")
	}
}
