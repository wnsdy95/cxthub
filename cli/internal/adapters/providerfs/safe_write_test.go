package providerfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteRepoFileAtomicRefusesSymlinkComponents(t *testing.T) {
	tests := []struct {
		name string
		set  func(t *testing.T, repo, outside string)
		rel  string
	}{
		{
			name: "directory",
			set: func(t *testing.T, repo, outside string) {
				t.Helper()
				if err := os.Symlink(outside, filepath.Join(repo, ".cxt")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
			rel: filepath.Join(".cxt", "config"),
		},
		{
			name: "file",
			set: func(t *testing.T, repo, outside string) {
				t.Helper()
				if err := os.Symlink(filepath.Join(outside, "target"), filepath.Join(repo, ".gitignore")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
			rel: ".gitignore",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			outside := t.TempDir()
			tc.set(t, repo, outside)
			if err := WriteRepoFileAtomic(repo, tc.rel, []byte("secret"), 0o644); err == nil {
				t.Fatal("symlinked repository path was accepted")
			}
			if _, err := os.Stat(filepath.Join(outside, "config")); !os.IsNotExist(err) {
				t.Fatalf("outside config created: %v", err)
			}
			if _, err := os.Stat(filepath.Join(outside, "target")); !os.IsNotExist(err) {
				t.Fatalf("outside target created: %v", err)
			}
		})
	}
}

func TestWriteRepoFileAtomicCreatesRealDirectories(t *testing.T) {
	repo := t.TempDir()
	rel := filepath.Join(".cxt", "capture", "claude.cursor")
	if err := WriteRepoFileAtomic(repo, rel, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(repo, rel))
	if err != nil || string(got) != "ok" {
		t.Fatalf("read back = %q, %v", got, err)
	}
}

func TestRemoveRepoFileRefusesSymlinkedDirectory(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "marker"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".cxt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := RemoveRepoFile(repo, filepath.Join(".cxt", "marker")); err == nil {
		t.Fatal("remove through symlinked directory was accepted")
	}
	if data, err := os.ReadFile(filepath.Join(outside, "marker")); err != nil || string(data) != "keep" {
		t.Fatalf("outside marker changed: %q, %v", data, err)
	}
}

func TestReadRegularFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "credentials.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ReadRegularFile(link); err == nil {
		t.Fatal("symlinked credential input was accepted")
	}
}

func TestEnsurePrivateDirTightensPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".cxt")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("private directory mode = %o, want 700", got)
	}
}

func TestReadRepoFileRejectsNestedSymlink(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "config"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".cxt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ReadRepoFile(repo, filepath.Join(".cxt", "config")); err == nil {
		t.Fatal("nested repository symlink was read")
	}
}

func TestReadRepoFileAllowsSymlinkedRepositoryRoot(t *testing.T) {
	parent := t.TempDir()
	realRepo := filepath.Join(parent, "real")
	if err := os.MkdirAll(filepath.Join(realRepo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realRepo, ".cxt", "config"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "linked")
	if err := os.Symlink(realRepo, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	got, err := ReadRepoFile(link, filepath.Join(".cxt", "config"))
	if err != nil || string(got) != "ok" {
		t.Fatalf("symlinked repo root read = %q, %v", got, err)
	}
}

func TestValidSessionID(t *testing.T) {
	valid := NewSessionID()
	if !ValidSessionID(valid) {
		t.Fatalf("generated session ID rejected: %q", valid)
	}
	for _, value := range []string{
		"",
		"../outside",
		"00000000-0000-0000-0000-00000000000*",
		"00000000-0000-0000-0000-000000000000/../x",
	} {
		if ValidSessionID(value) {
			t.Errorf("unsafe session ID accepted: %q", value)
		}
	}
}

func TestIsProviderSessionPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "projects", "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(dir, NewSessionID()+".jsonl")
	if err := os.WriteFile(session, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !IsProviderSessionPath(session) || !IsProviderSessionPath(session+".superseded") {
		t.Fatal("provider session path was rejected")
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	unclean := dir + string(filepath.Separator) + ".." + string(filepath.Separator) + "repo" + string(filepath.Separator) + filepath.Base(session)
	if IsProviderSessionPath(outside) || IsProviderSessionPath(link) || IsProviderSessionPath(unclean) {
		t.Fatal("unsafe provider session path was accepted")
	}
}
