package remotecfg

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestLoadSaveRoundTrip(t *testing.T) {
	root := t.TempDir()

	// No file → empty map, no error.
	r, err := Load(root)
	if err != nil || len(r) != 0 {
		t.Fatalf("empty config: got %v, %v", r, err)
	}

	r["origin"] = "http://127.0.0.1:8907/acme/demo"
	if err := Save(root, r); err != nil {
		t.Fatal(err)
	}
	got, err := Load(root)
	if err != nil || got["origin"] != r["origin"] {
		t.Fatalf("round-trip failed: got %v, %v", got, err)
	}

	u, ok := Origin(root)
	if !ok || u != r["origin"] {
		t.Fatalf("Origin: got %q, %v", u, ok)
	}
}

func TestLinkedWorktreeReadsAndWritesSharedConfig(t *testing.T) {
	primary := filepath.Join(t.TempDir(), "primary")
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(cwd string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(primary, "init", "-b", "main")
	git(primary, "config", "user.name", "cxt test")
	git(primary, "config", "user.email", "cxt@example.test")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	git(primary, "config", "core.hooksPath", hooks)
	git(primary, "config", "gc.auto", "0")
	git(primary, "commit", "--allow-empty", "-m", "initial")
	git(primary, "worktree", "add", "-b", "feature/app", linked)

	const first = "https://cxthub.example.com/alice/orders"
	if err := Save(primary, Remotes{"origin": first}); err != nil {
		t.Fatal(err)
	}
	if got, ok := Origin(linked); !ok || got != first {
		t.Fatalf("linked origin = %q, %v", got, ok)
	}
	const second = "https://cxthub.example.com/alice/platform"
	if err := Save(linked, Remotes{"origin": second}); err != nil {
		t.Fatal(err)
	}
	if got, ok := Origin(primary); !ok || got != second {
		t.Fatalf("primary origin after linked write = %q, %v", got, ok)
	}
	if _, err := os.Lstat(filepath.Join(linked, ".cxt")); !os.IsNotExist(err) {
		t.Fatalf("linked worktree gained split config: %v", err)
	}
}

func TestSaveRefusesSymlinkedCxtDirectory(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, ".cxt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := Save(repo, Remotes{"origin": "https://cxthub.example.com/alice/orders"}); err == nil {
		t.Fatal("symlinked .cxt directory was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "config")); !os.IsNotExist(err) {
		t.Fatalf("outside config created: %v", err)
	}
}

func TestValidate(t *testing.T) {
	// New repositories use <host>/<namespace>/<workspace>/<repository>.
	// Existing two-segment remotes remain valid because RepoID is derived from
	// the URL and rewriting one would fork its existing context DAG.
	valid := []string{
		"http://127.0.0.1:8907/acme/demo",
		"https://cxthub.example.com/alice/platform/backend",
	}
	for _, u := range valid {
		if err := Validate(u); err != nil {
			t.Errorf("Validate(%q) failed: %v", u, err)
		}
	}
	// Reject missing, over-deep, and malformed identities.
	invalid := []string{
		"", "ftp://host/a/b", "http://hostonly", "http://host/", "http://host/only",
		"http://host/a/b/c/d", "not a url at all ://", "https://token@host/a/b",
		"https://host/a/b?token=secret", "https://host/a/b#fragment", "https://host/a//b",
		"https://host/a/../b",
	}
	for _, u := range invalid {
		if err := Validate(u); err == nil {
			t.Errorf("Validate(%q) should not pass", u)
		}
	}
}

func TestSaveCanonicalizesAndLoadRejectsPoisonedRemote(t *testing.T) {
	repo := t.TempDir()
	if err := Save(repo, Remotes{"origin": "HTTPS://CXTHUB.EXAMPLE.COM/Alice/Orders/"}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got["origin"] != "https://cxthub.example.com/Alice/Orders" {
		t.Fatalf("canonical remote = %q", got["origin"])
	}

	config := filepath.Join(repo, ".cxt", "config")
	if err := os.WriteFile(config, []byte(`{"remotes":{"origin":"https://secret@cxthub.example.com/Alice/Orders"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(repo); err == nil {
		t.Fatal("credential-bearing stored remote was accepted")
	}
}

func TestSaveRejectsUnsafeRemoteName(t *testing.T) {
	if err := Save(t.TempDir(), Remotes{"-origin": "https://cxthub.example.com/alice/orders"}); err == nil {
		t.Fatal("option-like remote name was accepted")
	}
}

func TestRepoIDConvergence(t *testing.T) {
	// Variant URLs pointing to the same repo should converge to the same RepoID (zero-config convergence among team members).
	a := RepoIDFor("http://HOST:8907/acme/demo")
	b := RepoIDFor("http://host:8907/acme/demo/")
	c := RepoIDFor("http://host:8907/acme/demo.git")
	if a != b || b != c {
		t.Fatalf("Normalization convergence failure: %s / %s / %s", a, b, c)
	}
	if d := RepoIDFor("http://host:8907/acme/other"); d == a {
		t.Fatal("Another repo should not have the same ID")
	}
	backend := RepoIDFor("http://host:8907/acme/platform/backend")
	frontend := RepoIDFor("http://host:8907/acme/platform/frontend")
	if backend == frontend {
		t.Fatal("Repositories in the same workspace must have distinct IDs")
	}
}

func TestAPIBase(t *testing.T) {
	base, err := APIBase("https://cxthub.example.com/team/repo")
	if err != nil || base != "https://cxthub.example.com/api/v1" {
		t.Fatalf("APIBase: got %q, %v", base, err)
	}
	base, err = APIBase("http://127.0.0.1:8907/acme/demo")
	if err != nil || base != "http://127.0.0.1:8907/api/v1" {
		t.Fatalf("APIBase(port): got %q, %v", base, err)
	}
	if _, err := APIBase("https://token@cxthub.example.com/team/repo"); err == nil {
		t.Fatal("credential-bearing API base was accepted")
	}
}

// fakeGitCtx is an internal GitContext stub for decorator tests.
type fakeGitCtx struct{ repo domain.Repo }

func (f fakeGitCtx) CurrentRepo(ctx context.Context, cwd string) (domain.Repo, error) {
	return f.repo, nil
}
func (f fakeGitCtx) CurrentBranch(ctx context.Context, cwd string) (string, error) {
	return f.repo.DefaultBranch, nil
}

func TestWrapReanchorsIdentity(t *testing.T) {
	root := t.TempDir()
	inner := fakeGitCtx{repo: domain.Repo{
		ID:            domain.HashContent([]byte("/local/path")),
		LocalPath:     "/local/path",
		DefaultBranch: "main",
	}}
	g := Wrap(root, inner)

	// Origin not registered → return internal result as is.
	repo, err := g.CurrentRepo(context.Background(), root)
	if err != nil || repo.ID != inner.repo.ID {
		t.Fatalf("Fallback delegation failure: %v, %v", repo, err)
	}

	// Origin registered → resolve to URL-derived ID, maintain branch/path.
	url := "http://127.0.0.1:8907/acme/demo"
	if err := Save(root, Remotes{"origin": url}); err != nil {
		t.Fatal(err)
	}
	repo, err = g.CurrentRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID != RepoIDFor(url) || repo.RemoteURL != url {
		t.Fatalf("Resolution failure: %+v", repo)
	}
	if repo.DefaultBranch != "main" || repo.LocalPath != "/local/path" {
		t.Fatalf("Internal field preservation failure: %+v", repo)
	}
}
