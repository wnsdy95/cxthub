package domain

type DocSummaryMarkdownCursor struct {
	Version int         `json:"version"`
	Plan    ContentHash `json:"plan"`
}

type DocSummaryMarkdownFragment struct {
	next DocSummaryMarkdownCursor
}

func (fragment DocSummaryMarkdownFragment) Next() DocSummaryMarkdownCursor {
	return fragment.next
}
