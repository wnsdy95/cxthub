package documentquery

import (
	"io"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docFindEventReader struct {
	source docViewSource
	entry  domain.DocEventIndex
	next   int
}

func (reader *docFindEventReader) Read(target []byte) (int, error) {
	if len(target) == 0 {
		return 0, nil
	}
	if reader.next == reader.entry.Length {
		return 0, io.EOF
	}
	length := min(len(target), docViewBytes, reader.entry.Length-reader.next)
	raw, err := reader.source.read(reader.entry.Offset+reader.next, length)
	if err != nil {
		return 0, err
	}
	if len(raw) != length {
		return 0, domain.ErrIntegrity
	}
	reader.next += copy(target, raw)
	return length, nil
}
