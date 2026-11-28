//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// repoAddressHolders searches the whole database, apart from conversation
// content and records of what happened, for an identifier, and says where it
// is. It is written apart from the move, so that it does not share its rules.
func repoAddressHolders(t *testing.T, db *pgxpool.Pool, id domain.ContentHash) []string {
	t.Helper()
	ctx := context.Background()
	rows, err := db.Query(ctx, `SELECT c.table_name, c.column_name, c.data_type FROM information_schema.columns c
JOIN information_schema.tables t USING (table_schema, table_name)
WHERE c.table_schema='public' AND t.table_type='BASE TABLE'
  AND c.data_type IN ('text','character varying','json','jsonb','bytea','ARRAY')
  AND c.table_name <> 'blobs' AND NOT (c.table_name LIKE 'doc\_%' AND NOT EXISTS (
    SELECT 1 FROM information_schema.columns k WHERE k.table_schema='public' AND k.table_name=c.table_name AND k.column_name='repo_id'))
  AND c.table_name NOT IN ('schema_migrations','ownership_migrations','storage_usage_ledger','account_audit','organization_audit_events','enterprise_audit_events')`)
	if err != nil {
		t.Fatal(err)
	}
	type column struct{ table, name, kind string }
	var columns []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.table, &c.name, &c.kind); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, c)
	}
	rows.Close()
	var holders []string
	for _, c := range columns {
		value := fmt.Sprintf("%q::text", c.name)
		if c.kind == "bytea" {
			value = fmt.Sprintf("encode(%q, 'escape')", c.name)
		}
		var n int64
		if err := db.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %q WHERE strpos(%s, $1) > 0`, c.table, value), string(id)).Scan(&n); err != nil {
			t.Fatalf("%s.%s: %v", c.table, c.name, err)
		}
		if n > 0 {
			holders = append(holders, c.table+"."+c.name)
		}
	}
	return holders
}

// tableRows counts the rows of every table of the database.
func tableRows(t *testing.T, db *pgxpool.Pool) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	rows, err := db.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	rows.Close()
	counts := map[string]int64{}
	for _, name := range names {
		var n int64
		if err := db.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %q`, name)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts[name] = n
	}
	return counts
}

// A repository first registered under a development server's address is given
// the address its workspace is reached at now. Everything it held is held
// under the new identifier, reads and writes go on there, and what was said in
// its conversations and what was recorded stay as they were.
func TestPGRepositoryMovesToTheAddressItsWorkspaceHas(t *testing.T) {
	service, st, _ := collaborationPG(t)
	ctx := systemTestContext()
	db, err := pgxpool.New(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	now := time.Now().UTC()
	suffix := domain.NewID("")[:10]
	owner := domain.User{ID: domain.NewID("u_"), Username: "mover" + suffix, Email: "mover" + suffix + "@example.test", Name: "Mover"}
	member := domain.User{ID: domain.NewID("u_"), Username: "member" + suffix, Email: "member" + suffix + "@example.test", Name: "Member"}
	for _, user := range []domain.User{owner, member} {
		if err := st.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	namespace := domain.Namespace{ID: domain.NewID("ns_"), Slug: owner.Username, Kind: domain.NamespaceUser, UserID: owner.ID, CreatedAt: now}
	if err := st.CreateNamespace(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	workspace := domain.Repository{ID: domain.NewID("ws_"), Name: "App", OwnerID: owner.ID, OwnerUsername: owner.Username, OwnerNamespaceID: namespace.ID, Slug: "app", CreatedAt: now}
	if err := st.CreateRepository(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(ctx, domain.Membership{RepositoryID: workspace.ID, UserID: member.ID, Role: domain.RoleMember, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	oldURL := "http://localhost:5173/" + owner.Username + "/app"
	newURL := "https://cxthub.example/" + owner.Username + "/app"
	oldID, newID := domain.HashContent([]byte(normalizeGitURL(oldURL))), domain.HashContent([]byte(normalizeGitURL(newURL)))
	origin := "https://github.com/acme/app-" + suffix + ".git"
	if _, err := st.PutRepo(ctx, domain.Repo{ID: oldID, RemoteURL: oldURL, RepositoryID: workspace.ID, DefaultBranch: "main", GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	actor := anchoredActor(member.ID)

	// What the repository holds. The conversation quotes the identifier.
	conversation := domain.SessionDoc{CIR: domain.CIRDocument{}}
	conversation.CIR.Envelope.CIRVersion, conversation.CIR.Envelope.SourceProvider = "1", "claude"
	conversation.CIR.Events = []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "cxt remote -v says repo " + string(oldID)}}}}
	raw, err := domain.CanonicalBytes(conversation.CIR)
	if err != nil {
		t.Fatal(err)
	}
	conversation.Hash = domain.HashContent(raw)
	if _, err := st.PutDoc(ctx, oldID, conversation); err != nil {
		t.Fatal(err)
	}
	commit := anchoredTestSHA("c")
	report := domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 1,
		Content: &domain.AnchoredContent{Events: 1, Parts: []domain.AnchoredContentPart{{Doc: conversation.Hash, Events: 1}}},
		Segments: []domain.AnchoredSegment{{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m1", Events: 1},
			Commits: []domain.AnchoredCommit{{SHA: commit, Rule: domain.AnchoredLinkPrintedNumber}}}},
	}
	if _, err := service.PutAnchoredSession(actor, oldID, domain.ProviderClaude, "session-1", report); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutAnchoredSummary(actor, oldID, domain.ProviderClaude, "session-1", commit, domain.AnchoredSummaryBody{Summary: "Moved the parser."}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateAnchoredNote(actor, oldID, domain.AnchoredNoteTarget{Kind: "commit", Commit: commit}, "Look at this one."); err != nil {
		t.Fatal(err)
	}
	ledger := strings.Repeat("ab", 16)
	if _, err := service.RegisterBranchIDs(actor, oldID, []domain.BranchRegistration{{Name: "main", LocalID: ledger}}); err != nil {
		t.Fatal(err)
	}
	if err := service.ReportCollector(actor, oldID, domain.CollectorReport{Machine: "laptop", Pushes: []domain.CollectorPush{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReportWorktrees(actor, oldID, domain.WorktreeReport{Machine: "laptop", Worktrees: []domain.WorktreeReportEntry{{Name: "app", Branch: "main"}}}); err != nil {
		t.Fatal(err)
	}
	settings := domain.SettingsBundle{Kind: "claude", Files: []domain.SettingsFile{{Path: "settings.json", ContentB64: "e30="}}, UpdatedAt: now.Truncate(time.Second)}
	if err := st.PutSettingsBundle(ctx, oldID, settings); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSecretsEnvelope(ctx, oldID, []byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	// The state of the reading of the Git host names its repository inside.
	claimed, err := st.ClaimRemoteGitSync(ctx, oldID, origin, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRemoteGitSync(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	connection := domain.GitHubConnection{NamespaceID: namespace.ID, Installation: domain.GitHubInstallation{ID: now.UnixMicro(), AccountID: 7, Kind: "User", Login: owner.Username}, Enabled: true, Status: "connected",
		Repositories: []domain.GitHubRepository{{ID: 91, AccountID: 7, FullName: "acme/app-" + suffix}},
		Bindings:     []domain.GitHubBinding{{RepositoryID: workspace.ID, ContextRepoID: oldID, ExternalID: 91, GitOrigin: origin}}}
	if err := st.PutGitHubConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	// Records of the capture approach, which reference each other by the
	// repository, and what records what happened.
	organization, err := NewIdentityService(nil, st).CreateOrganization(ctx, owner, "Acme", "acme-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := string(domain.HashContent([]byte("snapshot " + suffix)))
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO snapshots (id, repo_id, branch, doc_hash, provider, fidelity) VALUES ($2, $1, 'main', $3, 'claude', 'full')`, []any{string(oldID), snapshot, string(conversation.Hash)}},
		{`INSERT INTO refs (repo_id, kind, name, target) VALUES ($1, 'branch', 'main', $2)`, []any{string(oldID), snapshot}},
		{`INSERT INTO session_archives (repo_id, key, snapshot_id, provider, session_id, archived_at, archived_by) VALUES ($1, $2, $2, 'claude', 'old', now(), 'someone')`, []any{string(oldID), snapshot}},
		{`INSERT INTO context_history (repo_id, id, event) VALUES ($1, $2, jsonb_build_object('repo', $1::text))`, []any{string(oldID), domain.NewID("")}},
		{`INSERT INTO repository_path_aliases (namespace_key, owner_handle, path, repository_id, context_repo_id) VALUES ($3, $3, 'app', $2, $1)`, []any{string(oldID), workspace.ID, "old-" + suffix}},
		{`INSERT INTO memory_read_object_wakes (hash, after_repo, through_repo) VALUES ($2, $1, $1)`, []any{string(oldID), string(conversation.Hash)}},
		{`INSERT INTO settings_objects (repo_id, hash, data) VALUES ($1, $2, jsonb_build_object('quoted', $1::text))`, []any{string(oldID), snapshot}},
		{`INSERT INTO organization_audit_events (id, organization_id, actor_id, action, target_id) VALUES ($2, $3, $4, 'repository.moved', $1)`, []any{string(oldID), domain.NewID("aud_"), organization.ID, owner.ID}},
	} {
		if _, err := db.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.sql, err)
		}
	}
	// A document whose upload is still being finished.
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "an upload that quotes " + string(oldID)}}}}}
	chunked, _ := domain.CanonicalBytes(cir)
	plan, _ := domain.PlanDocChunks(chunked)
	if _, _, err := st.PutChunks(ctx, oldID, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SubmitDocFinalization(ctx, oldID, inbound.ChunkedDoc{Hash: domain.HashContent(chunked), Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}); err != nil {
		t.Fatal(err)
	}

	// While a document of the repository is being finished, nothing moves.
	if _, err := service.MoveRepoAddress(ctx, oldURL, newURL, false); !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "still being finished") {
		t.Fatalf("a document is still being finished: %v", err)
	}
	// The worker would take the oldest job of any repository, which may be
	// another test's: this repository's job is claimed and finished.
	job, err := st.ClaimDocJob(ctx, oldID, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := runDocJob(ctx, service, st, job); err != nil {
		t.Fatal(err)
	}

	// What is refused. Each attempt only looks, so a refusal that is missing
	// shows as an answer and changes nothing.
	other := domain.Repository{ID: domain.NewID("ws_"), Name: "Other", OwnerID: owner.ID, OwnerUsername: owner.Username, OwnerNamespaceID: namespace.ID, Slug: "other", CreatedAt: now}
	third := domain.Repository{ID: domain.NewID("ws_"), Name: "Third", OwnerID: owner.ID, OwnerUsername: owner.Username, OwnerNamespaceID: namespace.ID, Slug: "third", CreatedAt: now}
	for _, repository := range []domain.Repository{other, third} {
		if err := st.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	thirdURL := "https://cxthub.example/" + owner.Username + "/third"
	thirdID := domain.HashContent([]byte(normalizeGitURL(thirdURL)))
	orphanURL := "http://localhost:5173/" + owner.Username + "/orphan"
	orphanID := domain.HashContent([]byte(normalizeGitURL(orphanURL)))
	for _, repo := range []domain.Repo{{ID: thirdID, RemoteURL: thirdURL, RepositoryID: third.ID, DefaultBranch: "main"}, {ID: orphanID, RemoteURL: orphanURL, DefaultBranch: "main"}} {
		if _, err := st.PutRepo(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	for name, attempt := range map[string]struct {
		from, to string
		want     error
		says     string
	}{
		"an address of no stored repository":  {"http://localhost:5173/" + owner.Username + "/nothing", newURL, domain.ErrNotFound, "no repository is stored"},
		"an address of another workspace":     {oldURL, "https://cxthub.example/" + owner.Username + "/other", domain.ErrConflict, "names another workspace"},
		"an address of no workspace":          {oldURL, "https://cxthub.example/" + owner.Username + "/nothing", domain.ErrConflict, "names no workspace"},
		"a stored repository of no workspace": {orphanURL, "https://cxthub.example/" + owner.Username + "/other", domain.ErrConflict, "belongs to no workspace"},
		"the same address":                    {oldURL, oldURL, domain.ErrValidation, "are the same address"},
		"an address that is no URL":           {oldURL, "not an address", domain.ErrValidation, "must be absolute URLs"},
		"the same address written otherwise":  {oldURL, "HTTP://LOCALHOST:5173/" + owner.Username + "/app/", domain.ErrValidation, "are the same address"},
	} {
		if _, err := service.MoveRepoAddress(ctx, attempt.from, attempt.to, false); !errors.Is(err, attempt.want) || !strings.Contains(err.Error(), attempt.says) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// What the store refuses, whoever asks it.
	for name, attempt := range map[string]struct {
		request domain.RepoAddressMoveRequest
		want    error
	}{
		"the workspace of another repository":           {domain.RepoAddressMoveRequest{From: oldID, To: newID, ToURL: newURL, RepositoryID: other.ID}, domain.ErrConflict},
		"an identifier that is not the address's":       {domain.RepoAddressMoveRequest{From: oldID, To: newID, ToURL: newURL + "-not", RepositoryID: workspace.ID}, domain.ErrValidation},
		"the same identifier":                           {domain.RepoAddressMoveRequest{From: oldID, To: oldID, ToURL: oldURL, RepositoryID: workspace.ID}, domain.ErrValidation},
		"an address another repository is stored under": {domain.RepoAddressMoveRequest{From: oldID, To: thirdID, ToURL: thirdURL, RepositoryID: workspace.ID}, domain.ErrConflict},
	} {
		if _, err := st.MoveRepoAddress(ctx, attempt.request); !errors.Is(err, attempt.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The identifier in a column that cannot be rewritten, a list of names or
	// bytes, stops the move and says where it is.
	if _, err := db.Exec(ctx, `UPDATE snapshots SET models = ARRAY['model ' || $2] WHERE id = $1`, snapshot, string(oldID)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO doc_finalization_jobs (repo_id, id, payload, state, created_at, next_attempt, lease_until) VALUES ($1, $2, convert_to($3, 'UTF8'), 'completed', now(), now(), now())`,
		string(thirdID), "quoting-"+suffix, `{"quoted":"`+string(oldID)+`"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.MoveRepoAddress(ctx, oldURL, newURL, false); !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "snapshots.models (ARRAY, 1 rows)") || !strings.Contains(err.Error(), "doc_finalization_jobs.payload (bytea, 1 rows)") {
		t.Fatalf("the identifier in columns that cannot be rewritten: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE snapshots SET models = '{}' WHERE id = $1`, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM doc_finalization_jobs WHERE repo_id = $1`, string(thirdID)); err != nil {
		t.Fatal(err)
	}
	// The tables' own triggers are switched off only while the move runs.
	switchedOff := func() int64 {
		t.Helper()
		var n int64
		if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgenabled = 'D'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := switchedOff(); n != 0 {
		t.Fatalf("%d triggers are switched off before the move", n)
	}
	before := repoAddressHolders(t, db, oldID)

	// Looked at first: what would change, and nothing changes.
	preview, err := service.MoveRepoAddress(ctx, oldURL, newURL, false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Applied || preview.From != oldID || preview.To != newID || preview.FromURL != oldURL || preview.ToURL != newURL || preview.RepositoryID != workspace.ID || preview.RemovedJobs != 1 {
		t.Fatalf("the preview: %+v", preview)
	}
	changed := map[string]bool{}
	for _, column := range preview.Changed {
		changed[column.Table+"."+column.Column] = column.Rows > 0
	}
	for _, column := range []string{"anchored_sessions.repo_id", "anchored_session_summaries.repo_id", "anchored_notes.repo_id", "branch_lifetimes.repo_id", "repo_blobs.repo_id",
		"repo_settings.repo_id", "repo_secrets.repo_id", "remote_git_state.repo_id", "github_connections.body", "snapshots.repo_id", "refs.repo_id", "session_archives.repo_id",
		"context_history.repo_id", "context_history.event", "repository_path_aliases.context_repo_id", "memory_read_object_wakes.after_repo", "memory_read_object_wakes.through_repo",
		"settings_objects.repo_id", "reported_collectors.repo_id", "reported_worktrees.repo_id"} {
		if !changed[column] {
			t.Errorf("the preview does not change %s", column)
		}
	}
	kept := map[string]bool{}
	for _, column := range preview.Kept {
		kept[column.Table+"."+column.Column] = true
	}
	if !kept["settings_objects.data"] || !kept["organization_audit_events.target_id"] || changed["settings_objects.data"] || changed["organization_audit_events.target_id"] {
		t.Fatalf("what the preview keeps: %+v", preview.Kept)
	}
	if after := repoAddressHolders(t, db, oldID); !reflect.DeepEqual(after, before) {
		t.Fatalf("the preview changed where the identifier is: %v, before %v", after, before)
	}
	if _, err := st.GetRepo(ctx, newID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the preview stored the new address: %v", err)
	}
	if n := switchedOff(); n != 0 {
		t.Fatalf("%d triggers are switched off after the preview", n)
	}

	// The move. It renames and records nothing: apart from the finished
	// document jobs it removes, every table holds as many rows as before.
	counted := tableRows(t, db)
	moved, err := service.MoveRepoAddress(ctx, oldURL, newURL, true)
	if err != nil {
		t.Fatal(err)
	}
	if !moved.Applied || !reflect.DeepEqual(moved.Changed, preview.Changed) || !reflect.DeepEqual(moved.Kept, preview.Kept) || moved.RemovedJobs != 1 {
		t.Fatalf("the move differs from its preview: %+v\npreview %+v", moved, preview)
	}
	counted["doc_finalization_jobs"] -= moved.RemovedJobs
	for table, rows := range tableRows(t, db) {
		if rows != counted[table] {
			t.Errorf("%s holds %d rows after the move and %d before", table, rows, counted[table])
		}
	}
	if left := repoAddressHolders(t, db, oldID); !reflect.DeepEqual(left, []string{"settings_objects.data"}) {
		t.Fatalf("the old identifier is still in %v", left)
	}
	if n := switchedOff(); n != 0 {
		t.Fatalf("%d triggers are switched off after the move", n)
	}
	if _, err := st.GetRepo(ctx, oldID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the old address: %v", err)
	}
	stored, err := st.GetRepo(ctx, newID)
	if err != nil || stored.RemoteURL != newURL || stored.RepositoryID != workspace.ID || stored.GitRemoteURL != origin || stored.DefaultBranch != "main" {
		t.Fatalf("the new address: %+v %v", stored, err)
	}
	// A client resolving the address the workspace has is given it.
	resolved, err := NewIdentityService(nil, st).ResolveRepositoryConnection(ctx, member.ID, newURL)
	if err != nil || resolved.RemoteURL != newURL || resolved.RepoID != newID || resolved.RepositoryID != workspace.ID {
		t.Fatalf("resolving the new address: %+v %v", resolved, err)
	}
	// What it holds reads under the new identifier.
	session, err := service.GetAnchoredSession(actor, newID, domain.ProviderClaude, "session-1")
	if err != nil || session.Body.Content == nil || session.Body.Content.Parts[0].Doc != conversation.Hash {
		t.Fatalf("the session: %+v %v", session, err)
	}
	// The Sessions tab lists it.
	if sessions, err := service.ListAnchoredSessions(actor, newID); err != nil || len(sessions) != 1 || sessions[0].SessionID != "session-1" {
		t.Fatalf("the list of sessions: %+v %v", sessions, err)
	}
	if doc, err := st.GetDoc(ctx, newID, conversation.Hash); err != nil || doc.Hash != conversation.Hash || !strings.Contains(doc.CIR.Events[0].Blocks[0].Text, string(oldID)) {
		t.Fatalf("the conversation, as it was said: %+v %v", doc, err)
	}
	// What is made from a conversation to search it says it as it was said too.
	var quoted int64
	if err := db.QueryRow(ctx, `SELECT count(*) FROM doc_search_events_v2 WHERE strpos(search_text, $1) > 0`, string(oldID)).Scan(&quoted); err != nil || quoted == 0 {
		t.Fatalf("the search of the uploaded conversation: %d %v", quoted, err)
	}
	if summaries, err := service.ListAnchoredSummaries(actor, newID, []string{commit}); err != nil || len(summaries) != 1 {
		t.Fatalf("the summaries: %+v %v", summaries, err)
	}
	if notes, err := service.ListAnchoredNotes(actor, newID); err != nil || len(notes) != 1 {
		t.Fatalf("the notes: %+v %v", notes, err)
	}
	if got, err := st.GetSettingsBundle(ctx, newID, "claude"); err != nil || !reflect.DeepEqual(got.Files, settings.Files) {
		t.Fatalf("the settings: %+v %v", got, err)
	}
	if got, err := st.GetSecretsEnvelope(ctx, newID); err != nil || strings.ReplaceAll(string(got), " ", "") != `{"version":1}` {
		t.Fatalf("the secrets: %s %v", got, err)
	}
	if state, err := st.GetRemoteGitState(ctx, newID); err != nil || state.RepoID != newID {
		t.Fatalf("the state of the reading of the Git host: %+v %v", state, err)
	}
	if bound, err := st.FindGitHubBinding(ctx, newID); err != nil || bound.NamespaceID != namespace.ID || bound.Bindings[0].ContextRepoID != newID {
		t.Fatalf("the GitHub binding: %+v %v", bound, err)
	}
	// It goes on under the new identifier, and nothing is taken under the old one.
	if receipt, err := service.PutAnchoredSession(actor, newID, domain.ProviderClaude, "session-2", report); err != nil || receipt.Revision != 1 || !receipt.Changed {
		t.Fatalf("another session goes up: %+v %v", receipt, err)
	}
	if _, err := service.PutAnchoredSession(actor, oldID, domain.ProviderClaude, "session-3", report); err == nil {
		t.Fatal("a report was taken under the old identifier")
	}
	// It moves once.
	if _, err := service.MoveRepoAddress(ctx, oldURL, newURL, true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("moving it again: %v", err)
	}
}
