package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitevidence"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func publicationFixtureGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory, "-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgsign=false", "-c", "user.name=Publication Fixture", "-c", "user.email=publication@example.test"}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func newPublicationGitFixture(t *testing.T) (publicationLocalGit, []string) {
	t.Helper()
	directory := t.TempDir()
	git := func(args ...string) string { return publicationFixtureGit(t, directory, args...) }
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-b", "main")
	git("remote", "add", "origin", publicationGitOrigin)
	write("nested/file", "base\n")
	write("deleted", "removed later\n")
	write("renamed", "moved later\n")
	write("executable", "executable later\n")
	if err := os.Symlink("nested/file", filepath.Join(directory, "link")); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "root")
	root := git("rev-parse", "HEAD")
	git("switch", "-qc", "feature")
	write("nested/file", "feature\n")
	git("rm", "deleted")
	git("mv", "renamed", "nested/moved")
	if err := os.Chmod(filepath.Join(directory, "executable"), 0700); err != nil {
		t.Fatal(err)
	}
	git("update-index", "--chmod=+x", "executable")
	git("add", "nested/file")
	git("commit", "-qm", "feature")
	feature := git("rev-parse", "HEAD")
	git("switch", "-q", "main")
	write("main-only", "main\n")
	git("add", "main-only")
	git("commit", "-qm", "main")
	main := git("rev-parse", "HEAD")
	git("merge", "--no-ff", "-qm", "merge", "feature")
	return publicationLocalGit{directory: directory}, []string{root, feature, main, git("rev-parse", "HEAD")}
}

func TestPublicationLocalGitFullTreesAndEveryParent(t *testing.T) {
	reader, commits := newPublicationGitFixture(t)
	ctx := context.Background()
	merge := commits[3]
	evidence, err := reader.ReadCommitTree(ctx, publicationGitOrigin, merge)
	if err != nil || evidence.Validate() != nil {
		t.Fatalf("invalid immutable tree: %+v %v", evidence, err)
	}
	if !reflect.DeepEqual(evidence.Commit.Parents, []string{commits[2], commits[1]}) || len(evidence.Nodes) != 2 {
		t.Fatalf("incomplete merge tree: %+v", evidence)
	}
	deltas, err := reader.ReadCommitDeltas(ctx, publicationGitOrigin, merge)
	if err != nil || len(deltas) != 2 {
		t.Fatalf("missing merge parent: %+v %v", deltas, err)
	}
	wantPaths := [][]string{{"deleted", "executable", "nested/file", "nested/moved", "renamed"}, {"main-only"}}
	for index, delta := range deltas {
		paths := []string{}
		for _, change := range delta.Changes {
			paths = append(paths, change.Path)
		}
		if !delta.Complete || delta.Parent != evidence.Commit.Parents[index] || !reflect.DeepEqual(paths, wantPaths[index]) {
			t.Fatalf("wrong per-parent changes: %+v", delta)
		}
		parent, err := reader.ReadCommitTree(ctx, publicationGitOrigin, delta.Parent)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := reader.tree(ctx, parent.Commit.Tree)
		if err != nil {
			t.Fatal(err)
		}
		entries = publicationLeafEntries(entries)
		for _, change := range delta.Changes {
			if entries[change.Path] != change.Before {
				t.Fatalf("wrong preimage: %+v", change)
			}
			if change.After.OID == "" {
				delete(entries, change.Path)
			} else {
				entries[change.Path] = change.After
			}
		}
		if _, err := domain.BuildGitTreeNodes(evidence.Commit.Tree, entries); err != nil {
			t.Fatalf("parent delta does not recreate exact merge tree: %v", err)
		}
	}
	root, err := reader.ReadCommitDeltas(ctx, publicationGitOrigin, commits[0])
	if err != nil || len(root) != 1 || root[0].Parent != "" || len(root[0].Changes) != 5 || root[0].Parents == nil {
		t.Fatalf("incomplete root: %+v %v", root, err)
	}
	for _, change := range root[0].Changes {
		if change.Before != (domain.GitEntry{}) || change.After.OID == "" {
			t.Fatalf("root is not all additions: %+v", change)
		}
	}
	heads, more, err := reader.ListGitHeads(ctx, publicationGitOrigin, 1)
	if err != nil || more || len(heads) != 2 {
		t.Fatalf("wrong actual refs: %+v %t %v", heads, more, err)
	}
}

func TestPublicationLocalGitRejectsUnverifiedObjects(t *testing.T) {
	reader, commits := newPublicationGitFixture(t)
	ctx := context.Background()
	if _, err := reader.ReadCommitTree(ctx, "https://github.com/other/repo", commits[3]); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("origin mismatch accepted: %v", err)
	}
	if _, err := reader.ReadCommitTree(ctx, publicationGitOrigin, "HEAD"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("mutable revision accepted: %v", err)
	}
	publicationFixtureGit(t, reader.directory, "tag", "-a", "fixture-tag", "-m", "annotated", commits[3])
	tag := publicationFixtureGit(t, reader.directory, "rev-parse", "fixture-tag")
	if _, err := reader.ReadCommitTree(ctx, publicationGitOrigin, tag); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("tag silently peeled to commit: %v", err)
	}
	publicationFixtureGit(t, reader.directory, "replace", commits[3], commits[0])
	evidence, err := reader.ReadCommitTree(ctx, publicationGitOrigin, commits[3])
	if err != nil || !reflect.DeepEqual(evidence.Commit.Parents, []string{commits[2], commits[1]}) {
		t.Fatalf("local replacement changed immutable evidence: %+v %v", evidence, err)
	}
	shallow := filepath.Join(t.TempDir(), "shallow")
	publicationFixtureGit(t, reader.directory, "-c", "protocol.file.allow=always", "clone", "--depth=1", "--no-local", "--single-branch", "file://"+reader.directory, shallow)
	reader.directory = shallow
	if _, err := reader.ReadCommitDeltas(ctx, publicationGitOrigin, commits[3]); err == nil {
		t.Fatal("unavailable parent produced complete deltas")
	}
}

func TestPublicationGitScanRecoversProviderFailureWithoutMovingContext(t *testing.T) {
	reader, commits := newPublicationGitFixture(t)
	core, storage := newFsckSvc(t)
	ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
	defer cancel()
	repo := hh(t.Name())
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: publicationGitOrigin}); err != nil {
		t.Fatal(err)
	}
	provider, err := NewGitScans(core, gitevidence.NewGitHub(func() string { return "" }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ObservePush(ctx, publicationGitOrigin, "refs/heads/main", commits[2], commits[3], false, "provider-unavailable"); err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimGitScan(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.run(ctx, job); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("fake provider unexpectedly available: %v", err)
	}
	failed, err := storage.GetGitScan(ctx, repo, job.ID)
	if err != nil || failed.State != "attention" {
		t.Fatalf("provider failure not durable: %+v %v", failed, err)
	}
	for repeat := 0; repeat < 2; repeat++ {
		count, err := scanPublicationFixture(ctx, core, repo, reader, commits[3])
		if err != nil || count != len(commits) {
			t.Fatalf("actual Git scan closure: %d %v", count, err)
		}
	}
	refs, err := core.ListRefs(ctx, repo)
	if err != nil || len(refs) != 0 {
		t.Fatalf("evidence scan changed context refs: %+v %v", refs, err)
	}
	history, err := core.ListHistory(ctx, repo)
	if err != nil || len(history) != 0 {
		t.Fatalf("evidence scan changed context history: %+v %v", history, err)
	}
}

func TestPublicationGitScanReadsRewrittenSourcesFromReferenceClone(t *testing.T) {
	client, commits := newPublicationGitFixture(t)
	git := func(args ...string) string { return publicationFixtureGit(t, client.directory, args...) }
	git("switch", "-qc", "rewritten-feature")
	var originals []string
	for _, body := range []string{"first source\n", "final source\n"} {
		if err := os.WriteFile(filepath.Join(client.directory, "nested", "file"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		git("add", "nested/file")
		git("commit", "-qm", body)
		originals = append(originals, git("rev-parse", "HEAD"))
	}
	git("reset", "--soft", commits[3])
	git("commit", "-qm", "squashed source")
	git("switch", "-q", "main")
	git("merge", "--no-ff", "-qm", "rewritten merge", "rewritten-feature")
	merge := git("rev-parse", "HEAD")
	for _, original := range originals {
		if refs := git("for-each-ref", "--contains", original); refs != "" {
			t.Fatalf("original source still reachable from fixture refs: %s %s", original, refs)
		}
	}
	forge := filepath.Join(t.TempDir(), "publication-code.git")
	git("init", "--bare", "-q", forge)
	git("push", "-q", forge, "main")
	publicationFixtureGit(t, forge, "symbolic-ref", "HEAD", "refs/heads/main")
	for _, referenced := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain_clone_fails_closed", true: "reference_clone_indexes_originals"}[referenced], func(t *testing.T) {
			host := filepath.Join(t.TempDir(), "publication-host")
			args := []string{"-c", "url." + forge + ".insteadOf=" + publicationGitOrigin, "clone", "-q"}
			if referenced {
				args = append(args, "--reference", client.directory)
			}
			publicationFixtureGit(t, client.directory, append(args, publicationGitOrigin, host)...)
			if origin := publicationFixtureGit(t, host, "config", "--get", "remote.origin.url"); origin != publicationGitOrigin {
				t.Fatalf("fixture changed external origin: %s", origin)
			}
			reader := publicationLocalGit{directory: host}
			core, storage := newFsckSvc(t)
			ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
			defer cancel()
			repo := hh(t.Name())
			if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: publicationGitOrigin}); err != nil {
				t.Fatal(err)
			}
			originalJob := domain.NewGitScan(repo, publicationGitOrigin, originals[1], time.Now().UTC())
			if err := storage.EnqueueGitScan(ctx, originalJob); err != nil {
				t.Fatal(err)
			}
			count, err := scanPublicationFixture(ctx, core, repo, reader, merge)
			if !referenced {
				var exitError *exec.ExitError
				if count != 0 || !errors.As(err, &exitError) || exitError.ExitCode() != 128 || !strings.Contains(err.Error(), originals[1]) || !strings.Contains(err.Error(), "fatal:") {
					t.Fatalf("missing original object did not fail closed with Git diagnostics: count=%d err=%v", count, err)
				}
				failed, err := storage.GetGitScan(ctx, repo, originalJob.ID)
				if err != nil || failed.State != "retrying" || failed.TreeIndexed || failed.Indexed {
					t.Fatalf("missing original object was indexed: %+v %v", failed, err)
				}
				return
			}
			if err != nil || count != len(commits)+2 {
				t.Fatalf("actual merge closure failed with older source queued first: count=%d err=%v", count, err)
			}
			scans, err := NewGitScans(core, reader)
			if err != nil {
				t.Fatal(err)
			}
			if err := scans.Process(ctx, 32); err != nil {
				t.Fatal(err)
			}
			for _, original := range originals {
				job, err := storage.GetGitScan(ctx, repo, domain.NewGitScan(repo, publicationGitOrigin, original, time.Now().UTC()).ID)
				if err != nil || job.State != "completed" || !job.TreeIndexed || !job.Indexed {
					t.Fatalf("original source not fully indexed: %+v %v", job, err)
				}
				evidence, err := client.ReadCommitTree(ctx, publicationGitOrigin, original)
				if err != nil {
					t.Fatal(err)
				}
				stored, err := storage.GetGitCommitTree(ctx, repo, publicationGitOrigin, original)
				if err != nil || !reflect.DeepEqual(stored, evidence.Commit) {
					t.Fatalf("original commit evidence changed: %+v %v", stored, err)
				}
				for _, node := range evidence.Nodes {
					stored, err := storage.GetGitTreeNode(ctx, repo, publicationGitOrigin, node.OID)
					if err != nil || !reflect.DeepEqual(stored, node) {
						t.Fatalf("original tree evidence changed: %+v %v", stored, err)
					}
				}
				deltas, err := client.ReadCommitDeltas(ctx, publicationGitOrigin, original)
				if err != nil || len(deltas) != 1 || len(deltas[0].Changes) != 1 {
					t.Fatalf("expected real nonempty original delta: %+v %v", deltas, err)
				}
				for _, delta := range deltas {
					stored, err := storage.GetGitDelta(ctx, repo, publicationGitOrigin, original, delta.Parent)
					if err != nil || !reflect.DeepEqual(stored, delta) {
						t.Fatalf("original parent delta changed: %+v %v", stored, err)
					}
				}
			}
			refs, err := core.ListRefs(ctx, repo)
			if err != nil || len(refs) != 0 {
				t.Fatalf("evidence scan changed context refs: %+v %v", refs, err)
			}
			history, err := core.ListHistory(ctx, repo)
			if err != nil || len(history) != 0 {
				t.Fatalf("evidence scan changed context history: %+v %v", history, err)
			}
		})
	}
}

func TestPublicationLocalGitBoundsFailureDiagnostics(t *testing.T) {
	reader, _ := newPublicationGitFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := reader.git(ctx, "-c", "alias.fixture-failure=!printf '%03000d' 0 >&2; exit 7", "fixture-failure")
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 7 || !strings.Contains(err.Error(), " [truncated]") || len(err.Error()) > 2300 {
		t.Fatalf("missing bounded Git failure diagnostic: %v", err)
	}
}
