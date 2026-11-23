package domain

type DocDecodedMarkdownCursor struct {
	Version int         `json:"version"`
	Plan    ContentHash `json:"plan"`
}

type DocDecodedMarkdownFragment struct {
	next DocDecodedMarkdownCursor
}

func (fragment DocDecodedMarkdownFragment) Next() DocDecodedMarkdownCursor {
	return fragment.next
}
