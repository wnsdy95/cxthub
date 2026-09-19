package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func historyFixture(t *testing.T) (string, *Container, *storage.FileStore, string, domain.ContentHash) {
	t.Helper()
	cwd := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	runLifecycleGit(t, cwd, "init", "-q", "-b", "main")
	runLifecycleGit(t, cwd, "config", "core.hooksPath", "/dev/null")
	runLifecycleGit(t, cwd, "config", "commit.gpgsign", "false")
	runLifecycleGit(t, cwd, "config", "user.name", "test")
	runLifecycleGit(t, cwd, "config", "user.email", "test@example.test")
	runLifecycleGit(t, cwd, "commit", "--allow-empty", "-qm", "base")
	ctx := context.Background()
	repo, err := gitctx.NewGitContextAdapter().CurrentRepo(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewFileStore(cwd)
	doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
	id, err := store.PutDoc(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo.ID, Branch: "main", Message: "baseline"}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreateBranchRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo.ID, Target: id}); err != nil {
		t.Fatal(err)
	}
	if err = store.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo.ID, Symbolic: "main"}); err != nil {
		t.Fatal(err)
	}
	c := &Container{List: app.NewListSessionsService(store), Fork: app.NewForkSessionService(store), History: app.NewContextHistoryService(store, store)}
	return cwd, c, store, repo.ID, id
}

func runBirthVote(t *testing.T, cwd string, c *Container, phase, line string, gitPID ...string) error {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
	_, _ = f.Seek(0, 0)
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old }()
	return runBranchTransaction(context.Background(), c, cwd, append([]string{phase}, gitPID...))
}

// Model the durable committed callback without starting the detached CLI helper
// from a Go test executable. All operations belong to disposable fixture repos.
func commitBirthJournal(t *testing.T, cwd, id string) {
	t.Helper()
	ctx := context.Background()
	j, err := branchjournal.Open(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Transaction(ctx, func() error {
		ops, err := j.List()
		if err != nil {
			return err
		}
		for _, op := range ops {
			if op.Event.ID == id {
				op.Phase = "committed"
				return j.Save(op)
			}
		}
		return domain.ErrNotFound
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBranchBirthDoesNotBorrowLaterCreationEvidence(t *testing.T) {
	for _, laterCallback := range []bool{false, true} {
		t.Run(strconv.FormatBool(laterCallback), func(t *testing.T) {
			cwd, c, store, repo, _ := historyFixture(t)
			ctx := context.Background()
			oid := gitOut(cwd, "rev-parse", "HEAD")
			line := strings.Repeat("0", 40) + " " + oid + " refs/heads/reused"
			if err := runBirthVote(t, cwd, c, "prepared", line, "10001"); err != nil {
				t.Fatal(err)
			}
			j, _ := branchjournal.Open(ctx, cwd)
			before, _ := j.List()
			abandoned := before[0]
			// The first process dies before creating the ref or reporting abort.
			// A different Git process later creates the same name at the same OID.
			if err := runBirthVote(t, cwd, c, "prepared", line, "10002"); err != nil {
				t.Fatal(err)
			}
			ops, _ := j.List()
			if len(ops) != 2 {
				t.Fatalf("separate transactions collapsed: %+v", ops)
			}
			later := ops[1]
			runLifecycleGit(t, cwd, "branch", "reused")
			if laterCallback {
				commitBirthJournal(t, cwd, later.Event.ID)
			}
			if err := replayBranchOperations(ctx, c, cwd); err != nil {
				t.Fatal(err)
			}
			ops, _ = j.List()
			if ops[0].Phase != "prepared" || ops[0].Resolved || ops[0].Event != abandoned.Event {
				t.Fatalf("abandoned operation borrowed another creation: %+v", ops[0])
			}
			events, err := store.ListHistoryEvents(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			if laterCallback {
				if ops[1].Phase != "applied" || len(events) != 1 || events[0].ID != later.Event.ID {
					t.Fatalf("independently confirmed birth did not replay: %+v %+v", ops, events)
				}
			} else if ops[1].Phase != "prepared" || len(events) != 0 {
				t.Fatalf("ambiguous transactions published history: %+v %+v", ops, events)
			}
			status, err := inspectBranchOperations(ctx, cwd)
			if err != nil || status[0].LocalState != "needs-git-evidence" {
				t.Fatalf("abandoned birth reported replayable: %+v %v", status, err)
			}
		})
	}
}

func TestBranchBirthNewPreparedVoteDoesNotReusePID(t *testing.T) {
	cwd, c, _, _, _ := historyFixture(t)
	line := strings.Repeat("0", 40) + " " + gitOut(cwd, "rev-parse", "HEAD") + " refs/heads/reused"
	for i := 0; i < 2; i++ {
		if err := runBirthVote(t, cwd, c, "prepared", line, "10001"); err != nil {
			t.Fatal(err)
		}
	}
	j, _ := branchjournal.Open(context.Background(), cwd)
	ops, err := j.List()
	if err != nil || len(ops) != 2 || ops[0].Event.ID == ops[1].Event.ID {
		t.Fatalf("new prepared vote reused an abandoned PID's lineage: %+v %v", ops, err)
	}
	// Terminal callbacks have no transaction nonce. With identical PID/ref/OID
	// votes, even an abort cannot identify one safely by wall-clock ordering.
	if err := runBirthVote(t, cwd, c, "aborted", line, "10001"); err == nil {
		t.Fatal("ambiguous terminal callback selected a vote by recency")
	}
	ops, _ = j.List()
	for _, op := range ops {
		if op.Phase != "prepared" || op.Resolved {
			t.Fatalf("ambiguous callback changed a vote: %+v", op)
		}
	}
}

type birthTrackingSync struct {
	inbound.SyncRepo
	target   domain.Ref
	cwds     []string
	branches []string
}

func (s *birthTrackingSync) ResolveRemoteBranch(_ context.Context, in inbound.SyncInput, branch string) (domain.Ref, error) {
	s.cwds = append(s.cwds, in.Cwd)
	s.branches = append(s.branches, branch)
	return s.target, nil
}

func TestBranchBirthReplayUsesOriginWorktreeConfig(t *testing.T) {
	for _, state := range []string{"present", "moved", "removed"} {
		for _, tracking := range []bool{false, true} {
			t.Run(state+"/tracking="+strconv.FormatBool(tracking), func(t *testing.T) {
				cwd, c, store, repo, source := historyFixture(t)
				ctx := context.Background()
				linked := filepath.Join(t.TempDir(), "linked")
				runLifecycleGit(t, cwd, "worktree", "add", "-b", "worktree-source", linked)
				runLifecycleGit(t, cwd, "config", "extensions.worktreeConfig", "true")
				if _, err := store.CreateBranchRef(ctx, domain.Ref{Kind: domain.RefBranch, RepoID: repo, Name: "worktree-source", Target: source}); err != nil {
					t.Fatal(err)
				}
				oid := gitOut(cwd, "rev-parse", "HEAD")
				runLifecycleGit(t, cwd, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
				for i, branch := range []string{"owner-task", "replay-task"} {
					runLifecycleGit(t, cwd, "update-ref", "refs/remotes/origin/"+branch, oid)
					if err := store.PutHistoryEvent(ctx, domain.HistoryEvent{ID: strings.Repeat(strconv.Itoa(i+1), 32), BranchID: branch, RepoID: repo, Branch: branch, Kind: "birth", GitAfter: oid, Source: source, Target: source, CreatedAt: time.Now().UTC()}); err != nil {
						t.Fatal(err)
					}
				}
				// Each worktree has a valid but different binding for the new ref.
				runLifecycleGit(t, cwd, "config", "--worktree", "branch.created.remote", "origin")
				runLifecycleGit(t, cwd, "config", "--worktree", "branch.created.merge", "refs/heads/replay-task")
				ownerRemote := "."
				if tracking {
					ownerRemote = "origin"
				}
				runLifecycleGit(t, linked, "config", "--worktree", "branch.created.remote", ownerRemote)
				runLifecycleGit(t, linked, "config", "--worktree", "branch.created.merge", "refs/heads/owner-task")
				line := strings.Repeat("0", 40) + " " + oid + " refs/heads/created"
				if err := runBirthVote(t, linked, c, "prepared", line); err != nil {
					t.Fatal(err)
				}
				runLifecycleGit(t, linked, "branch", "created")
				j, _ := branchjournal.Open(ctx, cwd)
				before, _ := j.List()
				commitBirthJournal(t, cwd, before[0].Event.ID)
				if state == "moved" {
					runLifecycleGit(t, cwd, "worktree", "move", linked, filepath.Join(t.TempDir(), "moved"))
				} else if state == "removed" {
					runLifecycleGit(t, cwd, "worktree", "remove", linked)
				}
				remote := &birthTrackingSync{target: domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "owner-task", Target: source}}
				c.Sync = remote
				err := replayBranchOperations(ctx, c, cwd)
				after, readErr := j.List()
				if readErr != nil || len(after) != 1 {
					t.Fatalf("journal: %+v %v", after, readErr)
				}
				if state == "removed" {
					if err == nil || after[0].Phase != "committed" || after[0].Resolved || after[0].LastError == "" || len(remote.branches) != 0 {
						t.Fatalf("missing owner configuration guessed an attachment: %+v calls=%v err=%v", after, remote.branches, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				wantKind, wantBranch := "birth", "created"
				if tracking {
					wantKind, wantBranch = "attach", "owner-task"
					if len(remote.branches) != 1 || remote.branches[0] != wantBranch {
						t.Fatalf("queried replaying worktree's remote branch: %v", remote.branches)
					}
				} else if len(remote.branches) != 0 {
					t.Fatalf("local owner acquired replaying worktree's tracking: %v", remote.branches)
				}
				if after[0].Phase != "applied" || after[0].Event.Kind != wantKind || after[0].Event.Branch != wantBranch || after[0].Event.WorktreeID != before[0].Event.WorktreeID {
					t.Fatalf("wrong birth provenance: %+v", after[0])
				}
			})
		}
	}
}

func TestBranchBirthReplayPreservesWorktreeProvenance(t *testing.T) {
	for _, removal := range []bool{false, true} {
		for _, tracking := range []bool{false, true} {
			t.Run("removed="+strconv.FormatBool(removal)+"/tracking="+strconv.FormatBool(tracking), func(t *testing.T) {
				cwd, c, store, repo, source := historyFixture(t)
				ctx := context.Background()
				linked := filepath.Join(t.TempDir(), "linked")
				runLifecycleGit(t, cwd, "worktree", "add", "-b", "worktree-source", linked)
				if _, err := store.CreateBranchRef(ctx, domain.Ref{Kind: domain.RefBranch, RepoID: repo, Name: "worktree-source", Target: source}); err != nil {
					t.Fatal(err)
				}
				oid := gitOut(linked, "rev-parse", "HEAD")
				wantBranch, wantKind := "relocated", "birth"
				remote := &birthTrackingSync{target: domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "team-task", Target: source}}
				if tracking {
					wantBranch, wantKind = "team-task", "attach"
					runLifecycleGit(t, cwd, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
					runLifecycleGit(t, cwd, "update-ref", "refs/remotes/origin/team-task", oid)
					if err := store.PutHistoryEvent(ctx, domain.HistoryEvent{ID: strings.Repeat("a", 32), BranchID: "remote-identity", RepoID: repo, Branch: wantBranch, Kind: "birth", GitAfter: oid, Source: source, Target: source, CreatedAt: time.Now().UTC()}); err != nil {
						t.Fatal(err)
					}
					c.Sync = remote
				}
				line := strings.Repeat("0", 40) + " " + oid + " refs/heads/relocated"
				if err := runBirthVote(t, linked, c, "prepared", line); err != nil {
					t.Fatal(err)
				}
				if tracking {
					runLifecycleGit(t, linked, "branch", "--track", "relocated", "origin/team-task")
				} else {
					runLifecycleGit(t, linked, "branch", "relocated")
				}
				j, _ := branchjournal.Open(ctx, cwd)
				before, _ := j.List()
				commitBirthJournal(t, cwd, before[0].Event.ID)
				if removal {
					runLifecycleGit(t, cwd, "worktree", "remove", linked)
				} else {
					runLifecycleGit(t, cwd, "worktree", "move", linked, filepath.Join(t.TempDir(), "moved"))
				}
				err := replayBranchOperations(ctx, c, cwd)
				if removal {
					// Pruning the origin also deletes its config. No binding was
					// resolved durably yet, so preserve the operation for recovery.
					after, _ := j.List()
					if err == nil || after[0].Phase != "committed" || after[0].Resolved || after[0].Event != before[0].Event || len(remote.cwds) != 0 {
						t.Fatalf("removed owner was resolved from another worktree: %+v calls=%v err=%v", after, remote.cwds, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				ref, err := store.GetRef(ctx, repo, domain.RefBranch, wantBranch)
				if err != nil || ref.Target != source {
					t.Fatalf("surviving native branch lost its context ref: %+v %v", ref, err)
				}
				after, _ := j.List()
				if after[0].Phase != "applied" || after[0].Event.Kind != wantKind || after[0].Worktree != before[0].Worktree || after[0].Event.WorktreeID != before[0].Event.WorktreeID {
					t.Fatalf("replay changed provenance or did not finish: %+v", after[0])
				}
				if tracking && (len(remote.cwds) != 1 || remote.cwds[0] != cwd) {
					t.Fatalf("tracking lookup used unavailable owner: %v", remote.cwds)
				}
			})
		}
	}
}

func TestBranchBirthReplayPreservesJournalOnRefReadFailure(t *testing.T) {
	cwd, c, store, repo, _ := historyFixture(t)
	ctx := context.Background()
	line := strings.Repeat("0", 40) + " " + gitOut(cwd, "rev-parse", "HEAD") + " refs/heads/broken"
	if err := runBirthVote(t, cwd, c, "prepared", line); err != nil {
		t.Fatal(err)
	}
	runLifecycleGit(t, cwd, "branch", "broken")
	j, _ := branchjournal.Open(ctx, cwd)
	before, _ := j.List()
	commitBirthJournal(t, cwd, before[0].Event.ID)
	if err := os.WriteFile(filepath.Join(cwd, ".git", "refs", "heads", "broken"), []byte("not-an-oid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := replayBranchOperations(ctx, c, cwd); err == nil {
		t.Fatal("ref read failure acknowledged as absent branch")
	}
	after, _ := j.List()
	if after[0].Phase != "committed" || after[0].Resolved || after[0].LastError == "" {
		t.Fatalf("failed operation acknowledged: %+v", after[0])
	}
	if _, err := store.GetRef(ctx, repo, domain.RefBranch, "broken"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed ref lookup published context: %v", err)
	}
}

type birthCheckpointSave struct{ calls []inbound.SaveInput }

func TestBranchBirthCheckpointRetainsEffectiveSavedHead(t *testing.T) {
	for _, worktreePosition := range []bool{false, true} {
		for _, growth := range []bool{false, true} {
			t.Run("position="+strconv.FormatBool(worktreePosition)+"/growth="+strconv.FormatBool(growth), func(t *testing.T) {
				cwd, c, st, repo, baseline := historyFixture(t)
				ctx := context.Background()
				home := t.TempDir()
				t.Setenv("HOME", home)
				for _, key := range []string{"CXT_WRAPPED", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "TERM_SESSION_ID", "ITERM_SESSION_ID"} {
					t.Setenv(key, "")
				}
				oid := gitOut(cwd, "rev-parse", "HEAD")
				if worktreePosition {
					st = storage.NewWorktreeFileStore(cwd, gitOut(cwd, "rev-parse", "--absolute-git-dir"), "main", oid)
					c.History = app.NewContextHistoryService(st, st)
					if err := c.History.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: oid, Snapshot: baseline}); err != nil {
						t.Fatal(err)
					}
				}
				c.List = app.NewListSessionsService(st)
				c.Save = app.NewSaveSessionService(gitctx.NewGitContextAdapter(), map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture()}, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec()}, st, capture.NewSessionCapture(st), storage.NewSyncOutbox())
				dir := filepath.Join(home, ".claude", "projects", providerfs.EncodeCwd(cwd))
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				idA, idB := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
				var saved []domain.ContentHash
				for _, id := range []string{idA, idB} {
					path := filepath.Join(dir, id+".jsonl")
					raw := fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"message":{"role":"user","content":%q}}`+"\n", id, cwd, id)
					if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
						t.Fatal(err)
					}
					out, err := c.Save.Save(ctx, inbound.SaveInput{Cwd: cwd, Branch: "main", Provider: domain.ProviderClaude, SessionPath: path})
					if err != nil {
						t.Fatal(err)
					}
					saved = append(saved, out.SnapshotID)
				}
				// A is unchanged, and the real Save service deliberately keeps B.
				pathA := filepath.Join(dir, idA+".jsonl")
				out, err := c.Save.Save(ctx, inbound.SaveInput{Cwd: cwd, Branch: "main", Provider: domain.ProviderClaude, SessionPath: pathA})
				ref, refErr := st.GetRef(ctx, repo, domain.RefBranch, "main")
				if err != nil || refErr != nil || out.SnapshotID != saved[0] || ref.Target != saved[1] {
					t.Fatalf("fixture must reproduce A output with preserved B: out=%+v ref=%+v err=%v/%v", out, ref, err, refErr)
				}
				memory, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: saved[1], Summary: "B memory"})
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := st.GetSnapshot(ctx, saved[1])
				if err != nil {
					t.Fatal(err)
				}
				snapshot.MemoryHash = memory
				if err := st.PutSnapshot(ctx, snapshot); err != nil {
					t.Fatal(err)
				}
				if worktreePosition {
					// A worktree can pin an older memory version on the same B.
					memory, err = st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: saved[1], Summary: "selected B memory"})
					if err != nil {
						t.Fatal(err)
					}
					if err := st.RecordWorkingMemory(ctx, saved[1], memory); err != nil {
						t.Fatal(err)
					}
				}
				if err := capture.TrackAppSession(cwd, domain.ProviderClaude, idA, pathA); err != nil {
					t.Fatal(err)
				}
				if growth {
					f, err := os.OpenFile(pathA, os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						t.Fatal(err)
					}
					_, err = fmt.Fprintln(f, `{"type":"user","message":{"role":"user","content":"new work after B"}}`)
					_ = f.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
				event, err := prepareBranchHistory(ctx, c, cwd, "new", oid, false)
				if err != nil {
					t.Fatal(err)
				}
				ref, err = st.GetRef(ctx, repo, domain.RefBranch, "main")
				if err != nil || event.Source != ref.Target || event.Target != ref.Target {
					t.Fatalf("birth regressed the effective selection: event=%+v ref=%+v err=%v", event, ref, err)
				}
				if !growth && (event.Target != saved[1] || event.MemoryHash != memory || !event.MemoryPinned) {
					t.Fatalf("unchanged ancestor replaced B or its memory pin: %+v", event)
				}
				if growth && (event.Target == saved[0] || event.Target == saved[1]) {
					t.Fatalf("new native bytes were not selected: %+v", event)
				}
			})
		}
	}
}

func (s *birthCheckpointSave) Save(_ context.Context, in inbound.SaveInput) (inbound.SaveOutput, error) {
	s.calls = append(s.calls, in)
	return inbound.SaveOutput{SnapshotID: domain.HashContent([]byte(in.SessionPath))}, nil
}

func TestBranchBirthCheckpointUsesExactCommandSession(t *testing.T) {
	for _, wrapper := range []bool{false, true} {
		for _, registered := range []bool{false, true} {
			t.Run("wrapper="+strconv.FormatBool(wrapper)+"/registered="+strconv.FormatBool(registered), func(t *testing.T) {
				cwd, c, _, _, _ := historyFixture(t)
				sessionHome := t.TempDir()
				t.Setenv("HOME", sessionHome)
				t.Setenv("CODEX_SESSION_ID", "")
				t.Setenv("TERM_SESSION_ID", "")
				t.Setenv("ITERM_SESSION_ID", "")
				ownedID, siblingID := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
				t.Setenv("CXT_WRAPPED", "")
				t.Setenv("CODEX_THREAD_ID", ownedID)
				if wrapper {
					t.Setenv("CXT_WRAPPED", "1")
					t.Setenv("CXT_WRAPPED_AGENT", "codex")
					t.Setenv("CXT_WRAPPER_PID", strconv.Itoa(os.Getppid()))
					t.Setenv("CXT_WRAPPED_SESSION_ID", ownedID)
					t.Setenv("CODEX_THREAD_ID", "")
				}
				owned := writeCodexRollout(t, sessionHome, cwd, ownedID, time.Now().Add(-time.Hour))
				sibling := writeCodexRollout(t, sessionHome, cwd, siblingID, time.Now())
				if registered {
					for id, path := range map[string]string{ownedID: owned, siblingID: sibling} {
						if err := capture.TrackAppSession(cwd, domain.ProviderCodex, id, path); err != nil {
							t.Fatal(err)
						}
					}
				}
				save := &birthCheckpointSave{}
				c.Save = save
				got := checkpointBranchSource(context.Background(), c, cwd, "main", "new", domain.HistoryEvent{})
				if len(save.calls) != 1 || save.calls[0].SessionPath != owned || save.calls[0].Branch != "main" || got != domain.HashContent([]byte(owned)) {
					t.Fatalf("birth selected another session: calls=%+v source=%s", save.calls, got)
				}
				// An explicit but unavailable identity must not fall through to the
				// registered sibling or to a newest-file lookup.
				missing := "33333333-3333-4333-8333-333333333333"
				t.Setenv("CODEX_THREAD_ID", missing)
				t.Setenv("CXT_WRAPPED_SESSION_ID", missing)
				save.calls = nil
				if got := checkpointBranchSource(context.Background(), c, cwd, "main", "new", domain.HistoryEvent{}); got != "" || len(save.calls) != 0 {
					t.Fatalf("missing owner fell through: calls=%+v source=%s", save.calls, got)
				}
			})
		}
	}
}

func TestBranchBirthCheckpointDoesNotGuessUnregisteredSession(t *testing.T) {
	cwd, c, _, _, _ := historyFixture(t)
	sessionHome := t.TempDir()
	t.Setenv("HOME", sessionHome)
	t.Setenv("CXT_WRAPPED", "")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
	writeCodexRollout(t, sessionHome, cwd, "11111111-1111-4111-8111-111111111111", time.Now())
	save := &birthCheckpointSave{}
	c.Save = save
	if got := checkpointBranchSource(context.Background(), c, cwd, "main", "new", domain.HistoryEvent{}); got != "" || len(save.calls) != 0 {
		t.Fatalf("archive recency invented an active owner: calls=%+v source=%s", save.calls, got)
	}
}

func TestBranchBirthCheckpointRequiresUnambiguousRegistry(t *testing.T) {
	cwd, c, _, _, _ := historyFixture(t)
	sessionHome := t.TempDir()
	t.Setenv("HOME", sessionHome)
	t.Setenv("CXT_WRAPPED", "")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
	firstID, secondID := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	first := writeCodexRollout(t, sessionHome, cwd, firstID, time.Now())
	if err := capture.TrackAppSession(cwd, domain.ProviderCodex, firstID, first); err != nil {
		t.Fatal(err)
	}
	save := &birthCheckpointSave{}
	c.Save = save
	if got := checkpointBranchSource(context.Background(), c, cwd, "main", "new", domain.HistoryEvent{}); got != domain.HashContent([]byte(first)) || len(save.calls) != 1 {
		t.Fatalf("single exact registered session was not captured: calls=%+v source=%s", save.calls, got)
	}
	second := writeCodexRollout(t, sessionHome, cwd, secondID, time.Now())
	if err := capture.TrackAppSession(cwd, domain.ProviderCodex, secondID, second); err != nil {
		t.Fatal(err)
	}
	save.calls = nil
	if got := checkpointBranchSource(context.Background(), c, cwd, "main", "new", domain.HistoryEvent{}); got != "" || len(save.calls) != 0 {
		t.Fatalf("registry order invented the birth source: calls=%+v source=%s", save.calls, got)
	}
}

func TestBranchBirthCheckpointUsesOfficialHookPath(t *testing.T) {
	for _, wrapper := range []bool{false, true} {
		t.Run("wrapper="+strconv.FormatBool(wrapper), func(t *testing.T) {
			cwd, c, _, _, _ := historyFixture(t)
			sessionHome := t.TempDir()
			t.Setenv("HOME", sessionHome)
			t.Setenv("CXT_WRAPPED", "")
			t.Setenv("CODEX_THREAD_ID", "")
			t.Setenv("CODEX_SESSION_ID", "")
			// Provider files keep the cwd spelling used by the app. The Git
			// journal uses the canonical worktree (e.g. /var vs /private/var).
			nativeCwd := filepath.Join(t.TempDir(), "native-cwd")
			if err := os.Symlink(cwd, nativeCwd); err != nil {
				t.Fatal(err)
			}
			nativeID := "11111111-1111-4111-8111-111111111111"
			id := "hook-session-opaque"
			if wrapper {
				id = nativeID
				t.Setenv("CXT_WRAPPED", "1")
				t.Setenv("CXT_WRAPPED_AGENT", "claude")
				t.Setenv("CXT_WRAPPER_PID", strconv.Itoa(os.Getppid()))
				t.Setenv("CXT_WRAPPED_SESSION_ID", id)
			}
			dir := filepath.Join(sessionHome, ".claude", "projects", providerfs.EncodeCwd(nativeCwd))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			owned := filepath.Join(dir, nativeID+".jsonl")
			raw := fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"message":{"role":"user","content":"owned session"}}`+"\n", nativeID, nativeCwd)
			if err := os.WriteFile(owned, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := capture.TrackAppSession(nativeCwd, domain.ProviderClaude, id, owned); err != nil {
				t.Fatal(err)
			}
			// A newer unrelated session is never evidence of branch ownership.
			siblingID := "22222222-2222-4222-8222-222222222222"
			sibling := writeCodexRollout(t, sessionHome, cwd, siblingID, time.Now())
			if wrapper {
				if err := capture.TrackAppSession(cwd, domain.ProviderCodex, siblingID, sibling); err != nil {
					t.Fatal(err)
				}
			}
			save := &birthCheckpointSave{}
			c.Save = save
			got := checkpointBranchSource(context.Background(), c, cwd, "main", "new", domain.HistoryEvent{})
			if got != domain.HashContent([]byte(owned)) || len(save.calls) != 1 || save.calls[0].SessionPath != owned || save.calls[0].Provider != domain.ProviderClaude {
				t.Fatalf("official exact path lost: calls=%+v source=%s", save.calls, got)
			}
			if err := os.Remove(owned); err != nil {
				t.Fatal(err)
			}
			save.calls = nil
			if got := checkpointBranchSource(context.Background(), c, cwd, "main", "new", domain.HistoryEvent{}); got != "" || len(save.calls) != 0 {
				t.Fatalf("missing official path selected sibling: calls=%+v source=%s", save.calls, got)
			}
		})
	}
}

func TestBranchBirthCheckpointUsesRegisteredOwnerAcrossWorktrees(t *testing.T) {
	cwd, c, _, _, _ := historyFixture(t)
	sessionHome := t.TempDir()
	t.Setenv("HOME", sessionHome)
	t.Setenv("CXT_WRAPPED", "")
	t.Setenv("CODEX_SESSION_ID", "")
	ownedID := "11111111-1111-4111-8111-111111111111"
	t.Setenv("CODEX_THREAD_ID", ownedID)
	owned := writeCodexRollout(t, sessionHome, cwd, ownedID, time.Now().Add(-time.Hour))
	if err := capture.TrackAppSession(cwd, domain.ProviderCodex, ownedID, owned); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	runLifecycleGit(t, cwd, "worktree", "add", "-b", "feature", linked)
	siblingID := "22222222-2222-4222-8222-222222222222"
	sibling := writeCodexRollout(t, sessionHome, linked, siblingID, time.Now())
	if err := capture.TrackAppSession(linked, domain.ProviderCodex, siblingID, sibling); err != nil {
		t.Fatal(err)
	}
	save := &birthCheckpointSave{}
	c.Save = save
	got := checkpointBranchSource(context.Background(), c, linked, "feature", "new", domain.HistoryEvent{})
	if got != domain.HashContent([]byte(owned)) || len(save.calls) != 1 || save.calls[0].SessionPath != owned || save.calls[0].Cwd != linked || save.calls[0].Branch != "feature" {
		t.Fatalf("birth lost exact owner across worktrees: calls=%+v source=%s", save.calls, got)
	}
}

func TestBranchBirthMissingOwnerRetainsVerifiedBaseline(t *testing.T) {
	cwd, c, _, _, baseline := historyFixture(t)
	sessionHome := t.TempDir()
	t.Setenv("HOME", sessionHome)
	t.Setenv("CXT_WRAPPED", "")
	t.Setenv("CODEX_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "33333333-3333-4333-8333-333333333333")
	siblingID := "22222222-2222-4222-8222-222222222222"
	sibling := writeCodexRollout(t, sessionHome, cwd, siblingID, time.Now())
	if err := capture.TrackAppSession(cwd, domain.ProviderCodex, siblingID, sibling); err != nil {
		t.Fatal(err)
	}
	save := &birthCheckpointSave{}
	c.Save = save
	ctx := context.Background()
	event, err := prepareBranchHistory(ctx, c, cwd, "new", gitOut(cwd, "rev-parse", "HEAD"), false)
	if err != nil || event.Source != baseline || event.Target != baseline || len(save.calls) != 0 {
		t.Fatalf("missing owner changed frozen baseline: %+v calls=%+v err=%v", event, save.calls, err)
	}
}

func TestBranchBirthRequiresCommittedCallbackForReplay(t *testing.T) {
	cwd, c, store, repoID, source := historyFixture(t)
	t.Setenv("CXT_KEEP_SESSION", "1")
	oid := gitOut(cwd, "rev-parse", "HEAD")
	line := strings.Repeat("0", 40) + " " + oid + " refs/heads/feature/durable"
	if err := runBirthVote(t, cwd, c, "prepared", line); err != nil {
		t.Fatal(err)
	}
	j, _ := branchjournal.Open(context.Background(), cwd)
	before, err := j.List()
	if err != nil || len(before) != 1 {
		t.Fatalf("journal: %v %v", before, err)
	}
	// Even when Git did commit, a missing committed callback cannot be inferred
	// from the reflog: an abandoned transaction and a later creation look alike.
	runLifecycleGit(t, cwd, "branch", "feature/durable")
	if err := replayBranchOperations(context.Background(), c, cwd); err != nil {
		t.Fatal(err)
	}
	if events, err := store.ListHistoryEvents(context.Background(), repoID); err != nil || len(events) != 0 {
		t.Fatalf("prepared-only birth published: %+v %v", events, err)
	}
	status, err := inspectBranchOperations(context.Background(), cwd)
	if err != nil || len(status) != 1 || status[0].LocalState != "needs-git-evidence" {
		t.Fatalf("missing callback not reported: %+v %v", status, err)
	}
	// A durable committed callback is sufficient, even if its replay helper
	// never started. Retrying preserves the original identity and source.
	commitBirthJournal(t, cwd, before[0].Event.ID)
	if err := replayBranchOperations(context.Background(), c, cwd); err != nil {
		t.Fatal(err)
	}
	if err := replayBranchOperations(context.Background(), c, cwd); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListHistoryEvents(context.Background(), repoID)
	if err != nil || len(events) != 1 || events[0].ID != before[0].Event.ID || events[0].Source != source {
		t.Fatalf("replay: %+v %v", events, err)
	}
	ref, err := store.GetRef(context.Background(), repoID, domain.RefBranch, "feature/durable")
	if err != nil || ref.Target != source {
		t.Fatalf("branch: %+v %v", ref, err)
	}
	if gitOut(cwd, "symbolic-ref", "--short", "HEAD") != "main" {
		t.Fatal("background creation switched Git")
	}
	head, _ := store.GetRef(context.Background(), repoID, domain.RefHEAD, "HEAD")
	if head.Symbolic != "main" {
		t.Fatal("background creation switched context")
	}
	after, _ := j.List()
	if after[0].Phase != "applied" {
		t.Fatalf("phase=%s", after[0].Phase)
	}
}

func TestBranchBirthAbortedTransactionDoesNotCreateContext(t *testing.T) {
	cwd, c, store, repoID, _ := historyFixture(t)
	line := strings.Repeat("0", 40) + " " + gitOut(cwd, "rev-parse", "HEAD") + " refs/heads/rejected"
	if err := runBirthVote(t, cwd, c, "prepared", line); err != nil {
		t.Fatal(err)
	}
	if err := runBirthVote(t, cwd, c, "aborted", line); err != nil {
		t.Fatal(err)
	}
	if err := replayBranchOperations(context.Background(), c, cwd); err != nil {
		t.Fatal(err)
	}
	events, _ := store.ListHistoryEvents(context.Background(), repoID)
	if len(events) != 0 {
		t.Fatal("aborted birth published")
	}
}

func TestBranchBirthRejectsDamagedReplicaAndJournal(t *testing.T) {
	for _, damage := range []string{"replica", "journal", "doc"} {
		t.Run(damage, func(t *testing.T) {
			cwd, c, _, _, source := historyFixture(t)
			j, _ := branchjournal.Open(context.Background(), cwd)
			if err := j.Enable(); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "replica":
				if err := os.Rename(filepath.Join(cwd, ".cxt"), filepath.Join(cwd, ".cxt-backup")); err != nil {
					t.Fatal(err)
				}
			case "journal":
				if err := os.WriteFile(filepath.Join(cwd, ".git", "cxt", "operations"), []byte("broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "doc":
				if err := os.WriteFile(filepath.Join(cwd, ".cxt", "objects", "docs", strings.TrimPrefix(string(source), "sha256:")), []byte("corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			line := strings.Repeat("0", 40) + " " + gitOut(cwd, "rev-parse", "HEAD") + " refs/heads/rejected"
			if err := runBirthVote(t, cwd, c, "prepared", line); err == nil {
				t.Fatal("damaged state accepted")
			}
		})
	}
}

func TestOrphanBirthRecordsMemoryProvenanceWithoutConversationParent(t *testing.T) {
	cwd, c, _, _, source := historyFixture(t)
	line := strings.Repeat("0", 40) + " ref:refs/heads/orphan-x HEAD"
	if err := runBirthVote(t, cwd, c, "prepared", line); err != nil {
		t.Fatal(err)
	}
	j, _ := branchjournal.Open(context.Background(), cwd)
	ops, err := j.List()
	if err != nil || len(ops) != 1 {
		t.Fatalf("journal: %+v %v", ops, err)
	}
	if ops[0].Event.Source != "" || ops[0].Event.MemorySource != source || ops[0].Event.Kind != "orphan" {
		t.Fatalf("orphan=%+v", ops[0].Event)
	}
}

func TestExistingSymbolicHeadChangeIsNotAnOrphanBirth(t *testing.T) {
	cwd, c, _, _, _ := historyFixture(t)
	runLifecycleGit(t, cwd, "branch", "existing")
	line := strings.Repeat("0", 40) + " ref:refs/heads/existing HEAD"
	if err := runBirthVote(t, cwd, c, "prepared", line); err != nil {
		t.Fatal(err)
	}
	j, _ := branchjournal.Open(context.Background(), cwd)
	ops, err := j.List()
	if err != nil || len(ops) != 0 {
		t.Fatalf("ordinary checkout recorded an orphan: %+v %v", ops, err)
	}
}

func TestEmptyBranchReplayDoesNotBindPreRemoteRepositoryIdentity(t *testing.T) {
	cwd, c, _, _, _ := historyFixture(t)
	j, err := branchjournal.Open(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Enable(); err != nil {
		t.Fatal(err)
	}
	if err := replayBranchOperations(context.Background(), c, cwd); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".git", "cxt", "repository")); !os.IsNotExist(err) {
		t.Fatalf("empty replay bound a repository before remote setup: %v", err)
	}
}

type waitingTrackingSync struct {
	inbound.SyncRepo
	entered chan struct{}
	release chan struct{}
	target  domain.Ref
}

func (s waitingTrackingSync) ResolveRemoteBranch(ctx context.Context, _ inbound.SyncInput, _ string) (domain.Ref, error) {
	close(s.entered)
	select {
	case <-s.release:
		return s.target, nil
	case <-ctx.Done():
		return domain.Ref{}, ctx.Err()
	}
}

func TestTrackingReplayDoesNotHoldTheGitCreationVoteDuringNetwork(t *testing.T) {
	cwd, c, store, repoID, source := historyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A remote-tracking ref and mapping suffice; no network or ambient auth is used.
	oid := gitOut(cwd, "rev-parse", "HEAD")
	runLifecycleGit(t, cwd, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	runLifecycleGit(t, cwd, "update-ref", "refs/remotes/origin/team-task", oid)
	known := domain.HistoryEvent{ID: strings.Repeat("a", 32), BranchID: "teammate-logical-branch", RepoID: repoID, Branch: "team-task", Kind: "birth", GitAfter: oid, Source: source, Target: source, CreatedAt: time.Now().UTC()}
	if err := store.PutHistoryEvent(ctx, known); err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("0", 40) + " " + oid + " refs/heads/team-task"
	if err := runBirthVote(t, cwd, c, "prepared", line); err != nil {
		t.Fatal(err)
	}
	runLifecycleGit(t, cwd, "branch", "--track", "team-task", "origin/team-task")
	journal, _ := branchjournal.Open(ctx, cwd)
	operations, _ := journal.List()
	commitBirthJournal(t, cwd, operations[0].Event.ID)
	entered, release := make(chan struct{}), make(chan struct{})
	c.Sync = waitingTrackingSync{entered: entered, release: release, target: domain.Ref{RepoID: repoID, Kind: domain.RefBranch, Name: "team-task", Target: source}}
	result := make(chan error, 1)
	go func() { result <- replayBranchOperations(ctx, c, cwd) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("tracking resolution never started")
	}
	j, _ := branchjournal.Open(ctx, cwd)
	voteCtx, voteCancel := context.WithTimeout(ctx, time.Second)
	err := j.Transaction(voteCtx, func() error { return nil })
	voteCancel()
	if err != nil {
		close(release)
		t.Fatalf("remote lookup blocked the prepared vote: %v", err)
	}
	if err := runBirthVote(t, cwd, c, "prepared", strings.Repeat("0", 40)+" "+oid+" refs/heads/independent"); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	events, err := store.ListHistoryEvents(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	attached := false
	for _, e := range events {
		if e.Kind == "attach" {
			attached = true
			if e.BranchID != known.BranchID || e.Target != source {
				t.Fatalf("tracking created another logical branch: %+v", e)
			}
		}
	}
	if !attached {
		t.Fatal("missing tracking attachment")
	}
}

func TestCodeSelectionUsesRecordedMemoryIncludingExplicitlyEmpty(t *testing.T) {
	cwd, _, store, repoID, id := historyFixture(t)
	code := gitOut(cwd, "rev-parse", "HEAD")
	snapshots, err := store.ListSnapshots(context.Background(), repoID, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshots[0].Message = "captured [git " + code + "]"
	snapshots[0].MemoryHash = domain.HashContent([]byte("new memory"))
	snapshots[0].CreatedAt = time.Unix(3, 0)
	for _, memory := range []domain.ContentHash{"", domain.HashContent([]byte("old memory"))} {
		event := domain.HistoryEvent{Kind: "position", Branch: "main", Target: id, MemoryHash: memory, MemoryPinned: true, GitAfter: code, CreatedAt: time.Unix(2, 0)}
		selected := contextSelectionAtCode(cwd, code, "main", snapshots, []domain.HistoryEvent{event})
		if selected.Snapshot != id || selected.MemoryHash != memory || !selected.MemoryPinned {
			t.Fatalf("selected current attachment instead of recorded code version: %+v", selected)
		}
	}
}

func TestCodeSelectionFollowsIdentityThroughRenameAndReusedName(t *testing.T) {
	cwd, _, _, repo, id := historyFixture(t)
	code := gitOut(cwd, "rev-parse", "HEAD")
	birth := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, BranchID: "original", Branch: "old", Kind: "birth", Target: id, GitAfter: code, MemoryPinned: true}
	rename := birth
	rename.ID, rename.Kind, rename.Branch, rename.PreviousBranch, rename.BindingParent = strings.Repeat("b", 32), "rename", "new", "old", birth.ID
	rename.GitAfter = ""
	reuse := birth
	reuse.ID, reuse.BranchID, reuse.BindingParent, reuse.Target = strings.Repeat("c", 32), "reused", rename.ID, domain.HashContent([]byte("different task"))
	reuse.CreatedAt = time.Now().UTC()
	history := []domain.HistoryEvent{reuse, rename, birth}
	for _, name := range []string{"new", "old"} {
		got := contextSelectionAtCode(cwd, code, name, nil, history)
		want := id
		if name == "old" {
			want = reuse.Target
		}
		if got.Snapshot != want || !got.MemoryPinned {
			t.Fatalf("%s selection mixed identities: %+v", name, got)
		}
	}
}
