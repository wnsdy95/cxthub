package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func initializationWireFixture() (domain.Repo, domain.RepositoryInitializationReceipt) {
	repo := domain.Repo{ID: domain.HashContent([]byte("initialization fixture")), DefaultBranch: "main", ContextProtocol: 1, RemoteURL: "https://synthetic.invalid/owner/repo", GitRemoteURL: "https://code.invalid/owner/repo", RepositoryID: "repository_fixture"}
	return repo, domain.RepositoryInitializationReceipt{Version: 1, CreationID: "init_" + strings.Repeat("a", 32), Repo: repo}
}

func initializationAnchor(repo string) domain.RepositoryInitializationAnchor {
	id := domain.HashContent([]byte("actual snapshot"))
	return domain.RepositoryInitializationAnchor{Ref: domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, BranchID: domain.LegacyContextBranchID(repo, "main"), Target: id}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{id: domain.HashContent([]byte("state"))}}
}

// Use the real HTTP transport and codecs against an owned loopback server.
func initializationHTTPClient(t *testing.T, handler http.HandlerFunc) *BackendClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer initialization-fixture-token" {
			t.Error("initialization request lost authenticated token")
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client := NewBackendClient(func() string { return server.URL + "/api/v1" }, func() string { return "initialization-fixture-token" }, domain.TeamIdentity{})
	client.httpc = server.Client()
	return client
}

func initializationJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func TestRepositoryInitializationWire(t *testing.T) {
	for _, operation := range []string{"begin", "read", "finalize"} {
		t.Run(operation, func(t *testing.T) {
			repo, receipt := initializationWireFixture()
			anchor := initializationAnchor(repo.ID)
			method, path := http.MethodPost, "/api/v1/repos/"+repo.ID+"/initialization"
			if operation == "read" {
				method = http.MethodGet
			}
			if operation == "finalize" {
				receipt.Anchor = &anchor
				path += "/finalize"
			}
			var calls atomic.Int32
			client := initializationHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != method || r.URL.Path != path || r.URL.RawQuery != "" || r.URL.ForceQuery {
					t.Errorf("unexpected initialization request: %s %s", r.Method, r.URL)
				}
				if operation == "read" {
					body, err := io.ReadAll(r.Body)
					if err != nil || len(body) != 0 {
						t.Errorf("receipt GET has a body: %q %v", body, err)
					}
				} else {
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if operation == "begin" && (len(body) != 3 || body["remote_url"] == nil || body["git_remote_url"] == nil || body["default_branch"] == nil) {
						t.Error("creation body included authority or local configuration")
					}
					if operation == "finalize" && (len(body) != 2 || body["creation_id"] == nil || body["anchor"] == nil) {
						t.Error("finalization body changed")
					}
				}
				initializationJSON(t, w, http.StatusOK, receipt)
			})
			var got domain.RepositoryInitializationReceipt
			var err error
			switch operation {
			case "finalize":
				got, err = client.FinalizeRepositoryInitialization(context.Background(), repo.ID, domain.RepositoryInitializationFinalize{CreationID: receipt.CreationID, Anchor: anchor})
			case "read":
				got, err = client.ReadRepositoryInitialization(context.Background(), repo.ID)
			default:
				repo.LocalPath = "/local/not-for-server"
				got, err = client.BeginRepositoryInitialization(context.Background(), repo)
			}
			if err != nil || calls.Load() != 1 || !reflect.DeepEqual(got, receipt) {
				t.Fatalf("receipt changed: %v calls=%d got=%+v", err, calls.Load(), got)
			}
		})
	}
}

func TestRepositoryInitializationRejectsFalseReceipts(t *testing.T) {
	for _, scenario := range []string{"wrong-repo", "wrong-protocol", "unknown-version", "invalid-receipt", "changed-creation-id", "missing-binding", "local-path", "missing-anchor", "changed-target", "changed-state", "missing-target-state", "modern-unproven-identity"} {
		t.Run(scenario, func(t *testing.T) {
			repo, receipt := initializationWireFixture()
			original := initializationAnchor(repo.ID)
			returned := initializationAnchor(repo.ID)
			receipt.Anchor = &returned
			switch scenario {
			case "wrong-protocol":
				receipt.Repo.ContextProtocol = 0
			case "wrong-repo":
				receipt.Repo.ID = domain.HashContent([]byte("foreign"))
			case "unknown-version":
				receipt.Version++
			case "invalid-receipt":
				receipt.CreationID = "init_" + strings.Repeat("A", 32)
			case "changed-creation-id":
				receipt.CreationID = "init_" + strings.Repeat("b", 32)
			case "missing-binding":
				receipt.Repo.RepositoryID = ""
			case "local-path":
				receipt.Repo.LocalPath = "/other-machine"
			case "missing-anchor":
				receipt.Anchor = nil
			case "changed-target":
				returned.Ref.Target = domain.HashContent([]byte("different target"))
			case "changed-state":
				returned.SnapshotStates[returned.Ref.Target] = domain.HashContent([]byte("new metadata"))
			case "missing-target-state":
				delete(returned.SnapshotStates, returned.Ref.Target)
			case "modern-unproven-identity":
				returned.Ref.BranchID = strings.Repeat("b", 32)
			}
			client := initializationHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
				initializationJSON(t, w, http.StatusOK, receipt)
			})
			got, err := client.FinalizeRepositoryInitialization(context.Background(), repo.ID, domain.RepositoryInitializationFinalize{CreationID: "init_" + strings.Repeat("a", 32), Anchor: original})
			if err == nil || !reflect.DeepEqual(got, domain.RepositoryInitializationReceipt{}) {
				t.Fatal("accepted an unproven receipt", got, err)
			}
		})
	}
}

func TestRepositoryInitializationReadRejectsFalseReceipts(t *testing.T) {
	for _, scenario := range []string{"wrong-repo", "wrong-version", "wrong-protocol", "invalid-creation-id", "missing-binding", "local-path", "invalid-default-branch", "returned-anchor", "wrong-anchor"} {
		t.Run(scenario, func(t *testing.T) {
			repo, receipt := initializationWireFixture()
			switch scenario {
			case "wrong-repo":
				receipt.Repo.ID = domain.HashContent([]byte("foreign"))
			case "wrong-version":
				receipt.Version++
			case "wrong-protocol":
				receipt.Repo.ContextProtocol = 0
			case "invalid-creation-id":
				receipt.CreationID = "init_" + strings.Repeat("A", 32)
			case "missing-binding":
				receipt.Repo.RepositoryID = ""
			case "local-path":
				receipt.Repo.LocalPath = "/other-machine"
			case "invalid-default-branch":
				receipt.Repo.DefaultBranch = "../escape"
			case "returned-anchor":
				anchor := initializationAnchor(repo.ID)
				receipt.Anchor = &anchor
			case "wrong-anchor":
				anchor := initializationAnchor(domain.HashContent([]byte("foreign")))
				receipt.Anchor = &anchor
			}
			var calls atomic.Int32
			client := initializationHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/"+repo.ID+"/initialization" || r.URL.RawQuery != "" {
					t.Error("receipt read escaped its exact repo endpoint", r.Method, r.URL)
				}
				initializationJSON(t, w, http.StatusOK, receipt)
			})
			got, err := client.ReadRepositoryInitialization(context.Background(), repo.ID)
			if err == nil || !reflect.DeepEqual(got, domain.RepositoryInitializationReceipt{}) || calls.Load() != 1 {
				t.Fatal("accepted invalid immutable creation receipt", got, err, calls.Load())
			}
			if scenario == "returned-anchor" && !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatal("even a valid branch anchor must be rejected by creation read", err)
			}
		})
	}
}

func TestRepositoryInitializationFailureDoesNotBroaden(t *testing.T) {
	for _, operation := range []string{"begin", "read", "finalize"} {
		for _, tc := range []struct {
			status int
			code   string
			want   error
		}{
			{401, "unauthorized", nil}, {403, "forbidden", nil}, {500, "internal", nil},
			{409, "conflict", nil},
			{409, "repository_initialization_conflict", domain.ErrRepositoryInitializationConflict},
			{404, "not_found", domain.ErrRepositoryInitializationUnsupported},
			{405, "method_not_allowed", domain.ErrRepositoryInitializationUnsupported},
			{501, "repository_initialization_unsupported", domain.ErrRepositoryInitializationUnsupported},
		} {
			t.Run(fmt.Sprintf("%s/%d-%s", operation, tc.status, tc.code), func(t *testing.T) {
				repo, receipt := initializationWireFixture()
				var calls atomic.Int32
				client := initializationHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					method, path := http.MethodPost, "/api/v1/repos/"+repo.ID+"/initialization"
					if operation == "read" {
						method = http.MethodGet
					}
					if operation == "finalize" {
						path += "/finalize"
					}
					if r.Method != method || r.URL.Path != path || r.URL.RawQuery != "" {
						t.Error("initialization failure caused unexpected request", r.Method, r.URL)
					}
					initializationJSON(t, w, tc.status, map[string]any{"error": map[string]string{"code": tc.code, "message": "synthetic rejection"}})
				})
				var err error
				switch operation {
				case "read":
					_, err = client.ReadRepositoryInitialization(context.Background(), repo.ID)
				case "finalize":
					_, err = client.FinalizeRepositoryInitialization(context.Background(), repo.ID, domain.RepositoryInitializationFinalize{CreationID: receipt.CreationID, Anchor: initializationAnchor(repo.ID)})
				default:
					_, err = client.BeginRepositoryInitialization(context.Background(), repo)
				}
				var typed *HTTPError
				if !errors.As(err, &typed) || typed.Status != tc.status || typed.Code != tc.code || calls.Load() != 1 ||
					errors.Is(err, domain.ErrNotFound) ||
					errors.Is(err, domain.ErrRepositoryInitializationConflict) != (tc.want == domain.ErrRepositoryInitializationConflict) ||
					errors.Is(err, domain.ErrRepositoryInitializationUnsupported) != (tc.want == domain.ErrRepositoryInitializationUnsupported) {
					t.Fatalf("failure classification/fallback changed: %v calls=%d", err, calls.Load())
				}
			})
		}
	}
}

func TestRepositoryInitialAnchorAvailabilityIsRuntimeState(t *testing.T) {
	for _, tc := range []struct {
		name           string
		protocol       int
		available, bad bool
	}{
		{"new-protected", 1, true, false}, {"published", 1, false, false}, {"existing-legacy", 0, false, false}, {"contradictory", 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := initializationWireFixture()
			repo.ContextProtocol = tc.protocol
			client := initializationHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/"+repo.ID || r.URL.RawQuery != "initial_branch=feature%2Fwire" {
					t.Error("state lookup changed transport", r.Method, r.URL)
				}
				initializationJSON(t, w, http.StatusOK, struct {
					domain.Repo
					Available bool `json:"initial_anchor_available"`
				}{repo, tc.available})
			})
			state, err := client.RepositoryInitializationState(context.Background(), repo.ID, "feature/wire")
			if tc.bad {
				if !errors.Is(err, domain.ErrHashMismatch) {
					t.Fatal(err)
				}
				return
			}
			if err != nil || state.InitialAnchorAvailable != tc.available || state.Repo.ContextProtocol != tc.protocol {
				t.Fatal(state, err)
			}
		})
	}
}

func TestRepositoryInitializationBranchQuery(t *testing.T) {
	for _, tc := range []struct{ branch, query string }{
		{"", ""},
		{"feature/topic", "initial_branch=feature%2Ftopic"},
		{"feature/a+b&x=y#z%1", "initial_branch=feature%2Fa%2Bb%26x%3Dy%23z%251"},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			repo, _ := initializationWireFixture()
			var calls atomic.Int32
			client := initializationHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/"+repo.ID || r.URL.RawQuery != tc.query || r.URL.ForceQuery {
					t.Error("incorrect branch query wire shape", r.Method, r.URL)
				}
				query := r.URL.Query()
				if tc.branch == "" {
					if len(query) != 0 {
						t.Error("empty branch sent a query", query)
					}
				} else if len(query) != 1 || len(query["initial_branch"]) != 1 || query.Get("initial_branch") != tc.branch {
					t.Error("canonical branch was changed or injected query fields", query)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || len(body) != 0 {
					t.Error("state GET has a body", string(body), err)
				}
				initializationJSON(t, w, http.StatusOK, repo)
			})
			state, err := client.RepositoryInitializationState(context.Background(), repo.ID, tc.branch)
			if err != nil || !reflect.DeepEqual(state.Repo, repo) || calls.Load() != 1 {
				t.Fatal(state, err, calls.Load())
			}
		})
	}
}

func TestRepositoryInitializationUnscopedRepositoryReads(t *testing.T) {
	for _, operation := range []string{"repository", "capabilities", "protocol"} {
		t.Run(operation, func(t *testing.T) {
			repo, _ := initializationWireFixture()
			var calls atomic.Int32
			client := initializationHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/"+repo.ID || r.URL.RawQuery != "" || r.URL.ForceQuery {
					t.Error("ordinary repository read sent initialization query", r.Method, r.URL)
				}
				initializationJSON(t, w, http.StatusOK, repo)
			})
			var err error
			switch operation {
			case "repository":
				_, err = client.Repository(context.Background(), repo.ID)
			case "capabilities":
				_, err = client.PullCapabilities(context.Background(), repo.ID)
			case "protocol":
				_, err = client.ContextProtocol(context.Background(), repo.ID)
			}
			if err != nil || calls.Load() != 1 {
				t.Fatal(err, calls.Load())
			}
		})
	}
}

func TestRepositoryInitializationRejectsInvalidInputBeforeHTTP(t *testing.T) {
	repo, _ := initializationWireFixture()
	var calls atomic.Int32
	client := initializationHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		initializationJSON(t, w, http.StatusOK, repo)
	})
	for _, branch := range []string{"../escape", "feature//nested", " bad", "bad?injected=true", "bad\nname"} {
		if _, err := client.RepositoryInitializationState(context.Background(), repo.ID, branch); !errors.Is(err, domain.ErrInvalidRef) {
			t.Errorf("invalid nonempty branch %q: %v", branch, err)
		}
	}
	if _, err := client.RepositoryInitializationState(context.Background(), "invalid-repo", "main"); err == nil {
		t.Error("invalid state repo accepted")
	}
	if _, err := client.ReadRepositoryInitialization(context.Background(), "invalid-repo"); err == nil {
		t.Error("invalid receipt repo accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input reached HTTP", calls.Load())
	}
}

func TestRepositoryInitializationMissingStateClassification(t *testing.T) {
	for _, tc := range []struct {
		status  int
		code    string
		missing bool
	}{
		{404, "not_found", true}, {404, "unsupported", false}, {404, "", false}, {403, "forbidden", false}, {401, "unauthorized", false}, {500, "internal", false},
	} {
		t.Run(fmt.Sprintf("%d-%s", tc.status, tc.code), func(t *testing.T) {
			repo, _ := initializationWireFixture()
			var calls atomic.Int32
			client := initializationHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/"+repo.ID || r.URL.RawQuery != "initial_branch=feature%2Fwire" {
					t.Error("lookup changed server state or branch", r.Method, r.URL)
				}
				initializationJSON(t, w, tc.status, map[string]any{"error": map[string]string{"code": tc.code, "message": "synthetic rejection"}})
			})
			_, err := client.RepositoryInitializationState(context.Background(), repo.ID, "feature/wire")
			var httpErr *HTTPError
			if errors.Is(err, domain.ErrNotFound) != tc.missing || errors.Is(err, domain.ErrRepositoryInitializationUnsupported) ||
				!errors.As(err, &httpErr) || httpErr.Status != tc.status || httpErr.Code != tc.code || calls.Load() != 1 {
				t.Fatal(err, calls.Load())
			}
		})
	}
}
