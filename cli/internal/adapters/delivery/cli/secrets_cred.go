package cli

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

// Team passphrase storage — same principle as git credential.helper store: keys are outside the repo.
//
// ~/.cxt/credentials.json (0600): {"<repoID>": "<passphrase>"}.
//
// Not storing in .env:
//  1. .cxtsecrets targets .env values — storing keys next to them leaks both key and lock.
//  2. .env is a representative file loaded by apps/docker and is a broad surface for leaks.
//  3. cxt init uses .env values as extraction targets, causing a self-reference in the masking list.
//
// Secret push/pull order: -p flag > CXT_SECRETS_PASSPHRASE > this repo.
// Storage is only done with --remember (opt-in like git).

func credentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cxt", "credentials.json"), nil
}

// loadStoredPassphrase returns the passphrase stored for repoID. Returns "" if not found.
func loadStoredPassphrase(repoID string) string {
	path, err := credentialsPath()
	if err != nil {
		return ""
	}
	b, err := providerfs.ReadRegularFile(path)
	if err != nil {
		return ""
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return m[repoID]
}

// storePassphrase stores the passphrase for repoID in 0600 (directory 0700).
func storePassphrase(repoID, pass string) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	m := map[string]string{}
	if b, err := providerfs.ReadRegularFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	m[repoID] = pass
	if err := providerfs.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return providerfs.WriteRegularFileAtomic(path, b, 0o600)
}

// hasFlag checks if the name flag is exactly in args.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}
