package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestStagingExpectedRevisionHasPreciseFailure(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	source := f.source(t, "source", "original")
	before := f.stage(t, source)
	stale := domain.HashContent([]byte("stale index"))
	checks := []func() error{
		func() error {
			_, err := f.svc.Stage(ctx, inbound.StageInput{Cwd: f.root, ExpectedRevision: stale, Sessions: []inbound.StageSession{source}})
			return err
		},
		func() error {
			_, err := f.svc.Unstage(ctx, inbound.UnstageInput{Cwd: f.root, ExpectedRevision: stale, All: true})
			return err
		},
		func() error {
			_, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root, ExpectedRevision: stale})
			return err
		},
	}
	for _, check := range checks {
		if err := check(); !errors.Is(err, domain.ErrIndexChanged) {
			t.Fatalf("stale index error: %v", err)
		}
	}
	after, _, err := f.store.ReadStaging(ctx, f.git.repo.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("failed operation changed index")
	}
	if _, err := f.svc.Unstage(ctx, inbound.UnstageInput{Cwd: f.root, All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root, ExpectedRevision: before.Revision}); !errors.Is(err, domain.ErrIndexChanged) {
		t.Fatalf("stale empty index reported wrong cause: %v", err)
	}
}

func TestObservedCodeMismatchHasPreciseFailure(t *testing.T) {
	f := newStagingFixture(t)
	source := f.source(t, "source", "original")
	f.stage(t, source)
	f.git.sha = strings.Repeat("b", 40)
	if _, err := f.svc.Commit(context.Background(), inbound.StagingCommitInput{Cwd: f.root}); !errors.Is(err, domain.ErrCodePositionMismatch) {
		t.Fatalf("staging code mismatch: %v", err)
	}
	svc, _, _, _, git, plan := selectedPullFixture(t)
	git.sha = strings.Repeat("b", 40)
	if _, err := svc.Apply(context.Background(), git.repo.LocalPath, plan); !errors.Is(err, domain.ErrCodePositionMismatch) {
		t.Fatalf("pull code mismatch: %v", err)
	}
	if err := checkAgentCode(context.Background(), agentCodeFixture{changed: true}, "", strings.Repeat("a", 40)); !errors.Is(err, domain.ErrCodePositionMismatch) {
		t.Fatalf("delivery code mismatch: %v", err)
	}
}

type commandFailureCodec struct {
	outbound.ProviderCodec
	err error
}

func (f commandFailureCodec) Encode(context.Context, domain.CIRDocument, domain.ProviderKind) ([]byte, error) {
	return nil, f.err
}

type emptyDeliveryMaterializer struct{ outbound.SessionMaterializer }

func (emptyDeliveryMaterializer) Materialize(context.Context, []byte, string) (string, string, error) {
	return "", "", nil
}

func TestPreparedInputEncodingAndMaterializationKeepTypedCause(t *testing.T) {
	ctx := context.Background()
	cause := errors.New("disk failure")
	for _, kind := range []string{"encode", "materialize", "missing session"} {
		t.Run(kind, func(t *testing.T) {
			var encoder outbound.ProviderCodec = codec.NewCodexCodec()
			var materializer outbound.SessionMaterializer = &agentMaterializerFixture{}
			switch kind {
			case "encode":
				encoder = commandFailureCodec{ProviderCodec: encoder, err: cause}
			case "materialize":
				materializer = &agentMaterializerFixture{err: cause}
			case "missing session":
				materializer = emptyDeliveryMaterializer{}
			}
			svc := NewLoadSessionService(nil, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: encoder}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: materializer}, nil, nil, nil).WithAgentContext(&agentPackageFixture{}).WithAgentCodePosition(agentCodeFixture{})
			_, err := svc.LoadAgentContext(ctx, inbound.PrepareAgentContextInput{Provider: domain.ProviderCodex, Cwd: t.TempDir()})
			if !errors.Is(err, domain.ErrDeliveryFailed) || kind != "missing session" && !errors.Is(err, cause) {
				t.Fatalf("typed delivery cause lost: %v", err)
			}
		})
	}
}
