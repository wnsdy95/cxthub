package domain

type GitPublicationWitnessQuery struct {
	Publication HistoryEvent
	GitHead     string
	IDs         []string
}

func (query GitPublicationWitnessQuery) Validate() error {
	if query.Publication.Kind != "publish" || ValidateHistoryEvent(query.Publication) != nil || ValidateGitOID(query.GitHead) != nil || len(query.IDs) > MaxGitAdmissionWitnesses {
		return ErrValidation
	}
	for index, id := range query.IDs {
		if !validAdmissionEventID(id) || id == query.Publication.ID || (index > 0 && query.IDs[index-1] >= id) {
			return ErrValidation
		}
	}
	return nil
}

func (query GitPublicationWitnessQuery) Matches(event HistoryEvent) bool {
	if query.Validate() != nil || ValidateHistoryEvent(event) != nil {
		return false
	}
	switch event.Kind {
	case "birth", "attach", "position", "advance":
		key := publicationKey(query.Publication)
		key.git = query.GitHead
		return key == publicationKey(event)
	default:
		return false
	}
}
