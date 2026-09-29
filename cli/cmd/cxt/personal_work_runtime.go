package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// The explicit artifact selects a source session, never the latest session.
// It is an input file, not an implicit write to the shared personal-work store.
type personalWorkArtifact struct {
	Version      int                      `json:"version"`
	RepositoryID string                   `json:"repository_id"`
	State        domain.PersonalWorkState `json:"state"`
}

type personalWorkBackend interface {
	PersonalWorkPrincipal(context.Context) (string, string, error)
	GetSnapshotRemote(context.Context, string, domain.ContentHash) (domain.Snapshot, error)
	FetchPersonalWorkDocument(context.Context, string, domain.ContentHash, int64) (domain.SessionDoc, int64, error)
}

type importedPersonalWork struct {
	repo  string
	state domain.PersonalWorkState
}

func (r importedPersonalWork) ReadPersonalWork(ctx context.Context, repo string, scope domain.PersonalWorkScope) (domain.PersonalWorkState, error) {
	if err := ctx.Err(); err != nil {
		return domain.PersonalWorkState{}, err
	}
	if repo != r.repo || scope != r.state.Scope || !scope.Complete() {
		return domain.PersonalWorkState{}, fmt.Errorf("%w: imported personal work scope mismatch", domain.ErrHashMismatch)
	}
	return r.state, nil
}

func importPersonalWork(ctx context.Context, remote personalWorkBackend, repo, cwd, path string, requested domain.PersonalWorkScope) (outbound.PersonalWorkReader, domain.PersonalWorkScope, error) {
	empty := domain.PersonalWorkScope{}
	if path == "" {
		if requested != empty {
			return nil, empty, fmt.Errorf("%w: personal scope requires an explicit --work-state artifact", domain.ErrAgentContextUnavailable)
		}
		return nil, empty, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, empty, err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	artifact, err := readPersonalWorkArtifact(path)
	if err != nil {
		return nil, empty, fmt.Errorf("--work-state: %w", err)
	}
	if remote == nil {
		return nil, empty, fmt.Errorf("%w: --work-state requires an authenticated backend", domain.ErrAgentContextUnavailable)
	}
	actor, email, err := remote.PersonalWorkPrincipal(ctx)
	if err != nil {
		return nil, empty, err
	}
	worktree, err := personalWorktreeID(ctx, cwd)
	if err != nil {
		return nil, empty, err
	}
	scope := domain.PersonalWorkScope{ActorID: actor, SessionID: artifact.State.Scope.SessionID, WorktreeID: worktree}
	if !scope.Complete() || artifact.Version != 1 || artifact.RepositoryID != repo || artifact.State.Scope != scope || (requested != empty && requested != scope) {
		return nil, empty, fmt.Errorf("%w: --work-state version, repository, authenticated principal, or worktree mismatch", domain.ErrHashMismatch)
	}
	if err := validatePersonalWorkSources(ctx, remote, repo, email, artifact.State); err != nil {
		return nil, empty, err
	}
	return importedPersonalWork{repo: repo, state: artifact.State}, scope, nil
}

// Match FileStore's worktree identity: the first 16 SHA-256 bytes of Git's
// absolute admin directory. Do not read a user-supplied environment identity.
func personalWorktreeID(ctx context.Context, cwd string) (string, error) {
	raw, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return "", fmt.Errorf("--work-state requires the actual Git worktree: %w", err)
	}
	dir := strings.TrimSpace(string(raw))
	if !filepath.IsAbs(dir) {
		return "", domain.ErrNotGitRepo
	}
	hash := sha256.Sum256([]byte(dir))
	return fmt.Sprintf("%x", hash[:16]), nil
}

func readPersonalWorkArtifact(path string) (personalWorkArtifact, error) {
	var out personalWorkArtifact
	info, err := os.Lstat(path)
	if err != nil {
		return out, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return out, fmt.Errorf("expected a regular JSON file of at most 1 MiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return out, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return out, fmt.Errorf("artifact file changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return out, err
	}
	if len(raw) > 1<<20 {
		return out, fmt.Errorf("artifact exceeds 1 MiB")
	}
	if err := rejectDuplicateWorkKeys(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return out, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil {
		return out, err
	}
	var tail any
	if err := d.Decode(&tail); err != io.EOF {
		return out, fmt.Errorf("expected exactly one JSON artifact")
	}
	return out, nil
}

// encoding/json otherwise accepts duplicate keys using last-value-wins, which
// makes an inspectable security-sensitive artifact ambiguous to its author.
func rejectDuplicateWorkKeys(d *json.Decoder) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			name = strings.ToLower(name) // encoding/json matches field names case-insensitively.
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid artifact key %q", key)
			}
			seen[name] = true
		}
		if err := rejectDuplicateWorkKeys(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func validatePersonalWorkSources(ctx context.Context, remote personalWorkBackend, repo, email string, state domain.PersonalWorkState) error {
	fail := func(reason string) error { return fmt.Errorf("%w: --work-state %s", domain.ErrHashMismatch, reason) }
	if len(state.Sources) == 0 || len(state.Sources) > 128 || len(state.Constraints) > 128 {
		return fail("requires 1..128 explicit sources and at most 128 exact constraints")
	}
	docs := map[domain.ContentHash]domain.SessionDoc{}
	const maxDocuments = 8
	remaining := int64(8 << 20)
	validate := func(source domain.AgentSourcePointer) ([]domain.Event, error) {
		if domain.ValidateContentHash(source.SnapshotID) != nil || source.DocHash != source.SnapshotID || source.MemoryHash != "" || source.Tool != "context_fetch" || source.StartEvent < 0 || source.EndEvent <= source.StartEvent {
			return nil, fail("source must address a document and a nonempty [start_event,end_event) range")
		}
		doc, found := docs[source.DocHash]
		if !found {
			if len(docs) >= maxDocuments || remaining <= 0 {
				return nil, fail("source validation exceeds 8 documents or 8 MiB; select a smaller explicit handoff")
			}
			snap, err := remote.GetSnapshotRemote(ctx, repo, source.SnapshotID)
			if err != nil {
				return nil, fmt.Errorf("--work-state source snapshot unavailable: %w", err)
			}
			if snap.RepoID != repo || snap.ID != source.SnapshotID || snap.DocHash != source.DocHash || snap.SessionID != state.Scope.SessionID || email == "" || snap.Author.Email != email {
				return nil, fail("source repository, recorded author email, or exact session mismatch")
			}
			var used int64
			doc, used, err = remote.FetchPersonalWorkDocument(ctx, repo, source.DocHash, remaining)
			if err != nil {
				return nil, fmt.Errorf("--work-state source document unavailable: %w", err)
			}
			if used <= 0 || used > remaining {
				return nil, fail("source validation exceeds the remaining 8 MiB document budget")
			}
			remaining -= used
			if doc.Hash != source.DocHash || domain.ValidateSessionDocHash(doc) != nil || doc.CIR.Envelope.SessionOriginID != state.Scope.SessionID {
				return nil, fail("source document hash or exact session mismatch")
			}
			docs[source.DocHash] = doc
		}
		if source.EndEvent > len(doc.CIR.Events) {
			return nil, fail("source event range is outside the document")
		}
		events := doc.CIR.Events[source.StartEvent:source.EndEvent]
		for _, event := range events {
			if event.Kind == domain.EventCompaction || event.CompactSummary || event.AgentMessage {
				return nil, fail("synthetic or compacted events are not personal provenance")
			}
			var text []string
			for _, block := range event.Blocks {
				text = append(text, block.Text)
			}
			if syntheticPersonalWorkText(strings.Join(text, "\n")) || syntheticPersonalWorkText(strings.Join(text, "")) {
				return nil, fail("synthetic context packets, branch seeds, or environment context are not personal provenance")
			}
		}
		return events, nil
	}
	for _, source := range state.Sources {
		if _, err := validate(source); err != nil {
			return err
		}
	}
	for _, constraint := range state.Constraints {
		events, err := validate(constraint.Source)
		if err != nil {
			return err
		}
		matched := false
		for _, event := range events {
			if event.Kind != domain.EventMessage || event.Role != "user" {
				continue
			}
			var parts []string
			for _, block := range event.Blocks {
				if block.Type == "text" {
					parts = append(parts, block.Text)
				}
			}
			// Match the complete user text (including whitespace), never a
			// substring that might drop a negation or an approval condition.
			if constraint.Text != "" && constraint.Text == strings.Join(parts, "\n") {
				matched = true
			}
		}
		if !matched {
			return fail("constraint is not verbatim complete user text in its addressed event range")
		}
	}
	return ctx.Err()
}

func syntheticPersonalWorkText(text string) bool {
	for _, marker := range []string{"[cxt context package v1]", "[cxt seed] Branch-switch context:", "[cxt] This session was resumed", "<environment_context"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
