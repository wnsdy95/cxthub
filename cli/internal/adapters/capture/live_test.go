package capture

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestObserveSessionTracksOnlyRegisteredGrowthAndRetries(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		t.Run(string(provider), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			// Use the same validated fixtures as the lifecycle registry tests.
			cwd := t.TempDir()
			captureGitRun(t, cwd, "init", "-b", "main")
			if err := os.MkdirAll(cwd+"/.cxt", 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cwd+"/.cxt/HEAD", []byte("ref: refs/heads/main\n"), 0600); err != nil {
				t.Fatal(err)
			}
			id := "11111111-1111-4111-8111-111111111111"
			path := writeCoordinatorSession(t, os.Getenv("HOME"), cwd, provider, id, time.Now())
			if err := TrackAppSession(cwd, provider, id, path); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			calls := 0
			err := observeSession(ctx, cwd, provider, id, time.Millisecond, time.Minute, func(context.Context) error {
				calls++
				switch calls {
				case 1:
					return errors.New("offline")
				case 2:
					f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
					_, _ = f.WriteString("{}\n")
					_ = f.Close()
				case 3:
					EndAppSession(cwd, provider, id)
				default:
					t.Fatal("observer repeated unchanged capture")
				}
				return nil
			})
			if err != nil || calls != 3 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("ending observation removed transcript")
			}
		})
	}
}

func TestWatchSessionExcludesIdleAndDuplicateObservers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	captureGitRun(t, cwd, "init", "-b", "main")
	_ = os.MkdirAll(cwd+"/.cxt", 0700)
	_ = os.WriteFile(cwd+"/.cxt/HEAD", []byte("ref: refs/heads/main\n"), 0600)
	id := "11111111-1111-4111-8111-111111111111"
	path := writeCoordinatorSession(t, os.Getenv("HOME"), cwd, domain.ProviderCodex, id, time.Now())
	if err := TrackAppSession(cwd, domain.ProviderCodex, id, path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := WatchSession(ctx, cwd, domain.ProviderCodex, id, func(context.Context) error {
		calls++
		err := WatchSession(ctx, cwd, domain.ProviderCodex, id, func(context.Context) error { t.Fatal("duplicate observer acquired lock"); return nil })
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(path, old, old)
	if err := WatchSession(context.Background(), cwd, domain.ProviderCodex, id, func(context.Context) error { t.Fatal("idle transcript recaptured"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := WatchSession(context.Background(), cwd, domain.ProviderCodex, "unregistered", func(context.Context) error { t.Fatal("unregistered sibling observed"); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestObserverPublishesDurableCaptureBeforeReadingMoreGrowth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	captureGitRun(t, cwd, "init", "-b", "main")
	_ = os.MkdirAll(cwd+"/.cxt", 0700)
	_ = os.WriteFile(cwd+"/.cxt/HEAD", []byte("ref: refs/heads/main\n"), 0600)
	id := "11111111-1111-4111-8111-111111111111"
	path := writeCoordinatorSession(t, os.Getenv("HOME"), cwd, domain.ProviderCodex, id, time.Now())
	if err := TrackAppSession(cwd, domain.ProviderCodex, id, path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	captures, uploads := 0, 0
	err := observeSession(ctx, cwd, domain.ProviderCodex, id, time.Millisecond, time.Minute, func(context.Context) error {
		captures++
		if captures > 2 {
			t.Fatal("unchanged transcript recaptured")
		}
		return nil
	}, func(context.Context) error {
		uploads++
		if uploads <= 2 {
			if captures != 1 {
				t.Fatal("new capture starved a durable upload retry")
			}
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString("{}\n")
			_ = f.Close()
			return errors.New("retry upload")
		}
		if uploads == 3 && captures != 1 {
			t.Fatal("retry replaced the pending checkpoint")
		}
		if uploads == 4 {
			if captures != 2 {
				t.Fatal("growth during retry was lost")
			}
			EndAppSession(cwd, domain.ProviderCodex, id)
		}
		return nil
	})
	if err != nil || captures != 2 || uploads != 4 {
		t.Fatalf("captures=%d uploads=%d err=%v", captures, uploads, err)
	}
}
