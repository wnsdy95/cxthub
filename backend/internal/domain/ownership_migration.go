package domain

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// RepositoryPathAlias resolves a historical address without deriving a new
// content repository ID from that address. Authorization always uses the target.
type RepositoryPathAlias struct {
	NamespaceID   string      `json:"namespace_id,omitempty"`
	Owner         string      `json:"owner"`
	Path          string      `json:"path"`
	RepositoryID  string      `json:"repository_id"`
	ContextRepoID ContentHash `json:"context_repo_id,omitempty"`
}

type RepositoryInviteTarget struct {
	Token        string `json:"token"`
	RepositoryID string `json:"repository_id"`
}

// OwnershipMigration is a complete plan, produced before persistence mutates
// anything. Repository is the legacy input shape until the storage cutover.
type OwnershipMigration struct {
	Repositories  []Repository
	Repos         []Repo
	Members       []Membership
	Invites       []Invite
	InviteTargets []RepositoryInviteTarget
	Aliases       []RepositoryPathAlias
	Targets       map[string][]string
}

// PlanRepositoryOwnership splits legacy visibility containers into one
// repository boundary per content repository. It is deterministic across input
// order and leaves the supplied records and all content-addressed IDs intact.
func PlanRepositoryOwnership(repositories []Repository, repos []Repo, members []Membership, invites []Invite) (OwnershipMigration, error) {
	out := OwnershipMigration{Targets: map[string][]string{}}
	containers := append([]Repository(nil), repositories...)
	sort.Slice(containers, func(i, j int) bool { return containers[i].ID < containers[j].ID })
	byID := map[string]Repository{}
	children := map[string][]Repo{}
	seenRepo := map[ContentHash]bool{}
	reserved := map[string]string{}
	ownerKey := func(w Repository) string {
		if w.OwnerNamespaceID != "" {
			return w.OwnerNamespaceID
		}
		return "handle:" + w.OwnerUsername
	}
	for _, w := range containers {
		if err := ValidateRepositoryRecord(w); err != nil {
			return out, err
		}
		if w.OwnerUsername == "" || !ValidRepositorySlug(w.Slug) || byID[w.ID].ID != "" {
			return out, fmt.Errorf("%w: ambiguous legacy repository owner or name", ErrIntegrity)
		}
		key := ownerKey(w) + "/" + w.Slug
		if reserved[key] != "" {
			return out, fmt.Errorf("%w: duplicate legacy repository address", ErrIntegrity)
		}
		reserved[key] = w.ID
		byID[w.ID] = w
	}
	for _, r := range repos {
		if err := ValidateContentHash(r.ID); err != nil {
			return out, err
		}
		if seenRepo[r.ID] {
			return out, fmt.Errorf("%w: duplicate content repository ID", ErrIntegrity)
		}
		seenRepo[r.ID] = true
		if r.RepositoryID == "" {
			// Unowned legacy content remains unowned and inaccessible.
			out.Repos = append(out.Repos, r)
			continue
		}
		if byID[r.RepositoryID].ID == "" {
			return out, fmt.Errorf("%w: missing legacy repository boundary", ErrIntegrity)
		}
		children[r.RepositoryID] = append(children[r.RepositoryID], r)
	}
	aliasTargets := map[string]string{}
	addAlias := func(w Repository, path string, id string, content ContentHash) error {
		aliases := []RepositoryPathAlias{{NamespaceID: w.OwnerNamespaceID, Owner: w.OwnerUsername, Path: path, RepositoryID: id, ContextRepoID: content}}
		if w.OwnerNamespaceID != "" {
			aliases = append(aliases, RepositoryPathAlias{Owner: w.OwnerUsername, Path: path, RepositoryID: id, ContextRepoID: content})
		}
		for _, alias := range aliases {
			key := alias.NamespaceID
			if key == "" {
				key = "handle:" + alias.Owner
			}
			key += "/" + path
			if target := aliasTargets[key]; target != "" {
				if target != id {
					return fmt.Errorf("%w: ambiguous repository alias", ErrIntegrity)
				}
				continue
			}
			aliasTargets[key] = id
			out.Aliases = append(out.Aliases, alias)
		}
		return nil
	}
	for _, w := range containers {
		list := children[w.ID]
		// Preserve the former two-segment address and boundary ID when present.
		legacyPath := func(r Repo) string {
			u, err := url.Parse(r.RemoteURL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
				return ""
			}
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) != 2 && len(parts) != 3 {
				return ""
			}
			for _, part := range parts {
				if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
					return ""
				}
			}
			return strings.Join(parts[1:], "/")
		}
		sort.Slice(list, func(i, j int) bool {
			a, b := !strings.Contains(legacyPath(list[i]), "/"), !strings.Contains(legacyPath(list[j]), "/")
			if a != b {
				return a
			}
			return list[i].ID < list[j].ID
		})
		if len(list) == 0 {
			out.Repositories = append(out.Repositories, w)
			out.Targets[w.ID] = []string{w.ID}
			if err := addAlias(w, w.Slug, w.ID, ""); err != nil {
				return out, err
			}
			continue
		}
		for i, r := range list {
			path := legacyPath(r)
			if path == "" {
				return out, fmt.Errorf("%w: invalid legacy repository connection", ErrIntegrity)
			}
			next := w
			if i > 0 {
				next.ID = "ws_" + strings.TrimPrefix(string(HashContent([]byte("repository-boundary:"+string(r.ID)))), "sha256:")[:32]
				if byID[next.ID].ID != "" {
					return out, fmt.Errorf("%w: repository boundary ID collision", ErrIntegrity)
				}
			}
			byID[next.ID] = next
			name := path
			if at := strings.LastIndexByte(name, '/'); at >= 0 {
				name = name[at+1:]
			}
			if !strings.Contains(path, "/") {
				name = w.Slug
			}
			if !ValidRepositorySlug(name) {
				// Legacy URL segments allowed characters excluded by today's naming
				// rules. Keep that exact alias and choose a stable valid display name.
				name = "repository-" + strings.TrimPrefix(string(r.ID), "sha256:")[:12]
			}
			candidate := name
			occupied := func(slug string) bool {
				holder := reserved[ownerKey(w)+"/"+slug]
				return holder != "" && !(slug == w.Slug && holder == w.ID && (!strings.Contains(path, "/") || len(list) == 1))
			}
			if occupied(candidate) {
				candidate = w.Slug + "-" + name
			}
			if len(candidate) > 64 || occupied(candidate) {
				suffix := "-" + strings.TrimPrefix(string(r.ID), "sha256:")[:12]
				if len(candidate) > 64-len(suffix) {
					candidate = strings.TrimRight(candidate[:64-len(suffix)], "-_")
				}
				candidate += suffix
			}
			if !ValidRepositorySlug(candidate) || occupied(candidate) {
				return out, fmt.Errorf("%w: canonical repository name collision", ErrIntegrity)
			}
			next.Slug = candidate
			if candidate != w.Slug {
				next.Name = candidate
			}
			reserved[ownerKey(w)+"/"+candidate] = next.ID
			out.Repositories = append(out.Repositories, next)
			out.Targets[w.ID] = append(out.Targets[w.ID], next.ID)
			r.RepositoryID = next.ID
			out.Repos = append(out.Repos, r)
			if err := addAlias(w, path, next.ID, r.ID); err != nil {
				return out, err
			}
			legacyURL, _ := url.Parse(r.RemoteURL)
			legacyOwner := strings.Split(strings.Trim(legacyURL.Path, "/"), "/")[0]
			if legacyOwner != w.OwnerUsername {
				old := w
				old.OwnerNamespaceID = ""
				old.OwnerUsername = legacyOwner
				if err := addAlias(old, path, next.ID, r.ID); err != nil {
					return out, err
				}
			}
			if err := addAlias(w, candidate, next.ID, r.ID); err != nil {
				return out, err
			}
			if len(list) == 1 {
				if err := addAlias(w, w.Slug, next.ID, r.ID); err != nil {
					return out, err
				}
			}
		}
	}
	seenMembers := map[string]bool{}
	for _, m := range members {
		key := m.RepositoryID + "/" + m.UserID
		if seenMembers[key] {
			return out, fmt.Errorf("%w: duplicate collaborator", ErrIntegrity)
		}
		seenMembers[key] = true
		if err := ValidateMembershipRecord(m); err != nil {
			return out, err
		}
		targets := out.Targets[m.RepositoryID]
		if len(targets) == 0 {
			return out, fmt.Errorf("%w: membership has no repository boundary", ErrIntegrity)
		}
		for _, id := range targets {
			copy := m
			copy.RepositoryID = id
			out.Members = append(out.Members, copy)
		}
	}
	seenInvites := map[string]bool{}
	for _, inv := range invites {
		if seenInvites[inv.Token] {
			return out, fmt.Errorf("%w: duplicate invite", ErrIntegrity)
		}
		seenInvites[inv.Token] = true
		if err := ValidateInviteRecord(inv); err != nil {
			return out, err
		}
		targets := out.Targets[inv.RepositoryID]
		if len(targets) == 0 {
			return out, fmt.Errorf("%w: invite has no repository boundary", ErrIntegrity)
		}
		inv.RepositoryID = targets[0]
		out.Invites = append(out.Invites, inv)
		for _, id := range targets {
			out.InviteTargets = append(out.InviteTargets, RepositoryInviteTarget{Token: inv.Token, RepositoryID: id})
		}
	}
	sort.Slice(out.Repos, func(i, j int) bool { return out.Repos[i].ID < out.Repos[j].ID })
	sort.Slice(out.Members, func(i, j int) bool {
		a, b := out.Members[i], out.Members[j]
		if a.RepositoryID != b.RepositoryID {
			return a.RepositoryID < b.RepositoryID
		}
		return a.UserID < b.UserID
	})
	sort.Slice(out.Invites, func(i, j int) bool { return out.Invites[i].Token < out.Invites[j].Token })
	sort.Slice(out.InviteTargets, func(i, j int) bool {
		a, b := out.InviteTargets[i], out.InviteTargets[j]
		if a.Token != b.Token {
			return a.Token < b.Token
		}
		return a.RepositoryID < b.RepositoryID
	})
	return out, nil
}
