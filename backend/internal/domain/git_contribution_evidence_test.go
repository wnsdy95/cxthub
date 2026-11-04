package domain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type gitContributionEvidenceFixture struct {
	metadata    Repo
	admission   GitContributionAdmission
	head        GitContextHead
	publication HistoryEvent
	events      []HistoryEvent
	history     map[string]HistoryEvent
}

func newGitContributionEvidenceFixture(test *testing.T) gitContributionEvidenceFixture {
	test.Helper()
	admission, _ := gitAdmissionFixture(test)
	destination, _ := gitAdmissionRead(test, admission.Destination, admission.AfterReadVersion+1, 1, strings.Repeat("d", 40))
	head := (GitContextHead{Destination: destination, AnchorID: admission.DestinationAnchorID,
		ContextTarget: HashContent([]byte("bound destination capture")), GraphRevision: 13, EvidenceRevision: 17}).WithID()
	if head.Validate() != nil {
		test.Fatal("invalid bound head fixture")
	}
	sourceAnchor := HistoryEvent{ID: admission.SourceAnchorID, RepoID: string(destination.RepoID), BranchID: admission.SourceBranchID,
		Branch: admission.SourceBranch, Kind: "birth", CreatedAt: time.Unix(100, 0).UTC()}
	destinationAnchor := sourceAnchor
	destinationAnchor.ID, destinationAnchor.BranchID, destinationAnchor.Branch = admission.DestinationAnchorID, destination.DestinationBranchID, destination.DestinationBranch
	ordinary := sourceAnchor
	ordinary.ID, ordinary.Kind = admission.OrdinaryObservationIDs[0], "position"
	ordinary.Branch, ordinary.LocalBranch, ordinary.WorktreeID = "saved-feature", admission.SourceBranch, strings.Repeat("e", 32)
	ordinary.Source, ordinary.Target, ordinary.GitAfter = admission.Capture, admission.Capture, admission.OriginalHead
	carry := ordinary
	carry.ID, carry.GitAfter = admission.CarryObservationIDs[0], admission.SourceHead
	publication := ordinary
	publication.ID, publication.Kind, publication.MemoryPinned = admission.PublicationID, "publish", true
	publication.MemoryHash, publication.MemorySource = HashContent([]byte("original publication memory")), admission.Capture
	fixture := gitContributionEvidenceFixture{metadata: Repo{ID: destination.RepoID, GitRemoteURL: destination.GitOrigin},
		admission: admission, head: head, publication: publication, history: map[string]HistoryEvent{}}
	fixture.appendHistory(sourceAnchor, destinationAnchor, ordinary, carry, publication)
	return fixture
}

func (fixture *gitContributionEvidenceFixture) appendHistory(events ...HistoryEvent) {
	fixture.events = append(fixture.events, events...)
	for _, event := range events {
		fixture.history[event.ID] = event
	}
}

func (fixture gitContributionEvidenceFixture) publicationIndex(test *testing.T) *GitPublicationIndex {
	test.Helper()
	index, err := NewGitPublicationIndex(context.Background(), fixture.metadata.ID, fixture.events)
	if err != nil {
		test.Fatal("invalid publication history fixture", err)
	}
	return index
}

func (fixture *gitContributionEvidenceFixture) renameDestination(test *testing.T) {
	test.Helper()
	first := fixture.history[fixture.admission.DestinationAnchorID]
	first.ID, first.Kind, first.PreviousBranch, first.BindingParent = strings.Repeat("6", 32), "rename", first.Branch, first.ID
	first.Branch = "release"
	second := first
	second.ID, second.PreviousBranch, second.BindingParent, second.Branch = strings.Repeat("7", 32), first.Branch, first.ID, "renamed-main"
	fixture.appendHistory(first, second)
	destination := fixture.head.Destination
	destination.DestinationBranch = second.Branch
	fixture.head.Destination, _ = gitAdmissionRead(test, destination, destination.ReadVersion, 1, destination.DestinationHead)
	fixture.head.AnchorID = second.ID
	fixture.head = fixture.head.WithID()
}

func TestNewGitPublicationEvidence(test *testing.T) {
	fixture := newGitContributionEvidenceFixture(test)
	creation := &GitCreation{Evidence: "unavailable"}
	fixture.events[0].Creation = creation
	beforeCreation := *creation
	before := append([]HistoryEvent(nil), fixture.events...)
	evidence, err := NewGitPublicationEvidence(context.Background(), fixture.metadata.ID, fixture.metadata, fixture.events)
	if err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.events, before) || !reflect.DeepEqual(*creation, beforeCreation) || fixture.events[0].Creation != creation {
		test.Fatal("construction mutated caller history or its creation pointer")
	}
	if len(evidence.History) != len(before) {
		test.Fatal("construction lost history events")
	}
	for _, event := range before {
		if evidence.History[event.ID] != event {
			test.Fatal("history map changed an original event", event.ID)
		}
	}
	for _, binding := range []ContextBranch{
		{ID: fixture.admission.SourceBranchID, Name: fixture.admission.SourceBranch, EventID: fixture.admission.SourceAnchorID},
		{ID: fixture.admission.Destination.DestinationBranchID, Name: fixture.admission.Destination.DestinationBranch, EventID: fixture.admission.DestinationAnchorID},
	} {
		if evidence.Bindings.Active[binding.Name] != binding || evidence.Bindings.ByID[binding.ID] != binding {
			test.Fatal("construction changed branch bindings", binding)
		}
	}
	fixture.events[0] = HistoryEvent{}
	if evidence.History[before[0].ID] != before[0] {
		test.Fatal("history map aliases the caller slice")
	}
	delete(evidence.History, fixture.publication.ID)
	if publication, err := evidence.Index.Publication(fixture.publication.ID); err != nil || publication != fixture.publication {
		test.Fatal("history map mutation changed publication index", publication, err)
	}
}

func TestNewGitPublicationEvidenceFailures(test *testing.T) {
	for _, scenario := range []string{"canceled", "foreign metadata", "foreign history", "invalid repo", "duplicate identical", "duplicate conflicting binding", "binding conflict"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitContributionEvidenceFixture(test)
			ctx := context.Background()
			repo, want := fixture.metadata.ID, ErrIntegrity
			switch scenario {
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx, want = canceled, context.Canceled
				fixture.events[0].RepoID = string(HashContent([]byte("foreign event")))
			case "foreign metadata":
				fixture.metadata.ID = HashContent([]byte("foreign metadata"))
			case "foreign history":
				fixture.events[4].RepoID = string(HashContent([]byte("foreign event")))
			case "invalid repo":
				repo, fixture.metadata.ID, want = "invalid", "invalid", ErrValidation
			case "duplicate identical":
				fixture.events = append(fixture.events, fixture.publication)
			case "duplicate conflicting binding":
				duplicate := fixture.events[0]
				duplicate.Branch = "conflicting-name"
				fixture.events = append(fixture.events, duplicate)
			case "binding conflict":
				conflicting := fixture.events[0]
				conflicting.ID, conflicting.Branch = strings.Repeat("9", 32), "conflicting-name"
				fixture.events = append(fixture.events, conflicting)
				_, want = ProjectContextBranches(fixture.events)
				if want == nil {
					test.Fatal("binding conflict fixture did not conflict")
				}
			}
			before := append([]HistoryEvent(nil), fixture.events...)
			evidence, err := NewGitPublicationEvidence(ctx, repo, fixture.metadata, fixture.events)
			if scenario == "binding conflict" {
				if err == nil || err.Error() != want.Error() {
					test.Fatal("binding failure was replaced", err, want)
				}
			} else if !errors.Is(err, want) {
				test.Fatal("index-first validation or metadata scope changed", err, want)
			}
			if !reflect.DeepEqual(evidence, GitPublicationEvidence{}) || !reflect.DeepEqual(fixture.events, before) {
				test.Fatal("failed construction leaked partial evidence or changed history")
			}
		})
	}
}

func TestGitAdmittedPublication(test *testing.T) {
	fixture := newGitContributionEvidenceFixture(test)
	fixture.renameDestination(test)
	archive := fixture.history[fixture.admission.SourceAnchorID]
	archive.ID, archive.Kind, archive.BindingParent = strings.Repeat("8", 32), "archive", archive.ID
	ordinary, carry := fixture.events[2], fixture.events[3]
	ordinary.ID, carry.ID = strings.Repeat("9", 32), strings.Repeat("a", 32)
	fixture.appendHistory(archive, ordinary, carry)
	evidence, err := NewGitPublicationEvidence(context.Background(), fixture.metadata.ID, fixture.metadata, fixture.events)
	if err != nil || !evidence.Bindings.ByID[fixture.admission.SourceBranchID].Archived {
		test.Fatal("invalid archived-source and rename-chain fixture", err)
	}
	publication, err := GitAdmittedPublication(fixture.metadata, evidence.Index, evidence.History, fixture.admission, fixture.head)
	if err != nil || publication != fixture.publication || publication.GitAfter == fixture.admission.SourceHead {
		test.Fatal("historical anchors or additional witnesses changed the original publication and pin", publication, err)
	}
}

func TestGitAdmittedPublicationMismatches(test *testing.T) {
	for _, scenario := range []string{"invalid admission", "invalid head", "admission repository", "head repository", "admission origin", "head origin", "missing publication", "source identity", "capture", "original head", "local branch precedence", "nil index"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitContributionEvidenceFixture(test)
			index, want := fixture.publicationIndex(test), ErrIntegrity
			switch scenario {
			case "admission repository":
				fixture.admission.Destination.RepoID = HashContent([]byte("foreign admission repo"))
			case "head repository":
				fixture.head.Destination.RepoID = HashContent([]byte("foreign head repo"))
			case "admission origin":
				fixture.admission.Destination.GitOrigin = "https://github.com/acme/other.git"
			case "head origin":
				fixture.head.Destination.GitOrigin = "https://github.com/acme/other.git"
			case "missing publication":
				fixture.admission.PublicationID, fixture.history, want = strings.Repeat("f", 32), nil, ErrNotFound
			case "source identity":
				fixture.admission.SourceBranchID = "other-source-incarnation"
			case "capture":
				fixture.admission.Capture = HashContent([]byte("other capture"))
			case "original head":
				fixture.admission.OriginalHead = strings.Repeat("f", 40)
			case "local branch precedence":
				fixture.admission.SourceBranch = fixture.publication.Branch
			case "nil index":
				index = nil
			}
			fixture.admission.Destination = fixture.admission.Destination.WithID()
			fixture.admission.SourceObservationID = gitAdmissionSourceObservation(fixture.admission).ID
			fixture.admission = fixture.admission.WithID()
			fixture.head.Destination = fixture.head.Destination.WithID()
			fixture.head = fixture.head.WithID()
			if fixture.admission.Validate() != nil || fixture.head.Validate() != nil {
				test.Fatal("mismatch fixture is not independently valid")
			}
			if scenario == "invalid admission" {
				fixture.admission.ID = "invalid"
			}
			if scenario == "invalid head" {
				fixture.head.ID = "invalid"
			}
			publication, err := GitAdmittedPublication(fixture.metadata, index, fixture.history, fixture.admission, fixture.head)
			if !errors.Is(err, want) || publication != (HistoryEvent{}) {
				test.Fatal("mismatched publication returned evidence or changed the error", publication, err, want)
			}
		})
	}
}

func TestGitAdmittedPublicationWitnessSubset(test *testing.T) {
	for _, scenario := range []string{"ordinary omitted", "carry omitted", "ordinary frozen superset", "carry frozen superset", "ordinary is carry", "carry is ordinary", "missing carry group"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitContributionEvidenceFixture(test)
			want := ErrConflict
			switch scenario {
			case "ordinary omitted":
				fixture.events[2].ID = strings.Repeat("9", 32)
			case "carry omitted":
				fixture.events[3].ID = strings.Repeat("9", 32)
			case "ordinary frozen superset":
				fixture.admission.OrdinaryObservationIDs = append(fixture.admission.OrdinaryObservationIDs, strings.Repeat("9", 32))
			case "carry frozen superset":
				fixture.admission.CarryObservationIDs = append(fixture.admission.CarryObservationIDs, strings.Repeat("9", 32))
			case "ordinary is carry":
				fixture.admission.OrdinaryObservationIDs = fixture.admission.CarryObservationIDs
			case "carry is ordinary":
				fixture.admission.CarryObservationIDs = fixture.admission.OrdinaryObservationIDs
			case "missing carry group":
				fixture.events[3].GitAfter, fixture.history, want = fixture.admission.OriginalHead, nil, ErrNotFound
			}
			fixture.admission = fixture.admission.WithID()
			if fixture.admission.Validate() != nil {
				test.Fatal("invalid frozen witness fixture")
			}
			publication, err := GitAdmittedPublication(fixture.metadata, fixture.publicationIndex(test), fixture.history, fixture.admission, fixture.head)
			if !errors.Is(err, want) || publication != (HistoryEvent{}) {
				test.Fatal("missing or swapped frozen witness accepted", publication, err, want)
			}
		})
	}
}

func TestGitAdmittedPublicationAnchors(test *testing.T) {
	for _, scenario := range []string{"legacy", "legacy identity mismatch", "missing source", "nonbinding source", "archived source", "source identity", "source name", "missing destination", "head name", "destination identity", "truncated ancestry", "missing ancestor", "nonbinding ancestor", "archived ancestor", "ancestor identity", "cyclic ancestry"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitContributionEvidenceFixture(test)
			want := ErrConflict
			if strings.HasPrefix(scenario, "legacy") {
				fixture.admission.SourceBranchID = LegacyContextBranchID(string(fixture.metadata.ID), fixture.admission.SourceBranch)
				fixture.admission.Destination.DestinationBranchID = LegacyContextBranchID(string(fixture.metadata.ID), fixture.admission.Destination.DestinationBranch)
				fixture.admission.SourceAnchorID, fixture.admission.DestinationAnchorID, fixture.head.AnchorID = "", "", ""
				fixture.head.Destination.DestinationBranchID = fixture.admission.Destination.DestinationBranchID
				fixture.events, fixture.history, want = fixture.events[2:], nil, nil
				for position := range fixture.events {
					fixture.events[position].BranchID, fixture.events[position].Branch = fixture.admission.SourceBranchID, fixture.admission.SourceBranch
					fixture.events[position].LocalBranch = ""
				}
				fixture.publication = fixture.events[2]
				if scenario == "legacy identity mismatch" {
					fixture.admission.SourceBranchID, want = "nonlegacy-source", ErrIntegrity
				}
			} else {
				fixture.renameDestination(test)
				anchorID := fixture.admission.SourceAnchorID
				if scenario == "missing destination" {
					anchorID = fixture.admission.DestinationAnchorID
				} else if scenario == "head name" || scenario == "destination identity" {
					anchorID = fixture.head.AnchorID
				} else if strings.Contains(scenario, "ancestr") || strings.Contains(scenario, "ancestor") {
					anchorID = fixture.history[fixture.head.AnchorID].BindingParent
				}
				event := fixture.history[anchorID]
				switch scenario {
				case "missing source", "missing destination", "missing ancestor":
					delete(fixture.history, anchorID)
				case "nonbinding source", "nonbinding ancestor":
					event.Kind = "position"
				case "archived source", "archived ancestor":
					event.Kind = "archive"
				case "source identity", "ancestor identity":
					event.BranchID = "other-incarnation"
				case "source name", "head name":
					event.Branch = "other-name"
				case "destination identity":
					fixture.head.Destination.DestinationBranchID, event.BranchID = "other-destination", "other-destination"
				case "truncated ancestry":
					event.BindingParent = ""
				case "cyclic ancestry":
					event.BindingParent = fixture.head.AnchorID
				}
				if !strings.HasPrefix(scenario, "missing") {
					fixture.history[anchorID] = event
				}
			}
			fixture.admission.Destination = fixture.admission.Destination.WithID()
			fixture.admission = fixture.admission.WithID()
			fixture.head.Destination = fixture.head.Destination.WithID()
			fixture.head = fixture.head.WithID()
			publication, err := GitAdmittedPublication(fixture.metadata, fixture.publicationIndex(test), fixture.history, fixture.admission, fixture.head)
			if !errors.Is(err, want) {
				test.Fatal("anchor identity or causal ancestry changed", err, want)
			}
			if want == nil && publication != fixture.publication || want != nil && publication != (HistoryEvent{}) {
				test.Fatal("anchor result replaced the publication or exposed failed evidence", publication)
			}
		})
	}
}

func TestGitSupplementPublication(test *testing.T) {
	for _, scenario := range []string{"renamed pinned empty", "invalid record", "nil index", "missing publication", "repository", "source identity", "capture", "original head", "unpinned", "unproven worktree"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitContributionEvidenceFixture(test)
			admission := fixture.admission
			record := GitContribution{RepoID: admission.Destination.RepoID, GitOrigin: admission.Destination.GitOrigin,
				ProviderRepositoryID: admission.Destination.ProviderRepositoryID, DestinationBranchID: admission.Destination.DestinationBranchID,
				SourceBranchID: admission.SourceBranchID, Capture: admission.Capture, OriginalHead: admission.OriginalHead,
				IntegrationSHA: fixture.head.Destination.DestinationHead}
			publicationID, want := fixture.publication.ID, ErrConflict
			switch scenario {
			case "renamed pinned empty":
				for position := 2; position < len(fixture.events); position++ {
					fixture.events[position].Branch = "renamed-feature"
				}
				fixture.events[4].MemoryHash, fixture.events[4].MemorySource, want = "", "", nil
			case "invalid record", "nil index":
				want = ErrIntegrity
			case "missing publication":
				publicationID, want = strings.Repeat("f", 32), ErrNotFound
			case "repository":
				record.RepoID = HashContent([]byte("foreign supplement repo"))
			case "source identity":
				record.SourceBranchID = "other-incarnation"
			case "capture":
				record.Capture = HashContent([]byte("foreign supplement capture"))
			case "original head":
				record.OriginalHead = strings.Repeat("f", 40)
			case "unpinned":
				fixture.events[4].MemoryPinned = false
			case "unproven worktree":
				fixture.events[4].WorktreeID, want = strings.Repeat("f", 32), ErrNotFound
			}
			record = record.WithID()
			if record.Validate() != nil {
				test.Fatal("invalid contribution fixture")
			}
			if scenario == "invalid record" {
				record.ID = "invalid"
			}
			index := fixture.publicationIndex(test)
			if scenario == "nil index" {
				index = nil
			}
			publication, err := GitSupplementPublication(index, record, publicationID)
			if !errors.Is(err, want) {
				test.Fatal("supplement publication qualification changed", err, want)
			}
			if want == nil && publication != fixture.events[4] || want != nil && publication != (HistoryEvent{}) {
				test.Fatal("supplement replaced the publication or exposed failed evidence", publication)
			}
		})
	}
}
