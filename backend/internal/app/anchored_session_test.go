package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func anchoredTestSHA(letter string) string { return strings.Repeat(letter, 40) }

func anchoredTestBody(lastRecord string) domain.AnchoredSessionBody {
	return domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 3, Commands: 1,
		Segments: []domain.AnchoredSegment{{
			AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1, Commands: 1},
			Commits:       []domain.AnchoredCommit{{SHA: anchoredTestSHA("a"), Rule: domain.AnchoredLinkPrintedNumber}},
		}},
		Uncommitted: &domain.AnchoredUncommitted{AnchoredRange: domain.AnchoredRange{First: lastRecord, Last: lastRecord, FirstOrdinal: 2, LastOrdinal: 2},
			Places: []domain.AnchoredPlace{{Worktree: "app", Branch: "main", Rule: domain.AnchoredPlaceRecorded}}},
	}
}

// anchoredSessionFixture is a private repository with two members and a
// viewer, on the development store.
func anchoredSessionFixture(t *testing.T) (*Service, *store.FSStore, domain.ContentHash) {
	t.Helper()
	service, storage := newFsckSvc(t)
	ctx := systemTestContext()
	repository := domain.Repository{ID: domain.NewID("ws_"), Name: "test", OwnerID: "owner", Visibility: domain.VisibilityPrivate, CreatedAt: time.Now().UTC()}
	if err := storage.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]domain.MemberRole{"member-a": domain.RoleMember, "member-b": domain.RoleMember, "viewer": domain.RoleViewer} {
		if err := storage.AddMember(ctx, domain.Membership{RepositoryID: repository.ID, UserID: user, Role: role, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	repo := hh(t.Name())
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repository.ID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	return service, storage, repo
}

func anchoredActor(user string) context.Context {
	return inbound.WithRepositoryActor(context.Background(), user)
}

func TestAnchoredSessionBelongsToItsUploader(t *testing.T) {
	service, storage, repo := anchoredSessionFixture(t)
	alice, bob := anchoredActor("member-a"), anchoredActor("member-b")

	first, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", anchoredTestBody("m3"))
	if err != nil || !first.Changed || first.Revision != 1 {
		t.Fatalf("first report: %+v %v", first, err)
	}
	// The same content again changes nothing.
	again, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", anchoredTestBody("m3"))
	if err != nil || again.Changed || again.Revision != 1 || again.BodyHash != first.BodyHash {
		t.Fatalf("same report again: %+v %v", again, err)
	}
	// A later read of the session replaces the report as a whole.
	replaced, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", anchoredTestBody("m3-later"))
	if err != nil || !replaced.Changed || replaced.Revision != 2 || replaced.BodyHash == first.BodyHash {
		t.Fatalf("replacing report: %+v %v", replaced, err)
	}
	stored, err := service.GetAnchoredSession(alice, repo, domain.ProviderClaude, "session-1")
	if err != nil || stored.Uploader != "member-a" || stored.Revision != 2 || stored.Body.Uncommitted.Last != "m3-later" || !stored.UpdatedAt.After(stored.CreatedAt.Add(-time.Second)) {
		t.Fatalf("stored report: %+v %v", stored, err)
	}

	// Another member can neither read nor replace it, and does not see it listed.
	if _, err := service.GetAnchoredSession(bob, repo, domain.ProviderClaude, "session-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another member read the report: %v", err)
	}
	if _, err := service.PutAnchoredSession(bob, repo, domain.ProviderClaude, "session-1", anchoredTestBody("m3")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("another member replaced the report: %v", err)
	}
	if listed, err := service.ListAnchoredSessions(bob, repo); err != nil || len(listed) != 0 {
		t.Fatalf("another member's list: %+v %v", listed, err)
	}
	if segments, err := service.AnchoredCommitSegments(bob, repo, anchoredTestSHA("a")); err != nil || len(segments) != 0 {
		t.Fatalf("another member's commit segments: %+v %v", segments, err)
	}
	// The same identifier under another tool is another session.
	if _, err := service.PutAnchoredSession(bob, repo, domain.ProviderCodex, "session-1", anchoredTestBody("m3")); err != nil {
		t.Fatalf("same identifier under another tool: %v", err)
	}

	listed, err := service.ListAnchoredSessions(alice, repo)
	if err != nil || len(listed) != 1 || listed[0].SessionID != "session-1" || listed[0].Records != 3 || listed[0].Segments != 1 || listed[0].Revision != 2 ||
		len(listed[0].Commits) != 1 || listed[0].Commits[0].SHA != anchoredTestSHA("a") || listed[0].Commits[0].SegmentNo != 1 {
		t.Fatalf("own list: %+v %v", listed, err)
	}
	segments, err := service.AnchoredCommitSegments(alice, repo, anchoredTestSHA("a"))
	if err != nil || len(segments) != 1 || segments[0].SessionID != "session-1" || segments[0].SegmentNo != 1 || segments[0].Rule != domain.AnchoredLinkPrintedNumber || segments[0].Segment.Last != "m2" {
		t.Fatalf("own commit segments: %+v %v", segments, err)
	}
	if segments, err := service.AnchoredCommitSegments(alice, repo, anchoredTestSHA("f")); err != nil || len(segments) != 0 {
		t.Fatalf("a commit no session is cut at: %+v %v", segments, err)
	}
	if stored, err := storage.GetAnchoredSession(context.Background(), repo, domain.ProviderClaude, "session-1"); err != nil || stored.BodyHash != replaced.BodyHash {
		t.Fatalf("store holds %+v %v", stored, err)
	}
}

func TestAnchoredSessionListPlacesSpawnedSessionsInTheirParent(t *testing.T) {
	service, _, repo := anchoredSessionFixture(t)
	alice, bob := anchoredActor("member-a"), anchoredActor("member-b")
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "parent", anchoredTestBody("m3")); err != nil {
		t.Fatal(err)
	}
	child := func(parent string, ordinal int) domain.AnchoredSessionBody {
		return domain.AnchoredSessionBody{RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Sub: true, Parent: parent,
			Spawn: &domain.AnchoredSpawn{AtRecord: "m", AtOrdinal: ordinal, Rule: domain.AnchoredSpawnToolCall}, Segments: []domain.AnchoredSegment{}}
	}
	for id, body := range map[string]domain.AnchoredSessionBody{
		"in-segment": child("parent", 1), "after-last-commit": child("parent", 2), "beyond": child("parent", 9), "parent-not-reported": child("elsewhere", 0),
	} {
		if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, id, body); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	// Another member's session under the same parent identifier is not placed
	// in a report that member cannot read.
	if _, err := service.PutAnchoredSession(bob, repo, domain.ProviderClaude, "others-child", child("parent", 1)); err != nil {
		t.Fatal(err)
	}
	placed := map[string]*domain.AnchoredSummarySpawn{}
	listed, err := service.ListAnchoredSessions(alice, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, summary := range listed {
		placed[summary.SessionID] = summary.SpawnedIn
	}
	if got := placed["in-segment"]; got == nil || *got != (domain.AnchoredSummarySpawn{SegmentNo: 1, Rule: domain.AnchoredSpawnToolCall}) {
		t.Errorf("in-segment placed at %+v", got)
	}
	if got := placed["after-last-commit"]; got == nil || *got != (domain.AnchoredSummarySpawn{Uncommitted: true, Rule: domain.AnchoredSpawnToolCall}) {
		t.Errorf("after-last-commit placed at %+v", got)
	}
	if placed["beyond"] != nil || placed["parent-not-reported"] != nil || placed["parent"] != nil {
		t.Errorf("placed without a point: %+v", placed)
	}
	theirs, err := service.ListAnchoredSessions(bob, repo)
	if err != nil || len(theirs) != 1 || theirs[0].SpawnedIn != nil {
		t.Fatalf("another member's list: %+v %v", theirs, err)
	}
}

func TestAnchoredSessionWriteNeedsAMemberAccount(t *testing.T) {
	service, _, repo := anchoredSessionFixture(t)
	for name, ctx := range map[string]context.Context{
		"a viewer":             anchoredActor("viewer"),
		"a stranger":           anchoredActor("stranger"),
		"the server itself":    systemTestContext(),
		"nobody in particular": context.Background(),
	} {
		if _, err := service.PutAnchoredSession(ctx, repo, domain.ProviderClaude, "session-1", anchoredTestBody("m3")); err == nil {
			t.Errorf("%s stored a report", name)
		}
	}
	if listed, err := service.ListAnchoredSessions(anchoredActor("member-a"), repo); err != nil || len(listed) != 0 {
		t.Fatalf("a refused report was stored: %+v %v", listed, err)
	}
	if _, err := service.ListAnchoredSessions(systemTestContext(), repo); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a list without an account: %v", err)
	}
}

func TestAnchoredSessionRejectsWhatItCannotStoreTruthfully(t *testing.T) {
	service, _, repo := anchoredSessionFixture(t)
	alice := anchoredActor("member-a")
	broken := anchoredTestBody("m3")
	broken.Records = 9
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", broken); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a report that does not cover its records: %v", err)
	}
	if _, err := service.PutAnchoredSession(alice, repo, "other-tool", "session-1", anchoredTestBody("m3")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an unknown tool: %v", err)
	}
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "../session", anchoredTestBody("m3")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an identifier that is not one: %v", err)
	}
	if _, err := service.AnchoredCommitSegments(alice, repo, "abc1234"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a short commit number: %v", err)
	}

	// A report replaces the earlier one as a whole, its parent included: a
	// relation that could not be read at first is corrected by the next report.
	child := anchoredTestBody("m3")
	child.Sub = true
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "agent-1", child); err != nil {
		t.Fatal(err)
	}
	child.Parent = "parent-one"
	if receipt, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "agent-1", child); err != nil || !receipt.Changed || receipt.Revision != 2 {
		t.Fatalf("the corrected parent: %+v %v", receipt, err)
	}
	if stored, err := service.GetAnchoredSession(alice, repo, domain.ProviderClaude, "agent-1"); err != nil || stored.Body.Parent != "parent-one" {
		t.Fatalf("the stored parent: %+v %v", stored, err)
	}
}

// A report may only name content this repository holds: the document goes up
// first through the path that verifies and counts it.
func TestAnchoredSessionContentMustBeInTheRepository(t *testing.T) {
	service, storage, repo := anchoredSessionFixture(t)
	alice := anchoredActor("member-a")
	doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
	doc.CIR.Envelope.CIRVersion, doc.CIR.Envelope.SourceProvider = "1", "claude"
	for index, text := range []string{"commit it", "committed", "one more thing"} {
		doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: index, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
	}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	body := anchoredTestBody("m3")
	body.Content = &domain.AnchoredContent{Events: 3, Parts: []domain.AnchoredContentPart{{Doc: doc.Hash, Events: 3}}}
	body.Segments[0].Events = 2
	body.Uncommitted.FirstEvent, body.Uncommitted.Events = 2, 1

	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", body); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("content the repository does not hold: %v", err)
	}
	if _, err := service.GetAnchoredSession(alice, repo, domain.ProviderClaude, "session-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a refused report was stored: %v", err)
	}
	// The same document in another repository does not count.
	other := hh(t.Name() + "/other")
	if _, err := storage.PutRepo(systemTestContext(), domain.Repo{ID: other, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.PutDoc(systemTestContext(), other, doc); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", body); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("content held by another repository: %v", err)
	}
	if _, err := storage.PutDoc(systemTestContext(), repo, doc); err != nil {
		t.Fatal(err)
	}
	receipt, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", body)
	if err != nil || !receipt.Changed {
		t.Fatalf("content the repository holds: %+v %v", receipt, err)
	}
	stored, err := service.GetAnchoredSession(alice, repo, domain.ProviderClaude, "session-1")
	if err != nil || stored.Body.Content == nil || len(stored.Body.Content.Parts) != 1 || stored.Body.Content.Parts[0].Doc != doc.Hash || stored.Body.Uncommitted.FirstEvent != 2 {
		t.Fatalf("stored content: %+v %v", stored.Body.Content, err)
	}
	// A segment read through its commit carries where it sits in the content.
	segments, err := service.AnchoredCommitSegments(alice, repo, anchoredTestSHA("a"))
	if err != nil || len(segments) != 1 || segments[0].Segment.FirstEvent != 0 || segments[0].Segment.Events != 2 {
		t.Fatalf("segment of the commit: %+v %v", segments, err)
	}
}
