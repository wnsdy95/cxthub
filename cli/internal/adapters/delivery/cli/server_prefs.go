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

	"github.com/wnsdy95/cxthub-cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
)

type serverPrefs struct {
	Endpoint string `json:"endpoint,omitempty"`
	LoadMode string `json:"load_mode,omitempty"`
	At       string `json:"at"`
}

func serverPrefsPath(cwd string) string {
	return filepath.Join(cxtRepoRoot(context.Background(), cwd), ".cxt", "server-prefs.json")
}

// serverLoadMode returns the load_mode personal setting of the logged-in account ("" = none/unlogged in).
// Freshness takes precedence: 400ms timeout GET /me → success refreshes cache, failure falls back to cache.
func serverLoadMode(cwd string) string {
	stateRoot := cxtRepoRoot(context.Background(), cwd)
	base, host, err := remoteAPIBase(cwd)
	if err != nil {
		return "" // Remote disconnected — no personal settings
	}
	tok := authcfg.Token(host)
	if tok == "" {
		observed, err := remotecfg.Observe(context.Background(), cwd)
		if err != nil {
			return ""
		}
		binding, err := observed.OriginEndpoint(os.Getenv("CXT_REMOTE"))
		if err != nil || binding.Endpoint != base {
			return ""
		}
		environmentBase := os.Getenv("CXT_REMOTE")
		if binding.RemoteURL != "" {
			environmentBase, err = remotecfg.APIBase(binding.RemoteURL)
			if err != nil {
				return ""
			}
		}
		tok = authcfg.TokenForDestination(base, environmentBase, os.Getenv("CXT_TOKEN"))
	}
	if tok == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if resp, err := loginHTTPClient().Do(req); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			var me struct {
				LoadMode string `json:"load_mode"`
			}
			if json.NewDecoder(resp.Body).Decode(&me) == nil {
				cache := serverPrefs{Endpoint: base, LoadMode: me.LoadMode, At: time.Now().UTC().Format(time.RFC3339)}
				if b, merr := json.Marshal(cache); merr == nil {
					_ = providerfs.WriteRepoFileAtomic(stateRoot, filepath.Join(".cxt", "server-prefs.json"), b, 0o644)
				}
				return me.LoadMode
			}
		}
	}
	// Offline/timout fallback: last successful value.
	if b, rerr := providerfs.ReadRepoFile(stateRoot, filepath.Join(".cxt", "server-prefs.json")); rerr == nil {
		var cache serverPrefs
		if json.Unmarshal(b, &cache) == nil && cache.Endpoint == base {
			return cache.LoadMode
		}
	}
	return ""
}
