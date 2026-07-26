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
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/authcfg"
)

// device flow login — `cxt login` (no args) default path (RFC 8628 pattern, gh compatibility).
// Token does not pass through screen/clipboard: server delivers directly to CLI.
// Fallback is always available: `cxt login <token>` (manual), CXT_TOKEN (CI).

// loginWithToken validates the token (GET /me) and stores it by host — manual/auto shared.
func loginWithToken(ctx context.Context, base, host, tok string) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
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
		Code      string `json:"code"`
		PollToken string `json:"poll_token"`
		ExpiresIn int    `json:"expires_in"`
		Interval  int    `json:"interval"`
	}
	label, _ := os.Hostname() // device name — displayed in web token list as "which device" (empty on failure)
	if err := postJSON(ctx, base+"/auth/device/start", map[string]any{"label": label}, &start); err != nil {
		return fmt.Errorf("pairing start failed: %w", err)
	}
	u, _ := url.Parse(base)
	verifyURL := u.Scheme + "://" + host + "/login/device?code=" + url.QueryEscape(start.Code)

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

// postJSON handles JSON round-trip (2xx errors only).
func postJSON(ctx context.Context, url string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
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
