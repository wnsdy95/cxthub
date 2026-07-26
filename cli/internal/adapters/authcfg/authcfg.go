// Package authcfg stores CLI login tokens — compatible with git credential store.
//
// ~/.cxt/auth.json (0600): {"<host>": "sess_cli_..."}. Stores tokens by host for
// concurrent use of multiple cxthub servers (company/personal). Issued from Web account settings ⚙, registered with `cxt login <token>`. CXT_TOKEN env always takes precedence (for CI).
package authcfg

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

func path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cxt", "auth.json"), nil
}

func load() map[string]string {
	p, err := path()
	if err != nil {
		return map[string]string{}
	}
	m := map[string]string{}
	if b, err := providerfs.ReadRegularFile(p); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func save(m map[string]string) error {
	p, err := path()
	if err != nil {
		return err
	}
	if err := providerfs.EnsurePrivateDir(filepath.Dir(p)); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return providerfs.WriteRegularFileAtomic(p, b, 0o600)
}

// Token returns the CLI token stored for the host. Returns "" if not found.
func Token(host string) string { return load()[host] }

// Save stores the token for the host.
func Save(host, token string) error {
	m := load()
	m[host] = token
	return save(m)
}

// Delete removes the token for the host (idempotent).
func Delete(host string) error {
	m := load()
	if _, ok := m[host]; !ok {
		return nil
	}
	delete(m, host)
	return save(m)
}
