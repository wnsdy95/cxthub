package domain

import (
	"errors"
	"testing"
)

func TestGitAdmittedPublicationRejectsMixedRepositoryEvidence(test *testing.T) {
	for _, name := range []string{"publication", "source-anchor", "destination-anchor", "rename-chain"} {
		test.Run(name, func(test *testing.T) {
			fixture := newGitContributionEvidenceFixture(test)
			if name == "rename-chain" {
				fixture.renameDestination(test)
			}
			index := fixture.publicationIndex(test)
			foreign := string(HashContent([]byte("foreign repository")))
			want := ErrConflict
			if name == "publication" {
				publication := index.publications[fixture.publication.ID]
				publication.RepoID = foreign
				index.publications[publication.ID] = publication
				want = ErrIntegrity
			} else {
				anchor := fixture.admission.SourceAnchorID
				if name == "destination-anchor" {
					anchor = fixture.admission.DestinationAnchorID
				}
				if name == "rename-chain" {
					anchor = fixture.history[fixture.head.AnchorID].BindingParent
				}
				event := fixture.history[anchor]
				event.RepoID = foreign
				fixture.history[anchor] = event
			}
			_, err := GitAdmittedPublication(fixture.metadata, index, fixture.history, fixture.admission, fixture.head)
			if !errors.Is(err, want) {
				test.Fatal("accepted cross-repository publication or anchor", err, want)
			}
		})
	}
}
