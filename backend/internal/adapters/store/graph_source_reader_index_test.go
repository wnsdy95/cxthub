package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphSourceCountingContext struct {
	context.Context
	checks int
}

func (ctx *graphSourceCountingContext) Err() error {
	ctx.checks++
	return ctx.Context.Err()
}

func graphSourceIndexedFixture(test testing.TB, unrelated int) (*GraphSourceReader, graphSourceFixture, []domain.PRParticipation) {
	test.Helper()
	fixture := newGraphSourceFixture(test)
	entries := fixture.entries(test)
	second := fixture.participation
	second.PublicationID = strings.Repeat("7", 32)
	second.ProviderRepositoryID++
	second = second.WithID()
	entries = append(entries, graphSourceTestEntry(test, fixture.metadata, second))
	selected := []domain.PRParticipation{fixture.participation, second}
	slices.SortFunc(selected, func(left, right domain.PRParticipation) int { return strings.Compare(left.ID, right.ID) })
	for index := range unrelated {
		delta := fixture.delta.Delta
		delta.Commit = fmt.Sprintf("%040x", index+1)
		participation := fixture.participation
		participation.Number = index + 100
		participation = participation.WithID()
		entries = append(entries, graphSourceTestEntry(test, fixture.metadata, domain.NewGitDelta(fixture.metadata.ID, fixture.metadata.GitRemoteURL, delta)), graphSourceTestEntry(test, fixture.metadata, participation))
	}
	slices.Reverse(entries)
	reader, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, entries)
	if err != nil {
		test.Fatal(err)
	}
	return reader, fixture, selected
}

func TestGraphSourceReaderIndexedReadBounds(test *testing.T) {
	var deltaChecks, participationChecks int
	for _, unrelated := range []int{0, 1000} {
		test.Run(strconv.Itoa(unrelated), func(test *testing.T) {
			reader, fixture, selected := graphSourceIndexedFixture(test, unrelated)
			ctx := &graphSourceCountingContext{Context: context.Background()}
			delta, err := reader.GetGitDelta(ctx, fixture.metadata.ID, fixture.metadata.GitRemoteURL, fixture.delta.Delta.Commit, "")
			graphSourceTestEqual(test, delta, err, fixture.delta.Delta)
			if unrelated == 0 {
				deltaChecks = ctx.checks
			} else if ctx.checks != deltaChecks {
				test.Fatalf("delta lookup visited unrelated evidence: %d checks, baseline %d", ctx.checks, deltaChecks)
			}
			ctx.checks = 0
			participations, err := reader.ListPRParticipations(ctx, fixture.metadata.ID, fixture.metadata.GitRemoteURL, fixture.participation.Number)
			graphSourceTestEqual(test, participations, err, selected)
			if unrelated == 0 {
				participationChecks = ctx.checks
			} else if ctx.checks != participationChecks {
				test.Fatalf("PR lookup visited unrelated evidence: %d checks, baseline %d", ctx.checks, participationChecks)
			}
			if _, err := reader.GetGitDelta(ctx, fixture.metadata.ID, fixture.metadata.GitRemoteURL, strings.Repeat("f", 40), ""); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("absent tuple selected unrelated delta", err)
			}
			missing, err := reader.ListPRParticipations(ctx, fixture.metadata.ID, fixture.metadata.GitRemoteURL, 99)
			graphSourceTestEqual(test, missing, err, []domain.PRParticipation{})
		})
	}
}

func BenchmarkGraphSourceReaderIndexedReads(benchmark *testing.B) {
	for _, unrelated := range []int{0, 1000, 10000} {
		benchmark.Run(strconv.Itoa(unrelated), func(benchmark *testing.B) {
			reader, fixture, _ := graphSourceIndexedFixture(benchmark, unrelated)
			benchmark.Run("delta", func(benchmark *testing.B) {
				benchmark.ReportAllocs()
				for benchmark.Loop() {
					if _, err := reader.GetGitDelta(context.Background(), fixture.metadata.ID, fixture.metadata.GitRemoteURL, fixture.delta.Delta.Commit, ""); err != nil {
						benchmark.Fatal(err)
					}
				}
			})
			benchmark.Run("participations", func(benchmark *testing.B) {
				benchmark.ReportAllocs()
				for benchmark.Loop() {
					if _, err := reader.ListPRParticipations(context.Background(), fixture.metadata.ID, fixture.metadata.GitRemoteURL, fixture.participation.Number); err != nil {
						benchmark.Fatal(err)
					}
				}
			})
		})
	}
}

func TestGraphSourceReaderHistoricalGitOrigin(test *testing.T) {
	fixture := newGraphSourceFixture(test)
	metadata := fixture.metadata
	metadata.GitRemoteURL = "https://github.com/acme/new-origin.git"
	reader, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, metadata, fixture.entries(test))
	if err != nil {
		test.Fatal(err)
	}
	repo, origin := fixture.metadata.ID, fixture.metadata.GitRemoteURL
	proofs, err := reader.ListGitContributionProofs(context.Background(), repo)
	graphSourceTestEqual(test, proofs, err, []domain.GitContributionProof{fixture.proof})
	delta, err := reader.GetGitDelta(context.Background(), repo, origin, fixture.delta.Delta.Commit, "")
	graphSourceTestEqual(test, delta, err, fixture.delta.Delta)
	if _, err := reader.GetGitDelta(context.Background(), repo, metadata.GitRemoteURL, fixture.delta.Delta.Commit, ""); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("historical identity rewritten into selected origin", err)
	}
	receipt, err := reader.GetGitHeadReceipt(context.Background(), repo, origin, fixture.receipt.Version)
	graphSourceTestEqual(test, receipt, err, fixture.receipt)
	if _, err := reader.GetGitHeadReceipt(context.Background(), repo, metadata.GitRemoteURL, fixture.receipt.Version); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("historical receipt rewritten into selected origin", err)
	}
}
