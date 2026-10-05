//go:build darwin || linux

package nativeclaude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

const suppliedTestSessionID = "715ee90d-53b1-411d-a317-9f16e638ee6c"

func suppliedArchivePath(o Options) string {
	root := ""
	for _, entry := range o.Env {
		const prefix = "CLAUDE_CONFIG_DIR="
		if len(entry) >= len(prefix) && entry[:len(prefix)] == prefix {
			root = entry[len(prefix):]
		}
	}
	return filepath.Join(root, "projects", providerfs.EncodeCwd(o.Cwd), o.SessionID+".jsonl")
}

func TestSessionSuppliedIDOwnsFreshArchive(t *testing.T) {
	f := ordinaryFixture(t, "preapproved")
	f.opts.SessionID = suppliedTestSessionID
	f.opts.Env = append(f.opts.Env, "CXT_WRAPPED_SESSION_ID="+suppliedTestSessionID)
	e := f.start(t, true)
	if e.SessionID() != suppliedTestSessionID {
		t.Fatal("supervisor identity changed")
	}
	if _, err := e.RunOrdinary(firstExchangeRunContext(t), "ordinary question", firstExchangeAllow, InteractionHandlers{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.VerifyArchive(context.Background(), suppliedArchivePath(f.opts)); err != nil {
		t.Fatal("archive scope changed", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := StartFirstExchange(ctx, f.opts); !errors.Is(err, ErrState) {
		t.Fatal("supplied identity resumed/reused", err)
	}
}

func TestSessionSuppliedIDPreservesPinnedPreQuerySession(t *testing.T) {
	o := unitOptions(t, "normal")
	o.SessionID = suppliedTestSessionID
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := StartFirstExchange(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.SessionID() != o.SessionID || s.HostVersion() != "2.1.287" {
		t.Fatal("no-query contract changed")
	}
	if _, err := s.ContextSummary(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionSuppliedIDRejectsMalformedBeforeProbe(t *testing.T) {
	for _, id := range []string{"not-a-uuid", "715EE90D-53B1-411D-A317-9F16E638EE6C", "00000000-0000-0000-0000-000000000000", "../715ee90d-53b1-411d-a317-9f16e638ee6c", suppliedTestSessionID + "\n"} {
		f := ordinaryFixture(t, "normal")
		f.opts.SessionID = id
		marker := filepath.Join(f.opts.Cwd, "version-launched")
		f.opts.Env = append(f.opts.Env, "CXT_ORDINARY_VERSION_ARCHIVE="+marker)
		if _, err := StartFirstExchange(context.Background(), f.opts); !errors.Is(err, ErrState) {
			t.Fatal("invalid identity accepted", err)
		}
		if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid identity launched helper")
		}
	}
}

func TestSessionSuppliedIDNeverOverwritesExistingArchive(t *testing.T) {
	for _, kind := range []string{"file", "directory", "symlink", "created-during-version"} {
		t.Run(kind, func(t *testing.T) {
			f := ordinaryFixture(t, "normal")
			f.opts.SessionID = suppliedTestSessionID
			path := suppliedArchivePath(f.opts)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			const body = "synthetic preexisting archive\n"
			switch kind {
			case "file":
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(f.opts.Cwd, "missing"), path); err != nil {
					t.Fatal(err)
				}
			case "created-during-version":
				f.opts.Env = append(f.opts.Env, "CXT_ORDINARY_VERSION_ARCHIVE="+path)
			}
			if _, err := StartFirstExchange(context.Background(), f.opts); !errors.Is(err, ErrState) {
				t.Fatal("existing path accepted", err)
			}
			if _, err := os.Lstat(f.recorder); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("protocol process was launched")
			}
			if kind == "file" || kind == "created-during-version" {
				raw, err := os.ReadFile(path)
				if err != nil || string(raw) != body {
					t.Fatal("existing archive overwritten")
				}
			}
			if kind == "symlink" {
				info, err := os.Lstat(path)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("symlink replaced")
				}
			}
		})
	}
}
