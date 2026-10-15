package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const graphProjectionExpandedBytes = 32 << 20

var errGraphProjectionCacheLimit = errors.New("graph projection exceeds cache budget")

type graphProjectionWriter struct {
	ctx       context.Context
	writer    io.Writer
	remaining int
}

func (writer *graphProjectionWriter) Write(body []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	if len(body) > writer.remaining {
		return 0, errGraphProjectionCacheLimit
	}
	written, err := writer.writer.Write(body)
	writer.remaining -= written
	return written, err
}

type graphProjectionReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader graphProjectionReader) Read(body []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(body)
}

func encodeGraphProjection(ctx context.Context, graph domain.GraphState) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	compressed := &graphProjectionWriter{ctx: ctx, writer: &buffer, remaining: graphProjectionCacheBytes}
	writer, err := gzip.NewWriterLevel(compressed, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	expanded := &graphProjectionWriter{ctx: ctx, writer: writer, remaining: graphProjectionExpandedBytes}
	err = json.NewEncoder(expanded).Encode(graph)
	closeErr := writer.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return bytes.Clone(buffer.Bytes()), ctx.Err()
}

func decodeGraphProjection(ctx context.Context, encoded []byte) (domain.GraphState, error) {
	if err := ctx.Err(); err != nil {
		return domain.GraphState{}, err
	}
	if len(encoded) > graphProjectionCacheBytes {
		return domain.GraphState{}, errGraphProjectionCacheLimit
	}
	reader, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		return domain.GraphState{}, errors.Join(domain.ErrIntegrity, err)
	}
	defer reader.Close()
	bounded := &io.LimitedReader{R: graphProjectionReader{ctx: ctx, reader: reader}, N: graphProjectionExpandedBytes + 1}
	decoder := json.NewDecoder(bounded)
	var graph domain.GraphState
	err = decoder.Decode(&graph)
	if err == nil {
		var extra any
		if tailErr := decoder.Decode(&extra); !errors.Is(tailErr, io.EOF) {
			err = errors.Join(domain.ErrIntegrity, tailErr)
		}
	}
	if ctx.Err() != nil {
		return domain.GraphState{}, ctx.Err()
	}
	if bounded.N == 0 {
		return domain.GraphState{}, errGraphProjectionCacheLimit
	}
	if err != nil {
		return domain.GraphState{}, errors.Join(domain.ErrIntegrity, err)
	}
	return graph, nil
}
