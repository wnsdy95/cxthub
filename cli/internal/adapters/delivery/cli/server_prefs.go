// server_prefs.go — Server personal settings (account global, GET /me) consumption.
//
// The point of setting consumption is "when the CLI runs load/checkout", so a push (server→local
// notification) channel is unnecessary: consume with a short timeout pull, and fallback to the last
// successful value cache (.cxt/server-prefs.json) on failure (offline) — "reflect immediately upon reaching the server" and
// offline safety simultaneously.
package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

type serverPrefs struct {
	LoadMode string `json:"load_mode,omitempty"`
	At       string `json:"at"`
}

func serverPrefsPath(cwd string) string { return filepath.Join(cwd, ".cxt", "server-prefs.json") }

// serverLoadMode returns the load_mode personal setting of the logged-in account ("" = none/unlogged in).
// Freshness takes precedence: 400ms timeout GET /me → success refreshes cache, failure falls back to cache.
func serverLoadMode(cwd string) string {
	base, host, err := remoteAPIBase(cwd)
	if err != nil {
		return "" // Remote disconnected — no personal settings
	}
	tok := authcfg.Token(host)
	if tok == "" {
		tok = os.Getenv("CXT_TOKEN")
	}
	if tok == "" {
		return ""
	}
	_ = host
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			var me struct {
				LoadMode string `json:"load_mode"`
			}
			if json.NewDecoder(resp.Body).Decode(&me) == nil {
				cache := serverPrefs{LoadMode: me.LoadMode, At: time.Now().UTC().Format(time.RFC3339)}
				if b, merr := json.Marshal(cache); merr == nil {
					_ = providerfs.WriteRepoFileAtomic(cwd, filepath.Join(".cxt", "server-prefs.json"), b, 0o644)
				}
				return me.LoadMode
			}
		}
	}
	// Offline/timout fallback: last successful value.
	if b, rerr := providerfs.ReadRepoFile(cwd, filepath.Join(".cxt", "server-prefs.json")); rerr == nil {
		var cache serverPrefs
		if json.Unmarshal(b, &cache) == nil {
			return cache.LoadMode
		}
	}
	return ""
}
