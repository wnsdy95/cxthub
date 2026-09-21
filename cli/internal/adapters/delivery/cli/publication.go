package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type commitCaptureOutcome struct {
	Provider    string             `json:"provider"`
	State       string             `json:"state"` // pending, saved, absent, failed
	SessionPath string             `json:"session_path,omitempty"`
	Target      domain.ContentHash `json:"target,omitempty"`
	Error       string             `json:"error,omitempty"`
}

// A Save transaction proves only one provider finished. This journal is written
// before the first Save and remains incomplete across any unprovable crash gap.
// Complete proofs can be replayed without inspecting a position or transcript.
type commitCapturePass struct {
	Version  int                    `json:"version"`
	Proof    domain.HistoryEvent    `json:"proof"`
	Initial  domain.ContentHash     `json:"initial,omitempty"`
	Outcomes []commitCaptureOutcome `json:"outcomes"`
	// Reuse an immutable ordinary observation rather than manufacturing a
	// newer position that could change the selected memory at this Git SHA.
	Observation *domain.HistoryEvent `json:"observation,omitempty"`
	Complete    bool                 `json:"complete"`
}

func (p *commitCapturePass) relativePath() string {
	return filepath.Join(".cxt", "worktrees", p.Proof.WorktreeID, "capture-passes", p.Proof.ID+".json")
}

func (p *commitCapturePass) pending(cause error) error {
	return fmt.Errorf("capture pass %s for Git %s remains pending (%s); automatic replay cannot infer missing capture completion: %w", p.Proof.ID, p.Proof.GitAfter, p.relativePath(), cause)
}

func (p *commitCapturePass) write(root string) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return providerfs.WriteRepoFileDurable(root, p.relativePath(), raw, 0600)
}

func (p *commitCapturePass) checkGit(cwd string) error {
	branch := p.Proof.LocalBranch
	if branch == "" {
		branch = p.Proof.Branch
	}
	gitDir := gitOut(cwd, "rev-parse", "--absolute-git-dir")
	key := sha256.Sum256([]byte(gitDir))
	if gitDir == "" || fmt.Sprintf("%x", key[:16]) != p.Proof.WorktreeID ||
		gitOut(cwd, "rev-parse", "HEAD") != p.Proof.GitAfter ||
		gitOut(cwd, "symbolic-ref", "--quiet", "--short", "HEAD") != branch {
		return fmt.Errorf("Git SHA, branch, or worktree changed during capture")
	}
	return nil
}

func beginCommitCapture(ctx context.Context, c *Container, cwd string, providers []string) (*commitCapturePass, error) {
	if c.History == nil {
		return nil, nil
	}
	position, err := c.History.CurrentPosition(ctx)
	if err != nil {
		return nil, err
	}
	oid := gitOut(cwd, "rev-parse", "HEAD")
	branch := gitOut(cwd, "symbolic-ref", "--quiet", "--short", "HEAD")
	if branch == "" {
		return nil, nil
	} // Detached work has no PR branch to finalize.
	if position.GitBranch() != branch || position.WorktreeID == "" || !validNonZeroGitOID(oid) {
		return nil, fmt.Errorf("cannot begin commit capture without its exact worktree identity")
	}
	id, err := branchjournal.NewID()
	if err != nil {
		return nil, err
	}
	p := &commitCapturePass{Version: 1, Initial: position.Snapshot, Proof: domain.HistoryEvent{
		ID: id, RepoID: position.RepoID, BranchID: position.BranchID, Branch: position.Branch,
		LocalBranch: position.LocalBranch, WorktreeID: position.WorktreeID,
		Kind: "position", GitAfter: oid, MemoryPinned: true, CreatedAt: time.Now().UTC(),
		MemoryHash: position.MemoryHash, MemorySource: position.MemorySource,
	}}
	for _, provider := range providers {
		p.Outcomes = append(p.Outcomes, commitCaptureOutcome{Provider: provider, State: "pending"})
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	if err := p.checkGit(cwd); err != nil {
		return nil, err
	}
	if err := p.write(cxtRepoRoot(ctx, cwd)); err != nil {
		return nil, fmt.Errorf("persist capture intent before Save: %w", err)
	}
	return p, nil
}

func (p *commitCapturePass) recordOutcome(root string, index int, path, state string, out inbound.SaveOutput, cause error) error {
	if p == nil {
		return nil
	}
	o := &p.Outcomes[index]
	o.State, o.SessionPath, o.Target = state, path, out.SnapshotID
	if state == "failed" && domain.ValidateContentHash(o.Target) != nil {
		o.Target = "" // A malformed Save result is a failed pass, not corrupt journal metadata.
	}
	if cause != nil {
		o.Error = cause.Error()
	}
	return p.write(root)
}

// Only the frozen baseline and this pass's Save outputs can become its final
// target. A mutable worktree tip may belong to another, incomplete pass.
func recordCommitPublication(ctx context.Context, c *Container, cwd string, p *commitCapturePass) error {
	if p == nil {
		return nil
	}
	if err := p.checkGit(cwd); err != nil {
		return p.pending(err)
	}
	if err := prepareCommitProof(ctx, c, p); err != nil {
		return p.pending(err)
	}
	if err := p.checkGit(cwd); err != nil {
		return p.pending(err)
	}
	if err := completeCapturePass(ctx, cwd, cxtRepoRoot(ctx, cwd), p); err != nil {
		return p.pending(err)
	}
	return publishCommitCapture(ctx, c, cwd, p, nil)
}

var errCaptureCompletionUnproven = errors.New("capture completion is unproven")

func prepareCommitProof(ctx context.Context, c *Container, p *commitCapturePass) error {
	p.Proof.Source, p.Proof.Target = "", ""
	p.Observation = nil
	targets := []domain.ContentHash{}
	if p.Initial != "" {
		targets = append(targets, p.Initial)
	}
	for _, o := range p.Outcomes {
		switch o.State {
		case "saved":
			if err := domain.ValidateContentHash(o.Target); err != nil {
				return err
			}
			targets = append(targets, o.Target)
		case "absent":
		default:
			return fmt.Errorf("%w: %s capture is %s: %s", errCaptureCompletionUnproven, o.Provider, o.State, o.Error)
		}
	}
	if len(targets) > 0 {
		if c.List == nil {
			return fmt.Errorf("snapshot graph unavailable")
		}
		listed, err := c.List.List(ctx, inbound.ListInput{RepoID: p.Proof.RepoID})
		if err != nil {
			return err
		}
		byID := map[domain.ContentHash]domain.Snapshot{}
		for _, s := range listed.Snapshots {
			byID[s.ID] = s
		}
		contains := func(tip, ancestor domain.ContentHash) bool {
			queue, seen := []domain.ContentHash{tip}, map[domain.ContentHash]bool{}
			for len(queue) > 0 {
				id := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				if id == ancestor {
					return true
				}
				if seen[id] {
					continue
				}
				seen[id] = true
				if s, ok := byID[id]; ok && s.RepoID == p.Proof.RepoID {
					queue = append(queue, s.ReachabilityParents()...)
				}
			}
			return false
		}
		for i := len(targets) - 1; i >= 0; i-- {
			tip, complete := targets[i], true
			for _, target := range targets {
				if !contains(tip, target) {
					complete = false
					break
				}
			}
			if complete {
				p.Proof.Source, p.Proof.Target = tip, tip
				break
			}
		}
		if p.Proof.Target == "" {
			return fmt.Errorf("%w: no saved result contains every capture and the frozen baseline", errCaptureCompletionUnproven)
		}
		accepted, err := publicationHistory(ctx, c, p.Proof.RepoID)
		if err != nil {
			return err
		}
		p.Observation = p.observation(p.Proof.Target, accepted)
		if p.Observation == nil {
			// Dedup can leave no observation at the new SHA. Only the frozen
			// initial selection supplies memory provenance in that case.
			if p.Proof.Target != p.Initial {
				return fmt.Errorf("%w: saved result has no immutable memory selection", errCaptureCompletionUnproven)
			}
			if _, err := c.History.ValidateHistorySource(ctx, p.Proof); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *commitCapturePass) validate() error {
	if p.Version != 1 || p.Proof.Kind != "position" || p.Proof.WorktreeID == "" ||
		!validNonZeroGitOID(p.Proof.GitAfter) || p.Proof.Source != p.Proof.Target || len(p.Outcomes) == 0 {
		return domain.ErrHashMismatch
	}
	if err := domain.ValidateHistoryEvent(p.Proof); err != nil {
		return err
	}
	if err := domain.ValidateOptionalContentHash(p.Initial); err != nil {
		return err
	}
	member := p.Proof.Target == p.Initial
	hasTarget := p.Initial != ""
	for _, o := range p.Outcomes {
		if o.Provider != domain.ProviderClaude && o.Provider != domain.ProviderCodex {
			return domain.ErrHashMismatch
		}
		switch o.State {
		case "saved":
			if err := domain.ValidateContentHash(o.Target); err != nil {
				return err
			}
			member = member || p.Proof.Target == o.Target
			hasTarget = true
		case "absent", "pending":
			if o.Target != "" {
				return domain.ErrHashMismatch
			}
		case "failed":
			if err := domain.ValidateOptionalContentHash(o.Target); err != nil {
				return err
			}
		default:
			return domain.ErrHashMismatch
		}
		if p.Complete && o.State != "saved" && o.State != "absent" {
			return domain.ErrHashMismatch
		}
	}
	if p.Complete && (!member || hasTarget && p.Proof.Target == "") {
		return domain.ErrHashMismatch
	}
	if p.Observation != nil {
		if !p.matchesObservation(p.Proof.Target, *p.Observation) {
			return domain.ErrHashMismatch
		}
		if err := domain.ValidateHistoryEvent(*p.Observation); err != nil {
			return err
		}
	}
	return nil
}

func (p *commitCapturePass) matchesObservation(target domain.ContentHash, e domain.HistoryEvent) bool {
	return e.Kind != "publish" && e.Kind != "pr-merge" && e.RepoID == p.Proof.RepoID &&
		e.BranchID == p.Proof.BranchID && e.Branch == p.Proof.Branch && e.LocalBranch == p.Proof.LocalBranch &&
		e.WorktreeID == p.Proof.WorktreeID && e.GitAfter == p.Proof.GitAfter && e.Target == target
}

func (p *commitCapturePass) observation(target domain.ContentHash, accepted map[string]domain.HistoryEvent) *domain.HistoryEvent {
	var latest *domain.HistoryEvent
	for _, e := range accepted {
		if p.matchesObservation(target, e) && (latest == nil || e.CreatedAt.After(latest.CreatedAt) || e.CreatedAt.Equal(latest.CreatedAt) && e.ID > latest.ID) {
			copy := e
			latest = &copy
		}
	}
	return latest
}

// Recovery may use immutable observations of this pass's own recorded outputs.
// It never infers a missing/failed Save from the current worktree selection.
func recoverCommitCapture(ctx context.Context, c *Container, cwd, root string, p *commitCapturePass, accepted map[string]domain.HistoryEvent) error {
	for _, o := range p.Outcomes {
		if o.State != "saved" && o.State != "absent" {
			return fmt.Errorf("%w: %s capture is %s: %s", errCaptureCompletionUnproven, o.Provider, o.State, o.Error)
		}
		if o.State == "saved" && p.observation(o.Target, accepted) == nil {
			return fmt.Errorf("%w: %s output has no exact stored Git observation", errCaptureCompletionUnproven, o.Provider)
		}
	}
	if err := prepareCommitProof(ctx, c, p); err != nil {
		return err
	}
	if p.Proof.Target != "" && p.observation(p.Proof.Target, accepted) == nil {
		return fmt.Errorf("%w: final target has no exact stored Git observation", errCaptureCompletionUnproven)
	}
	return completeCapturePass(ctx, cwd, root, p)
}

// A live pass and replay can both observe the last durable outcome. Adopt the
// first completion under the journal lock; never overwrite an accepted proof.
func completeCapturePass(ctx context.Context, cwd, root string, p *commitCapturePass) error {
	j, err := branchjournal.Open(ctx, cwd)
	if err != nil {
		return err
	}
	return j.Transaction(ctx, func() error {
		raw, err := providerfs.ReadRepoFile(root, p.relativePath())
		if err != nil {
			return err
		}
		var stored commitCapturePass
		if json.Unmarshal(raw, &stored) != nil {
			return domain.ErrHashMismatch
		}
		if err := stored.validate(); err != nil {
			return err
		}
		left, right := stored, *p
		left.Complete, right.Complete = false, false
		left.Observation, right.Observation = nil, nil
		left.Proof.Source, left.Proof.Target, right.Proof.Source, right.Proof.Target = "", "", "", ""
		if !reflect.DeepEqual(left, right) {
			return domain.ErrHashMismatch
		}
		if stored.Complete {
			*p = stored
			return nil
		}
		p.Complete = true
		if err := p.validate(); err != nil {
			return err
		}
		return p.write(root)
	})
}

func publishCommitCapture(ctx context.Context, c *Container, cwd string, p *commitCapturePass, accepted map[string]domain.HistoryEvent) error {
	if !p.Complete {
		return p.pending(fmt.Errorf("pass interrupted or capture failed before durable completion"))
	}
	if p.Proof.Target == "" {
		return nil // No initial context and no active provider.
	}
	if accepted == nil {
		var err error
		accepted, err = publicationHistory(ctx, c, p.Proof.RepoID)
		if err != nil {
			return err
		}
	}
	ordinary := p.Proof
	if p.Observation != nil {
		ordinary = *p.Observation
	}
	if err := recordPublicationEvent(ctx, c, ordinary, accepted); err != nil {
		return err
	}
	e := p.Proof
	e.Kind = "publish"
	e.Creation = nil // The immutable creation observation is retained separately.
	// Eligibility carries no new memory selection. The ordinary observation
	// above retains the exact memory tuple and is uploaded first.
	e.MemoryHash, e.MemorySource, e.MemoryPinned = "", "", true
	return persistPublicationKnown(ctx, c, cwd, e, accepted)
}

func replayCommitCaptures(ctx context.Context, c *Container, cwd, root, repo, worktree string, accepted map[string]domain.HistoryEvent) error {
	rel := filepath.Join(".cxt", "worktrees", worktree, "capture-passes")
	dir, err := providerfs.EnsureRepoDir(root, rel, 0700)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var pending []error
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := providerfs.ReadRepoFile(root, filepath.Join(rel, entry.Name()))
		if err != nil {
			return err
		}
		var p commitCapturePass
		if json.Unmarshal(raw, &p) != nil || p.Version != 1 || p.Proof.Kind != "position" ||
			p.Proof.RepoID != repo || p.Proof.WorktreeID != worktree || p.Proof.ID+".json" != entry.Name() {
			return domain.ErrHashMismatch
		}
		if err := p.validate(); err != nil {
			return err
		}
		if !p.Complete {
			if err := recoverCommitCapture(ctx, c, cwd, root, &p, accepted); err != nil {
				if errors.Is(err, errCaptureCompletionUnproven) {
					hookWarn("%v", p.pending(err))
					continue
				}
				return err
			}
		}
		if err := publishCommitCapture(ctx, c, cwd, &p, accepted); err != nil {
			pending = append(pending, err)
		}
	}
	return errors.Join(pending...)
}

func publicationID(e domain.HistoryEvent) string {
	e.ID, e.CreatedAt = "", time.Time{}
	raw, _ := json.Marshal(e)
	h := sha256.Sum256(append([]byte("publication\x00"), raw...))
	return fmt.Sprintf("%x", h[:16])
}

func persistPublication(ctx context.Context, c *Container, cwd string, e domain.HistoryEvent) error {
	return persistPublicationKnown(ctx, c, cwd, e, nil)
}

func publicationHistory(ctx context.Context, c *Container, repo string) (map[string]domain.HistoryEvent, error) {
	events, err := c.History.ListHistory(ctx, repo)
	if err != nil {
		return nil, err
	}
	accepted := make(map[string]domain.HistoryEvent, len(events))
	for _, e := range events {
		accepted[e.ID] = e
	}
	return accepted, nil
}

func recordPublicationEvent(ctx context.Context, c *Container, e domain.HistoryEvent, accepted map[string]domain.HistoryEvent) error {
	if old, ok := accepted[e.ID]; ok {
		if !reflect.DeepEqual(old, e) {
			return domain.ErrHashMismatch
		}
		return nil // An accepted immutable event never needs its transcript reconstructed again.
	}
	if _, err := c.History.ValidateHistorySource(ctx, e); err != nil {
		return err
	}
	if err := c.History.RecordHistory(ctx, e); err != nil {
		return err
	}
	accepted[e.ID] = e
	return nil
}

func persistPublicationKnown(ctx context.Context, c *Container, cwd string, e domain.HistoryEvent, accepted map[string]domain.HistoryEvent) error {
	e.ID = publicationID(e)
	e.CreatedAt = time.Now().UTC()
	if err := domain.ValidateHistoryEvent(e); err != nil {
		return err
	}
	root := cxtRepoRoot(ctx, cwd)
	rel := filepath.Join(".cxt", "worktrees", e.WorktreeID, "publication-journal", e.ID+".json")
	if accepted == nil {
		var err error
		accepted, err = publicationHistory(ctx, c, e.RepoID)
		if err != nil {
			return err
		}
	}
	readKnown := func() (bool, error) {
		raw, err := providerfs.ReadRepoFile(root, rel)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		var stored domain.HistoryEvent
		if json.Unmarshal(raw, &stored) != nil || publicationID(stored) != e.ID || stored.ID != e.ID {
			return false, domain.ErrHashMismatch
		}
		if err := domain.ValidateHistoryEvent(stored); err != nil {
			return false, err
		}
		e = stored
		return true, nil
	}
	// An existing exact record was made durable before any history write.
	// Replaying it needs neither another fsync nor the creation lock.
	if known, err := readKnown(); err != nil {
		return err
	} else if known {
		return recordPublicationEvent(ctx, c, e, accepted)
	}
	// Time is fixed durably before history publication. Concurrent creators
	// serialize on Git's journal lock so they cannot disagree about that time.
	j, err := branchjournal.Open(ctx, cwd)
	if err != nil {
		return err
	}
	return j.Transaction(ctx, func() error {
		if known, err := readKnown(); err != nil {
			return err
		} else if known {
			return recordPublicationEvent(ctx, c, e, accepted)
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if err = providerfs.WriteRepoFileDurable(root, rel, raw, 0600); err != nil {
			return err
		}
		return recordPublicationEvent(ctx, c, e, accepted)
	})
}

// All worktrees share the replica, including finalization jobs whose original
// worktree is no longer checked out. Never rescan a live transcript on retry.
func replayPublications(ctx context.Context, c *Container, cwd string) error {
	if c.History == nil {
		return nil
	}
	p, err := c.History.CurrentPosition(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	accepted, err := publicationHistory(ctx, c, p.RepoID)
	if err != nil {
		return err
	}
	root := cxtRepoRoot(ctx, cwd)
	dir, err := providerfs.EnsureRepoDir(root, filepath.Join(".cxt", "worktrees"), 0700)
	if err != nil {
		return err
	}
	worktrees, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var pending []error
	for _, wt := range worktrees {
		if !wt.IsDir() {
			return domain.ErrHashMismatch
		}
		if err := replayCommitCaptures(ctx, c, cwd, root, p.RepoID, wt.Name(), accepted); err != nil {
			pending = append(pending, err)
		}
		rel := filepath.Join(".cxt", "worktrees", wt.Name(), "publication-journal")
		journal, err := providerfs.EnsureRepoDir(root, rel, 0700)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(journal)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			raw, err := providerfs.ReadRepoFile(root, filepath.Join(rel, entry.Name()))
			if err != nil {
				return err
			}
			var e domain.HistoryEvent
			if json.Unmarshal(raw, &e) != nil || e.Kind != "publish" || e.RepoID != p.RepoID || e.WorktreeID != wt.Name() || e.ID != publicationID(e) || entry.Name() != e.ID+".json" {
				return domain.ErrHashMismatch
			}
			if err = recordPublicationEvent(ctx, c, e, accepted); err != nil {
				return err
			}
		}
	}
	return errors.Join(pending...)
}
