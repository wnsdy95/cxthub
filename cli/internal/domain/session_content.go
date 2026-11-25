package domain

import "errors"

// ErrSessionContentNotAccepted means this one session's conversation could
// not be stored as a document: the server does not take chunked documents, or
// the document is larger than one manifest may describe. Other sessions are
// not affected.
var ErrSessionContentNotAccepted = errors.New("session content was not accepted as a document")
