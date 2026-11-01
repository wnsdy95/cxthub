package domain

import (
	"bytes"
	"strconv"
)

func canonicalMessageEventSequence(version string, raw []byte) (int, bool) {
	const prefix = `{"blocks":[`
	if len(raw) > canonicalCheckBytes || !bytes.HasPrefix(raw, []byte(prefix)) {
		return 0, false
	}
	return canonicalMessageEventBodySequence(version, raw[len(prefix):])
}

func canonicalMessageEventBodySequence(version string, remaining []byte) (int, bool) {
	var text []byte
	if len(remaining) > 0 && remaining[0] == ']' {
		remaining = remaining[1:]
	} else {
		const textPrefix = `{"text":`
		const textSuffix = `,"type":"text"}]`
		if !bytes.HasPrefix(remaining, []byte(textPrefix)) || len(remaining) == len(textPrefix) || remaining[len(textPrefix)] != '"' {
			return 0, false
		}
		text, remaining = docRangeJSONValue(remaining[len(textPrefix):])
		if len(text) == 0 || text[0] != '"' || !bytes.HasPrefix(remaining, []byte(textSuffix)) {
			return 0, false
		}
		remaining = remaining[len(textSuffix):]
	}
	const message = `,"kind":"message","role":`
	if !bytes.HasPrefix(remaining, []byte(message)) {
		return 0, false
	}
	roleBody := remaining[len(message):]
	if len(roleBody) == 0 || roleBody[0] != '"' {
		return 0, false
	}
	role, remaining := docRangeJSONValue(roleBody)
	var speaker Role
	switch string(role) {
	case `"user"`:
		speaker = RoleUser
	case `"assistant"`:
		speaker = RoleAssistant
	case `"system"`:
		speaker = RoleSystem
	case `"developer"`:
		speaker = RoleDeveloper
	default:
		return 0, false
	}
	const sequencePrefix = `,"seq":`
	if !bytes.HasPrefix(remaining, []byte(sequencePrefix)) || remaining[len(remaining)-1] != '}' {
		return 0, false
	}
	number := remaining[len(sequencePrefix) : len(remaining)-1]
	sequence, err := strconv.Atoi(string(number))
	if err != nil || !bytes.Equal(number, []byte(strconv.Itoa(sequence))) {
		return 0, false
	}
	event := CIREvent{Kind: EventMessage, Role: speaker, Seq: sequence, Blocks: []ContentBlock{}}
	if text != nil {
		decoded, valid := canonicalStringValue(text)
		if !valid {
			return 0, false
		}
		event.Blocks = []ContentBlock{{Type: "text", Text: decoded}}
	}
	if ValidateCIRVersion(CIRDocument{Envelope: CIREnvelope{CIRVersion: version}, Events: []CIREvent{event}}) != nil {
		return 0, false
	}
	return sequence, true
}
