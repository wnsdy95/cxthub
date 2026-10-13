package domain

import (
	"bytes"
	"encoding/json"
)

type singleDeliveryAcknowledgement struct {
	value bool
	seen  bool
}

func (field *singleDeliveryAcknowledgement) UnmarshalJSON(raw []byte) error {
	if field.seen || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ErrHashMismatch
	}
	field.seen = true
	return json.Unmarshal(raw, &field.value)
}

func (merge *BranchContextMerge) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ErrHashMismatch
	}
	type wire BranchContextMerge
	var decoded wire
	input := struct {
		*wire
		Acknowledgement singleDeliveryAcknowledgement `json:"delivery_acknowledged"`
	}{wire: &decoded}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	decoded.DeliveryAcknowledged = input.Acknowledgement.value
	decoded.deliveryAcknowledgementAbsent = !input.Acknowledgement.seen
	*merge = BranchContextMerge(decoded)
	return nil
}

func (merge BranchContextMerge) MarshalJSON() ([]byte, error) {
	type wire BranchContextMerge
	if !merge.legacyReceiptEncoding || merge.DeliveryAcknowledged {
		return json.Marshal(wire(merge))
	}
	return json.Marshal(struct {
		Acknowledgement *bool `json:"delivery_acknowledged,omitempty"`
		wire
	}{wire: wire(merge)})
}

func (merge *BranchContextMerge) PreserveLegacyReceiptEncoding() bool {
	merge.legacyReceiptEncoding = merge.deliveryAcknowledgementAbsent && !merge.DeliveryAcknowledged
	return merge.legacyReceiptEncoding
}
