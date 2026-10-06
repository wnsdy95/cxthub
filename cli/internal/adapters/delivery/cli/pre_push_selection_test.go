package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func setGitPushInput(t *testing.T, raw string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "pre-push-stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(raw); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = original; _ = f.Close() })
}

func TestGitPrePushRejectsUnselectedWorkBeforeReplay(t *testing.T) {
	for _, kind := range []string{"non-origin", "wrong-url", "empty", "tag", "delete", "raw-object", "malformed-tail", "stale-object"} {
		t.Run(kind, func(t *testing.T) {
			cwd, c, _, repo, _ := historyFixture(t)
			t.Setenv("CXT_REMOTE", "https://example.invalid/context")
			runLifecycleGit(t, cwd, "remote", "add", "origin", "https://example.invalid/code.git")
			oid := gitOut(cwd, "rev-parse", "HEAD")
			zero := strings.Repeat("0", 40)
			args := []string{"origin", "https://example.invalid/code.git"}
			input := "refs/heads/main " + oid + " refs/heads/main " + zero + "\n"
			switch kind {
			case "non-origin":
				args[0] = "upstream"
			case "wrong-url":
				args[1] = "https://example.invalid/different.git"
			case "empty":
				input = ""
			case "tag":
				input = "refs/tags/v1 " + oid + " refs/tags/v1 " + zero + "\n"
			case "delete":
				input = "(delete) " + zero + " refs/heads/main " + oid + "\n"
			case "raw-object":
				input = oid + " " + oid + " refs/heads/main " + zero + "\n"
			case "malformed-tail":
				input += "broken trailing row\n"
			case "stale-object":
				runLifecycleGit(t, cwd, "commit", "--allow-empty", "-qm", "moved")
			}
			syncer := &diagnosticRetrySync{}
			c.Sync = syncer
			// Any use of this interface's embedded nil methods panics: rejected
			// input must not even resolve history or start background publication.
			c.History = &pushSelectionHistory{}
			c.ResolveRepo = func(context.Context, string) (domain.Repo, error) {
				t.Fatal("unexpected repository/history access")
				return domain.Repo{ID: repo}, nil
			}
			wakes := 0
			c.WakeHistoricalSync = func(string) { wakes++ }
			if err := runGitPrePush(context.Background(), c, cwd, args, strings.NewReader(input)); err != nil {
				t.Fatal(err)
			}
			if len(syncer.inputs) != 0 || wakes != 0 {
				t.Fatalf("unselected publication: pushes=%d wakes=%d", len(syncer.inputs), wakes)
			}
		})
	}
}

type pushSelectionHistory struct {
	inbound.ContextHistory
	bindings map[string]domain.LocalBranchBinding
	events   []domain.HistoryEvent
}

func (h *pushSelectionHistory) ListHistory(ctx context.Context, repo string) ([]domain.HistoryEvent, error) {
	if h.events != nil {
		return h.events, nil
	}
	return h.ContextHistory.ListHistory(ctx, repo)
}

func TestGitPrePushOtherAliasRequiresExactCodePublication(t *testing.T) {
	for _, mode := range []string{"matching", "other-code", "foreign-identity", "missing-publication", "missing-ordinary", "missing-attachment"} {
		t.Run(mode, func(t *testing.T) {
			cwd, c, _, repo, target := historyFixture(t)
			t.Setenv("CXT_REMOTE", "https://example.invalid/context")
			runLifecycleGit(t, cwd, "remote", "add", "origin", "https://example.invalid/code.git")
			runLifecycleGit(t, cwd, "branch", "local")
			oid := gitOut(cwd, "rev-parse", "refs/heads/local")
			h := &pushSelectionHistory{ContextHistory: c.History, bindings: map[string]domain.LocalBranchBinding{}}
			for _, local := range []string{"local", "other-alias"} {
				h.bindings[local] = domain.LocalBranchBinding{LocalBranch: local, Branch: "shared", BranchID: "R", Tracking: true}
			}
			e := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, BranchID: "R", Branch: "shared", LocalBranch: "other-alias", WorktreeID: strings.Repeat("2", 32), Kind: "attach", Source: target, Target: target, GitAfter: oid, CreatedAt: time.Unix(1, 0).UTC()}
			h.events = append(h.events, e)
			e.ID, e.Kind, e.CreatedAt = strings.Repeat("3", 32), "position", time.Unix(2, 0).UTC()
			e.WorktreeID = strings.Repeat("4", 32) // Existing same-alias cross-worktree parity.
			h.events = append(h.events, e)
			e.ID, e.Kind = strings.Repeat("5", 32), "publish"
			h.events = append(h.events, e)
			switch mode {
			case "other-code":
				for i := range h.events {
					h.events[i].GitAfter = strings.Repeat("a", 40)
				}
			case "foreign-identity":
				for i := range h.events {
					h.events[i].BranchID = "foreign"
				}
			case "missing-publication":
				h.events = h.events[:2]
			case "missing-ordinary":
				h.events = append(h.events[:1], h.events[2])
			case "missing-attachment":
				h.events = h.events[1:]
			}
			c.History = h
			c.ResolveRepo = func(context.Context, string) (domain.Repo, error) { return domain.Repo{ID: repo}, nil }
			syncer := &diagnosticRetrySync{}
			c.Sync = syncer
			input := "refs/heads/local " + oid + " refs/heads/other-alias " + strings.Repeat("0", 40) + "\n"
			if err := runGitPrePush(context.Background(), c, cwd, []string{"origin", "https://example.invalid/code.git"}, strings.NewReader(input)); err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "matching" {
				want = 1
			}
			if len(syncer.inputs) != want {
				t.Fatalf("alias publication calls=%d want=%d", len(syncer.inputs), want)
			}
		})
	}
}

type changingPushSelection struct {
	inbound.SyncRepo
	inputs     []inbound.SyncInput
	afterFirst func()
}

func (s *changingPushSelection) Push(_ context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	s.inputs = append(s.inputs, in)
	if len(s.inputs) == 1 {
		if s.afterFirst != nil {
			s.afterFirst()
		}
		return inbound.SyncOutput{}, domain.ErrSyncConflict
	}
	return inbound.SyncOutput{}, nil
}

func TestGitPrePushFreezesIdentityAndCodeAcrossAppendRetry(t *testing.T) {
	for _, kind := range []string{"stable", "identity-reused", "code-moved", "callee-mutates-input"} {
		t.Run(kind, func(t *testing.T) {
			cwd, c, _, repo, _ := historyFixture(t)
			t.Setenv("CXT_REMOTE", "https://example.invalid/context")
			runLifecycleGit(t, cwd, "remote", "add", "origin", "https://example.invalid/code.git")
			// The pushed branch is not HEAD. Current worktree state must not
			// replace the ref Git supplied to this hook.
			runLifecycleGit(t, cwd, "branch", "topic")
			oid := gitOut(cwd, "rev-parse", "refs/heads/topic")
			id := domain.LegacyContextBranchID(repo, "topic")
			history := &pushSelectionHistory{ContextHistory: c.History, bindings: map[string]domain.LocalBranchBinding{"topic": {LocalBranch: "topic", Branch: "topic", BranchID: id}}}
			c.History = history
			c.ResolveRepo = func(context.Context, string) (domain.Repo, error) { return domain.Repo{ID: repo}, nil }
			syncer := &changingPushSelection{}
			syncer.afterFirst = func() {
				switch kind {
				case "identity-reused":
					b := history.bindings["topic"]
					b.BranchID = "replacement"
					history.bindings["topic"] = b
				case "code-moved":
					runLifecycleGit(t, cwd, "commit", "--allow-empty", "-qm", "moved")
					runLifecycleGit(t, cwd, "branch", "-f", "topic", "HEAD")
				case "callee-mutates-input":
					syncer.inputs[0].Publication.Branches[0].BranchID = "injected"
				}
			}
			c.Sync = syncer
			wakes := 0
			c.WakeHistoricalSync = func(string) { wakes++ }
			input := "refs/heads/topic " + oid + " refs/heads/topic " + strings.Repeat("0", 40) + "\n"
			if err := runGitPrePush(context.Background(), c, cwd, []string{"origin", "https://example.invalid/code.git"}, strings.NewReader(input)); err != nil {
				t.Fatal(err)
			}
			want := 2
			if kind == "identity-reused" || kind == "code-moved" {
				want = 1
			}
			if len(syncer.inputs) != want || wakes != 1 {
				t.Fatalf("calls=%d want=%d wakes=%d", len(syncer.inputs), want, wakes)
			}
			if want == 2 {
				next := syncer.inputs[1]
				if !next.Append || next.Force || !reflect.DeepEqual(next.Publication.Branches, []domain.PublicationBranch{{Branch: "topic", BranchID: id}}) {
					t.Fatalf("retry retargeted: %+v", next)
				}
			}
		})
	}
}

func (h *pushSelectionHistory) ResolveLocalBranch(_ context.Context, _, name string) (domain.LocalBranchBinding, error) {
	b, ok := h.bindings[name]
	if !ok {
		return domain.LocalBranchBinding{}, domain.ErrNotFound
	}
	return b, nil
}

func TestGitPushSelectionMapsOnlyRecordedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, destination string
		mutate            func(map[string]domain.LocalBranchBinding)
		bad               bool
	}{
		{name: "same local name", destination: "local"},
		{name: "canonical name", destination: "team/topic"},
		{name: "recorded alias", destination: "other-alias"},
		{name: "arbitrary refspec", destination: "new-name", bad: true},
		{name: "foreign alias", destination: "other-alias", bad: true, mutate: func(b map[string]domain.LocalBranchBinding) {
			x := b["other-alias"]
			x.BranchID = "foreign"
			b["other-alias"] = x
		}},
		{name: "inactive alias", destination: "other-alias", bad: true, mutate: func(b map[string]domain.LocalBranchBinding) {
			x := b["other-alias"]
			x.Inactive = true
			b["other-alias"] = x
		}},
		{name: "unproven alias", destination: "other-alias", bad: true, mutate: func(b map[string]domain.LocalBranchBinding) {
			x := b["other-alias"]
			x.Tracking = false
			b["other-alias"] = x
		}},
		{name: "inactive source", destination: "local", bad: true, mutate: func(b map[string]domain.LocalBranchBinding) { x := b["local"]; x.Inactive = true; b["local"] = x }},
		{name: "missing source identity", destination: "local", bad: true, mutate: func(b map[string]domain.LocalBranchBinding) { x := b["local"]; x.BranchID = ""; b["local"] = x }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bindings := map[string]domain.LocalBranchBinding{}
			for _, alias := range []string{"local", "other-alias"} {
				bindings[alias] = domain.LocalBranchBinding{LocalBranch: alias, Branch: "team/topic", BranchID: "R", Tracking: true}
			}
			if tc.mutate != nil {
				tc.mutate(bindings)
			}
			c := &Container{History: &pushSelectionHistory{bindings: bindings}, ResolveRepo: func(context.Context, string) (domain.Repo, error) { return domain.Repo{ID: "repo"}, nil }}
			updates := []gitPushUpdate{{LocalRef: "refs/heads/local", RemoteRef: "refs/heads/" + tc.destination}}
			got, err := resolveGitPushScope(context.Background(), c, "unused", updates)
			if tc.bad {
				if err == nil || len(got.Branches) != 0 {
					t.Fatalf("invalid selection authorized: %+v, %v", got, err)
				}
			} else if err != nil || !reflect.DeepEqual(got.Branches, []domain.PublicationBranch{{Branch: "team/topic", BranchID: "R"}}) {
				t.Fatalf("scope=%+v err=%v", got, err)
			}
		})
	}
}

func TestGitPushSelectionAllRowsPreflightAndIdentityDedup(t *testing.T) {
	c := &Container{History: &pushSelectionHistory{bindings: map[string]domain.LocalBranchBinding{
		"one": {LocalBranch: "one", Branch: "shared", BranchID: "R", Tracking: true},
		"two": {LocalBranch: "two", Branch: "shared", BranchID: "R", Tracking: true},
	}}, ResolveRepo: func(context.Context, string) (domain.Repo, error) { return domain.Repo{ID: "repo"}, nil }}
	rows := []gitPushUpdate{{LocalRef: "refs/heads/one", RemoteRef: "refs/heads/shared"}, {LocalRef: "refs/heads/two", RemoteRef: "refs/heads/two"}}
	scope, err := resolveGitPushScope(context.Background(), c, "unused", rows)
	if err != nil || len(scope.Branches) != 1 {
		t.Fatalf("same identity not coalesced: %+v %v", scope, err)
	}
	rows = append(rows, gitPushUpdate{LocalRef: "refs/heads/missing", RemoteRef: "refs/heads/missing"})
	if partial, err := resolveGitPushScope(context.Background(), c, "unused", rows); err == nil || len(partial.Branches) != 0 {
		t.Fatalf("partial authorization escaped: %+v %v", partial, err)
	}
	if _, err := resolveGitPushScope(context.Background(), c, "unused", nil); err == nil {
		t.Fatal("empty selection became broad push")
	}
}

func TestGitPushSelectionVerifiesRealGitOriginAndObject(t *testing.T) {
	cwd, _, _, _, _ := historyFixture(t)
	runLifecycleGit(t, cwd, "remote", "add", "origin", "https://example.invalid/repo.git")
	ctx := context.Background()
	for _, args := range [][]string{nil, {"origin"}, {"other", "https://example.invalid/repo.git"}, {"https://example.invalid/repo.git", "https://example.invalid/repo.git"}, {"origin", "https://example.invalid/other.git"}} {
		if validateGitPushRemote(ctx, cwd, args) == nil {
			t.Fatalf("unverified remote accepted: %v", args)
		}
	}
	if err := validateGitPushRemote(ctx, cwd, []string{"origin", "https://example.invalid/repo.git"}); err != nil {
		t.Fatal(err)
	}
	updates := []gitPushUpdate{{LocalRef: "refs/heads/main", LocalOID: gitOut(cwd, "rev-parse", "HEAD")}}
	if err := checkGitPushObjects(ctx, cwd, updates); err != nil {
		t.Fatal(err)
	}
	runLifecycleGit(t, cwd, "commit", "--allow-empty", "-qm", "concurrent code move")
	if checkGitPushObjects(ctx, cwd, updates) == nil {
		t.Fatal("stale Git selection accepted")
	}
}

func TestGitPushSelectionRequiresCompleteBoundedInput(t *testing.T) {
	for _, width := range []int{40, 64} {
		a, b, zero := strings.Repeat("a", width), strings.Repeat("b", width), strings.Repeat("0", width)
		row := "refs/heads/topic " + a + " refs/heads/topic " + b
		for name, test := range map[string]struct {
			input string
			count int
			bad   bool
		}{
			"empty":                         {"", 0, false},
			"whitespace":                    {"\n \t\n", 0, false},
			"update":                        {row + "\n", 1, false},
			"no final newline":              {row, 1, false},
			"create":                        {"refs/heads/topic " + a + " refs/heads/topic " + zero, 1, false},
			"identical duplicate":           {row + "\n" + row, 1, false},
			"different destination":         {row + "\nrefs/heads/topic " + a + " refs/heads/canonical " + b, 2, false},
			"tag":                           {"refs/tags/v1 " + a + " refs/tags/v1 " + zero, 0, false},
			"tag to branch":                 {"refs/tags/v1 " + a + " refs/heads/topic " + zero, 0, false},
			"delete":                        {"(delete) " + zero + " refs/heads/topic " + b, 0, false},
			"expression":                    {"HEAD~1 " + a + " refs/heads/topic " + b, 0, false},
			"raw oid":                       {a + " " + a + " refs/heads/topic " + b, 0, false},
			"heads prefix":                  {"refs/heads-other/topic " + a + " refs/heads/topic " + b, 0, false},
			"missing field after valid row": {row + "\nrefs/heads/other " + a, 0, true},
			"extra field":                   {row + " extra", 0, true},
			"nonhex":                        {"refs/heads/topic " + strings.Repeat("z", width) + " refs/heads/topic " + b, 0, true},
			"different oid format":          {"refs/heads/topic " + a + " refs/heads/topic " + strings.Repeat("b", 104-width), 0, true},
			"both zero":                     {"(delete) " + zero + " refs/heads/topic " + zero, 0, true},
			"zero without delete":           {"refs/heads/topic " + zero + " refs/heads/topic " + b, 0, true},
			"delete with oid":               {"(delete) " + a + " refs/heads/topic " + b, 0, true},
			"invalid destination":           {"refs/heads/topic " + a + " main " + b, 0, true},
			"invalid named source":          {"refs/heads/../topic " + a + " refs/heads/topic " + b, 0, true},
			"destination contradiction":     {row + "\nrefs/heads/other " + a + " refs/heads/topic " + b, 0, true},
			"source contradiction":          {row + "\nrefs/heads/topic " + b + " refs/heads/other " + a, 0, true},
			"valid prefix over limit":       {row + "\n" + strings.Repeat(" ", maxPrePushBytes), 0, true},
		} {
			t.Run(name+"/"+strconv.Itoa(width), func(t *testing.T) {
				got, err := readGitPushUpdates(strings.NewReader(test.input))
				if (err != nil) != test.bad || len(got) != test.count {
					t.Fatalf("selected=%d err=%v, want count=%d bad=%v", len(got), err, test.count, test.bad)
				}
				if test.bad && got != nil {
					t.Fatal("invalid suffix authorized a partial prefix")
				}
				for _, u := range got {
					if u.LocalRef != "refs/heads/topic" || u.LocalOID != a {
						t.Fatalf("selection changed source: %+v", u)
					}
				}
			})
		}
	}
}

type failedGitPushReader struct{ sent bool }

func (r *failedGitPushReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	r.sent = true
	return copy(p, "refs/heads/topic "+strings.Repeat("a", 40)+" refs/heads/topic "+strings.Repeat("b", 40)+"\n"), nil
}

func TestGitPushSelectionReadErrorDoesNotAuthorizePrefix(t *testing.T) {
	got, err := readGitPushUpdates(&failedGitPushReader{})
	if !errors.Is(err, io.ErrUnexpectedEOF) || got != nil {
		t.Fatalf("got %d selections and %v", len(got), err)
	}
}
