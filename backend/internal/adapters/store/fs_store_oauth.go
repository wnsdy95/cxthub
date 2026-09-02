package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.OAuthStore = (*FSStore)(nil)

var fsOAuthLocks sync.Map

func (s *FSStore) oauthLock() *sync.Mutex {
	lock, _ := fsOAuthLocks.LoadOrStore(s.dataDir, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *FSStore) oauthClientsDir() string {
	return filepath.Join(s.dataDir, "oauth", "clients")
}

func (s *FSStore) oauthRequestsDir() string {
	return filepath.Join(s.dataDir, "oauth", "requests")
}

func (s *FSStore) oauthCodesDir() string {
	return filepath.Join(s.dataDir, "oauth", "codes")
}

func oauthRecordPath(dir, id string) string {
	return filepath.Join(dir, opaqueName(id)+".json")
}

func writeExclusiveJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return domain.ErrConflict
		}
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

func pruneExpiredOAuthRequests(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var req domain.OAuthAuthorizationRequest
		if err := readJSON(path, &req); err != nil || domain.ValidateOAuthAuthorizationRequestRecord(req) != nil {
			return domain.ErrIntegrity
		}
		if !now.Before(req.ExpiresAt) {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func pruneExpiredOAuthCodes(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var code domain.OAuthAuthorizationCode
		if err := readJSON(path, &code); err != nil || domain.ValidateOAuthAuthorizationCodeRecord(code) != nil {
			return domain.ErrIntegrity
		}
		if !now.Before(code.ExpiresAt) {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (s *FSStore) CreateOAuthClient(_ context.Context, client domain.OAuthClient) error {
	if err := domain.ValidateOAuthClientRecord(client); err != nil {
		return err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	return writeExclusiveJSON(oauthRecordPath(s.oauthClientsDir(), client.ID), client)
}

func (s *FSStore) GetOAuthClient(_ context.Context, clientID string) (domain.OAuthClient, error) {
	if err := domain.ValidateExternalID(clientID); err != nil {
		return domain.OAuthClient{}, err
	}
	var client domain.OAuthClient
	err := readJSON(oauthRecordPath(s.oauthClientsDir(), clientID), &client)
	if err == nil && (client.ID != clientID || domain.ValidateOAuthClientRecord(client) != nil) {
		return domain.OAuthClient{}, domain.ErrIntegrity
	}
	return client, err
}

func (s *FSStore) CreateOAuthAuthorizationRequest(_ context.Context, req domain.OAuthAuthorizationRequest) error {
	if err := domain.ValidateOAuthAuthorizationRequestRecord(req); err != nil {
		return err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	if err := pruneExpiredOAuthRequests(s.oauthRequestsDir(), time.Now().UTC()); err != nil {
		return err
	}
	return writeExclusiveJSON(oauthRecordPath(s.oauthRequestsDir(), req.ID), req)
}

func (s *FSStore) GetOAuthAuthorizationRequest(_ context.Context, requestID string) (domain.OAuthAuthorizationRequest, error) {
	if err := domain.ValidateExternalID(requestID); err != nil {
		return domain.OAuthAuthorizationRequest{}, err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	path := oauthRecordPath(s.oauthRequestsDir(), requestID)
	var req domain.OAuthAuthorizationRequest
	err := readJSON(path, &req)
	if err == nil && (req.ID != requestID || domain.ValidateOAuthAuthorizationRequestRecord(req) != nil) {
		return domain.OAuthAuthorizationRequest{}, domain.ErrIntegrity
	}
	if err == nil && !time.Now().UTC().Before(req.ExpiresAt) {
		_ = os.Remove(path)
		return domain.OAuthAuthorizationRequest{}, domain.ErrNotFound
	}
	return req, err
}

func (s *FSStore) ApproveOAuthAuthorizationRequest(_ context.Context, requestID, userID, codeHash string, codeExpiresAt time.Time) (domain.OAuthAuthorizationCode, error) {
	if err := domain.ValidateExternalID(userID); err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()

	requestPath := oauthRecordPath(s.oauthRequestsDir(), requestID)
	var req domain.OAuthAuthorizationRequest
	if err := readJSON(requestPath, &req); err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	if req.ID != requestID || domain.ValidateOAuthAuthorizationRequestRecord(req) != nil {
		return domain.OAuthAuthorizationCode{}, domain.ErrIntegrity
	}
	if !time.Now().UTC().Before(req.ExpiresAt) {
		_ = os.Remove(requestPath)
		return domain.OAuthAuthorizationCode{}, domain.ErrUnauthorized
	}
	now := time.Now().UTC()
	if !codeExpiresAt.After(now) {
		return domain.OAuthAuthorizationCode{}, domain.ErrValidation
	}
	code := domain.OAuthAuthorizationCode{
		CodeHash: codeHash, ClientID: req.ClientID, RedirectURI: req.RedirectURI,
		UserID: userID, CodeChallenge: req.CodeChallenge, Resource: req.Resource,
		Scope: req.Scope, CreatedAt: now, ExpiresAt: codeExpiresAt,
	}
	if err := domain.ValidateOAuthAuthorizationCodeRecord(code); err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	if err := pruneExpiredOAuthCodes(s.oauthCodesDir(), now); err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	codePath := oauthRecordPath(s.oauthCodesDir(), codeHash)
	if err := writeExclusiveJSON(codePath, code); err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	if err := os.Remove(requestPath); err != nil {
		_ = os.Remove(codePath)
		return domain.OAuthAuthorizationCode{}, err
	}
	return code, nil
}

func (s *FSStore) DenyOAuthAuthorizationRequest(_ context.Context, requestID string) (domain.OAuthAuthorizationRequest, error) {
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	path := oauthRecordPath(s.oauthRequestsDir(), requestID)
	var req domain.OAuthAuthorizationRequest
	if err := readJSON(path, &req); err != nil {
		return domain.OAuthAuthorizationRequest{}, err
	}
	if req.ID != requestID || domain.ValidateOAuthAuthorizationRequestRecord(req) != nil {
		return domain.OAuthAuthorizationRequest{}, domain.ErrIntegrity
	}
	if !time.Now().UTC().Before(req.ExpiresAt) {
		_ = os.Remove(path)
		return domain.OAuthAuthorizationRequest{}, domain.ErrNotFound
	}
	if err := os.Remove(path); err != nil {
		return domain.OAuthAuthorizationRequest{}, err
	}
	return req, nil
}

func (s *FSStore) ConsumeOAuthAuthorizationCode(_ context.Context, codeHash, clientID, redirectURI, codeChallenge string) (domain.OAuthAuthorizationCode, error) {
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	path := oauthRecordPath(s.oauthCodesDir(), codeHash)
	var code domain.OAuthAuthorizationCode
	if err := readJSON(path, &code); err != nil {
		return domain.OAuthAuthorizationCode{}, domain.ErrUnauthorized
	}
	if domain.ValidateOAuthAuthorizationCodeRecord(code) != nil {
		return domain.OAuthAuthorizationCode{}, domain.ErrIntegrity
	}
	if !time.Now().UTC().Before(code.ExpiresAt) {
		_ = os.Remove(path)
		return domain.OAuthAuthorizationCode{}, domain.ErrUnauthorized
	}
	if code.CodeHash != codeHash || code.ClientID != clientID || code.RedirectURI != redirectURI || code.CodeChallenge != codeChallenge {
		return domain.OAuthAuthorizationCode{}, domain.ErrUnauthorized
	}
	if err := os.Remove(path); err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	return code, nil
}
