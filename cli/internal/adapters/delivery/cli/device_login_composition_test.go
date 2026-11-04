package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

func testDeviceLoginContainer() *Container {
	return &Container{DeviceLoginClient: func(base string) (outbound.DeviceLoginClient, error) {
		return backendclient.NewDeviceLoginClient(base), nil
	}}
}

type deviceLoginCompositionClient struct {
	start  func(context.Context, string) (domain.DeviceLoginAuthorization, error)
	poll   func(context.Context, string, string) (domain.DeviceLoginApproval, error)
	verify func(context.Context, string) (string, error)
}

func (client *deviceLoginCompositionClient) Start(ctx context.Context, label string) (domain.DeviceLoginAuthorization, error) {
	return client.start(ctx, label)
}

func (client *deviceLoginCompositionClient) Poll(ctx context.Context, code, secret string) (domain.DeviceLoginApproval, error) {
	return client.poll(ctx, code, secret)
}

func (client *deviceLoginCompositionClient) VerifyToken(ctx context.Context, token string) (string, error) {
	return client.verify(ctx, token)
}

func TestDeviceLoginRequiresCompositionBeforeSideEffects(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	for _, container := range []*Container{nil, {}, {DeviceLoginClient: func(string) (outbound.DeviceLoginClient, error) { return nil, nil }}, {DeviceLoginClient: func(string) (outbound.DeviceLoginClient, error) {
		var client *deviceLoginCompositionClient
		return client, nil
	}}} {
		if err := container.deviceLogin(context.Background(), "https://api.example.test/api/v1", "api.example.test"); err == nil {
			test.Fatal("device login created an implicit transport")
		}
		if err := container.loginWithToken(context.Background(), "https://api.example.test/api/v1", "api.example.test", "not-for-output"); err == nil {
			test.Fatal("manual login created an implicit transport")
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".cxt")); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("missing login client modified credential storage", err)
	}
}

func TestDeviceLoginInjectedClientStaysBoundAcrossApproval(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Setenv("CXT_NO_BROWSER", "0")
	ctx := context.WithValue(context.Background(), struct{}{}, "request-scope")
	var calls []string
	client := &deviceLoginCompositionClient{
		start: func(actual context.Context, label string) (domain.DeviceLoginAuthorization, error) {
			if actual != ctx {
				test.Fatal("login lost caller context")
			}
			calls = append(calls, "start")
			return domain.DeviceLoginAuthorization{Code: "PAIR-1", PollToken: "private-poll", Interval: 1, ExpiresIn: 30, VerificationURIComplete: "https://app.example.test/login/device?code=PAIR-1"}, nil
		},
		poll: func(actual context.Context, code, secret string) (domain.DeviceLoginApproval, error) {
			if actual != ctx || code != "PAIR-1" || secret != "private-poll" {
				test.Fatal("poll changed approval scope")
			}
			calls = append(calls, "poll")
			return domain.DeviceLoginApproval{Status: "approved", Token: "private-issued"}, nil
		},
		verify: func(actual context.Context, token string) (string, error) {
			if actual != ctx || token != "private-issued" {
				test.Fatal("verification changed approval scope")
			}
			calls = append(calls, "verify")
			return "fixture", nil
		},
	}
	factories := 0
	container := &Container{DeviceLoginClient: func(base string) (outbound.DeviceLoginClient, error) {
		factories++
		if base != "https://api.example.test/api/v1" {
			test.Fatal("login selected another API endpoint")
		}
		return client, nil
	}, OpenLoginBrowser: func(target string) {
		if target != "https://app.example.test/login/device?code=PAIR-1" {
			test.Fatal("browser received an unsafe endpoint")
		}
		calls = append(calls, "browser")
	}}
	output, err := captureContractStdout(test, func() error { return container.deviceLogin(ctx, "https://api.example.test/api/v1", "api.example.test") })
	if err != nil || factories != 1 || strings.Join(calls, ",") != "start,browser,poll,verify" || strings.Contains(output, "private-poll") || strings.Contains(output, "private-issued") {
		test.Fatal("login lost order, destination binding or secret confinement", err, factories, calls)
	}
}

func TestDeviceLoginFailedVerificationDoesNotStoreToken(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	failure := errors.New("verification unavailable")
	container := &Container{DeviceLoginClient: func(string) (outbound.DeviceLoginClient, error) {
		return &deviceLoginCompositionClient{verify: func(context.Context, string) (string, error) { return "", failure }}, nil
	}}
	if err := container.loginWithToken(context.Background(), "https://api.example.test/api/v1", "api.example.test", "private-input"); !errors.Is(err, failure) {
		test.Fatal("verification failure was hidden", err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".cxt", "auth.json")); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("failed verification saved the token", err)
	}
}
