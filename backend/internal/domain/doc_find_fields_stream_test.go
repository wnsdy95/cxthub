package domain

import (
	"io"
)

type docFindFieldsTrackingReader struct {
	source     io.Reader
	chunk      int
	maxRequest int
	readCalls  int
	onRead     func()
}

func (reader *docFindFieldsTrackingReader) Read(buffer []byte) (int, error) {
	reader.readCalls++
	reader.maxRequest = max(reader.maxRequest, len(buffer))
	if reader.chunk > 0 {
		buffer = buffer[:min(len(buffer), reader.chunk)]
	}
	count, err := reader.source.Read(buffer)
	if reader.onRead != nil {
		reader.onRead()
	}
	return count, err
}

type docFindFieldsErrorReader struct {
	err error
}

func (reader docFindFieldsErrorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

type docFindFieldsDataErrorReader struct {
	data string
	err  error
}

func (reader *docFindFieldsDataErrorReader) Read(buffer []byte) (int, error) {
	count := copy(buffer, reader.data)
	reader.data = reader.data[count:]
	return count, reader.err
}

type docFindFieldsRepeatedReader struct {
	remaining int
	character byte
}

func (reader *docFindFieldsRepeatedReader) Read(buffer []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	count := min(reader.remaining, len(buffer))
	for position := range count {
		buffer[position] = reader.character
	}
	reader.remaining -= count
	return count, nil
}
