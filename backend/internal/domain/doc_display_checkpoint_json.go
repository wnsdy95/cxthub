package domain

import (
	"encoding/json"
	"unicode/utf8"
)

func marshalDocDisplayProjectionState(state DocDisplayProjectionState) ([]byte, error) {
	if !utf8.ValidString(string(state.Source)) || !docDisplayDirectoryUTF8Valid(state.Directory) {
		return canonicalJSON(state)
	}
	return json.Marshal(struct {
		Directory DocRangeDirectoryAccumulator `json:"directory"`
		Done      bool                         `json:"done"`
		Source    ContentHash                  `json:"source"`
		TextBytes int                          `json:"text_bytes"`
		Version   int                          `json:"version"`
	}{Directory: state.Directory, Done: state.Done, Source: state.Source, TextBytes: state.TextBytes, Version: state.Version})
}

func docDisplayDirectoryUTF8Valid(directory DocRangeDirectoryAccumulator) bool {
	for _, level := range directory.Levels {
		for _, hash := range level {
			if !utf8.ValidString(string(hash)) {
				return false
			}
		}
	}
	return true
}
