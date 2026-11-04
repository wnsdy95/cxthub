package store

import "github.com/wnsdy95/cxthub/backend/internal/domain"

func validateBreakGlassRead(organization, repository, user, grant string) error {
	for _, err := range []error{domain.ValidateOrganizationID(organization), domain.ValidateRepositoryID(repository), domain.ValidateExternalID(user), domain.ValidateBreakGlassID(grant)} {
		if err != nil {
			return err
		}
	}
	return nil
}
