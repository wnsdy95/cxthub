package domain

import "errors"

// ErrSessionContentNotAccepted means this one session's conversation could
// not be stored as a document: the server does not take chunked documents, or
// the document is larger than one manifest may describe. Other sessions are
// not affected.
var ErrSessionContentNotAccepted = errors.New("session content was not accepted as a document")

// SessionContent is the conversation behind a session's own records: one list
// of events for each range of SessionRecordRanges, in the same order. A record
// may give no event, one or several.
type SessionContent struct {
	Envelope Envelope
	Ranges   [][]Event
	// Missing names records that are no longer where the tool keeps them.
	// Content with a missing record is incomplete and is not uploaded.
	Missing []string
}

// Document joins the ranges into one document and returns where each range
// starts in it and how many events it holds.
//
// The document's envelope names no folder and no branch. Where a session
// worked is the report's to say, by worktree name and with the rule that read
// it; a full path stays on this computer.
func (c SessionContent) Document() (CIRDocument, []int, []int) {
	doc := CIRDocument{Envelope: c.Envelope}
	doc.Envelope.Cwd, doc.Envelope.GitBranch = "", ""
	first, count := make([]int, len(c.Ranges)), make([]int, len(c.Ranges))
	for index, events := range c.Ranges {
		first[index], count[index] = len(doc.Events), len(events)
		doc.Events = append(doc.Events, events...)
	}
	return doc, first, count
}
