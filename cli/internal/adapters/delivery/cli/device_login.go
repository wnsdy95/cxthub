package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// device flow login — `cxt login` (no args) default path (RFC 8628 pattern, gh compatibility).
// Token does not pass through screen/clipboard: server delivers directly to CLI.
// Fallback is always available: `cxt login <token>` (manual), CXT_TOKEN (CI).

// loginTarget allows authentication before a repository connection exists.
// Explicit targets are server origins, never credential-bearing URLs or paths.
func loginTarget(cwd, raw string) (base, host string, err error) {
	if raw == "" {
		if err = requireRemote(cwd); err != nil {
			return "", "", err
		}
		return remoteAPIBase(cwd)
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return "", "", fmt.Errorf("--server requires a server origin, for example https://cxthub.com")
	}
	base, err = remotecfg.CanonicalEndpoint(u.Scheme + "://" + u.Host + "/api/v1")
	if err != nil {
		return "", "", err
	}
	u, _ = url.Parse(base)
	return base, u.Host, nil
}

func (c *Container) loginClient(base string) (outbound.DeviceLoginClient, error) {
	if c == nil || c.DeviceLoginClient == nil {
		return nil, fmt.Errorf("device login client is not configured")
	}
	client, err := c.DeviceLoginClient(base)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("device login client is not configured")
	}
	value := reflect.ValueOf(client)
	if (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) && value.IsNil() {
		return nil, fmt.Errorf("device login client is not configured")
	}
	return client, nil
}

// loginWithToken validates the token (GET /me) and stores it by host — manual/auto shared.
func (c *Container) loginWithToken(ctx context.Context, base, host, tok string) error {
	client, err := c.loginClient(base)
	if err != nil {
		return err
	}
	return storeLoginToken(ctx, client, host, tok)
}

func storeLoginToken(ctx context.Context, client outbound.DeviceLoginClient, host, tok string) error {
	username, err := client.VerifyToken(ctx, tok)
	if err != nil {
		return err
	}
	if err := authcfg.Save(host, tok); err != nil {
		return err
	}
	fmt.Printf("✓ Logged in to %s as %s (~/.cxt/auth.json, 0600)\n", host, username)
	return nil
}

// deviceLogin initiates pairing and receives/saves the token after browser approval.
func (c *Container) deviceLogin(ctx context.Context, base, host string) error {
	client, err := c.loginClient(base)
	if err != nil {
		return err
	}
	label, _ := os.Hostname() // device name — displayed in web token list as "which device" (empty on failure)
	start, err := client.Start(ctx, label)
	if err != nil {
		return fmt.Errorf("pairing start failed: %w", err)
	}
	verifyURL, err := deviceVerificationURL(base, host, start.Code, start.VerificationURIComplete)
	if err != nil {
		return err
	}

	fmt.Printf("Check code in browser and approve: %s\n", start.Code)
	fmt.Printf("  %s\n", verifyURL)
	if os.Getenv("CXT_NO_BROWSER") != "1" && c.OpenLoginBrowser != nil {
		c.OpenLoginBrowser(verifyURL)
	}

	interval := time.Duration(max(start.Interval, 2)) * time.Second
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		poll, err := client.Poll(ctx, start.Code, start.PollToken)
		if err != nil {
			return fmt.Errorf("pairing expired or canceled — run cxt login again (%w)", err)
		}
		if poll.Status == "approved" && poll.Token != "" {
			return storeLoginToken(ctx, client, host, poll.Token)
		}
	}
	return fmt.Errorf("approval wait time (%d seconds) exceeded — run cxt login again", start.ExpiresIn)
}

func deviceVerificationURL(base, host, code, complete string) (string, error) {
	api, err := url.Parse(base)
	if err != nil || api.Host != host || code == "" || len(code) > 128 {
		return "", fmt.Errorf("invalid device verification response")
	}
	if canonical, err := remotecfg.CanonicalEndpoint(base); err != nil || canonical != base {
		return "", fmt.Errorf("invalid device API transport")
	}
	if complete == "" {
		complete = api.Scheme + "://" + host + "/login/device?code=" + url.QueryEscape(code)
	}
	verification, err := url.Parse(complete)
	if err != nil || len(complete) > 2048 || verification.User != nil || verification.Opaque != "" || strings.Contains(complete, "#") || verification.Path != "/login/device" || verification.RawPath != "" {
		return "", fmt.Errorf("invalid device verification URL")
	}
	if _, err := remotecfg.CanonicalEndpoint(verification.Scheme + "://" + verification.Host + "/api/v1"); err != nil {
		return "", fmt.Errorf("unsafe device verification URL")
	}
	if verification.Scheme == "http" {
		if _, err := remotecfg.CanonicalEndpoint("http://" + api.Host + "/api/v1"); err != nil {
			return "", fmt.Errorf("loopback verification requires a loopback API endpoint")
		}
	}
	query, err := url.ParseQuery(verification.RawQuery)
	if err != nil || len(query) != 1 || len(query["code"]) != 1 || query.Get("code") != code {
		return "", fmt.Errorf("device verification URL must contain only the emitted pairing code")
	}
	return verification.String(), nil
}
