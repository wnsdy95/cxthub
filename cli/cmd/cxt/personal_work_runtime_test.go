package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type personalBackendFixture struct {
	actor, email string
	snap         domain.Snapshot
	doc          domain.SessionDoc
	err          error
	reads        int
}

func (f *personalBackendFixture) PersonalWorkPrincipal(context.Context) (string, string, error) {
	f.reads++
	return f.actor, f.email, f.err
}
func (f *personalBackendFixture) GetSnapshotRemote(context.Context, string, domain.ContentHash) (domain.Snapshot, error) {
	f.reads++
	return f.snap, f.err
}
func (f *personalBackendFixture) FetchPersonalWorkDocument(context.Context, string, domain.ContentHash, int64) (domain.SessionDoc, int64, error) {
	f.reads++
	raw, err := json.Marshal(f.doc)
	if err != nil {
		return domain.SessionDoc{}, 0, err
	}
	return f.doc, int64(len(raw)), f.err
}

func personalFixture(t *testing.T) (string, personalWorkArtifact, *personalBackendFixture) {
	t.Helper()
	cwd := t.TempDir()
	if out, err := exec.Command("git", "-C", cwd, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	worktree, err := personalWorktreeID(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	state := domain.PersonalWorkState{Scope: domain.PersonalWorkScope{ActorID: "dev:alice@example.test", SessionID: "11111111-1111-4111-8111-111111111111", WorktreeID: worktree}, Goal: "Finish P6", Acceptance: []string{"Explicit sources only"}, Completed: []string{"schema"}, Remaining: []string{"tests"}, LastVerification: "go test", NextStep: "Review without deploying"}
	f := &personalBackendFixture{actor: state.Scope.ActorID, email: "alice@example.test"}
	f.doc.CIR = domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: state.Scope.SessionID, Fidelity: domain.FidelityFull}, Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", Seq: 0, Blocks: []domain.ContentBlock{{Type: "text", Text: "Do not deploy.\n  Keep IDs unchanged. "}}},
		{Kind: domain.EventMessage, Role: "assistant", Seq: 1, Blocks: []domain.ContentBlock{{Type: "text", Text: "Tests remain."}}},
	}}
	f.snap = domain.Snapshot{RepoID: string(domain.HashContent([]byte("repo"))), SessionID: state.Scope.SessionID, Author: domain.TeamIdentity{Email: f.email}, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
	a := personalWorkArtifact{Version: 1, RepositoryID: f.snap.RepoID, State: state}
	a.State.Sources = []domain.AgentSourcePointer{{StartEvent: 0, EndEvent: 2, Tool: "context_fetch"}}
	a.State.Constraints = []domain.ExactUserConstraint{{Text: f.doc.CIR.Events[0].Blocks[0].Text, Source: domain.AgentSourcePointer{StartEvent: 0, EndEvent: 1, Tool: "context_fetch"}}}
	rehashPersonalFixture(t, &a, f)
	return cwd, a, f
}
func rehashPersonalFixture(t *testing.T, a *personalWorkArtifact, f *personalBackendFixture) {
	t.Helper()
	raw, err := domain.CanonicalBytes(f.doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	f.doc.Hash = domain.HashContent(raw)
	f.snap.ID, f.snap.DocHash = f.doc.Hash, f.doc.Hash
	for i := range a.State.Sources {
		a.State.Sources[i].SnapshotID, a.State.Sources[i].DocHash = f.doc.Hash, f.doc.Hash
	}
	for i := range a.State.Constraints {
		a.State.Constraints[i].Source.SnapshotID, a.State.Constraints[i].Source.DocHash = f.doc.Hash, f.doc.Hash
	}
}
func writePersonalFixture(t *testing.T, cwd string, a personalWorkArtifact) string {
	t.Helper()
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, "handoff.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExplicitPersonalWorkValidatesAndPreservesEntireState(t *testing.T) {
	cwd, a, backend := personalFixture(t)
	writePersonalFixture(t, cwd, a)
	reader, scope, err := importPersonalWork(context.Background(), backend, a.RepositoryID, cwd, "handoff.json", domain.PersonalWorkScope{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := reader.ReadPersonalWork(context.Background(), a.RepositoryID, scope)
	if err != nil || !reflect.DeepEqual(state, a.State) {
		t.Fatalf("state changed: %#v / %v", state, err)
	}
	if backend.reads != 3 {
		t.Fatalf("source cache/auth calls: %d", backend.reads)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !os.IsNotExist(err) {
		t.Fatal("import wrote implicit state")
	}
	for _, mutate := range []func(*domain.PersonalWorkScope){func(s *domain.PersonalWorkScope) { s.ActorID = "bob" }, func(s *domain.PersonalWorkScope) { s.SessionID = "other" }, func(s *domain.PersonalWorkScope) { s.WorktreeID = "other" }} {
		bad := scope
		mutate(&bad)
		if _, err := reader.ReadPersonalWork(context.Background(), a.RepositoryID, bad); err == nil {
			t.Fatal("reader accepted mismatched scope")
		}
	}
	if _, err := reader.ReadPersonalWork(context.Background(), "other-repo", scope); err == nil {
		t.Fatal("reader accepted mismatched repository")
	}
}

func TestExplicitPersonalWorkRejectsUnboundOrInventedSources(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, *personalWorkArtifact, *personalBackendFixture){
		"wrong principal": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) { a.State.Scope.ActorID = "bob" },
		"email is not backend user ID": func(t *testing.T, a *personalWorkArtifact, f *personalBackendFixture) {
			a.State.Scope.ActorID = f.email
		},
		"wrong worktree": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.State.Scope.WorktreeID = "other"
		},
		"wrong repository": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.RepositoryID = string(domain.HashContent([]byte("other")))
		},
		"unsupported version": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) { a.Version = 2 },
		"wrong session": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.State.Scope.SessionID = "other"
		},
		"wrong author": func(t *testing.T, _ *personalWorkArtifact, f *personalBackendFixture) {
			f.snap.Author.Email = "bob@example.test"
		},
		"wrong snapshot repository": func(t *testing.T, _ *personalWorkArtifact, f *personalBackendFixture) { f.snap.RepoID = "other" },
		"wrong snapshot session":    func(t *testing.T, _ *personalWorkArtifact, f *personalBackendFixture) { f.snap.SessionID = "other" },
		"wrong document session": func(t *testing.T, a *personalWorkArtifact, f *personalBackendFixture) {
			f.doc.CIR.Envelope.SessionOriginID = "other"
			rehashPersonalFixture(t, a, f)
		},
		"tampered document": func(t *testing.T, _ *personalWorkArtifact, f *personalBackendFixture) {
			f.doc.CIR.Events[0].Blocks[0].Text = "Deploy now."
		},
		"empty sources": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) { a.State.Sources = nil },
		"second invalid source": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.State.Sources = append(a.State.Sources, domain.AgentSourcePointer{})
		},
		"memory source": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.State.Sources[0].MemoryHash = domain.HashContent([]byte("memory"))
		},
		"wrong doc pointer": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.State.Sources[0].DocHash = domain.HashContent([]byte("other"))
		},
		"wrong tool": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.State.Sources[0].Tool = "approve"
		},
		"negative range": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.State.Sources[0].StartEvent = -1
		},
		"outside range": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.State.Constraints[0].Source.EndEvent = 3
		},
		"empty range": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.State.Constraints[0].Source.EndEvent = 0
		},
		"constraint source mismatch": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.State.Constraints[0].Source.StartEvent = 1
			a.State.Constraints[0].Source.EndEvent = 2
		},
		"invented constraint": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.State.Constraints[0].Text = "Approved to deploy"
		},
		"dropped negation": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.State.Constraints[0].Text = "deploy."
		},
		"normalized whitespace": func(t *testing.T, a *personalWorkArtifact, _ *personalBackendFixture) {
			a.State.Constraints[0].Text = strings.TrimSpace(a.State.Constraints[0].Text)
		},
		"assistant approval": func(t *testing.T, a *personalWorkArtifact, f *personalBackendFixture) {
			f.doc.CIR.Events[0].Role = "assistant"
			rehashPersonalFixture(t, a, f)
		},
		"synthetic package": func(t *testing.T, a *personalWorkArtifact, f *personalBackendFixture) {
			f.doc.CIR.Events[0].Blocks[0].Text = "[cxt context package v1]\n{}"
			a.State.Constraints[0].Text = f.doc.CIR.Events[0].Blocks[0].Text
			rehashPersonalFixture(t, a, f)
		},
		"compaction summary": func(t *testing.T, a *personalWorkArtifact, f *personalBackendFixture) {
			f.doc.CIR.Events[0].CompactSummary = true
			rehashPersonalFixture(t, a, f)
		},
		"agent message": func(t *testing.T, a *personalWorkArtifact, f *personalBackendFixture) {
			f.doc.CIR.Envelope.CIRVersion = "2"
			f.doc.CIR.Events[0].AgentMessage = true
			rehashPersonalFixture(t, a, f)
		},
		"revoked source": func(t *testing.T, _ *personalWorkArtifact, f *personalBackendFixture) {
			f.err = errors.New("forbidden")
		},
	} {
		t.Run(name, func(t *testing.T) {
			cwd, a, f := personalFixture(t)
			mutate(t, &a, f)
			path := writePersonalFixture(t, cwd, a)
			reader, _, err := importPersonalWork(context.Background(), f, f.snap.RepoID, cwd, path, domain.PersonalWorkScope{})
			if err == nil || reader != nil {
				t.Fatalf("unsafe state accepted: %v", err)
			}
		})
	}
}

func TestPersonalWorkHasNoImplicitLatestOrEnvironmentIdentity(t *testing.T) {
	cwd, a, f := personalFixture(t)
	t.Setenv("CXT_ACTOR_ID", a.State.Scope.ActorID)
	t.Setenv("CXT_SESSION_ID", a.State.Scope.SessionID)
	t.Setenv("CXT_WORK_STATE", writePersonalFixture(t, cwd, a))
	reader, scope, err := importPersonalWork(context.Background(), f, a.RepositoryID, cwd, "", domain.PersonalWorkScope{})
	if err != nil || reader != nil || scope.Complete() || f.reads != 0 {
		t.Fatalf("implicit inheritance: %v %v", scope, err)
	}
	if _, _, err := importPersonalWork(context.Background(), f, a.RepositoryID, cwd, "", a.State.Scope); err == nil {
		t.Fatal("accepted arbitrary actor scope without explicit artifact")
	}
	other := a.State.Scope
	other.SessionID = "other"
	if _, _, err := importPersonalWork(context.Background(), f, a.RepositoryID, cwd, "handoff.json", other); err == nil {
		t.Fatal("accepted conflicting caller scope")
	}
	otherTree := t.TempDir()
	if out, err := exec.Command("git", "-C", otherTree, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	if _, _, err := importPersonalWork(context.Background(), f, a.RepositoryID, otherTree, filepath.Join(cwd, "handoff.json"), domain.PersonalWorkScope{}); err == nil {
		t.Fatal("handoff leaked into another actual worktree")
	}
}

func TestPersonalWorkArtifactRejectsAmbiguousOrNonregularFiles(t *testing.T) {
	cwd, a, _ := personalFixture(t)
	valid, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{string(valid) + " {}", strings.Replace(string(valid), `"version":1`, `"version":1,"version":1`, 1), strings.Replace(string(valid), `"goal":`, `"unknown":true,"goal":`, 1), strings.Repeat(" ", (1<<20)+1)} {
		path := filepath.Join(cwd, "bad.json")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readPersonalWorkArtifact(path); err == nil {
			t.Fatal("accepted ambiguous or oversized JSON")
		}
	}
	path := writePersonalFixture(t, cwd, a)
	link := filepath.Join(cwd, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, cwd, filepath.Join(cwd, "missing")} {
		if _, err := readPersonalWorkArtifact(path); err == nil {
			t.Fatal("accepted nonregular or missing file")
		}
	}
}
