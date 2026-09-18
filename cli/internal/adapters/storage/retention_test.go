package storage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestRetentionChild(t *testing.T) {
	root := os.Getenv("CXT_RETENTION_TEST_ROOT")
	if root == "" {
		return
	}
	err := NewFileStore(root).WithObjectsRetained(context.Background(), func() error {
		fmt.Println("retained")
		_, err := bufio.NewReader(os.Stdin).ReadByte()
		return err
	})
	if err != nil {
		os.Exit(2)
	}
}

func TestObjectRetentionCoordinatesProcessesAndReleasesOnExit(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRetentionChild$")
	cmd.Env = append(os.Environ(), "CXT_RETENTION_TEST_ROOT="+root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "retained\n" {
			t.Fatalf("child: %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not acquire lease")
	}
	st := NewFileStore(root)
	if acquired, err := st.TryCollectObjects(context.Background(), func() error { t.Fatal("collector entered during network read"); return nil }); err != nil || acquired {
		t.Fatalf("collection lock: %v %v", acquired, err)
	}
	if err := st.WithObjectsRetained(context.Background(), func() error { return nil }); err != nil {
		t.Fatal("readers were serialized", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if acquired, err := st.TryCollectObjects(context.Background(), func() error { return nil }); err != nil || !acquired {
		t.Fatalf("dead reader retained lease: %v %v", acquired, err)
	}
}

func TestObjectRetentionCancellationDoesNotEnterCollector(t *testing.T) {
	st := NewFileStore(t.TempDir())
	_, err := st.TryCollectObjects(context.Background(), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		defer cancel()
		err := st.WithObjectsRetained(ctx, func() error { t.Fatal("reader entered during collection"); return nil })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancellation lost: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCaptureWritesWaitForCollection(t *testing.T) {
	st := NewFileStore(t.TempDir())
	full := bigDoc(2)
	prior := full
	prior.Events = full.Events[:1]
	base, err := st.PutDoc(context.Background(), domain.SessionDoc{CIR: prior})
	if err != nil {
		t.Fatal(err)
	}
	delta := full
	delta.Events = full.Events[1:]
	for name, write := range map[string]func(context.Context) (domain.ContentHash, error){
		"whole document": func(ctx context.Context) (domain.ContentHash, error) {
			return st.PutDoc(ctx, domain.SessionDoc{CIR: full})
		},
		"incremental capture": func(ctx context.Context) (domain.ContentHash, error) {
			return st.AppendCaptureDoc(ctx, base, delta)
		},
	} {
		t.Run(name, func(t *testing.T) {
			acquired, err := st.TryCollectObjects(context.Background(), func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				if _, err := write(ctx); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("capture wrote chunks while collection held its reservation: %v", err)
				}
				return nil
			})
			if !acquired || err != nil {
				t.Fatalf("collection: acquired=%v error=%v", acquired, err)
			}
			id, err := write(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.GetDoc(context.Background(), id); err != nil {
				t.Fatal("capture did not resume with a complete document", err)
			}
		})
	}
}
