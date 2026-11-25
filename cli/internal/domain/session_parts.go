package domain

import (
	"crypto/sha256"
	"encoding/hex"
)

// A session's conversation is kept in several documents, the parts, so that a
// session that goes on costs what was added and not the whole of it.
//
// The session's own records are taken in order. A part is sealed at the
// record where the bytes of its records, as the tool keeps them, reach
// SessionPartBytes; what follows the last sealed part is the open part. A
// sealed part never changes: its records are the same records, and records
// never change once written. The open part is built again on every run.
//
// Where the parts are cut depends on nothing but the records and their sizes,
// so cutting only what follows a sealed part gives the cuts that cutting the
// whole session gives. That is what lets a run go on from what an earlier run
// remembered and still send the documents a run from the start would send.

// SessionPartBytes is where a part is sealed.
const SessionPartBytes = 4 << 20

// CutSessionParts says how many records each part holds, given the size of
// each record in order. sealed says whether the last part reached the limit;
// when it did not, the last part is the open one. No records give no part.
func CutSessionParts(sizes []int, limit int) (records []int, sealed bool) {
	held, bytes := 0, 0
	for _, size := range sizes {
		held, bytes = held+1, bytes+size
		if bytes >= limit {
			records = append(records, held)
			held, bytes = 0, 0
		}
	}
	if held > 0 {
		return append(records, held), false
	}
	return records, len(records) > 0
}

// SessionRecordChain names an ordered run of records together with every
// record before it: previous is the chain up to where the run begins, empty
// at the start of the session.
func SessionRecordChain(previous string, ids []string) string {
	hash := sha256.New()
	hash.Write([]byte(previous))
	for _, id := range ids {
		hash.Write([]byte{0})
		hash.Write([]byte(id))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// SessionPartResume is where the conversion of a session's records goes on:
// what the records before it left behind. Token is the reader's own note of
// where the tool keeps the records that follow; empty reads from the start.
type SessionPartResume struct {
	Envelope Envelope `json:"envelope"`
	Events   int      `json:"events"`
	Token    string   `json:"token,omitempty"`
}

// SessionPartRead is one part as a reader converted it: how many events each
// of its records gave, the events themselves, and where conversion stands
// after it.
type SessionPartRead struct {
	Counts   []int
	Events   []Event
	Envelope Envelope
	Sealed   bool
	Token    string
}

// Records is how many records the part holds: it has a count for each.
func (p SessionPartRead) Records() int { return len(p.Counts) }

// SessionPartsRead is what a reader returns for the records it was asked
// for: the parts in order, and the records that are no longer where the tool
// keeps them. With a missing record nothing of the read is used.
type SessionPartsRead struct {
	Parts   []SessionPartRead
	Missing []string
}

// SessionPartState is a sealed part as a run remembers it. Chain names its
// records and all before them; Counts holds the events of each of its
// records; Part is the document that holds them, with no document when the
// records gave no event; Resume is where conversion goes on after it.
type SessionPartState struct {
	Chain  string              `json:"chain"`
	Counts []int               `json:"counts"`
	Part   AnchoredContentPart `json:"part"`
	Resume SessionPartResume   `json:"resume"`
}

// Records is how many records the part holds: it has a count for each.
func (p SessionPartState) Records() int { return len(p.Counts) }

// KeptSessionParts returns how many of the remembered sealed parts still
// hold for the session's records as they are now: the leading parts whose
// records are the same records in the same order. A part that does not hold
// ends the list.
func KeptSessionParts(remembered []SessionPartState, ids []string) int {
	chain, at := "", 0
	for index, part := range remembered {
		if part.Records() > len(ids)-at {
			return index
		}
		chain = SessionRecordChain(chain, ids[at:at+part.Records()])
		if chain != part.Chain {
			return index
		}
		at += part.Records()
	}
	return len(remembered)
}

// SessionRangeEvents places each range of records in the conversation:
// where it starts and how many events it holds. counts gives the events of
// each record in order; the ranges are runs of those records, one after
// another. ok is false when the ranges do not hold exactly the records.
func SessionRangeEvents(ranges [][]string, counts []int) (first, count []int, ok bool) {
	first, count = make([]int, len(ranges)), make([]int, len(ranges))
	at, events := 0, 0
	for index, ids := range ranges {
		if len(ids) > len(counts)-at {
			return nil, nil, false
		}
		first[index] = events
		for _, held := range counts[at : at+len(ids)] {
			count[index] += held
		}
		at, events = at+len(ids), events+count[index]
	}
	return first, count, at == len(counts)
}

// Document is the part as the document that is uploaded. Its envelope names
// no folder and no branch: where a session worked is the report's to say, by
// worktree name and with the rule that read it; a full path stays on this
// computer.
func (p SessionPartRead) Document() CIRDocument {
	doc := CIRDocument{Envelope: p.Envelope, Events: p.Events}
	doc.Envelope.Cwd, doc.Envelope.GitBranch = "", ""
	return doc
}

// SessionPartsConversation joins parts read from the start of a session into
// its whole conversation, with the envelope the last part left.
func SessionPartsConversation(parts []SessionPartRead) CIRDocument {
	doc := CIRDocument{}
	for _, part := range parts {
		doc.Envelope = part.Document().Envelope
		doc.Events = append(doc.Events, part.Events...)
	}
	return doc
}

// SessionPartDocument is one document of a conversation and what the server
// knows it by.
type SessionPartDocument struct {
	Doc      ContentHash
	Document CIRDocument
}

// SessionDocumentsConversation joins the documents of a conversation, in the
// order of its parts, into the whole conversation.
func SessionDocumentsConversation(parts []SessionPartDocument) CIRDocument {
	doc := CIRDocument{}
	for _, part := range parts {
		doc.Envelope = part.Document.Envelope
		doc.Events = append(doc.Events, part.Document.Events...)
	}
	return doc
}
