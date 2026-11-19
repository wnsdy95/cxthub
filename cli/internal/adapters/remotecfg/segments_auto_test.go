package remotecfg

import (
	"context"
	"testing"
)

func TestSegmentsAutoIsOnUnlessTurnedOff(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	// A repository without settings, or whose settings cannot be read, keeps the default.
	if !SegmentsAuto(root) {
		t.Fatal("the default must be on")
	}
	if err := configFixtureSave(root, Remotes{"origin": "http://127.0.0.1:8907/acme/demo"}); err != nil {
		t.Fatal(err)
	}
	if !SegmentsAuto(root) {
		t.Fatal("a repository with settings but no choice keeps the default")
	}
	if err := SetSegmentsAuto(ctx, root, "off"); err != nil || SegmentsAuto(root) {
		t.Fatalf("turned off: %v auto=%v", err, SegmentsAuto(root))
	}
	// Other settings stay as they were.
	if remotes, err := Load(root); err != nil || remotes["origin"] == "" {
		t.Fatalf("remotes after the change: %v %v", remotes, err)
	}
	for _, on := range []string{"on", ""} {
		if err := SetSegmentsAuto(ctx, root, "off"); err != nil {
			t.Fatal(err)
		}
		if err := SetSegmentsAuto(ctx, root, on); err != nil || !SegmentsAuto(root) {
			t.Fatalf("turned on with %q: %v", on, err)
		}
	}
	if err := SetSegmentsAuto(ctx, root, "sometimes"); err == nil || !SegmentsAuto(root) {
		t.Fatalf("an unknown value: %v", err)
	}
}
