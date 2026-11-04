package outbound

import (
	"reflect"
	"slices"
	"testing"
)

func TestGraphEvidenceReaderExposesOnlyProjectionReads(t *testing.T) {
	want := []string{
		"GetRepo", "GetGitCommitTree", "GetGitTreeNode", "GetGitDelta",
		"GetPRHistoryCheckpoint", "GetPRRefObservation", "ListPRParticipations", "ListPRContributions",
		"ListGitContributions", "GetGitHeadReceipt", "GetGitContributionAdmission", "GetGitContextHead", "ListGitContributionProofs",
	}
	slices.Sort(want)
	reader := reflect.TypeFor[GraphEvidenceReader]()
	var got []string
	for index := range reader.NumMethod() {
		got = append(got, reader.Method(index).Name)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("projection capabilities changed: got %v, want %v", got, want)
	}
}
