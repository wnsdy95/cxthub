package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func scanPublicationFixture(ctx context.Context, core *Service, repo domain.ContentHash, reader publicationLocalGit, merge string) (int, error) {
	scans, err := NewGitScans(core, reader)
	if err != nil {
		return 0, err
	}
	ctx = core.workerDocumentIdentityContext(inbound.WithSystemActor(ctx))
	expected := map[string]domain.GitTreeEvidence{}
	queue := []string{merge}
	for len(queue) != 0 {
		commit := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, seen := expected[commit]; seen {
			continue
		}
		if len(expected) >= 100 {
			return 0, fmt.Errorf("publication fixture exceeds 100 commits")
		}
		evidence, err := reader.ReadCommitTree(ctx, publicationGitOrigin, commit)
		if err != nil {
			return 0, err
		}
		expected[commit] = evidence
		queue = append(queue, evidence.Commit.Parents...)
	}
	before := ""
	if parents := expected[merge].Commit.Parents; len(parents) != 0 {
		before = parents[0]
	}
	count, err := scans.ObservePush(ctx, publicationGitOrigin, "refs/heads/main", before, merge, false, "publication-fixture:"+merge)
	if err != nil {
		return 0, err
	}
	if count != 1 {
		return 0, fmt.Errorf("fixture push observation matched %d repositories", count)
	}
	for attempts := 0; attempts < 512; attempts++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		complete := true
		for commit := range expected {
			identity := domain.NewGitScan(repo, publicationGitOrigin, commit, time.Now().UTC()).ID
			job, err := scans.store.GetGitScan(ctx, repo, identity)
			if errors.Is(err, domain.ErrNotFound) {
				complete = false
				continue
			}
			if err != nil {
				return 0, err
			}
			if job.State == "completed" && job.TreeIndexed && job.Indexed {
				continue
			}
			complete = false
			if job.State == "attention" || job.State == "retrying" {
				if err := scans.RetryScan(ctx, repo, identity); err != nil {
					return 0, err
				}
			}
		}
		if complete {
			for commit, evidence := range expected {
				cached, err := core.meta.(outbound.GitTreeStore).GetGitCommitTree(ctx, repo, publicationGitOrigin, commit)
				if err != nil || !reflect.DeepEqual(cached, evidence.Commit) {
					return 0, fmt.Errorf("stored commit evidence differs for %s: %v", commit, err)
				}
				for _, node := range evidence.Nodes {
					cached, err := core.meta.(outbound.GitTreeStore).GetGitTreeNode(ctx, repo, publicationGitOrigin, node.OID)
					if err != nil || !reflect.DeepEqual(cached, node) {
						return 0, fmt.Errorf("stored tree node differs for %s: %v", node.OID, err)
					}
				}
				deltas, err := reader.ReadCommitDeltas(ctx, publicationGitOrigin, commit)
				if err != nil {
					return 0, err
				}
				for _, delta := range deltas {
					cached, err := scans.store.GetGitDelta(ctx, repo, publicationGitOrigin, commit, delta.Parent)
					if err != nil || !reflect.DeepEqual(cached, delta) {
						return 0, fmt.Errorf("stored parent delta differs for %s/%s: %v", commit, delta.Parent, err)
					}
				}
			}
			return len(expected), nil
		}
		job, err := scans.store.ClaimGitScan(ctx, repo, time.Now().UTC(), 2*time.Minute)
		if errors.Is(err, domain.ErrNotFound) {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(20 * time.Millisecond):
				continue
			}
		}
		if err != nil {
			return 0, err
		}
		if err := scans.run(ctx, job); err != nil {
			return 0, fmt.Errorf("fixture scan %s: %w", job.Commit, err)
		}
	}
	return 0, fmt.Errorf("publication fixture scan did not drain")
}

func TestPublicationE2EGitEvidence(t *testing.T) {
	root := os.Getenv("CXT_E2E_GIT_FIXTURE_ROOT")
	if root == "" {
		t.Skip("isolated publication fixture only")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("absolute isolated fixture root required")
	}
	repo := domain.ContentHash(os.Getenv("CXT_E2E_GIT_FIXTURE_REPO"))
	merge := os.Getenv("CXT_E2E_GIT_FIXTURE_MERGE")
	if domain.ValidateContentHash(repo) != nil || domain.ValidateGitOID(merge) != nil {
		t.Fatal("exact fixture repository and Git commit required")
	}
	ctx, cancel := context.WithTimeout(inbound.WithSystemActor(context.Background()), 10*time.Second)
	defer cancel()
	reader := publicationLocalGit{directory: filepath.Join(root, "publication-host")}
	origin, err := reader.git(ctx, "config", "--get", "remote.origin.url")
	if err != nil || strings.TrimSpace(string(origin)) != publicationGitOrigin {
		t.Fatalf("unexpected fixture Git origin: %v", err)
	}
	storage, err := store.Open(filepath.Join(root, "data"), os.Getenv("CXT_E2E_DSN"), false)
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := storage.(interface{ Close() }); ok {
		defer closer.Close()
	}
	metadata, err := storage.GetRepo(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := url.Parse(metadata.RemoteURL)
	if err != nil || remote.Scheme != "http" || remote.Hostname() != "127.0.0.1" || metadata.GitRemoteURL != publicationGitOrigin {
		t.Fatal("helper requires the isolated loopback publication repository")
	}
	core := NewService(storage, storage, nil, gitengine.NewEngine(storage), storage)
	commits, err := scanPublicationFixture(ctx, core, repo, reader, merge)
	if err != nil {
		diagnosticCtx, cancelDiagnostics := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancelDiagnostics()
		jobs, diagnosticErr := storage.(outbound.GitScanStore).ListGitScans(diagnosticCtx, repo, "", 100)
		t.Fatalf("real Git evidence scan failed: %v; jobs=%+v; diagnostics error=%v", err, jobs, diagnosticErr)
	}
	t.Logf("scanned %d immutable local commits, trees and all parent deltas through GitScans", commits)
}
