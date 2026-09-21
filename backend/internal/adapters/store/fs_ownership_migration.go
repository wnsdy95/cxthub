package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type ownershipFileChange struct {
	Path   string          `json:"path"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after"`
}
type ownershipFileJournal struct {
	Version string                `json:"version"`
	Changes []ownershipFileChange `json:"changes"`
	Targets map[string][]string   `json:"targets"`
}

// Only metadata keys change. Never replace words inside conversations, secrets,
// audit descriptions, or free-form values. This is the explicit legacy decoder.
func decodeLegacyOwnership(data []byte) ([]byte, error) {
	if !json.Valid(data) {
		return nil, domain.ErrIntegrity
	}
	var raw any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, err
	}
	var convert func(any) error
	convert = func(v any) error {
		switch value := v.(type) {
		case map[string]any:
			for key, item := range value {
				next := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(key, "workspaces", "repositories"), "workspace", "repository"), "enterprise_id", "organization_id")
				if next != key {
					if _, exists := value[next]; exists {
						return domain.ErrIntegrity
					}
					delete(value, key)
					value[next] = item
				}
				if err := convert(item); err != nil {
					return err
				}
			}
			if value["kind"] == "enterprise" && value["organization_id"] != nil {
				value["kind"] = "organization"
			}
		case []any:
			for _, item := range value {
				if err := convert(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := convert(raw); err != nil {
		return nil, err
	}
	return json.Marshal(raw)
}

func (s *FSStore) migrateOwnership() error {
	journalPath := filepath.Join(s.dataDir, "ownership-migration-v1.json")
	donePath := filepath.Join(s.dataDir, "ownership-migration-v1.complete.json")
	if _, err := os.Stat(donePath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var journal ownershipFileJournal
	if err := readJSON(journalPath, &journal); err == nil {
		return s.applyOwnershipJournal(journal, journalPath, donePath)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	legacy := false
	for _, dir := range []string{"workspaces", "enterprises"} {
		if _, err := os.Stat(filepath.Join(s.dataDir, dir)); err == nil {
			legacy = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !legacy {
		return nil
	}
	changes := map[string]json.RawMessage{}
	stage := func(path string, value any) error {
		data, err := json.Marshal(value)
		if err == nil {
			changes[path] = data
		}
		return err
	}
	var boundaries []domain.Repository
	var repos []domain.Repo
	var members []domain.Membership
	membersByKey := map[string]domain.Membership{}
	opaqueMember := map[string]bool{}
	var invites []domain.Invite
	var grants []domain.BreakGlassGrant
	folders := map[string]string{
		"workspaces": "repositories", "enterprises": "organizations",
		"enterprise-members": "organization-members", "enterprise-policies": "organization-policies",
		"enterprise-audit": "organization-audit", "enterprise-break-glass": "organization-break-glass",
		"members": "members", "invites": "invites", "namespaces": "namespaces", "notification-outbox": "notification-outbox",
	}
	for source, destination := range folders {
		base := filepath.Join(s.dataDir, source)
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
			if errors.Is(walkErr, os.ErrNotExist) && path == base {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: symlink in ownership metadata", domain.ErrIntegrity)
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				return nil
			}
			data, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			converted, e := decodeLegacyOwnership(data)
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(base, path)
			if e != nil {
				return e
			}
			switch source {
			case "workspaces":
				var v domain.Repository
				if e = json.Unmarshal(converted, &v); e != nil {
					return e
				}
				if v.ID+".json" != rel {
					return domain.ErrIntegrity
				}
				boundaries = append(boundaries, v)
			case "members":
				var v domain.Membership
				if e = json.Unmarshal(converted, &v); e != nil {
					return e
				}
				if filepath.Dir(rel) != v.RepositoryID || domain.ValidateMembershipRecord(v) != nil {
					return domain.ErrIntegrity
				}
				isOpaque := filepath.Base(rel) == opaqueName(v.UserID)+".json"
				if !isOpaque && filepath.Base(rel) != safeName(v.UserID)+".json" {
					return domain.ErrIntegrity
				}
				key := v.RepositoryID + "/" + v.UserID
				if !opaqueMember[key] || isOpaque {
					membersByKey[key] = v
					opaqueMember[key] = isOpaque
				}
			case "invites":
				var v domain.Invite
				if e = json.Unmarshal(converted, &v); e != nil {
					return e
				}
				if v.Token+".json" != rel {
					return domain.ErrIntegrity
				}
				invites = append(invites, v)
			case "enterprise-break-glass":
				var v domain.BreakGlassGrant
				if e = json.Unmarshal(converted, &v); e != nil {
					return e
				}
				if e = domain.ValidateBreakGlassGrant(v); e != nil {
					return e
				}
				grants = append(grants, v)
			default:
				if e = validateLegacyOrganizationMetadata(source, rel, converted); e != nil {
					return e
				}
				changes[filepath.Join(destination, rel)] = converted
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("read legacy %s: %w", source, err)
		}
	}
	paths, err := filepath.Glob(filepath.Join(s.dataDir, "repos", "*", "repo.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		converted, e := decodeLegacyOwnership(data)
		if e != nil {
			return e
		}
		var r domain.Repo
		if e = json.Unmarshal(converted, &r); e != nil {
			return e
		}
		if hexOf(r.ID) != filepath.Base(filepath.Dir(path)) {
			return domain.ErrIntegrity
		}
		repos = append(repos, r)
	}
	for _, member := range membersByKey {
		members = append(members, member)
	}
	plan, err := domain.PlanRepositoryOwnership(boundaries, repos, members, invites)
	if err != nil {
		return fmt.Errorf("plan ownership migration: %w", err)
	}
	for _, r := range plan.Repositories {
		if err = stage(filepath.Join("repositories", r.ID+".json"), r); err != nil {
			return err
		}
	}
	for _, r := range plan.Repos {
		if err = stage(filepath.Join("repos", hexOf(r.ID), "repo.json"), r); err != nil {
			return err
		}
	}
	for _, m := range plan.Members {
		if err = stage(filepath.Join("members", m.RepositoryID, opaqueName(m.UserID)+".json"), m); err != nil {
			return err
		}
	}
	for _, inv := range plan.Invites {
		if err = stage(filepath.Join("invites", inv.Token+".json"), inv); err != nil {
			return err
		}
	}
	for _, a := range plan.Aliases {
		key := a.NamespaceID
		if key == "" {
			key = "handle:" + a.Owner
		}
		if err = stage(filepath.Join("repository-aliases", opaqueName(key+"/"+a.Path)+".json"), a); err != nil {
			return err
		}
	}
	for _, inv := range plan.Invites {
		var targets []string
		for _, target := range plan.InviteTargets {
			if target.Token == inv.Token {
				targets = append(targets, target.RepositoryID)
			}
		}
		if err = stage(filepath.Join("repository-invite-targets", inv.Token+".json"), targets); err != nil {
			return err
		}
	}
	for _, g := range grants {
		for _, target := range plan.Targets[g.RepositoryID] {
			next := g
			next.RepositoryID = target
			if target != g.RepositoryID {
				next.ID = "bg_" + hexOf(domain.HashContent([]byte(g.ID + ":" + target)))[:32]
			}
			if err = stage(filepath.Join("organization-break-glass", next.ID+".json"), next); err != nil {
				return err
			}
		}
	}
	journal = ownershipFileJournal{Version: "repository-ownership-v1", Targets: plan.Targets}
	for path, after := range changes {
		before, e := os.ReadFile(filepath.Join(s.dataDir, path))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		journal.Changes = append(journal.Changes, ownershipFileChange{Path: path, Before: before, After: after})
	}
	sort.Slice(journal.Changes, func(i, j int) bool { return journal.Changes[i].Path < journal.Changes[j].Path })
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if err = writeAtomicMode(journalPath, data, 0600); err != nil {
		return err
	}
	return s.applyOwnershipJournal(journal, journalPath, donePath)
}

func (s *FSStore) applyOwnershipJournal(journal ownershipFileJournal, source, completed string) error {
	if journal.Version != "repository-ownership-v1" {
		return domain.ErrIntegrity
	}
	seen := map[string]bool{}
	allowed := map[string]bool{"repositories": true, "repos": true, "members": true, "invites": true, "repository-aliases": true, "repository-invite-targets": true, "organizations": true, "organization-members": true, "organization-policies": true, "organization-audit": true, "organization-break-glass": true, "namespaces": true, "notification-outbox": true}
	for _, change := range journal.Changes {
		parts := strings.Split(filepath.ToSlash(change.Path), "/")
		if seen[change.Path] || !allowed[parts[0]] || len(parts) < 2 || !strings.HasSuffix(change.Path, ".json") || parts[0] == "repos" && (len(parts) != 3 || parts[2] != "repo.json") {
			return domain.ErrIntegrity
		}
		seen[change.Path] = true
		if !filepath.IsLocal(change.Path) || !json.Valid(change.After) {
			return domain.ErrIntegrity
		}
		for parent := change.Path; parent != "."; parent = filepath.Dir(parent) {
			info, err := os.Lstat(filepath.Join(s.dataDir, parent))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil && info.Mode()&os.ModeSymlink != 0 {
				return domain.ErrIntegrity
			}
		}
		current, err := os.ReadFile(filepath.Join(s.dataDir, change.Path))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		equal := func(a, b []byte) bool {
			if len(a) == 0 || len(b) == 0 {
				return len(a) == len(b)
			}
			var x, y bytes.Buffer
			return json.Compact(&x, a) == nil && json.Compact(&y, b) == nil && bytes.Equal(x.Bytes(), y.Bytes())
		}
		if !equal(current, change.Before) && !equal(current, change.After) {
			return fmt.Errorf("%w: ownership metadata changed during recovery", domain.ErrConflict)
		}
	}
	for _, change := range journal.Changes {
		path := filepath.Join(s.dataDir, change.Path)
		if err := writeAtomic(path, change.After); err != nil {
			return err
		}
	}
	// Keep the journal, including overwritten metadata, as the recovery archive.
	if err := os.Rename(source, completed); err != nil {
		return err
	}
	dir, err := os.Open(s.dataDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *FSStore) repositoryAlias(_ context.Context, key, path string) (domain.RepositoryPathAlias, error) {
	var alias domain.RepositoryPathAlias
	err := readJSON(filepath.Join(s.dataDir, "repository-aliases", opaqueName(key+"/"+path)+".json"), &alias)
	if err == nil && (alias.Path != path || alias.NamespaceID != key && "handle:"+alias.Owner != key) {
		return alias, domain.ErrIntegrity
	}
	return alias, err
}

// Validate the converted administrative records before committing any metadata.
func validateLegacyOrganizationMetadata(source, rel string, data []byte) error {
	decode := func(value any) error { return json.Unmarshal(data, value) }
	switch source {
	case "enterprises":
		var v domain.Organization
		if err := decode(&v); err != nil {
			return err
		}
		if rel != v.ID+".json" {
			return domain.ErrIntegrity
		}
		return domain.ValidateOrganizationRecord(v)
	case "enterprise-members":
		var v domain.OrganizationMembership
		if err := decode(&v); err != nil {
			return err
		}
		if filepath.Dir(rel) != v.OrganizationID {
			return domain.ErrIntegrity
		}
		return domain.ValidateOrganizationMembershipRecord(v)
	case "enterprise-policies":
		var v domain.OrganizationPolicy
		if err := decode(&v); err != nil {
			return err
		}
		if rel != v.OrganizationID+".json" {
			return domain.ErrIntegrity
		}
		return domain.ValidateOrganizationPolicy(v)
	case "enterprise-audit":
		var v domain.OrganizationAuditEvent
		if err := decode(&v); err != nil {
			return err
		}
		if filepath.Dir(rel) != v.OrganizationID {
			return domain.ErrIntegrity
		}
		return domain.ValidateOrganizationAuditEvent(v)
	case "namespaces":
		if filepath.Dir(rel) == "aliases" {
			var v struct {
				NamespaceID string `json:"namespace_id"`
			}
			if err := decode(&v); err != nil {
				return err
			}
			return domain.ValidateNamespaceID(v.NamespaceID)
		}
		var v domain.Namespace
		if err := decode(&v); err != nil {
			return err
		}
		return domain.ValidateNamespaceRecord(v)
	}
	return nil
}
