package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/secretscrypto"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type secretsRemote interface {
	SecretsOrigin() string
	PullSecrets(context.Context, string) ([]byte, error)
	PushSecrets(context.Context, string, []byte, bool, string, string) (string, error)
}

type secretsBaseline struct {
	Origin      string `json:"origin"`
	Repo        string `json:"repo"`
	Revision    string `json:"revision"`
	Fingerprint string `json:"fingerprint"`
	PlainHash   string `json:"plain_hash"`
}

func secretsPlainHash(plain []byte) string {
	sum := sha256.Sum256(plain)
	return hex.EncodeToString(sum[:])
}

// This file belongs to this worktree, never the shared context object store.
const secretsBaselinePath = ".cxt/secrets-baseline.json"

func syncSecrets(ctx context.Context, remote secretsRemote, cwd, repo, sub, pass string, rotate, force bool) error {
	return storage.NewFileStore(cwd).WithSecretsLock(ctx, func() error {
		baseline := secretsBaseline{Origin: remote.SecretsOrigin(), Repo: repo}
		if raw, err := providerfs.ReadRepoFile(cwd, secretsBaselinePath); err == nil {
			if err := json.Unmarshal(raw, &baseline); err != nil {
				return fmt.Errorf("invalid secrets baseline: %w", err)
			}
			if baseline.Origin != remote.SecretsOrigin() || baseline.Repo != repo {
				return fmt.Errorf("secrets baseline belongs to another repository or server; preserve .cxtsecrets and remove %s before pulling the correct version", secretsBaselinePath)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		plain, readErr := providerfs.ReadRepoFile(cwd, ".cxtsecrets")
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		if sub == "push" {
			if readErr != nil {
				return fmt.Errorf(".cxtsecrets not found")
			}
			if err := secretscrypto.ValidatePassphrase(pass); err != nil {
				if rotate {
					return err
				}
				fmt.Fprintf(os.Stderr, "warning: %v; required for --rotate\n", err)
			}
			if baseline.Revision == "" {
				_, err := remote.PullSecrets(ctx, repo)
				if !errors.Is(err, domain.ErrNotFound) {
					if err != nil {
						return err
					}
					return fmt.Errorf("no editing baseline for existing server secrets; keep a copy of .cxtsecrets, pull and compare before applying your edits")
				}
				baseline.Revision = "absent"
			}
			env, err := secretscrypto.Encrypt(pass, string(plain), repo)
			if err != nil {
				return err
			}
			env.UpdatedAt = time.Now().UTC()
			raw, err := json.Marshal(env)
			if err != nil {
				return err
			}
			revision, err := remote.PushSecrets(ctx, repo, raw, rotate, baseline.Fingerprint, baseline.Revision)
			if err != nil {
				if strings.Contains(err.Error(), "secrets_conflict") || strings.Contains(err.Error(), "rotate_conflict") {
					return fmt.Errorf("secrets changed since your last pull; your .cxtsecrets draft is unchanged. Copy it aside, pull --force, compare and reapply edits before pushing: %w", err)
				}
				return err
			}
			if revision == "" {
				return fmt.Errorf("missing secrets revision acknowledgment")
			}
			baseline.Revision, baseline.Fingerprint = revision, env.Fingerprint
		} else {
			raw, err := remote.PullSecrets(ctx, repo)
			if err != nil {
				return fmt.Errorf("could not read server secrets: %w", err)
			}
			var meta struct {
				Revision string `json:"revision"`
			}
			if err = json.Unmarshal(raw, &meta); err != nil {
				return err
			}
			if meta.Revision == "" {
				return fmt.Errorf("server does not support secrets edit revisions; update the server")
			}
			var env secretscrypto.Envelope
			if err = json.Unmarshal(raw, &env); err != nil {
				return err
			}
			decrypted, err := secretscrypto.Decrypt(pass, env, repo)
			if err != nil {
				return err
			}
			if readErr == nil && string(plain) != decrypted && !force && (baseline.PlainHash == "" || baseline.PlainHash != secretsPlainHash(plain)) {
				return fmt.Errorf("local .cxtsecrets has unsaved edits; preserve a copy and use pull --force only after reviewing the overwrite")
			}
			plain = []byte(decrypted)
			if err = providerfs.WriteRepoFileAtomic(cwd, ".cxtsecrets", plain, 0600); err != nil {
				return err
			}
			baseline.Revision, baseline.Fingerprint = meta.Revision, env.Fingerprint
		}
		baseline.PlainHash = secretsPlainHash(plain)
		raw, err := json.Marshal(baseline)
		if err != nil {
			return err
		}
		if err = providerfs.WriteRepoFileAtomic(cwd, secretsBaselinePath, raw, 0600); err != nil {
			return fmt.Errorf("secrets %s completed, but baseline persistence failed; keep your draft and pull before editing again: %w", sub, err)
		}
		fmt.Printf("✓ Secrets %s completed; editing revision recorded\n", sub)
		return nil
	})
}
