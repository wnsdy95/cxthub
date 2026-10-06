package remotecfg

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Cooperating config writers share the capture gate. Manual config edits, old
// clients and Git-origin changes require quiesced operations: they do not honor
// this flock, and a final reread cannot detect an external exact-byte ABA.

// ErrChanged means the exact observed config no longer owns this mutation.
var ErrChanged = fmt.Errorf("repository config changed; retry from current state: %w", domain.ErrSyncConflict)

// Observation binds conditional publication to a verified shared root and exact
// bytes (including mutation_id). Its version is written only by mutations; passive reads create no state.
// Absent, empty, malformed and valid files remain distinct observations.
type Observation struct {
	root   string
	raw    []byte
	exists bool
}

func mutationRoot(ctx context.Context, root string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if roots, err := gitctx.ResolveRepositoryRoots(ctx, root); err == nil {
		root = roots.SharedRoot
	} else {
		// Standalone adapter roots remain supported. A broken Git/worktree
		// marker must not silently redirect a managed write to a second .cxt.
		absolute, pathErr := filepath.Abs(root)
		if pathErr != nil {
			return "", pathErr
		}
		for dir := absolute; ; dir = filepath.Dir(dir) {
			if _, markerErr := os.Lstat(filepath.Join(dir, ".git")); markerErr == nil {
				return "", err
			} else if !os.IsNotExist(markerErr) {
				return "", markerErr
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}

// Observe is read-only, including when config is missing or malformed.
func Observe(ctx context.Context, root string) (Observation, error) {
	root, err := mutationRoot(ctx, root)
	if err != nil {
		return Observation{}, err
	}
	return observeAtRoot(root)
}

func observeAtRoot(root string) (Observation, error) {
	o := Observation{root: root}
	raw, err := providerfs.ReadRepoFile(root, ".cxt/config")
	if os.IsNotExist(err) {
		return o, nil
	}
	if err != nil {
		return Observation{}, err
	}
	o.raw, o.exists = raw, true
	return o, nil
}

func (o Observation) Remotes() (Remotes, error) {
	fields, err := o.fields()
	if err != nil {
		return nil, err
	}
	return decodeRemotes(fields)
}

func (o Observation) fields() (map[string]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if !o.exists {
		return fields, nil
	}
	// Validate existing known-field types while retaining unknown raw fields.
	var typed fileConfig
	if err := json.Unmarshal(o.raw, &typed); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(o.raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf(".cxt/config must be a JSON object")
	}
	if id, ok := fields["mutation_id"]; ok {
		var version string
		if json.Unmarshal(id, &version) != nil || len(version) != 32 {
			return nil, fmt.Errorf("invalid config mutation_id")
		}
		if _, err := hex.DecodeString(version); err != nil {
			return nil, fmt.Errorf("invalid config mutation_id")
		}
	}
	return fields, nil
}

func decodeRemotes(fields map[string]json.RawMessage) (Remotes, error) {
	var remotes Remotes
	if raw, ok := fields["remotes"]; ok {
		if err := json.Unmarshal(raw, &remotes); err != nil {
			return nil, err
		}
	}
	return canonicalRemotes(remotes)
}

func sameObservation(a, b Observation) bool {
	return a.root == b.root && a.exists == b.exists && bytes.Equal(a.raw, b.raw)
}

// mutate is the only config publication path. Callbacks perform bounded local
// validation/field edits only: no network, capture, ref or snapshot lock calls.
func mutate(ctx context.Context, root string, expected *Observation, repair bool, edit func(map[string]json.RawMessage, Observation) error) error {
	if root == "" {
		return ErrChanged
	}
	_, err := providerfs.WithCxtLock(ctx, root, "first-tracking", "repo", syscall.LOCK_EX, true, func() error {
		current, err := observeAtRoot(root)
		if err != nil {
			return err
		}
		if expected != nil && !sameObservation(current, *expected) {
			return ErrChanged
		}
		fields, err := current.fields()
		// Explicit repair may replace syntactically broken JSON after backup.
		// A parseable object with invalid fields is not an empty config: keep
		// its preferences and connection evidence intact for explicit correction.
		if err != nil && (!repair || json.Valid(current.raw)) {
			return err
		}
		if err != nil {
			fields = map[string]json.RawMessage{}
		}
		if err := edit(fields, current); err != nil {
			return err
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		fields["mutation_id"], _ = json.Marshal(hex.EncodeToString(id[:]))
		data, err := json.MarshalIndent(fields, "", "  ")
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		perm := os.FileMode(0644) // Retain existing config/repair publication modes.
		if repair {
			perm = 0600
		}
		return providerfs.WriteRepoFileDurable(root, ".cxt/config", append(data, '\n'), perm)
	})
	return err
}

func setField(ctx context.Context, root, key string, value any) error {
	root, err := mutationRoot(ctx, root)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return mutate(ctx, root, nil, false, func(fields map[string]json.RawMessage, _ Observation) error { fields[key] = raw; return nil })
}

// Replace is a whole-remotes CAS, primarily for import/fixtures. Ordinary
// commands use Add/Remove so caller-owned stale maps never overwrite peers.
func Replace(ctx context.Context, expected Observation, remotes Remotes) error {
	canonical, err := canonicalRemotes(remotes)
	if err != nil {
		return err
	}
	return mutate(ctx, expected.root, &expected, false, func(fields map[string]json.RawMessage, _ Observation) error {
		fields["remotes"], _ = json.Marshal(canonical)
		return nil
	})
}

// Add publishes only after connection work has completed. validateLocal may
// reread local Git evidence but must not perform network or acquire store locks.
func Add(ctx context.Context, expected Observation, name, url string, validateLocal func(context.Context) error) error {
	canonical, err := canonicalRemotes(Remotes{name: url})
	if err != nil {
		return err
	}
	return mutate(ctx, expected.root, &expected, false, func(fields map[string]json.RawMessage, _ Observation) error {
		remotes, err := decodeRemotes(fields)
		if err != nil {
			return err
		}
		if _, ok := remotes[name]; ok {
			return fmt.Errorf("remote %q already exists: %w", name, ErrChanged)
		}
		if validateLocal != nil {
			if err := validateLocal(ctx); err != nil {
				return err
			}
		}
		remotes[name] = canonical[name]
		fields["remotes"], _ = json.Marshal(remotes)
		return nil
	})
}

func Remove(ctx context.Context, expected Observation, name string) error {
	if !validRemoteName(name) {
		return fmt.Errorf("invalid remote name")
	}
	return mutate(ctx, expected.root, &expected, false, func(fields map[string]json.RawMessage, _ Observation) error {
		remotes, err := decodeRemotes(fields)
		if err != nil {
			return err
		}
		if _, ok := remotes[name]; !ok {
			return fmt.Errorf("remote %q not found", name)
		}
		delete(remotes, name)
		fields["remotes"], _ = json.Marshal(remotes)
		return nil
	})
}

// RepairOrigin conditions publication on the pre-fetch bytes/version, backs up
// exactly those bytes, and keeps valid preferences/other remotes. Object repair
// and config publication are separate operations; no network runs here.
func RepairOrigin(ctx context.Context, expected Observation, origin, backup string) error {
	canonical, err := CanonicalURL(origin)
	if err != nil {
		return err
	}
	return mutate(ctx, expected.root, &expected, true, func(fields map[string]json.RawMessage, current Observation) error {
		remotes := Remotes{}
		if raw, ok := fields["remotes"]; ok {
			if err := json.Unmarshal(raw, &remotes); err != nil {
				return err
			}
		}
		if old, err := CanonicalURL(remotes["origin"]); err == nil && old != canonical {
			return ErrChanged
		}
		if remotes == nil {
			remotes = Remotes{}
		}
		// Replace only the damaged/absent origin before validating the map;
		// an invalid unrelated remote must still reject the repair.
		remotes["origin"] = canonical
		remotes, err := canonicalRemotes(remotes)
		if err != nil {
			return err
		}
		if current.exists {
			if err := providerfs.WriteRepoFileDurable(backup, "config.before", current.raw, 0600); err != nil {
				return err
			}
		}
		fields["remotes"], _ = json.Marshal(remotes)
		return nil
	})
}
