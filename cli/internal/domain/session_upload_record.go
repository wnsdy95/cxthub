package domain

// SessionUploadLogEntry is one line of the log kept on this computer for a
// session: what one run sent for it and what the server answered. A run in
// which nothing was sent and nothing failed leaves no line, and a failure
// that repeats is logged once.
type SessionUploadLogEntry struct {
	At string `json:"at"`
	// Automatic is true for a run of the collector and false for a run a
	// person asked for.
	Automatic bool `json:"automatic,omitempty"`
	// Report is what happened to the report: stored, unchanged or refused.
	Report   SegmentPushState `json:"report,omitempty"`
	Revision int64            `json:"revision,omitempty"`
	// ReportHash is the fingerprint of the report that was sent. It is the
	// fingerprint of report.json as it was after this run.
	ReportHash string `json:"report_hash,omitempty"`
	// ServerHash is the hash the server answered for the report it stored,
	// which is what the server's session list shows. It is absent for a run
	// that sent no report.
	ServerHash ContentHash `json:"server_hash,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	// Content is what happened to the conversation. The report names Parts
	// documents for it, of which Doc is the last, the one that is another
	// when the session goes on; Events is how many events they hold.
	Content       SegmentContentState `json:"content,omitempty"`
	Doc           ContentHash         `json:"doc,omitempty"`
	Parts         int                 `json:"parts,omitempty"`
	Events        int                 `json:"events,omitempty"`
	ContentReason string              `json:"content_reason,omitempty"`
	Summaries     int                 `json:"summaries,omitempty"`
	SummaryReason string              `json:"summary_reason,omitempty"`
	// Action names something other than an upload that was done to the
	// session on the server, such as removed, archived or restored.
	Action string `json:"action,omitempty"`
}

// Eventful reports whether the entry says that something was sent, done or
// failed. An entry that is not eventful is not logged.
func (e SessionUploadLogEntry) Eventful() bool {
	return e.Report == SegmentPushStored || e.Report == SegmentPushRefused || e.Summaries > 0 || e.Action != "" ||
		e.Reason != "" || e.ContentReason != "" || e.SummaryReason != ""
}

// SameOutcome reports whether two entries say the same thing at different
// times.
func (e SessionUploadLogEntry) SameOutcome(other SessionUploadLogEntry) bool {
	e.At, other.At = "", ""
	return e == other
}

// SessionUploadRecord is what is kept on this computer for one session: the
// report and the conversation document as they were last sent, and the log.
// Conversation is nil when no copy of it is kept.
type SessionUploadRecord struct {
	Provider  ProviderKind
	SessionID string
	Report    *AnchoredSessionBody
	// ConversationKept says that a copy of the conversation is kept, whether
	// or not it was read into Conversation.
	ConversationKept bool
	// Parts holds the documents of the conversation in the order the report
	// names them, when they were asked for and every one of them is kept.
	Parts []SessionPartDocument
	Log   []SessionUploadLogEntry
}

// SessionUploadListing is what the records kept on this computer say about
// one session, without the conversation itself.
type SessionUploadListing struct {
	Provider  ProviderKind `json:"provider"`
	SessionID string       `json:"session_id"`
	// ReportKept and ConversationKept say which copies are there.
	ReportKept       bool `json:"report_kept"`
	ConversationKept bool `json:"conversation_kept"`
	// Segments, Records, Doc, Parts and Events are read from the report
	// copy. The conversation is kept in Parts documents; Doc is the last.
	Segments int         `json:"segments"`
	Records  int         `json:"records"`
	Doc      ContentHash `json:"doc,omitempty"`
	Parts    int         `json:"parts,omitempty"`
	Events   int         `json:"events,omitempty"`
	// ReportHash is the fingerprint of the report copy, to compare with the
	// log and with the server.
	ReportHash string `json:"report_hash,omitempty"`
	// Log holds every line when one session was asked for, and only the
	// last line in a list of all sessions.
	Log   []SessionUploadLogEntry `json:"log"`
	Lines int                     `json:"lines"`
}

// SessionUploadListingOf reads a record into a listing. lastOnly keeps only
// the last line of the log.
func SessionUploadListingOf(record SessionUploadRecord, lastOnly bool) SessionUploadListing {
	out := SessionUploadListing{Provider: record.Provider, SessionID: record.SessionID, ReportKept: record.Report != nil,
		ConversationKept: record.ConversationKept, Log: record.Log, Lines: len(record.Log)}
	if out.Log == nil {
		out.Log = []SessionUploadLogEntry{}
	}
	if lastOnly && len(out.Log) > 1 {
		out.Log = out.Log[len(out.Log)-1:]
	}
	if report := record.Report; report != nil {
		out.Segments, out.Records, out.ReportHash = len(report.Segments), report.Records, Fingerprint(*report)
		if report.Content != nil {
			out.Doc, out.Parts, out.Events = report.Content.Last(), len(report.Content.Parts), report.Content.Events
		}
	}
	return out
}

// SessionUploadKey names a session whose record is kept.
type SessionUploadKey struct {
	Provider  ProviderKind `json:"provider"`
	SessionID string       `json:"session_id"`
}

// SegmentRemoveResult is what removing a session from the server removed:
// the session asked for first, then the sessions it spawned.
type SegmentRemoveResult struct {
	Version int                      `json:"version"`
	Removed []AnchoredSessionRemoval `json:"removed"`
}

// SegmentAllowResult is what allowing a removed session again did: the
// removal was taken back and the session was uploaded from the tool's
// records, as far as this computer still holds them.
type SegmentAllowResult struct {
	Version int              `json:"version"`
	Allowed SessionUploadKey `json:"allowed"`
	// Upload is the run that sent the session again. Its NotFound names the
	// session when the tool's records are no longer on this computer.
	Upload SegmentPushResult `json:"upload"`
}

// AnchoredContentPage is a page of a session's conversation as the server
// gives it to this reader: events as JSON, whole where they fit and in parts
// where one alone is larger. NextIndex is the event to go on with and
// NextOffset the byte within it; NextIndex is -1 at the end of what the
// reader may read.
type AnchoredContentPage struct {
	Fragments []struct {
		Index    int    `json:"event_index"`
		Offset   int    `json:"byte_offset"`
		Complete bool   `json:"event_complete"`
		JSON     string `json:"json_fragment"`
	} `json:"fragments"`
	Total      int      `json:"total_events"`
	Open       [][2]int `json:"open_events"`
	NextIndex  int      `json:"next_index"`
	NextOffset int      `json:"next_offset"`
}

// SessionForkResult says what a fork made: a Git branch at a commit, a new
// session of a tool that continues a conversation, or both.
type SessionForkResult struct {
	Version int `json:"version"`
	// Branch is the Git branch that was made at Commit; empty when none was.
	Branch string `json:"branch,omitempty"`
	Commit string `json:"commit,omitempty"`
	// Session is the session whose conversation the new one continues, Tool
	// the tool the new session is for, and ResumeCommand what starts it.
	Session       *SessionUploadKey `json:"session,omitempty"`
	Tool          ProviderKind      `json:"tool,omitempty"`
	ResumeCommand string            `json:"resume_command,omitempty"`
	SessionPath   string            `json:"session_path,omitempty"`
	// Events counts the events the new session holds of the conversation.
	// Omitted counts the events at its beginning that were left out to fit
	// the tool's context, and OmittedAs says what stands in for them:
	// "summary" for the summary the session has at the commit, "note" for a
	// line that says how many were left out. NotReadable counts events up to
	// the commit that this reader may not read.
	Events      int    `json:"events,omitempty"`
	Omitted     int    `json:"omitted_events,omitempty"`
	OmittedAs   string `json:"omitted_as,omitempty"`
	NotReadable int    `json:"not_readable_events,omitempty"`
	// SameTool is true when the new session is of the tool that recorded
	// the conversation, which keeps it as it was; another tool gets a
	// rebuilt conversation.
	SameTool bool `json:"same_tool,omitempty"`
}
