package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// SegmentFinding is one thing the diagnosis found: what is so, why it
// matters, and the command that puts it right when there is one. A finding
// with no Fix is something to know, not something to do.
type SegmentFinding struct {
	// Code names the kind of finding and does not change between versions.
	Code string `json:"code"`
	// Problem is false for a finding that only says how things are.
	Problem bool   `json:"problem"`
	What    string `json:"what"`
	Why     string `json:"why,omitempty"`
	Fix     string `json:"fix,omitempty"`
	// Session and Commit name what the finding is about, when it is about one.
	Session *SessionUploadKey `json:"session,omitempty"`
	Commit  string            `json:"commit,omitempty"`
}

// SegmentDiagnosis is the answer of cxt segments doctor. Checked says what
// was looked at, so that an empty list of findings means something.
type SegmentDiagnosis struct {
	Version  int              `json:"version"`
	Checked  []string         `json:"checked"`
	Problems int              `json:"problems"`
	Findings []SegmentFinding `json:"findings"`
}

// AnchoredSessionListed is one of the caller's sessions as the server lists
// it.
type AnchoredSessionListed struct {
	Provider   ProviderKind `json:"provider"`
	SessionID  string       `json:"session_id"`
	Revision   int64        `json:"revision"`
	BodyHash   ContentHash  `json:"body_hash"`
	Records    int          `json:"records"`
	Segments   int          `json:"segments"`
	Shared     bool         `json:"shared"`
	ArchivedAt string       `json:"archived_at,omitempty"`
	Commits    []struct {
		SHA      string   `json:"sha"`
		OnRemote *bool    `json:"on_remote,omitempty"`
		Copies   []string `json:"copies,omitempty"`
	} `json:"commits"`
}

// SegmentDiagnosisServer is what the server said. Sessions holds the
// caller's own sessions only.
type SegmentDiagnosisServer struct {
	Sessions []AnchoredSessionListed
	Removed  []AnchoredSessionRemoval
	// RemoteSyncedAt is when the server last finished reading the repository
	// from GitHub; empty when it never did.
	RemoteSyncedAt string
	// GitHub says whether the server reads the repository on GitHub through
	// the GitHub App.
	GitHub GitHubRepositoryConnection
	// Summarized names, for the commits that were asked about, the sessions
	// that have a summary there: commit -> "tool/session" -> true.
	Summarized map[string]map[string]bool
}

// SegmentDiagnosisInput is every fact the diagnosis looks at. Each one was
// read now: from the tools' records and Git, from the files cxt keeps on this
// computer, and from the server. None is taken from a hook having run.
type SegmentDiagnosisInput struct {
	Now time.Time
	// SetUp says cxt is set up in the repository; AutoOn that the collector
	// is not turned off; Connected that a server is configured.
	SetUp, AutoOn, Connected bool
	// Collector is the collector's memory; nil when it never ran here.
	Collector *CollectorState
	Report    SegmentReport
	Kept      []SessionUploadListing
	// OnOrigin names commits that a remote-tracking branch of origin holds
	// in this clone.
	OnOrigin map[string]bool
	// Server is nil when the server was not read; ServerError then says why.
	Server      *SegmentDiagnosisServer
	ServerError string
}

// segmentDiagnosisListLimit bounds how many sessions one kind of finding
// names one by one; beyond it they are counted.
const segmentDiagnosisListLimit = 10

// collectorBehindAfter is how long after a session's last record the
// collector may still not have run before that is a finding.
const collectorBehindAfter = 10 * time.Minute

// DiagnoseSegments compares what this computer reads now with what cxt keeps
// here and what the server holds, and says each thing that does not fit, with
// the command that puts it right.
func DiagnoseSegments(in SegmentDiagnosisInput) SegmentDiagnosis {
	out := SegmentDiagnosis{Version: 1, Checked: []string{}, Findings: []SegmentFinding{}}
	add := func(finding SegmentFinding) {
		if finding.Problem {
			out.Problems++
		}
		out.Findings = append(out.Findings, finding)
	}
	name := func(key SessionUploadKey) string { return string(key.Provider) + " " + key.SessionID }
	keyOf := func(provider ProviderKind, id string) SessionUploadKey {
		return SessionUploadKey{Provider: provider, SessionID: id}
	}
	short := func(sha string) string {
		if len(sha) > 10 {
			return sha[:10]
		}
		return sha
	}

	// 1. Whether uploads happen at all.
	out.Checked = append(out.Checked, "set-up", "collector")
	if !in.SetUp {
		add(SegmentFinding{Code: "not-set-up", Problem: true, What: "cxt is not set up in this repository",
			Why: "the collector runs only where cxt is set up, and no copy of what is uploaded is kept here", Fix: "cxt setup"})
	}
	if !in.Connected {
		add(SegmentFinding{Code: "not-connected", Problem: true, What: "this repository is not connected to a CXTHub server",
			Why: "nothing can be uploaded", Fix: "cxt setup <server-url>"})
	}
	var lastRun time.Time
	switch {
	case !in.AutoOn:
		add(SegmentFinding{Code: "collector-off", Problem: true, What: "automatic upload is turned off in this repository",
			Why: "sessions are uploaded only by cxt segments push", Fix: "cxt config segments.auto on"})
	case in.Collector == nil || in.Collector.LastRun == nil:
		if in.SetUp && in.Connected {
			add(SegmentFinding{Code: "collector-never-ran", What: "the collector has not run in this repository yet",
				Why: "it starts with the next Git or agent hook, and uploads sessions recorded from then on", Fix: "cxt segments push"})
		}
	default:
		run := in.Collector.LastRun
		lastRun, _ = time.Parse(time.RFC3339Nano, run.At)
		if run.Error != "" {
			fix := "cxt segments push"
			if strings.Contains(run.Error, "401") || strings.Contains(strings.ToLower(run.Error), "login") || strings.Contains(strings.ToLower(run.Error), "unauthorized") {
				fix = "cxt login"
			}
			add(SegmentFinding{Code: "collector-failing", Problem: true, What: fmt.Sprintf("the collector's last run, at %s, failed: %s", run.At, run.Error),
				Why: "nothing is uploaded automatically while it fails; it tries again 10 minutes after a failure", Fix: fix})
		}
	}

	// 2. What could not be read.
	out.Checked = append(out.Checked, "unread-records")
	for _, unread := range in.Report.Unread {
		fix := ""
		switch {
		case strings.Contains(unread.Reason, "sqlite3 program is not installed"):
			fix = "install the sqlite3 program"
		case strings.Contains(unread.Reason, "A newer cxt is needed"):
			fix = "update cxt"
		}
		tool := ""
		if unread.Provider != "" {
			tool = "[" + string(unread.Provider) + "] "
		}
		add(SegmentFinding{Code: "unread-records", Problem: true, What: tool + unread.Source + ": " + unread.Reason,
			Why: "no report covers records that were not read", Fix: fix})
	}

	// 3. Links made by the weaker rule.
	out.Checked = append(out.Checked, "links")
	for _, commit := range in.Report.Commits {
		for _, session := range commit.Sessions {
			if session.Rule == SegmentLinkFolderAndTime {
				key := keyOf(session.Provider, session.ID)
				add(SegmentFinding{Code: "weak-link", Session: &key, Commit: commit.SHA,
					What: fmt.Sprintf("commit %s is linked to %s because a command of that session was running in the same folder when the commit was made, not because the session printed the commit's number", short(commit.SHA), name(key)),
					Why:  "another session working in the same folder at that moment could be the one that made the commit; nothing re-links a commit by hand"})
			}
		}
	}

	local := map[SessionUploadKey]SegmentedSession{}
	for _, session := range in.Report.Sessions {
		local[keyOf(session.Provider, session.ID)] = session
	}
	kept := map[SessionUploadKey]SessionUploadListing{}
	for _, listing := range in.Kept {
		kept[keyOf(listing.Provider, listing.SessionID)] = listing
	}

	// 4. The collector against what the tools recorded since it last ran.
	if !lastRun.IsZero() && in.AutoOn {
		behind := 0
		for _, session := range in.Report.Sessions {
			ended, err := time.Parse(time.RFC3339Nano, session.EndedAt)
			if err == nil && ended.After(lastRun) && in.Now.Sub(ended) > collectorBehindAfter {
				behind++
			}
		}
		if behind > 0 {
			add(SegmentFinding{Code: "collector-behind", Problem: true,
				What: fmt.Sprintf("%d sessions have records newer than the collector's last run (%s), which is more than %s ago", behind, in.Collector.LastRun.At, collectorBehindAfter),
				Why:  "a hook wakes the collector; none has run since, or the collector did not start", Fix: "cxt segments push"})
		}
	}

	// 5. The copies kept on this computer.
	if in.SetUp {
		out.Checked = append(out.Checked, "copies")
		gone := 0
		for key := range kept {
			if _, held := local[key]; !held {
				gone++
			}
		}
		if gone > 0 {
			add(SegmentFinding{Code: "records-gone", What: fmt.Sprintf("the tools no longer hold the records of %d sessions that were uploaded from this computer", gone),
				Why: "their copies under .cxt/sessions are the only ones on this computer; cxt segments resend sends such a copy again if the server lost the session"})
		}
	}

	if in.Server == nil {
		if in.ServerError != "" {
			fix := ""
			lower := strings.ToLower(in.ServerError)
			if strings.Contains(lower, "401") || strings.Contains(lower, "login") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "token") {
				fix = "cxt login"
			}
			add(SegmentFinding{Code: "server-not-read", Problem: true, What: "the server could not be read: " + in.ServerError,
				Why: "nothing on this computer was compared with the server", Fix: fix})
		}
		return out
	}

	// The connection with GitHub, where the server says of it. A server
	// without a GitHub App, or one that does not say, is not looked at.
	if state := in.Server.GitHub.State; state == GitHubRepositoryConnected || state == GitHubRepositoryNotConnected {
		out.Checked = append(out.Checked, "github-connection")
		if state == GitHubRepositoryNotConnected {
			add(SegmentFinding{Code: "github-not-connected", Problem: true, What: "this repository is not connected to GitHub on the server",
				Why: "without the connection the server reads GitHub only with a credential its operator set, if any; a session is shown with its commits only for pushes the server reads",
				Fix: "an owner of the repository connects it at " + in.Server.GitHub.ConnectURL})
		}
	}

	// 6. Each session of this computer against the server.
	out.Checked = append(out.Checked, "server-sessions", "pushed-commits")
	listed := map[SessionUploadKey]AnchoredSessionListed{}
	for _, session := range in.Server.Sessions {
		if !session.Shared {
			listed[keyOf(session.Provider, session.SessionID)] = session
		}
	}
	removed := map[SessionUploadKey]AnchoredSessionRemoval{}
	for _, removal := range in.Server.Removed {
		removed[keyOf(removal.Provider, removal.SessionID)] = removal
	}
	keys := make([]SessionUploadKey, 0, len(local))
	for key := range local {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return name(keys[i]) < name(keys[j]) })
	var absent []SessionUploadKey
	noCopy := 0
	for _, key := range keys {
		session := local[key]
		if !ValidAnchoredSessionID(key.SessionID) {
			continue // said by cxt segments push as refused
		}
		if removal, gone := removed[key]; gone {
			who := "you removed it"
			if !removal.ByYou {
				who = "someone who manages the repository removed it"
			}
			add(SegmentFinding{Code: "removed", Session: ptrKey(key), What: fmt.Sprintf("%s is not on the server: %s on %s", name(key), who, removal.RemovedAt),
				Why: "no upload sends it until it is allowed again", Fix: "cxt segments allow " + name(key)})
			continue
		}
		server, there := listed[key]
		if !there {
			absent = append(absent, key)
			continue
		}
		if server.ArchivedAt != "" {
			add(SegmentFinding{Code: "archived", Session: ptrKey(key), What: fmt.Sprintf("%s is archived on the server since %s", name(key), server.ArchivedAt),
				Why: "an archived session is left out of commits, the search and the summaries, for every reader", Fix: "cxt segments unarchive " + name(key)})
		}
		if server.Records < session.Records {
			add(SegmentFinding{Code: "server-behind", Problem: true, Session: ptrKey(key),
				What: fmt.Sprintf("the server holds %d of the %d records this computer has of %s", server.Records, session.Records, name(key)),
				Why:  "the collector has not sent what was recorded since, or its memory says it did", Fix: "cxt segments push " + name(key)})
		}
		copyOf, copied := kept[key]
		switch {
		case !copied && in.SetUp:
			noCopy++
		case copied:
			if sent := lastServerHash(copyOf.Log); sent != "" && sent != server.BodyHash {
				add(SegmentFinding{Code: "server-differs", Problem: true, Session: ptrKey(key),
					What: fmt.Sprintf("the report of %s on the server (revision %d) is not the one this computer last sent", name(key), server.Revision),
					Why:  "it was uploaded from another computer, or changed on the server since", Fix: "cxt segments push " + name(key)})
			}
		}
		// A commit this clone shows on origin that the server has not seen there.
		for _, commit := range server.Commits {
			seen := (commit.OnRemote != nil && *commit.OnRemote) || len(commit.Copies) > 0
			if in.OnOrigin[commit.SHA] && !seen {
				read := "it never read the repository from GitHub"
				if in.Server.RemoteSyncedAt != "" {
					read = "its last read of GitHub was at " + in.Server.RemoteSyncedAt
				}
				add(SegmentFinding{Code: "push-not-seen", Problem: true, Session: ptrKey(key), Commit: commit.SHA,
					What: fmt.Sprintf("commit %s of %s is on origin here, but the server has not seen it there: %s", short(commit.SHA), name(key), read),
					Why:  "the part of the session that explains the commit opens to teammates only when the server sees the commit on GitHub", Fix: "cxt segments refresh"})
			}
		}
		// A commit of the session that has no summary on the server.
		for _, segment := range session.Segments {
			for _, commit := range segment.Commits {
				sessions, asked := in.Server.Summarized[commit.SHA]
				if asked && !sessions[string(key.Provider)+"/"+key.SessionID] {
					add(SegmentFinding{Code: "summary-missing", Session: ptrKey(key), Commit: commit.SHA,
						What: fmt.Sprintf("%s has no summary on the server at commit %s", name(key), short(commit.SHA)),
						Why:  "a session is given the summaries along its branch; a conversation that says nothing to write down has none", Fix: "cxt segments push " + name(key)})
				}
			}
		}
	}
	if len(in.Server.Summarized) > 0 {
		out.Checked = append(out.Checked, "summaries")
	}
	if len(absent) > segmentDiagnosisListLimit {
		why := "they are uploaded only on request"
		if in.Collector != nil && in.Collector.EnabledAt != "" {
			why = "sessions recorded before automatic reading began here (" + in.Collector.EnabledAt + ") are uploaded only on request"
		}
		add(SegmentFinding{Code: "not-on-server", Problem: true, What: fmt.Sprintf("%d sessions the tools recorded for this repository on this computer are not on the server", len(absent)),
			Why: why, Fix: "cxt segments push"})
	} else {
		for _, key := range absent {
			add(SegmentFinding{Code: "not-on-server", Problem: true, Session: ptrKey(key), What: name(key) + " is not on the server",
				Why: "the collector uploads only sessions recorded since it first ran here, and only while it works", Fix: "cxt segments push " + name(key)})
		}
	}
	if noCopy > 0 {
		add(SegmentFinding{Code: "no-copy", What: fmt.Sprintf("no copy is kept under .cxt/sessions for %d sessions that are on the server", noCopy),
			Why: "they were uploaded before copies were kept, or from another folder", Fix: "cxt segments push"})
	}
	// Sessions the server holds that this computer can only send from a copy.
	for key, listing := range kept {
		if _, held := local[key]; held {
			continue
		}
		if _, there := listed[key]; there {
			continue
		}
		if _, gone := removed[key]; gone || !listing.ReportKept {
			continue
		}
		add(SegmentFinding{Code: "only-in-copy", Problem: true, Session: ptrKey(key),
			What: name(key) + " is neither on the server nor among the tools' records; only the copy under .cxt/sessions is left",
			Why:  "the tool removed its files and the server no longer holds the session", Fix: "cxt segments resend " + name(key)})
	}
	sort.SliceStable(out.Findings, func(i, j int) bool { return out.Findings[i].Problem && !out.Findings[j].Problem })
	return out
}

func ptrKey(key SessionUploadKey) *SessionUploadKey { return &key }

// lastServerHash is the hash the server answered for the report of the
// newest log line that sent one.
func lastServerHash(log []SessionUploadLogEntry) ContentHash {
	for index := len(log) - 1; index >= 0; index-- {
		if log[index].ServerHash != "" {
			return log[index].ServerHash
		}
	}
	return ""
}
