package http

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net/http"
	"net/url"
	"strings"
)

const documentIdentitiesHeader = "X-Cxt-Doc-Identities"

func (s *Server) withDocumentIdentityPeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(documentIdentitiesHeader)
		if len(values) == 0 {
			next.ServeHTTP(w, r.WithContext(inbound.WithDocumentIdentities(r.Context(), nil)))
			return
		}
		if !validDocumentIdentityDeclaration(values) {
			s.writeError(w, 400, "bad_request", "invalid document identity declaration")
			return
		}
		next.ServeHTTP(w, r.WithContext(inbound.WithDocumentIdentities(r.Context(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})))
	})
}
func (s *Server) documentIdentityCapabilities() ([]domain.DocumentIdentity, bool) {
	if cap, ok := s.b.(inbound.DocumentIdentityCapabilities); ok {
		return cap.DocumentIdentitiesSupported(), cap.RootPublicationEnabled()
	}
	return []domain.DocumentIdentity{domain.DocumentIdentityLegacy}, false
}

func validDocumentIdentityDeclaration(values []string) bool {
	return len(values) == 1 && len(values[0]) <= 256 && strings.TrimSpace(values[0]) == string(domain.DocumentIdentityRootV1)
}

// Only the EventSource route accepts the query declaration. A peer cannot
// authenticate itself or acquire compatibility on other routes with this URL.
func (s *Server) withDocumentIdentityStream(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			s.writeError(w, 400, "bad_request", "invalid stream query")
			return
		}
		if values, present := query["doc_identities"]; present {
			if len(r.Header.Values(documentIdentitiesHeader)) != 0 || !validDocumentIdentityDeclaration(values) {
				s.writeError(w, 400, "bad_request", "invalid stream document identity declaration")
				return
			}
			r = r.WithContext(inbound.WithDocumentIdentities(r.Context(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1}))
		}
		next(w, r)
	}
}

func documentIdentitySupported(ids []domain.DocumentIdentity, required domain.DocumentIdentity) bool {
	if required == domain.DocumentIdentityLegacy {
		return true
	}
	for _, id := range ids {
		if id == required {
			return true
		}
	}
	return false
}
