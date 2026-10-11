package domain

type GitAdmissionMatch struct {
	RepoID               ContentHash            `json:"repo_id"`
	GitOrigin            string                 `json:"git_origin"`
	Provider             GitProviderObservation `json:"provider"`
	Page                 int                    `json:"page"`
	Version              int64                  `json:"version,string"`
	ThroughPublicationID string                 `json:"through_publication_id"`
	AfterPublicationID   string                 `json:"after_publication_id"`
	AfterDestination     string                 `json:"after_destination"`
}

func (match GitAdmissionMatch) Validate() error {
	if ValidateContentHash(match.RepoID) != nil || !validPRContributionIdentity(match.GitOrigin) || match.Provider.Validate() != nil || match.Page < 1 || match.Version < 0 {
		return ErrValidation
	}
	if match.ThroughPublicationID == "" {
		if match.AfterPublicationID != "" || match.AfterDestination != "" {
			return ErrValidation
		}
		return nil
	}
	if !validAdmissionEventID(match.ThroughPublicationID) || match.Version == 0 {
		return ErrValidation
	}
	if match.AfterPublicationID != "" && (!validAdmissionEventID(match.AfterPublicationID) || match.AfterPublicationID > match.ThroughPublicationID) {
		return ErrValidation
	}
	if match.AfterDestination != "" && (ValidateBranchName(match.AfterDestination) != nil || match.AfterPublicationID == match.ThroughPublicationID) {
		return ErrValidation
	}
	return nil
}
