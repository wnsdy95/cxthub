package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
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

// loginWithToken validates the token (GET /me) and stores it by host — manual/auto shared.
func loginWithToken(ctx context.Context, base, host, tok string) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := loginHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("token verification failed (%s) — run cxt login again", resp.Status)
	}
	var me struct {
		Username string `json:"username"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&me)
	if err := authcfg.Save(host, tok); err != nil {
		return err
	}
	fmt.Printf("✓ Logged in to %s as %s (~/.cxt/auth.json, 0600)\n", host, me.Username)
	return nil
}

// deviceLogin initiates pairing and receives/saves the token after browser approval.
func deviceLogin(ctx context.Context, base, host string) error {
	var start struct {
		Code                    string `json:"code"`
		PollToken               string `json:"poll_token"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
		VerificationURIComplete string `json:"verification_uri_complete"`
	}
	label, _ := os.Hostname() // device name — displayed in web token list as "which device" (empty on failure)
	if err := postJSON(ctx, base+"/auth/device/start", map[string]any{"label": label}, &start); err != nil {
		return fmt.Errorf("pairing start failed: %w", err)
	}
	verifyURL, err := deviceVerificationURL(base, host, start.Code, start.VerificationURIComplete)
	if err != nil {
		return err
	}

	fmt.Printf("Check code in browser and approve: %s\n", start.Code)
	fmt.Printf("  %s\n", verifyURL)
	if os.Getenv("CXT_NO_BROWSER") != "1" {
		openBrowser(verifyURL) // best-effort — fallback to manual URL if it fails
	}

	interval := time.Duration(max(start.Interval, 2)) * time.Second
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		var poll struct {
			Status string `json:"status"`
			Token  string `json:"token"`
		}
		err := postJSON(ctx, base+"/auth/device/poll",
			map[string]string{"code": start.Code, "poll_token": start.PollToken}, &poll)
		if err != nil {
			return fmt.Errorf("pairing expired or canceled — run cxt login again (%w)", err)
		}
		if poll.Status == "approved" && poll.Token != "" {
			return loginWithToken(ctx, base, host, poll.Token)
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

// postJSON handles JSON round-trip (2xx errors only).
func postJSON(ctx context.Context, url string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := loginHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func loginHTTPClient() *http.Client {
	client := *http.DefaultClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

// openBrowser opens the URL in the default browser (best-effort per platform).
func openBrowser(u string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", u).Start()
	case "linux":
		_ = exec.Command("xdg-open", u).Start()
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	}
}
