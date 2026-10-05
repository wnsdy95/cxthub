package nativeclaude

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// These are local limits for one fresh exchange, not native protocol maxima.
const (
	exchangeMetadataRows       = 128
	exchangeMetadataBytes      = 32 << 20
	exchangeMetadataText       = 4096
	exchangeCostModels         = 64
	exchangeCostBytes          = 128 << 10
	exchangeContextValue       = 64 << 10
	exchangeAttachmentBytes    = 2 << 20 // Includes JSON escaping of four values.
	exchangeNativeNumberMax    = 1e15
	exchangeNativeTotalCostMax = 1e9
)

// newExchangeMetadataValidator is scoped to one archive read. It freezes the
// owned input/leaf identities and bounds auxiliary rows independently of the
// transcript chain. Metadata is not evidence of query completion, permission,
// provider capacity, or an empty command queue. The caller binds the full file.
//
// Claude 2.1.287 source: queue recorder zOr (185985900), last-prompt reader
// (190097959), ATIS reader (190099966), and cost schema c3 (185883360).
func newExchangeMetadataValidator(s *Session, q *firstQuestionState, reference ReferenceReceipt, validatedTip *string) func(map[string]json.RawMessage, string) error {
	if s == nil || q == nil || s.id == "" || q.id == "" || reference.MessageID == "" ||
		reference.SessionID != s.id || len(q.archiveMessages) == 0 {
		return func(map[string]json.RawMessage, string) error { return ErrProtocol }
	}
	sid, cwd, questionID, referenceID := s.id, s.cwd, q.id, reference.MessageID
	leaf := q.archiveMessages[len(q.archiveMessages)-1].id
	type input struct {
		id, hash string
		bytes    int
	}
	inputs := []input{
		{referenceID, reference.PayloadHash, reference.UTF8Bytes},
		{referenceID, reference.NativeContentHash, reference.NativeUTF8Bytes},
		{questionID, q.hash, q.bytes},
	}
	rows, totalBytes := 0, 0
	var atisValue, costHash string
	atisSeen, costSeen := false, false
	return func(m map[string]json.RawMessage, kind string) error {
		rows++
		if rows > exchangeMetadataRows || leaf == "" {
			return ErrProtocol
		}
		for key, raw := range m {
			if len(key)+len(raw) > exchangeMetadataBytes-totalBytes {
				return ErrProtocol
			}
			totalBytes += len(key) + len(raw)
		}
		actualKind, err := stringField(m, "type")
		if err != nil || actualKind != kind {
			return ErrProtocol
		}
		owner, err := stringField(m, "sessionId")
		if err != nil || owner != sid {
			return ErrProtocol
		}
		if raw, ok := m["cwd"]; ok {
			value, err := exchangeString(raw, 32<<10)
			if err != nil || value != cwd {
				return ErrProtocol
			}
		}
		switch kind {
		case "queue-operation":
			if !exchangeKeys(m, "type", "sessionId", "cwd", "operation", "timestamp", "content", "commandUuid") {
				return ErrProtocol
			}
			op, err := stringField(m, "operation")
			if err != nil || (op != "enqueue" && op != "dequeue") {
				return ErrProtocol
			}
			stamp, err := stringField(m, "timestamp")
			if err != nil {
				return ErrProtocol
			}
			// zOr uses Date.toISOString(), including exactly three millis digits.
			parsed, err := time.Parse("2006-01-02T15:04:05.000Z", stamp)
			if err != nil || parsed.Format("2006-01-02T15:04:05.000Z") != stamp {
				return ErrProtocol
			}
			command := ""
			if _, ok := m["commandUuid"]; ok {
				command, err = stringField(m, "commandUuid")
				if err != nil || (command != referenceID && command != questionID) {
					return ErrProtocol
				}
			}
			if raw, ok := m["content"]; ok {
				content, err := exchangeString(raw, MaxReferenceBytes+len(nativeReferencePrefix))
				if err != nil {
					return ErrProtocol
				}
				hash, matched := hashText(content), false
				for _, in := range inputs {
					if len(content) == in.bytes && hash == in.hash && (command == "" || command == in.id) {
						matched = true
					}
				}
				if !matched {
					return ErrProtocol
				}
			}
			// remove/reason/deliveryId have other ingress semantics, outside this
			// one-reference/one-question exchange. Native may omit content/UUID.
			return nil
		case "last-prompt":
			if !exchangeKeys(m, "type", "sessionId", "cwd", "lastPrompt", "leafUuid", "explicit", "rewound") {
				return ErrProtocol
			}
			if raw, ok := m["lastPrompt"]; ok {
				if _, err := exchangeString(raw, exchangeMetadataText); err != nil {
					return ErrProtocol
				}
			}
			if _, ok := m["leafUuid"]; ok {
				selected, err := stringField(m, "leafUuid")
				want := leaf
				// The ordinary chain reader validates progressive checkpoints
				// against its current tip, then audits the final selection at EOF.
				if validatedTip != nil {
					want = *validatedTip
				}
				if err != nil || selected != want {
					return ErrProtocol
				}
			}
			if raw, ok := m["explicit"]; ok {
				explicit, err := exchangeBool(raw)
				if err != nil || (explicit && m["leafUuid"] == nil) {
					return ErrProtocol
				}
			}
			if raw, ok := m["rewound"]; ok {
				rewound, err := exchangeBool(raw)
				if err != nil || rewound {
					return ErrProtocol
				}
			}
			return nil
		case "atis-latch":
			if !exchangeKeys(m, "type", "sessionId", "cwd", "atis") {
				return ErrProtocol
			}
			value, err := exchangeString(m["atis"], exchangeMetadataText)
			if err != nil {
				return ErrProtocol
			}
			for i := range value {
				if value[i] < 0x21 || value[i] > 0x7e {
					return ErrProtocol
				}
			}
			if atisSeen && value != atisValue {
				return ErrProtocol
			}
			atisValue, atisSeen = value, true
			return nil
		case "cost-state":
			if err := validateExchangeCost(m); err != nil {
				return err
			}
			raw, err := json.Marshal(m)
			if err != nil {
				return ErrProtocol
			}
			hash, err := archiveContentHash(raw)
			if err != nil || (costSeen && hash != costHash) {
				return ErrProtocol
			}
			costHash, costSeen = hash, true
			return nil
		default:
			return ErrProtocol
		}
	}
}

// validateExchangeAttachment accepts only the initial 2.1.287 announcements.
// The containing archive row must separately bind sessionID and ancestry.
// Context strings can contain instructions: acceptance classifies native-added
// overhead; it does not make that text inert or prove prior token admission.
func validateExchangeAttachment(raw json.RawMessage, sessionID string) (string, error) {
	if sessionID == "" || len(raw) > exchangeAttachmentBytes {
		return "", ErrProtocol
	}
	m, err := object(raw)
	if err != nil {
		return "", ErrProtocol
	}
	kind, err := stringField(m, "type")
	if err != nil {
		return "", ErrProtocol
	}
	switch kind {
	case "session_context":
		if !exchangeKeys(m, "type", "context") {
			return "", ErrProtocol
		}
		values, err := object(m["context"])
		if err != nil || !exchangeKeys(values, "userEmail", "attachedProject", "gitStatus", "perforceMode") {
			return "", ErrProtocol
		}
		for _, value := range values {
			if _, err := exchangeString(value, exchangeContextValue); err != nil {
				return "", ErrProtocol
			}
		}
	case "date":
		if !exchangeKeys(m, "type", "date") {
			return "", ErrProtocol
		}
		date, err := exchangeString(m["date"], 10)
		if err != nil || len(date) != 10 {
			return "", ErrProtocol
		}
		parsed, err := time.Parse("2006-01-02", date)
		if err != nil || parsed.Year() < 1 || parsed.Format("2006-01-02") != date {
			return "", ErrProtocol
		}
	default:
		return "", ErrProtocol
	}
	return kind, nil
}

func validateExchangeCost(m map[string]json.RawMessage) error {
	size := 0
	for key, raw := range m {
		size += len(key) + len(raw)
		if size > exchangeCostBytes {
			return ErrProtocol
		}
	}
	if !exchangeKeys(m, "type", "sessionId", "cwd", "totalCostUSD", "totalAPIDuration", "totalAPIDurationWithoutRetries", "totalToolDuration", "totalLinesAdded", "totalLinesRemoved", "totalDuration", "startTime", "modelUsage", "hasUnknownModelCost") {
		return ErrProtocol
	}
	for _, key := range []string{"totalCostUSD", "totalAPIDuration", "totalAPIDurationWithoutRetries", "totalToolDuration", "totalLinesAdded", "totalLinesRemoved", "totalDuration", "startTime"} {
		maximum := exchangeNativeNumberMax
		if key == "totalCostUSD" {
			maximum = exchangeNativeTotalCostMax
		}
		if _, err := exchangeNumber(m[key], maximum); err != nil {
			return err
		}
	}
	if raw, ok := m["hasUnknownModelCost"]; ok {
		if _, err := exchangeBool(raw); err != nil {
			return err
		}
	}
	models, err := object(m["modelUsage"])
	if err != nil || len(models) > exchangeCostModels {
		return ErrProtocol
	}
	var sums [4]float64
	for model, raw := range models {
		if len(model) == 0 || len(model) > 512 || !utf8.ValidString(model) || strings.IndexFunc(model, func(r rune) bool { return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) }) >= 0 {
			return ErrProtocol
		}
		usage, err := object(raw)
		if err != nil || !exchangeKeys(usage, "inputTokens", "outputTokens", "cacheReadInputTokens", "cacheCreationInputTokens", "webSearchRequests", "costUSD", "thinkingTokens") {
			return ErrProtocol
		}
		for i, key := range []string{"inputTokens", "outputTokens", "cacheReadInputTokens", "cacheCreationInputTokens", "webSearchRequests", "costUSD"} {
			n, err := exchangeNumber(usage[key], exchangeNativeNumberMax)
			if err != nil {
				return err
			}
			if i < len(sums) {
				sums[i] += n
				if sums[i] > exchangeNativeNumberMax {
					return ErrProtocol
				}
			}
		}
		if raw, ok := usage["thinkingTokens"]; ok {
			if _, err := exchangeNumber(raw, exchangeNativeNumberMax); err != nil {
				return err
			}
		}
	}
	return nil
}

func exchangeKeys(m map[string]json.RawMessage, allowed ...string) bool {
	for key := range m {
		found := false
		for _, candidate := range allowed {
			if key == candidate {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func exchangeString(raw json.RawMessage, limit int) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || len(trimmed) > 6*limit+2 || trimmed[0] != '"' || !utf8.Valid(raw) {
		return "", ErrProtocol
	}
	var value string
	if json.Unmarshal(trimmed, &value) != nil || len(value) > limit {
		return "", ErrProtocol
	}
	return value, nil
}

func exchangeBool(raw json.RawMessage) (bool, error) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, ErrProtocol
	}
}

func exchangeNumber(raw json.RawMessage, maximum float64) (float64, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, ErrProtocol
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > maximum {
		return 0, ErrProtocol
	}
	return value, nil
}
