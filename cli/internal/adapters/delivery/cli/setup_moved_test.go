package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

// A clone connected through another address than the server's own keeps the
// workspace it was connected to. When the server names that workspace under
// the address setup is given, the repository moved there and the origin
// follows it; anything else is not proven and changes nothing.
func TestSetupFollowsARepositoryTheServerMoved(t *testing.T) {
	ctx := context.Background()
	const old, moved = "http://localhost:5173/acme/project", "https://cxthub.example/acme/project"
	pinned := t.TempDir()
	writeCapturePushOrigin(t, pinned, old)
	setTestEndpoint(t, pinned, old, "https://cxthub.example/api/v1")
	plain := t.TempDir()
	writeCapturePushOrigin(t, plain, "https://cxthub.example/acme/older")

	same := domain.RepositoryConnection{RepositoryID: "repository-fixture", RepoID: remotecfg.RepoIDFor(moved), RemoteURL: moved}
	reply := same
	var asked []string
	var failure error
	c := &Container{ResolveConnection: func(_ context.Context, requested string) (domain.RepositoryConnection, error) {
		asked = append(asked, requested)
		return reply, failure
	}}
	if got, err := setupOriginMoved(ctx, c, pinned, old, moved+"/"); err != nil || !got || len(asked) != 1 || asked[0] != moved {
		t.Fatalf("the same workspace under the requested address: %v %v asked %v", got, err, asked)
	}

	elsewhere := "https://elsewhere.example/acme/project"
	for name, test := range map[string]struct {
		root, current string
		reply         domain.RepositoryConnection
		failure       error
		err, asks     bool
	}{
		"another workspace under the requested address": {root: pinned, current: old, reply: domain.RepositoryConnection{RepositoryID: "another-repository", RepoID: same.RepoID, RemoteURL: moved}, asks: true},
		"an answer on another server":                   {root: pinned, current: old, reply: domain.RepositoryConnection{RepositoryID: same.RepositoryID, RepoID: remotecfg.RepoIDFor(elsewhere), RemoteURL: elsewhere}, err: true, asks: true},
		"an identifier that is not of the address":      {root: pinned, current: old, reply: domain.RepositoryConnection{RepositoryID: same.RepositoryID, RepoID: remotecfg.RepoIDFor(old), RemoteURL: moved}, err: true, asks: true},
		"an answer without a workspace":                 {root: pinned, current: old, reply: domain.RepositoryConnection{RepoID: same.RepoID, RemoteURL: moved}, err: true, asks: true},
		"a server that does not answer":                 {root: pinned, current: old, reply: same, failure: errors.New("synthetic outage"), err: true, asks: true},
		"a clone that keeps no workspace":               {root: plain, current: "https://cxthub.example/acme/older", reply: same},
	} {
		asked, reply, failure = nil, test.reply, test.failure
		got, err := setupOriginMoved(ctx, c, test.root, test.current, moved)
		if got || (err != nil) != test.err || (len(asked) > 0) != test.asks {
			t.Fatalf("%s: moved %v, %v, asked %v", name, got, err, asked)
		}
	}
	asked, reply, failure = nil, same, nil
	if got, err := setupOriginMoved(ctx, &Container{}, pinned, old, moved); err != nil || got {
		t.Fatalf("without a way to ask the server: %v %v", got, err)
	}
}

// Before the server moved the repository, it answers for the requested
// address with the repository under the address the clone is connected to.
// Setup says so, and what changes it, instead of an invalid identity; any
// other answer it cannot take keeps the error it had.
func TestSetupSaysWhereTheServerKeepsARepositoryItDidNotMove(t *testing.T) {
	ctx := context.Background()
	const old, moved = "http://localhost:5173/acme/project", "https://cxthub.example/acme/project"
	reply := domain.RepositoryConnection{RepositoryID: "repository-fixture", RepoID: remotecfg.RepoIDFor(old), RemoteURL: old}
	var failure error
	c := &Container{ResolveConnection: func(_ context.Context, requested string) (domain.RepositoryConnection, error) {
		if requested != moved {
			t.Fatalf("asked about %s", requested)
		}
		return reply, failure
	}}
	same, err := setupRemoteMatches(ctx, c, old, moved)
	if same || err == nil || !strings.Contains(err.Error(), "the server keeps this repository under "+old+", the address this clone is connected to, not under "+moved+"; nothing was changed") || !strings.Contains(err.Error(), "cxt-admin repo-address") {
		t.Fatalf("a repository the server did not move: %v %v", same, err)
	}
	const other = "http://localhost:5173/acme/other"
	for name, answer := range map[string]domain.RepositoryConnection{
		"another repository on another server":     {RepositoryID: "repository-fixture", RepoID: remotecfg.RepoIDFor(other), RemoteURL: other},
		"this repository without a workspace":      {RepoID: remotecfg.RepoIDFor(old), RemoteURL: old},
		"an identifier that is not of its address": {RepositoryID: "repository-fixture", RepoID: remotecfg.RepoIDFor(old), RemoteURL: other},
	} {
		reply = answer
		if same, err := setupRemoteMatches(ctx, c, old, moved); same || err == nil || !strings.Contains(err.Error(), "invalid identity or a different server") {
			t.Fatalf("%s: %v %v", name, same, err)
		}
	}
	reply, failure = domain.RepositoryConnection{}, errors.New("synthetic outage")
	if same, err := setupRemoteMatches(ctx, c, old, moved); same || err == nil || !strings.Contains(err.Error(), "repository identity could not be verified") {
		t.Fatalf("a server that does not answer: %v %v", same, err)
	}
	// Moved: the requested address is another repository's identity, which
	// the moved origin then takes.
	reply, failure = domain.RepositoryConnection{RepositoryID: "repository-fixture", RepoID: remotecfg.RepoIDFor(moved), RemoteURL: moved}, nil
	if same, err := setupRemoteMatches(ctx, c, old, moved); same || err != nil {
		t.Fatalf("a repository the server moved: %v %v", same, err)
	}
	if same, err := setupRemoteMatches(ctx, &Container{}, old, moved); same || err != nil {
		t.Fatalf("without a way to ask the server: %v %v", same, err)
	}
}

// The origin of the clone becomes the address the server keeps its repository
// under. The identity its conversation documents are sent in is bound to the
// origin: it is confirmed for the new origin, and without that confirmation
// the documents are sent in the legacy identity.
func TestSetupMovesTheOriginWithTheIdentityOfItsDocuments(t *testing.T) {
	ctx := context.Background()
	const old, moved = "http://localhost:5173/acme/project", "https://cxthub.example/acme/project"
	clone := func(identity domain.DocumentIdentity) string {
		t.Helper()
		root := t.TempDir()
		writeCapturePushOrigin(t, root, old)
		setTestEndpoint(t, root, old, "https://cxthub.example/api/v1")
		if identity != domain.DocumentIdentityLegacy {
			observed, err := remotecfg.Observe(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			if err := remotecfg.SetCaptureDocumentIdentity(ctx, observed, identity); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	var registered, confirmed []string
	var unregistered, unconfirmed error
	c := &Container{
		PrepareRemoteConnection: func(_ context.Context, _, raw, _ string) (PreparedRemoteConnection, error) {
			registered = append(registered, raw)
			return PreparedRemoteConnection{URL: raw,
				Connect:       func(context.Context) (inbound.ConnectOutput, error) { return inbound.ConnectOutput{}, nil },
				ValidateLocal: func(context.Context) error { return nil }}, unregistered
		},
		SetCaptureIdentity: func(ctx context.Context, cwd string, identity domain.DocumentIdentity) error {
			confirmed = append(confirmed, string(identity))
			if unconfirmed != nil {
				return unconfirmed
			}
			observed, err := remotecfg.Observe(ctx, cwd)
			if err != nil {
				return err
			}
			return remotecfg.SetCaptureDocumentIdentity(ctx, observed, identity)
		},
	}
	after := func(root string) (string, domain.DocumentIdentity) {
		t.Helper()
		origin, _ := remotecfg.Origin(root)
		identity, err := remotecfg.CaptureIdentity(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		return origin, identity
	}
	root := clone(domain.DocumentIdentityRootV1)
	if err := setupMoveOrigin(ctx, c, root, old, moved); err != nil {
		t.Fatal(err)
	}
	if origin, identity := after(root); origin != moved || identity != domain.DocumentIdentityRootV1 || len(registered) != 1 || registered[0] != moved || len(confirmed) != 1 {
		t.Fatalf("a clone sending root documents: %s %s registered %v confirmed %v", origin, identity, registered, confirmed)
	}

	registered, confirmed, unconfirmed = nil, nil, errors.New("synthetic refusal")
	root = clone(domain.DocumentIdentityRootV1)
	if err := setupMoveOrigin(ctx, c, root, old, moved); err != nil {
		t.Fatal(err)
	}
	if origin, identity := after(root); origin != moved || identity != domain.DocumentIdentityLegacy || len(confirmed) != 1 {
		t.Fatalf("a root identity the server does not confirm: %s %s confirmed %v", origin, identity, confirmed)
	}

	registered, confirmed, unconfirmed = nil, nil, nil
	root = clone(domain.DocumentIdentityLegacy)
	if err := setupMoveOrigin(ctx, c, root, old, moved); err != nil {
		t.Fatal(err)
	}
	if origin, identity := after(root); origin != moved || identity != domain.DocumentIdentityLegacy || len(confirmed) != 0 {
		t.Fatalf("a clone sending legacy documents: %s %s confirmed %v", origin, identity, confirmed)
	}

	unregistered = errors.New("synthetic outage")
	root = clone(domain.DocumentIdentityLegacy)
	if err := setupMoveOrigin(ctx, c, root, old, moved); err == nil || !strings.Contains(err.Error(), "run cxt setup "+moved+" again") {
		t.Fatalf("a registration that fails: %v", err)
	}
	if origin, has := remotecfg.Origin(root); has {
		t.Fatalf("a registration that fails left the origin %s", origin)
	}
}
