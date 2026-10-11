package domain

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func admissionMatchFixture() GitAdmissionMatch {
	return GitAdmissionMatch{
		RepoID: HashContent([]byte("admission match repo")), GitOrigin: "https://github.com/acme/api.git",
		Provider: GitProviderObservation{InstallationID: 17, RepositoryID: 29}, Page: 2, Version: 3,
		ThroughPublicationID: strings.Repeat("b", 32),
	}
}

func TestGitAdmissionMatchValidCursorStates(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		version int64
		through string
		after   string
		branch  string
	}{
		{"initial scope only", 0, "", "", ""},
		{"reset sweep", 4, "", "", ""},
		{"new finite sweep", 1, strings.Repeat("b", 32), "", ""},
		{"inside first publication", 2, strings.Repeat("b", 32), "", "release/stable"},
		{"fully visited publication", 3, strings.Repeat("b", 32), strings.Repeat("9", 32), ""},
		{"inside next publication", 4, strings.Repeat("b", 32), strings.Repeat("9", 32), "release/stable"},
		{"exhausted finite sweep", 5, strings.Repeat("b", 32), strings.Repeat("b", 32), ""},
		{"lowest publication ID", 1, strings.Repeat("0", 32), "", "main"},
		{"highest publication ID", 1, strings.Repeat("f", 32), strings.Repeat("f", 32), ""},
		{"full width version", math.MaxInt64, strings.Repeat("b", 32), strings.Repeat("9", 32), "main"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			match := admissionMatchFixture()
			match.Version, match.ThroughPublicationID = scenario.version, scenario.through
			match.AfterPublicationID, match.AfterDestination = scenario.after, scenario.branch
			before := match
			if err := match.Validate(); err != nil {
				t.Fatalf("valid cursor rejected: %+v %v", match, err)
			}
			if match != before {
				t.Fatal("validation changed cursor state")
			}
		})
	}
}

func TestGitAdmissionMatchRejectsInvalidScope(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*GitAdmissionMatch)
	}{
		{"missing repository", func(match *GitAdmissionMatch) { match.RepoID = "" }},
		{"invalid repository", func(match *GitAdmissionMatch) { match.RepoID = "repo" }},
		{"missing origin", func(match *GitAdmissionMatch) { match.GitOrigin = "" }},
		{"leading origin whitespace", func(match *GitAdmissionMatch) { match.GitOrigin = " " + match.GitOrigin }},
		{"trailing origin whitespace", func(match *GitAdmissionMatch) { match.GitOrigin += " " }},
		{"origin control character", func(match *GitAdmissionMatch) { match.GitOrigin += "\nother" }},
		{"invalid origin encoding", func(match *GitAdmissionMatch) { match.GitOrigin = string([]byte{0xff}) }},
		{"unqualified installation", func(match *GitAdmissionMatch) { match.Provider.InstallationID = 0 }},
		{"negative installation", func(match *GitAdmissionMatch) { match.Provider.InstallationID = -1 }},
		{"unqualified provider repository", func(match *GitAdmissionMatch) { match.Provider.RepositoryID = 0 }},
		{"negative provider repository", func(match *GitAdmissionMatch) { match.Provider.RepositoryID = -1 }},
		{"zero page", func(match *GitAdmissionMatch) { match.Page = 0 }},
		{"negative page", func(match *GitAdmissionMatch) { match.Page = -1 }},
		{"negative version", func(match *GitAdmissionMatch) { match.Version = -1 }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			for _, initial := range []bool{false, true} {
				match := admissionMatchFixture()
				if initial {
					match.Version, match.ThroughPublicationID = 0, ""
				}
				scenario.mutate(&match)
				if err := match.Validate(); !errors.Is(err, ErrValidation) {
					t.Fatalf("invalid scope accepted: initial=%v match=%+v err=%v", initial, match, err)
				}
			}
		})
	}
}

func TestGitAdmissionMatchZeroVersionRequiresScopeOnly(t *testing.T) {
	for _, through := range []string{"", strings.Repeat("b", 32)} {
		for _, after := range []string{"", strings.Repeat("9", 32)} {
			for _, branch := range []string{"", "main"} {
				match := admissionMatchFixture()
				match.Version, match.ThroughPublicationID = 0, through
				match.AfterPublicationID, match.AfterDestination = after, branch
				err := match.Validate()
				if through == "" && after == "" && branch == "" {
					if err != nil {
						t.Fatal("scope-only initial cursor rejected", err)
					}
				} else if !errors.Is(err, ErrValidation) {
					t.Fatalf("zero version retained progress: %+v %v", match, err)
				}
			}
		}
	}
}

func TestGitAdmissionMatchRequiresCanonicalPublicationIDs(t *testing.T) {
	for _, field := range []string{"through", "after"} {
		for _, malformed := range []struct{ name, id string }{
			{"short", strings.Repeat("1", 31)},
			{"long", strings.Repeat("1", 33)},
			{"uppercase", strings.Repeat("A", 32)},
			{"nonhex", strings.Repeat("g", 32)},
			{"space", " " + strings.Repeat("1", 31)},
			{"control", strings.Repeat("1", 31) + "\n"},
		} {
			t.Run(field+"/"+malformed.name, func(t *testing.T) {
				match := admissionMatchFixture()
				if field == "through" {
					match.ThroughPublicationID = malformed.id
				} else {
					match.ThroughPublicationID, match.AfterPublicationID = strings.Repeat("f", 32), malformed.id
				}
				if err := match.Validate(); !errors.Is(err, ErrValidation) {
					t.Fatalf("noncanonical publication cursor accepted: %+v %v", match, err)
				}
			})
		}
	}
}

func TestGitAdmissionMatchRejectsInconsistentBounds(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		through string
		after   string
		branch  string
	}{
		{"visited ID without bound", "", strings.Repeat("9", 32), ""},
		{"destination without bound", "", "", "main"},
		{"both cursors without bound", "", strings.Repeat("9", 32), "main"},
		{"visited beyond bound", strings.Repeat("9", 32), strings.Repeat("a", 32), ""},
		{"partial beyond bound", strings.Repeat("9", 32), strings.Repeat("a", 32), "main"},
		{"destination after exhausted bound", strings.Repeat("a", 32), strings.Repeat("a", 32), "main"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			match := admissionMatchFixture()
			match.ThroughPublicationID, match.AfterPublicationID, match.AfterDestination = scenario.through, scenario.after, scenario.branch
			before := match
			if err := match.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("inconsistent cursor accepted: %+v %v", match, err)
			}
			if match != before {
				t.Fatal("rejected validation changed cursor state")
			}
		})
	}
}

func TestGitAdmissionMatchDestinationCursorUsesBranchNames(t *testing.T) {
	for _, destination := range []string{"main", "feature/nested", strings.Repeat("a", 1024)} {
		match := admissionMatchFixture()
		match.AfterDestination = destination
		if err := match.Validate(); err != nil {
			t.Fatalf("first publication destination cursor rejected: %q %v", destination, err)
		}
	}
	for _, destination := range []string{"-main", "../main", "main..old", "/main", "main/", "main//next", "main.lock", "main@{1}", "main branch", "main\nbranch", strings.Repeat("a", 1025)} {
		for _, after := range []string{"", strings.Repeat("9", 32)} {
			match := admissionMatchFixture()
			match.AfterPublicationID, match.AfterDestination = after, destination
			if err := match.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("invalid destination cursor accepted: %+v %v", match, err)
			}
		}
	}
}

func TestGitAdmissionMatchRoundTripPreservesQualifiedCursor(t *testing.T) {
	for _, after := range []string{"", strings.Repeat("9", 32)} {
		match := admissionMatchFixture()
		match.Version = math.MaxInt64
		match.Provider = GitProviderObservation{InstallationID: math.MaxInt64 - 1, RepositoryID: math.MaxInt64}
		match.AfterPublicationID, match.AfterDestination = after, "release/stable"
		raw, err := json.Marshal(match)
		if err != nil {
			t.Fatal(err)
		}
		var restored GitAdmissionMatch
		if err := json.Unmarshal(raw, &restored); err != nil {
			t.Fatal(err)
		}
		if restored != match || restored.Validate() != nil {
			t.Fatalf("round trip changed provider/page scope or cursor: before=%+v after=%+v", match, restored)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["version"]) != `"9223372036854775807"` {
			t.Fatal("version lost its full-width string representation", string(fields["version"]))
		}
	}
}
