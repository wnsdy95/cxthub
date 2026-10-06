package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type setupGitTracking struct{ local, remote, code, upstream, origin, gitdir string }

// Read the actual tracking configuration and checked-out object. A same-name
// server branch, a push refspec or a guessed default is never an attachment.
func readSetupGitTracking(ctx context.Context, cwd string) (setupGitTracking, error) {
	var g setupGitTracking
	read := func(args ...string) (string, error) {
		raw, err := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...).Output()
		if err != nil {
			return "", fmt.Errorf("cannot read Git tracking evidence (%s): %w", args[0], err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	ref, err := read("symbolic-ref", "-q", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			return g, ctx.Err()
		}
		var failure *exec.ExitError
		if errors.As(err, &failure) && failure.ExitCode() == 1 {
			return g, nil // Detached HEAD is registration only.
		}
		return g, err
	}
	g.local = strings.TrimPrefix(ref, "refs/heads/")
	if ref != "refs/heads/"+g.local || domain.ValidateBranchName(g.local) != nil {
		return g, domain.ErrInvalidRef
	}
	raw, err := read("for-each-ref", "--format=%(refname)%00%(upstream:remotename)%00%(upstream:remoteref)%00%(upstream)%00%(objectname)", ref)
	if err != nil {
		return g, err
	}
	if raw == "" {
		return setupGitTracking{}, nil
	}
	fields := strings.Split(raw, "\x00")
	if len(fields) != 5 || fields[0] != ref {
		return g, domain.ErrInvalidRef
	}
	if fields[1] == "" {
		return setupGitTracking{}, nil
	}
	if fields[1] != "origin" || !strings.HasPrefix(fields[2], "refs/heads/") || !strings.HasPrefix(fields[3], "refs/remotes/origin/") {
		return g, fmt.Errorf("setup tracking requires a branch upstream on Git origin")
	}
	g.remote = strings.TrimPrefix(fields[2], "refs/heads/")
	if domain.ValidateBranchName(g.remote) != nil {
		return g, domain.ErrInvalidRef
	}
	g.code = fields[4]
	if !domain.ValidGitOID(g.code) {
		return g, domain.ErrCodePositionMismatch
	}
	g.upstream, err = read("rev-parse", "--verify", fields[3]+"^{commit}")
	if err != nil || !domain.ValidGitOID(g.upstream) {
		return g, domain.ErrCodePositionMismatch
	}
	// First connection is a clone at its observed upstream, not an implicit
	// tracking decision for independently-created/ahead/diverged local code.
	if g.code != g.upstream {
		return g, fmt.Errorf("checked-out code differs from Git upstream; use explicit cxt pull to choose context")
	}
	g.origin, err = read("config", "--get", "remote.origin.url")
	if err != nil {
		return g, err
	}
	g.gitdir, err = read("rev-parse", "--absolute-git-dir")
	return g, err
}

func setupFirstTracking(ctx context.Context, c *Container, cwd string) (string, error) {
	if _, ok := remotecfg.Origin(cwd); !ok {
		return "", nil
	}
	service, ok := c.History.(inbound.FirstSetupTracking)
	if !ok {
		return "", fmt.Errorf("first-connection tracking service unavailable")
	}
	repo, err := remotecfg.Wrap(cwd, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
	if err != nil {
		return "", err
	}
	pristine, err := service.TrackingPristine(ctx, repo.ID)
	if err != nil {
		return "", err
	}
	if !pristine {
		return "existing local context preserved; automatic attachment skipped", nil
	}
	frozen, err := readSetupGitTracking(ctx, cwd)
	if err != nil || frozen.remote == "" {
		return "", err
	}
	if c.Sync == nil {
		return "", fmt.Errorf("repository connection unavailable")
	}
	var expected *domain.WorkingPosition
	position, err := c.History.CurrentPosition(ctx)
	if err == nil {
		expected = &position
	} else if !errors.Is(err, domain.ErrNotFound) {
		return "", err
	}
	conn, err := c.Sync.Connect(ctx, inbound.SyncInput{Cwd: cwd})
	if err != nil {
		return "", err
	}
	repo, err = remotecfg.Wrap(cwd, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
	if err != nil {
		return "", err
	}
	if conn.Repo.ID != repo.ID || gitctx.NormalizeRemoteURL(conn.Repo.GitRemoteURL) == "" || gitctx.NormalizeRemoteURL(conn.Repo.GitRemoteURL) != gitctx.NormalizeRemoteURL(frozen.origin) {
		return "", fmt.Errorf("connected repository does not prove this Git origin")
	}
	// Eligibility is affirmative server evidence, never a fallback from a failed
	// branch/doc fetch. Unpublished aliases need an explicit context decision.
	initial := false
	if query, ok := c.Sync.(inbound.SetupInitialCapture); ok && frozen.local == frozen.remote {
		state, available, err := query.InitialCaptureEligibility(ctx, inbound.SyncInput{Cwd: cwd, RepoID: repo.ID}, frozen.remote)
		if err != nil {
			return "", err
		}
		if available {
			if state.ID != repo.ID || state.ContextProtocol != 1 || gitctx.NormalizeRemoteURL(state.GitRemoteURL) == "" || gitctx.NormalizeRemoteURL(state.GitRemoteURL) != gitctx.NormalizeRemoteURL(frozen.origin) || state.RemoteURL != conn.Repo.RemoteURL {
				return "", domain.ErrHashMismatch
			}
			initial = true
		}
	}
	wt := sha256.Sum256([]byte(frozen.gitdir))
	var apply func() error
	status := "tracking attached at checked-out code"
	if initial {
		next := domain.WorkingPosition{RepoID: repo.ID, Branch: frozen.local, BranchID: domain.LegacyContextBranchID(repo.ID, frozen.local), WorktreeID: fmt.Sprintf("%x", wt[:16]), GitCommit: frozen.code}
		apply = func() error {
			return service.InitializeCapturePosition(ctx, expected, next)
		}
		status = "new repository ready for first capture; no remote context yet"
	} else {
		observer, ok := c.Sync.(inbound.RemoteBranchObserver)
		if !ok {
			return "", fmt.Errorf("verified branch observation unavailable")
		}

		observed, err := observer.ResolveRemoteBranchObservation(ctx, inbound.SyncInput{Cwd: cwd, RepoID: repo.ID}, frozen.remote)
		if err != nil {
			return "", err
		}
		if observed.Ref.RepoID != repo.ID || observed.Ref.Name != frozen.remote {
			return "", domain.ErrHashMismatch
		}
		ancestry, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-list", "--first-parent", frozen.code).Output()
		if err != nil {
			return "", fmt.Errorf("cannot prove checked-out Git ancestry")
		}
		id, err := branchjournal.NewID()
		if err != nil {
			return "", err
		}
		e := domain.HistoryEvent{ID: id, RepoID: repo.ID, Branch: frozen.local, GitAfter: frozen.code, WorktreeID: fmt.Sprintf("%x", wt[:16]), CreatedAt: time.Now().UTC()}
		attachment, err := service.PrepareTrackingAttachment(ctx, e, observed, strings.Fields(string(ancestry)))
		if err != nil {
			return "", err
		}
		apply = func() error {
			return service.ApplyTrackingAttachment(ctx, inbound.TrackingAttachmentInput{Attachment: attachment, ExpectedPosition: expected, SelectPosition: true, RequirePristine: true, ObservedSnapshots: observed.Snapshots})
		}
	}
	j, err := branchjournal.Open(ctx, cwd)
	if err != nil {
		return "", err
	}
	err = j.Transaction(ctx, func() error {
		current, err := readSetupGitTracking(ctx, cwd)
		if err != nil {
			return err
		}
		if current != frozen {
			return fmt.Errorf("Git tracking changed during setup: %w", domain.ErrCodePositionMismatch)
		}
		ops, err := j.List()
		if err != nil {
			return err
		}
		for _, op := range ops {
			if op.Phase != "aborted" {
				return fmt.Errorf("recorded Git branch operation must be resolved before first setup attachment")
			}
		}

		origin, ok := remotecfg.Origin(cwd)
		if !ok || remotecfg.RepoIDFor(origin) != repo.ID {
			return domain.ErrSelectionChanged
		}
		return apply()
	})
	if err != nil {
		return "", err
	}
	return status, nil
}
