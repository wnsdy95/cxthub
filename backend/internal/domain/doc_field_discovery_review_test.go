package domain_test

import ()

type docFieldReviewDeferredEOFReader struct {
	data       []byte
	emptyReads int
	terminal   error
	probes     int
}

func (reader *docFieldReviewDeferredEOFReader) Read(buffer []byte) (int, error) {
	if len(reader.data) > 0 {
		count := copy(buffer, reader.data)
		reader.data = reader.data[count:]
		return count, nil
	}
	reader.probes++
	if reader.emptyReads > 0 {
		reader.emptyReads--
		return 0, nil
	}
	return 0, reader.terminal
}
