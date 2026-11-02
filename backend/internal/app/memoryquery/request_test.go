package memoryquery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func requestInput() (domain.ContentHash, domain.EffectiveMemoryRequest) {
	repo := domain.ContentHash("sha256:" + strings.Repeat("1", 64))
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{
		SnapshotID: domain.ContentHash("sha256:" + strings.Repeat("2", 64)), CodeCommit: strings.Repeat("a", 40),
	}}
	return repo, request
}

func TestNormalizeRequestPreservesSelectionAndPagination(test *testing.T) {
	repo, original := requestInput()
	for _, content := range []string{"", "all", "claims", "prompt"} {
		for _, limit := range []int{0, 1, 50} {
			test.Run(fmt.Sprintf("%s/%d", content, limit), func(test *testing.T) {
				request := original
				request.Content, request.Limit, request.Cursor = content, limit, strings.Repeat("x", 1024)
				want := request
				if content == "all" {
					want.Content = ""
				}
				if limit == 0 {
					want.Limit = 20
				}
				normalized, err := NormalizeRequest(repo, request)
				if err != nil || normalized != want || request.Content != content || request.Limit != limit {
					test.Fatal("normalization changed selection, cursor, caller input, or defaults", normalized, err)
				}
			})
		}
	}
}

func TestNormalizeRequestValidationKeepsOriginalPrecedence(test *testing.T) {
	for _, scenario := range []string{"repo", "snapshot", "code", "memory", "branch", "branch-and-memory", "content", "negative-limit", "large-limit", "long-cursor"} {
		test.Run(scenario, func(test *testing.T) {
			repo, request := requestInput()
			request.Content = "all"
			switch scenario {
			case "repo":
				repo = "invalid"
			case "snapshot":
				request.Selection.SnapshotID = "invalid"
			case "code":
				request.Selection.CodeCommit = "invalid"
			case "memory":
				request.Selection.MemoryHash = "invalid"
			case "branch":
				request.Selection.Branch = "../invalid"
			case "branch-and-memory":
				request.Selection.Branch, request.Selection.MemoryHash = "main", repo
			case "content":
				request.Content = "invalid"
			case "negative-limit":
				request.Limit = -1
			case "large-limit":
				request.Limit = 51
			case "long-cursor":
				request.Cursor = strings.Repeat("x", 1025)
			}
			want := request
			if want.Content == "all" {
				want.Content = ""
			}
			normalized, err := NormalizeRequest(repo, request)
			if !errors.Is(err, domain.ErrValidation) || normalized != want {
				test.Fatal("validation changed returned normalization or defaulted an invalid request", normalized, err)
			}
			target, err := ReadTarget(repo, request)
			if !errors.Is(err, domain.ErrValidation) || target != (domain.MemoryReadTarget{}) {
				test.Fatal("invalid request produced a durable target", target, err)
			}
		})
	}
}

func TestReadTargetIgnoresPaginationAndPreservesWireHash(test *testing.T) {
	repo, request := requestInput()
	request.Content, request.Limit = "all", 1
	first, err := ReadTarget(repo, request)
	if err != nil {
		test.Fatal(err)
	}
	request.Limit, request.Cursor, request.Content = 50, "continuation", ""
	second, err := ReadTarget(repo, request)
	if err != nil || first != second {
		test.Fatal("page limit or cursor changed durable target", err)
	}
	wantWire := `{"version":1,"rules":1,"repo_id":"` + string(repo) + `","selection":{"snapshot_id":"` + string(request.Selection.SnapshotID) + `","code_commit":"` + request.Selection.CodeCommit + `"},"content":""}`
	raw, err := domain.MemoryReadTargetBytes(context.Background(), first)
	if err != nil || string(raw) != wantWire {
		test.Fatal("durable target wire representation changed", string(raw), err)
	}
	identifier, err := domain.MemoryReadTargetID(context.Background(), second)
	wantID := domain.HashContent(append([]byte("cxthub:memory-read-target:v1\x00"), []byte(wantWire)...))
	if err != nil || identifier != wantID {
		test.Fatal("durable target hash or prefix changed", identifier, err)
	}
	for _, content := range []string{"claims", "prompt"} {
		request.Content = content
		target, err := ReadTarget(repo, request)
		if err != nil || target == first || target.Content != content {
			test.Fatal("view mode did not distinguish target", target, err)
		}
	}
}

func TestReadTargetBindsRepositoryAndSelection(test *testing.T) {
	repo, original := requestInput()
	first, err := ReadTarget(repo, original)
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []string{"repo", "branch", "snapshot", "code", "memory"} {
		test.Run(scenario, func(test *testing.T) {
			selectedRepo, request := repo, original
			switch scenario {
			case "repo":
				selectedRepo = original.Selection.SnapshotID
			case "branch":
				request.Selection.Branch = "main"
			case "snapshot":
				request.Selection.SnapshotID = repo
			case "code":
				request.Selection.CodeCommit = strings.Repeat("b", 40)
			case "memory":
				request.Selection.MemoryHash = repo
			}
			target, err := ReadTarget(selectedRepo, request)
			if err != nil || target == first || target.RepoID != selectedRepo || target.Selection != request.Selection {
				test.Fatal("durable target lost repository or selection binding", target, err)
			}
		})
	}
}

func TestReadTargetRetainsTargetValidationResult(test *testing.T) {
	repo, request := requestInput()
	request.Selection.Branch = string([]byte{0xff})
	normalized, err := NormalizeRequest(repo, request)
	if err != nil {
		test.Fatal("request normalization added target-only validation", err)
	}
	want := domain.MemoryReadTarget{Version: domain.MemoryReadWorkVersion, Rules: domain.MemoryReadRules,
		RepoID: repo, Selection: normalized.Selection, Content: normalized.Content}
	target, err := ReadTarget(repo, request)
	if !errors.Is(err, domain.ErrValidation) || target != want {
		test.Fatal("target validation changed its returned value", target, err)
	}
}

func TestOffsetCanonicalCursorAndValidationPrecedence(test *testing.T) {
	state := domain.ContentHash("sha256:" + strings.Repeat("3", 64))
	raw, err := json.Marshal(Cursor{Version: 1, State: state, Index: 60})
	wantWire := `{"v":1,"state":"` + string(state) + `","index":60}`
	if err != nil || string(raw) != wantWire {
		test.Fatal("v1 cursor wire representation changed", string(raw), err)
	}
	encode := func(raw string) string { return base64.RawURLEncoding.EncodeToString([]byte(raw)) }
	for _, scenario := range []struct {
		name    string
		encoded string
		want    int
		err     error
	}{
		{name: "empty"},
		{name: "canonical", encoded: encode(wantWire), want: 60},
		{name: "unknown-field", encoded: encode(`{"v":1,"state":"` + string(state) + `","index":7,"future":true}`), want: 7},
		{name: "omitted-index", encoded: encode(`{"v":1,"state":"` + string(state) + `"}`)},
		{name: "bad-base64", encoded: "!", err: domain.ErrValidation},
		{name: "padded-base64", encoded: encode(wantWire) + "=", err: domain.ErrValidation},
		{name: "bad-json", encoded: encode("{"), err: domain.ErrValidation},
		{name: "wrong-shape", encoded: encode("[]"), err: domain.ErrValidation},
		{name: "null", encoded: encode("null"), err: domain.ErrValidation},
		{name: "wrong-version-and-stale", encoded: encode(`{"v":2,"state":"stale","index":1}`), err: domain.ErrValidation},
		{name: "negative-and-stale", encoded: encode(`{"v":1,"state":"stale","index":-1}`), err: domain.ErrValidation},
		{name: "fraction", encoded: encode(`{"v":1,"state":"` + string(state) + `","index":1.5}`), err: domain.ErrValidation},
		{name: "string-index", encoded: encode(`{"v":1,"state":"` + string(state) + `","index":"1"}`), err: domain.ErrValidation},
		{name: "stale", encoded: encode(`{"v":1,"state":"stale","index":1}`), err: domain.ErrEffectiveMemoryCursorStale},
		{name: "missing-state", encoded: encode(`{"v":1,"index":1}`), err: domain.ErrEffectiveMemoryCursorStale},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			offset, err := Offset(scenario.encoded, state)
			if !errors.Is(err, scenario.err) || offset != scenario.want {
				test.Fatal("cursor offset or validation precedence changed", offset, err)
			}
		})
	}
}

func TestMemoryQueryBoundsMatchOriginalLimits(test *testing.T) {
	if MaxPageBytes != 256<<10 || MaxItems != 16384 {
		test.Fatal("query page or item bound changed", MaxPageBytes, MaxItems)
	}
	if MaxItems != domain.MaxMemoryReadItems {
		test.Fatal("query item bound differs from the stored result bound", MaxItems)
	}
}
