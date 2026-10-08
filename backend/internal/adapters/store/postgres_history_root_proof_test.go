//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func historyRootProofFixture(t *testing.T) historyProofFixturePG {
	t.Helper()
	s, ctx := chunkReusePG(t)
	ctx = rootPublicationContext(ctx)
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	// A power-of-two tile repeats across full 512 KiB root chunks while remaining
	// unique to this fixture in the synthetic persistent global CAS.
	tile := strings.Repeat(strings.TrimPrefix(string(repo), "sha256:"), 8192)
	f := uniqueRootPublicationFixturePG(t, strings.Repeat(tile, 4))
	seedRootPG(t, ctx, s, repo, f)
	if len(f.bodies) >= len(f.manifest.Chunks) {
		t.Fatal("fixture needs repeated occurrences")
	}
	snap := domain.Snapshot{ID: f.hash, RepoID: repo, DocHash: f.hash, DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
	if err := s.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	snap, err := s.GetSnapshot(ctx, repo, snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored := map[domain.ContentHash][]byte{f.hash: docCompress(f.manifestBytes)}
	for h, b := range f.bodies {
		stored[h] = docCompress(b)
	}
	return historyProofFixturePG{s: s, ctx: ctx, repo: repo, snap: snap, chunks: f.bodies, stored: stored, last: f.manifest.Chunks[len(f.manifest.Chunks)-1].Hash}
}
func TestP9PGHistoryRootProofCurrentClosure(t *testing.T) {
	for _, mode := range []string{"valid", "tail", "descriptor", "grant-reinsert", "snapshot-identity", "busy-chunk", "busy-grant", "same-rr"} {
		t.Run(mode, func(t *testing.T) {
			f := historyRootProofFixture(t)
			err := f.s.WithinHistoryDocumentProof(f.ctx, f.repo, func(ctx context.Context, p outbound.HistoryDocumentProof) error {
				var mutateDuring func() error
				corrupt := func() error {
					bad := bytes.Clone(f.chunks[f.last])
					bad[len(bad)-1] ^= 1
					_, err := f.s.pool.Exec(f.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(f.last), docCompress(bad))
					return err
				}
				if mode == "same-rr" {
					mutateDuring = corrupt
				}
				if err := f.prepare(ctx, p, mutateDuring); err != nil {
					return err
				}
				tx, err := f.s.pool.Begin(f.ctx)
				if err != nil {
					return err
				}
				defer tx.Rollback(f.ctx)
				switch mode {
				case "tail":
					if err := corrupt(); err != nil {
						return err
					}
				case "descriptor":
					_, err = tx.Exec(f.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(f.snap.DocHash))
				case "grant-reinsert":
					_, err = tx.Exec(f.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, string(f.repo), string(f.last))
					if err == nil {
						_, err = tx.Exec(f.ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, string(f.repo), string(f.last))
					}
				case "snapshot-identity":
					// Identity updates are already forbidden by the schema. Delete/reinsert
					// exercises the separate current-row pin fence without disabling it.
					err = f.s.DeleteSnapshot(f.ctx, f.repo, f.snap.ID)
					if err == nil {
						changed := f.snap
						changed.DocIdentity = domain.DocumentIdentityLegacy
						err = f.s.PutSnapshot(f.ctx, changed)
					}
				case "busy-chunk":
					_, err = tx.Exec(f.ctx, `SELECT 1 FROM blobs WHERE hash=$1 FOR UPDATE`, string(f.last))
				case "busy-grant":
					_, err = tx.Exec(f.ctx, `SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2 FOR UPDATE`, string(f.repo), string(f.last))
				}
				if err != nil {
					return err
				}
				if !strings.HasPrefix(mode, "busy-") {
					if err := tx.Commit(f.ctx); err != nil {
						return err
					}
				}
				return f.s.WithinRepository(ctx, f.repo, func(write context.Context) error {
					if err := f.s.AdvanceRepositoryRevision(write, f.repo, false); err != nil {
						return err
					}
					if err := p.Pin(write); err != nil {
						return err
					}
					if mode == "valid" {
						// The current owned chunk tuple stays retained through the write boundary.
						blocked, cancel := context.WithTimeout(f.ctx, 30*time.Millisecond)
						defer cancel()
						_, err := f.s.pool.Exec(blocked, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(f.last))
						if err == nil {
							return errors.New("root chunk not pinned")
						}
					}
					return nil
				})
			})
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("changed root proof admitted: %v", err)
			}
			if mode != "valid" {
				revision, err := f.s.RepositoryRevision(f.ctx, f.repo)
				if err != nil || revision.Graph != 0 {
					t.Fatal("pin failure did not roll back", revision, err)
				}
			}
		})
	}
}
func TestP9PGHistoryRootCaptureRejectsWrongFramingAndIdentity(t *testing.T) {
	for _, mode := range []string{"legacy-ref", "root-ref-legacy-body", "duplicate-field", "reordered", "missing-owned-chunk"} {
		t.Run(mode, func(t *testing.T) {
			f := historyRootProofFixture(t)
			switch mode {
			case "legacy-ref":
				f.snap.DocIdentity = domain.DocumentIdentityLegacy
				if err := f.s.DeleteSnapshot(f.ctx, f.repo, f.snap.ID); err != nil {
					t.Fatal(err)
				}
				if err := f.s.PutSnapshot(f.ctx, f.snap); err != nil {
					t.Fatal(err)
				}
			case "root-ref-legacy-body":
				raw := []byte(`{"envelope":{"cir_version":"1"},"events":[]}`)
				if _, err := f.s.pool.Exec(f.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(f.snap.DocHash), raw); err != nil {
					t.Fatal(err)
				}
			case "duplicate-field":
				raw, _ := docDecompress(f.stored[f.snap.DocHash])
				raw = append([]byte(`{"identity":"cxt-manifest-sha256-v1",`), raw[1:]...)
				if _, err := f.s.pool.Exec(f.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(f.snap.DocHash), raw); err != nil {
					t.Fatal(err)
				}
			case "reordered":
				raw, _ := docDecompress(f.stored[f.snap.DocHash])
				m, err := domain.DecodeConversationManifest(raw)
				if err != nil {
					t.Fatal(err)
				}
				m.Chunks[0], m.Chunks[1] = m.Chunks[1], m.Chunks[0]
				raw, err = json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.s.pool.Exec(f.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(f.snap.DocHash), raw); err != nil {
					t.Fatal(err)
				}
			case "missing-owned-chunk":
				if _, err := f.s.pool.Exec(f.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, string(f.repo), string(f.last)); err != nil {
					t.Fatal(err)
				}
			}
			err := f.s.WithinHistoryDocumentProof(f.ctx, f.repo, func(ctx context.Context, p outbound.HistoryDocumentProof) error {
				err := f.s.WithinReadSnapshot(ctx, func(read context.Context) error { return p.Capture(read, []domain.Snapshot{f.snap}) })
				if err == nil {
					t.Fatal("bad root captured")
				}
				if pin := f.s.WithinRepository(ctx, f.repo, p.Pin); !errors.Is(pin, domain.ErrConflict) {
					t.Fatalf("failed capture left usable pins: %v", pin)
				}
				return err
			})
			if err == nil {
				t.Fatal("bad root admitted")
			}
		})
	}
}
