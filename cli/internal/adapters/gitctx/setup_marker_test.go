package gitctx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMarkSetUpWritesTheMarkOnceAndKeepsWhatIsThere(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	ctx := context.Background()
	if state := InspectContextRoot(ctx, root); state.Initialized || state.Exists {
		t.Fatalf("a new repository is not set up: %+v", state)
	}
	marked, err := NewSetupMarker().MarkSetUp(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if state := InspectContextRoot(ctx, root); !state.Initialized || state.Root != marked {
		t.Fatalf("after the mark the repository is set up at %s: %+v", marked, state)
	}
	// What an earlier version wrote there stays as it is.
	head := filepath.Join(marked, ".cxt", "HEAD")
	if err := os.WriteFile(head, []byte("written before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSetupMarker().MarkSetUp(ctx, root); err != nil {
		t.Fatal(err)
	}
	if kept, err := os.ReadFile(head); err != nil || string(kept) != "written before\n" {
		t.Fatalf("the mark was rewritten: %q %v", kept, err)
	}
}

func TestMarkSetUpRefusesADirectoryThatIsALink(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, ".cxt")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSetupMarker().MarkSetUp(context.Background(), root); err == nil {
		t.Fatal("a .cxt that is a link was accepted")
	}
	if state := InspectContextRoot(context.Background(), root); state.Initialized {
		t.Fatalf("a linked .cxt counts as set up: %+v", state)
	}
}
