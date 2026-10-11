package app

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const publicationGitOrigin = "https://git.example.test/acme/publication.git"

type publicationLocalGit struct {
	directory string
}

func (reader publicationLocalGit) git(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", reader.directory}, args...)...)
	command.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_GRAFT_FILE="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=")
	return command.Output()
}

func (reader publicationLocalGit) commit(ctx context.Context, origin, oid string) (domain.GitCommitTree, error) {
	commit := domain.GitCommitTree{Commit: oid, Parents: []string{}}
	if origin != publicationGitOrigin || domain.ValidateGitOID(oid) != nil {
		return commit, domain.ErrValidation
	}
	raw, err := reader.git(ctx, "cat-file", "commit", oid)
	if err != nil {
		return commit, err
	}
	object := append([]byte(fmt.Sprintf("commit %d\x00", len(raw))), raw...)
	actual := ""
	if len(oid) == 40 {
		actual = fmt.Sprintf("%x", sha1.Sum(object))
	} else {
		actual = fmt.Sprintf("%x", sha256.Sum256(object))
	}
	if actual != oid {
		return commit, domain.ErrIntegrity
	}
	header := strings.SplitN(string(raw), "\n\n", 2)[0]
	for _, line := range strings.Split(header, "\n") {
		if strings.HasPrefix(line, "tree ") {
			if commit.Tree != "" {
				return commit, domain.ErrIntegrity
			}
			commit.Tree = strings.TrimPrefix(line, "tree ")
		}
		if strings.HasPrefix(line, "parent ") {
			commit.Parents = append(commit.Parents, strings.TrimPrefix(line, "parent "))
		}
	}
	return commit, commit.Validate()
}

func (reader publicationLocalGit) tree(ctx context.Context, oid string) (map[string]domain.GitEntry, error) {
	raw, err := reader.git(ctx, "ls-tree", "-r", "-t", "-z", "--full-tree", oid)
	if err != nil {
		return nil, err
	}
	entries := map[string]domain.GitEntry{}
	for _, record := range bytes.Split(raw, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		parts := bytes.SplitN(record, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return nil, domain.ErrIntegrity
		}
		fields := strings.Fields(string(parts[0]))
		name := string(parts[1])
		if len(fields) != 3 || entries[name].OID != "" {
			return nil, domain.ErrIntegrity
		}
		entries[name] = domain.GitEntry{Mode: fields[0], OID: fields[2]}
	}
	if _, err := domain.BuildGitTreeNodes(oid, entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (reader publicationLocalGit) ReadCommitTree(ctx context.Context, origin, oid string) (domain.GitTreeEvidence, error) {
	commit, err := reader.commit(ctx, origin, oid)
	if err != nil {
		return domain.GitTreeEvidence{}, err
	}
	entries, err := reader.tree(ctx, commit.Tree)
	if err != nil {
		return domain.GitTreeEvidence{}, err
	}
	return domain.BuildGitTreeEvidence(commit, entries)
}

func publicationLeafEntries(entries map[string]domain.GitEntry) map[string]domain.GitEntry {
	for name, entry := range entries {
		if entry.Mode == "040000" {
			delete(entries, name)
		}
	}
	return entries
}

func (reader publicationLocalGit) ReadCommitDeltas(ctx context.Context, origin, oid string) ([]domain.GitCommitDelta, error) {
	commit, err := reader.commit(ctx, origin, oid)
	if err != nil {
		return nil, err
	}
	entries, err := reader.tree(ctx, commit.Tree)
	if err != nil {
		return nil, err
	}
	after := publicationLeafEntries(entries)
	parents := commit.Parents
	if len(parents) == 0 {
		parents = []string{""}
	}
	deltas := make([]domain.GitCommitDelta, 0, len(parents))
	for _, parent := range parents {
		before := map[string]domain.GitEntry{}
		if parent != "" {
			prior, err := reader.commit(ctx, origin, parent)
			if err != nil {
				return nil, err
			}
			entries, err := reader.tree(ctx, prior.Tree)
			if err != nil {
				return nil, err
			}
			before = publicationLeafEntries(entries)
		}
		delta := domain.GitCommitDelta{Commit: oid, Parent: parent, Parents: append([]string{}, commit.Parents...), Changes: []domain.GitPathChange{}, Complete: true}
		for name, entry := range before {
			if after[name] != entry {
				delta.Changes = append(delta.Changes, domain.GitPathChange{Path: name, Before: entry, After: after[name]})
			}
		}
		for name, entry := range after {
			if _, exists := before[name]; !exists {
				delta.Changes = append(delta.Changes, domain.GitPathChange{Path: name, After: entry})
			}
		}
		sort.Slice(delta.Changes, func(left, right int) bool { return delta.Changes[left].Path < delta.Changes[right].Path })
		if err := delta.Validate(); err != nil {
			return nil, err
		}
		deltas = append(deltas, delta)
	}
	return deltas, nil
}

func (reader publicationLocalGit) ListGitHeads(ctx context.Context, origin string, page int) ([]outbound.GitHead, bool, error) {
	if origin != publicationGitOrigin || page < 1 {
		return nil, false, domain.ErrValidation
	}
	heads := []outbound.GitHead{}
	if page > 1 {
		return heads, false, nil
	}
	raw, err := reader.git(ctx, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/")
	if err != nil {
		return nil, false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || domain.ValidateGitOID(fields[1]) != nil {
			return nil, false, domain.ErrIntegrity
		}
		heads = append(heads, outbound.GitHead{Ref: fields[0], Commit: fields[1]})
	}
	return heads, false, nil
}
