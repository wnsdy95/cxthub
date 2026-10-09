// Package capturejournal stores capture recovery decisions independently of the
// original attempts. Reads neither create directories nor run capture/replay.
package capturejournal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type Journal struct{ root, cwd string }

func New(root, cwd string) *Journal { return &Journal{root: root, cwd: cwd} }

func attemptPath(p domain.CaptureAttempt) string {
	return filepath.Join(".cxt", "worktrees", p.Proof.WorktreeID, "capture-passes", p.Proof.ID+".json")
}
func resolutionPath(p domain.CaptureAttempt) string {
	return filepath.Join(".cxt", "worktrees", p.Proof.WorktreeID, "capture-resolutions", p.Proof.ID+".json")
}

func (j *Journal) readDir(relative string) ([]os.DirEntry, error) {
	root, err := filepath.EvalSymlinks(j.root)
	if err != nil {
		return nil, err
	}
	path := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return nil, domain.ErrHashMismatch
		}
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, domain.ErrHashMismatch
		}
	}
	return os.ReadDir(path)
}

func (j *Journal) ListCaptureAttempts(ctx context.Context, repo string) ([]domain.CaptureAttempt, error) {
	worktrees, err := j.readDir(filepath.Join(".cxt", "worktrees"))
	if err != nil {
		return nil, err
	}
	out := []domain.CaptureAttempt{}
	for _, w := range worktrees {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !w.IsDir() || w.Type()&os.ModeSymlink != 0 {
			return nil, domain.ErrHashMismatch
		}
		rel := filepath.Join(".cxt", "worktrees", w.Name(), "capture-passes")
		files, err := j.readDir(rel)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if filepath.Ext(f.Name()) != ".json" {
				continue
			}
			raw, err := providerfs.ReadRepoFile(j.root, filepath.Join(rel, f.Name()))
			if err != nil {
				return nil, err
			}
			var p domain.CaptureAttempt
			if json.Unmarshal(raw, &p) != nil || p.Proof.RepoID != repo || p.Proof.WorktreeID != w.Name() || p.Proof.ID+".json" != f.Name() {
				return nil, domain.ErrHashMismatch
			}
			if err := p.Validate(); err != nil {
				return nil, err
			}
			out = append(out, p)
		}
	}
	return out, nil
}

func (j *Journal) ReadCaptureResolution(ctx context.Context, p domain.CaptureAttempt) (*domain.CaptureResolution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	raw, err := providerfs.ReadRepoFile(j.root, resolutionPath(p))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r domain.CaptureResolution
	if json.Unmarshal(raw, &r) != nil {
		return nil, domain.ErrHashMismatch
	}
	if err := validateResolution(r, p); err != nil {
		return nil, err
	}
	return &r, nil
}

func validateResolution(r domain.CaptureResolution, p domain.CaptureAttempt) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if r.Version != 1 || r.RepoID != p.Proof.RepoID || r.WorktreeID != p.Proof.WorktreeID || r.AttemptID != p.Proof.ID || r.CreatedAt.IsZero() || r.CreatedAt.Before(p.Proof.CreatedAt) || p.Complete {
		return domain.ErrHashMismatch
	}
	if err := domain.ValidateContentHash(r.AttemptHash); err != nil {
		return err
	}
	switch r.Kind {
	case "superseded":
		if r.ReplacementID == r.AttemptID || !validID(r.ReplacementID) || !validID(r.PublicationID) || r.Reason != "" {
			return domain.ErrHashMismatch
		}
		return domain.ValidateContentHash(r.ReplacementHash)
	case "acknowledged-gap":
		if strings.TrimSpace(r.Reason) == "" || r.Reason != strings.TrimSpace(r.Reason) || len(r.Reason) > 1000 || r.ReplacementID != "" || r.ReplacementHash != "" || r.PublicationID != "" {
			return domain.ErrHashMismatch
		}
	default:
		return domain.ErrHashMismatch
	}
	return nil
}
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (j *Journal) WriteCaptureResolution(ctx context.Context, r domain.CaptureResolution, witnesses []domain.CaptureAttempt) error {
	if len(witnesses) == 0 || witnesses[0].Proof.ID != r.AttemptID || witnesses[0].Fingerprint() != r.AttemptHash {
		return domain.ErrHashMismatch
	}
	if err := validateResolution(r, witnesses[0]); err != nil {
		return err
	}
	if r.Kind == "superseded" && (len(witnesses) != 2 || witnesses[1].Proof.ID != r.ReplacementID || witnesses[1].Fingerprint() != r.ReplacementHash) {
		return domain.ErrHashMismatch
	}
	lock, err := branchjournal.Open(ctx, j.cwd)
	if err != nil {
		return err
	}
	return lock.Transaction(ctx, func() error {
		for _, expected := range witnesses {
			if err := expected.Validate(); err != nil {
				return err
			}
			raw, err := providerfs.ReadRepoFile(j.root, attemptPath(expected))
			if err != nil {
				return err
			}
			var current domain.CaptureAttempt
			if json.Unmarshal(raw, &current) != nil {
				return domain.ErrHashMismatch
			}
			if current.Fingerprint() != expected.Fingerprint() {
				return domain.ErrSyncConflict
			}
		}
		p := witnesses[0]
		old, err := j.ReadCaptureResolution(ctx, p)
		if err != nil {
			return err
		}
		if old != nil {
			comparison := r
			comparison.CreatedAt = old.CreatedAt
			if !reflect.DeepEqual(*old, comparison) {
				return domain.ErrSyncConflict
			}
			return nil
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := providerfs.WriteRepoFileDurable(j.root, resolutionPath(p), b, 0600); err != nil {
			return err
		}

		return nil
	})
}

func (j *Journal) ReadCaptureRetry(ctx context.Context, p domain.CaptureAttempt) (*domain.CaptureRetryState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	raw, err := providerfs.ReadRepoFile(j.root, filepath.Join(".cxt", "worktrees", p.Proof.WorktreeID, "capture-retries", p.Proof.ID+".json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r domain.CaptureRetryState
	if json.Unmarshal(raw, &r) != nil || r.Version != 1 || r.Attempt != p.Proof.ID || r.Tries < 1 || r.Tries > 8 || r.Next.IsZero() {
		return nil, domain.ErrHashMismatch
	}
	return &r, nil
}
