package domain

import "unicode/utf8"

const (
	docMarkdownLinePlain byte = iota
	docMarkdownLineBlank
	docMarkdownLineFence
	docMarkdownLineHeading
	docMarkdownLineList
)

const (
	docMarkdownScanLeading byte = iota
	docMarkdownScanTickOne
	docMarkdownScanTickTwo
	docMarkdownScanFence
	docMarkdownScanHashOne
	docMarkdownScanHashTwo
	docMarkdownScanHashThree
	docMarkdownScanHeadingSpace
	docMarkdownScanHeadingBody
	docMarkdownScanDigits
	docMarkdownScanListMarker
	docMarkdownScanListSpace
	docMarkdownScanListBody
	docMarkdownScanPlain
)

type docMarkdownPlanLine struct {
	From        int  `json:"from"`
	To          int  `json:"to"`
	Next        int  `json:"next"`
	ContentFrom int  `json:"content_from"`
	Kind        byte `json:"kind"`
}

type docMarkdownLineScan struct {
	From        int  `json:"from"`
	Next        int  `json:"next"`
	ContentFrom int  `json:"content_from"`
	Stage       byte `json:"stage"`
}

func newDocMarkdownLineScan(from int) docMarkdownLineScan {
	return docMarkdownLineScan{From: from, Next: from, ContentFrom: from}
}

func (scan docMarkdownLineScan) validate(total int) error {
	if total < 0 || total > MaxConversationDocumentBytes || scan.From < 0 || scan.From > scan.Next || scan.Next > total || scan.ContentFrom < scan.From || scan.ContentFrom > scan.Next || scan.Stage > docMarkdownScanPlain {
		return ErrIntegrity
	}
	length := scan.Next - scan.From
	switch scan.Stage {
	case docMarkdownScanLeading:
		if scan.ContentFrom != scan.From {
			return ErrIntegrity
		}
	case docMarkdownScanHashOne, docMarkdownScanHashTwo, docMarkdownScanHashThree:
		if length != int(scan.Stage-docMarkdownScanHashOne)+1 || scan.ContentFrom != scan.From {
			return ErrIntegrity
		}
	case docMarkdownScanHeadingSpace, docMarkdownScanListSpace:
		if length < 2 || scan.ContentFrom != scan.Next {
			return ErrIntegrity
		}
	case docMarkdownScanHeadingBody, docMarkdownScanListBody:
		if scan.ContentFrom-scan.From < 2 || scan.ContentFrom >= scan.Next {
			return ErrIntegrity
		}
	default:
		minimum := 1
		if scan.Stage == docMarkdownScanTickTwo {
			minimum = 2
		} else if scan.Stage == docMarkdownScanFence {
			minimum = 3
		}
		if length < minimum || scan.ContentFrom != scan.From {
			return ErrIntegrity
		}
	}
	return nil
}

func (scan *docMarkdownLineScan) consume(character rune, width int) error {
	if scan.validate(MaxConversationDocumentBytes) != nil || character == '\n' || !utf8.ValidRune(character) || width != utf8.RuneLen(character) || scan.Next > MaxConversationDocumentBytes-width {
		return ErrIntegrity
	}
	next := scan.Next + width
	space := docMarkdownWhitespace(character)
	switch scan.Stage {
	case docMarkdownScanLeading:
		switch {
		case space:
		case character == '`':
			scan.Stage = docMarkdownScanTickOne
		case character == '#' && scan.Next == scan.From:
			scan.Stage = docMarkdownScanHashOne
		case character == '-' || character == '*':
			scan.Stage = docMarkdownScanListMarker
		case character >= '0' && character <= '9':
			scan.Stage = docMarkdownScanDigits
		default:
			scan.Stage = docMarkdownScanPlain
		}
	case docMarkdownScanTickOne, docMarkdownScanTickTwo:
		if character == '`' {
			scan.Stage++
		} else {
			scan.Stage = docMarkdownScanPlain
		}
	case docMarkdownScanHashOne, docMarkdownScanHashTwo, docMarkdownScanHashThree:
		if character == '#' && scan.Stage < docMarkdownScanHashThree {
			scan.Stage++
		} else if space {
			scan.Stage, scan.ContentFrom = docMarkdownScanHeadingSpace, next
		} else {
			scan.Stage = docMarkdownScanPlain
		}
	case docMarkdownScanHeadingSpace:
		if space {
			scan.ContentFrom = next
		} else {
			scan.Stage = docMarkdownScanHeadingBody
		}
	case docMarkdownScanHeadingBody:
		if character == '\r' || character == '\u2028' || character == '\u2029' {
			scan.Stage, scan.ContentFrom = docMarkdownScanPlain, scan.From
		}
	case docMarkdownScanDigits:
		if character == '.' || character == ')' {
			scan.Stage = docMarkdownScanListMarker
		} else if character < '0' || character > '9' {
			scan.Stage = docMarkdownScanPlain
		}
	case docMarkdownScanListMarker:
		if space {
			scan.Stage, scan.ContentFrom = docMarkdownScanListSpace, next
		} else {
			scan.Stage = docMarkdownScanPlain
		}
	case docMarkdownScanListSpace:
		if space {
			scan.ContentFrom = next
		} else {
			scan.Stage = docMarkdownScanListBody
		}
	}
	scan.Next = next
	return nil
}

func (scan docMarkdownLineScan) finish() (docMarkdownPlanLine, error) {
	if err := scan.validate(scan.Next); err != nil {
		return docMarkdownPlanLine{}, err
	}
	line := docMarkdownPlanLine{From: scan.From, To: scan.Next, Next: scan.Next + 1, ContentFrom: scan.From}
	switch scan.Stage {
	case docMarkdownScanLeading:
		line.Kind = docMarkdownLineBlank
	case docMarkdownScanFence:
		line.Kind = docMarkdownLineFence
	case docMarkdownScanHeadingSpace, docMarkdownScanHeadingBody:
		line.Kind, line.ContentFrom = docMarkdownLineHeading, scan.ContentFrom
	case docMarkdownScanListSpace, docMarkdownScanListBody:
		line.Kind, line.ContentFrom = docMarkdownLineList, scan.ContentFrom
	}
	return line, nil
}
