//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestP5FrozenRootVerification(t *testing.T) {
	for _, text := range []string{"", "ordinary root", strings.Repeat("x", 3*domain.ConversationManifestChunkBytes)} {
		t.Run(string(domain.HashContent([]byte(text))), func(t *testing.T) {
			ctx := context.Background()
			s := NewFSStore(t.TempDir())
			repo := domain.HashContent([]byte(t.Name()))
			f := rootFixture(t, text)
			seedRootFS(t, s, repo, f)
			if err := verifyFrozenDoc(ctx, s, repo, f.hash, f.manifestBytes); err != nil {
				t.Fatal("valid frozen root rejected", err)
			}
			if err := verifyFrozenDoc(ctx, s, repo, domain.HashContent([]byte("wrong")), f.manifestBytes); err == nil {
				t.Fatal("wrong root accepted")
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if err := verifyFrozenDoc(cancelled, s, repo, f.hash, f.manifestBytes); err == nil {
				t.Fatal("cancel ignored")
			}
			for h := range f.bodies {
				if err := os.WriteFile(s.chunkPath(repo, h), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := verifyFrozenDoc(ctx, s, repo, f.hash, f.manifestBytes); err == nil {
					t.Fatal("corrupt closure accepted")
				}
				break
			}
		})
	}
}

func TestP5FrozenRootJobClosureAndLease(t *testing.T) {
	for _, mode := range []string{"waiting", "running", "completed", "empty", "missing_chunk", "corrupt_chunk", "missing_completed_doc", "wrong_completed_doc", "source_changed"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s := NewFSStore(t.TempDir())
			repo := domain.HashContent([]byte(t.Name()))
			if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
				t.Fatal(err)
			}
			text := "root job " + mode
			if mode == "empty" {
				text = ""
			}
			f := rootFixture(t, text)
			seedRootFS(t, s, repo, f)
			j := p5RootJob(t, repo, f)
			now := time.Now().UTC()
			if mode == "running" {
				j = j.Claim(now, time.Hour)
			}
			if mode == "completed" || mode == "missing_completed_doc" || mode == "wrong_completed_doc" {
				j.State = "completed"
			}
			switch mode {
			case "missing_chunk":
				for h := range f.bodies {
					if err := os.Remove(s.chunkPath(repo, h)); err != nil {
						t.Fatal(err)
					}
					break
				}
			case "corrupt_chunk":
				for h := range f.bodies {
					if err := os.WriteFile(s.chunkPath(repo, h), []byte("bad"), 0600); err != nil {
						t.Fatal(err)
					}
					break
				}
			case "missing_completed_doc":
				if err := os.Remove(s.docPath(repo, f.hash)); err != nil {
					t.Fatal(err)
				}
			case "wrong_completed_doc":
				if err := os.WriteFile(s.docPath(repo, f.hash), []byte(`{"identity":null}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			frozen, err := inspectFrozenFS(s.dataDir)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "source_changed" {
				for h := range f.bodies {
					if err := os.WriteFile(s.chunkPath(repo, h), []byte("changed"), 0600); err != nil {
						t.Fatal(err)
					}
					break
				}
			}
			got, err := verifyFrozenDocJob(ctx, frozen, j, now)
			valid := mode == "waiting" || mode == "running" || mode == "completed" || mode == "empty"
			if !valid {
				if err == nil {
					t.Fatal("invalid imported job accepted")
				}
				return
			}
			if err != nil || got.DocumentRef() != j.DocumentRef() {
				t.Fatal(got, err)
			}
			if mode == "running" {
				if got.State != "retrying" || got.Version != j.Version+1 || !got.LeaseUntil.IsZero() || got.Fences(j, now) {
					t.Fatal("old FS worker claim survived import")
				}
			} else if got.State != j.State {
				t.Fatal("job state changed")
			}
		})
	}
}

func TestP5FrozenUnknownDescriptorCannotDowngrade(t *testing.T) {
	f := rootFixture(t, "valid CIR projection")
	var noncanonical bytes.Buffer
	if err := json.Indent(&noncanonical, f.canonical, "", "  "); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyFrozenDocWithReader(context.Background(), domain.HashContent(f.canonical), noncanonical.Bytes(), nil); err != nil {
		t.Fatal("valid noncanonical legacy rejected", err)
	}
	for _, prefix := range []string{`{"format":"future","chunks":[],`, `{"identity":null,`, `{"format":"cxt-doc-chunks-v999","chunks":[],`} {
		raw := append([]byte(prefix), f.canonical[1:]...)
		_, err := verifyFrozenDocWithReader(context.Background(), domain.HashContent(f.canonical), raw, func(context.Context, domain.ContentHash) ([]byte, error) {
			t.Fatal("unexpected body read")
			return nil, nil
		})
		if err == nil {
			t.Fatal("unknown representation downgraded")
		}
	}
}

// A prepared root can exist without a published descriptor or snapshot. Its
// imported job still requires the opt-in marker before any DB write.
func TestP5FrozenPreparedJobRequiresRootRepository(t *testing.T) {
	ctx := context.Background()
	source := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	if _, err := source.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	f := rootFixture(t, "root job without published descriptor")
	job := p5RootJob(t, repo, f)
	for h, body := range f.bodies {
		if err := writeAtomic(source.chunkPath(repo, h), docCompress(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.writeDocJob(job); err != nil {
		t.Fatal(err)
	}
	frozen, err := inspectFrozenFS(source.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	// No pool: reaching a write would panic rather than mask this pre-write gate.
	pg := &PostgresStore{}
	err = pg.importJobs(ctx, frozen, []domain.Repo{{ID: repo}})
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("unmarked prepared root accepted", err)
	}
}
