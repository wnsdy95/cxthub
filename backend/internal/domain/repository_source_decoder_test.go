package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
)

func repositoryDecoderFixture(test *testing.T) (ContentHash, []RepositoryViewSourceEntry) {
	test.Helper()
	repo, entries, _ := pendingProjectionFixture(test, 1)
	for index := range entries {
		if entries[index].Kind == "repository" {
			entries[index].Value = bytes.Replace(entries[index].Value, []byte(`"id":`), []byte(`"remote_url_supported":true,"git_remote_url_supported":true,"topics":["context"],"id":`), 1)
		}
	}
	return repo, entries
}

func repositoryDecoderParity(test *testing.T, repo ContentHash, entries []RepositoryViewSourceEntry, previous *RepositoryViewSourceDecoder) *RepositoryViewSourceDecoder {
	test.Helper()
	ctx := context.Background()
	wantRepo, wantView, wantErr := DecodeRepositoryViewSource(ctx, repo, entries)
	gotRepo, gotView, next, err := DecodeRepositoryViewSourceReusing(ctx, repo, entries, previous)
	if (err == nil) != (wantErr == nil) || !reflect.DeepEqual(gotRepo, wantRepo) || !reflect.DeepEqual(gotView, wantView) {
		test.Fatalf("decoded reuse differs: errors %v / %v; repo equal %t; view equal %t", err, wantErr, reflect.DeepEqual(gotRepo, wantRepo), reflect.DeepEqual(gotView, wantView))
	}
	if err != nil && next != nil {
		test.Fatal("failed finalization retained a partial decoder")
	}
	for _, class := range []error{ErrIntegrity, ErrValidation, ErrUnsupportedDocumentIdentity, ErrRepositoryViewSourceLimit, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, class) != errors.Is(wantErr, class) {
			test.Fatal("error class changed", class, err, wantErr)
		}
	}
	return next
}

func TestRepositorySourceDecoderReusesOnlyUnchangedRows(test *testing.T) {
	repo, entries := repositoryDecoderFixture(test)
	base := repositoryDecoderParity(test, repo, entries, nil)
	if base == nil || len(base.rows) != len(entries) {
		test.Fatal("missing full baseline")
	}
	unchanged := slices.Clone(entries)
	for index := range unchanged {
		unchanged[index].Sequence++
	}
	slices.Reverse(unchanged)
	reused := repositoryDecoderParity(test, repo, unchanged, base)
	for key, row := range base.rows {
		if reused.rows[key].value != row.value {
			test.Fatal("unchanged row was decoded again", key)
		}
	}
	changed := slices.Clone(entries)
	var changedKey [2]string
	for index := range changed {
		if changed[index].Kind == "snapshot" {
			changed[index].Value = bytes.Replace(changed[index].Value, []byte(`"message":`), []byte(`"memory_hash":"`+string(HashContent([]byte("new memory")))+`","message":`), 1)
			changedKey = [2]string{changed[index].Kind, changed[index].Key}
			break
		}
	}
	updated := repositoryDecoderParity(test, repo, changed, base)
	decoded := 0
	for key, row := range base.rows {
		if updated.rows[key].value != row.value {
			decoded++
			if key != changedKey {
				test.Fatal("unrelated row was decoded again", key)
			}
		}
	}
	if decoded != 1 {
		test.Fatal("changed bytes reused stale decoded state", decoded)
	}
}

func TestRepositorySourceDecoderChangesRemovalsAndRestoration(test *testing.T) {
	repo, entries := repositoryDecoderFixture(test)
	base := repositoryDecoderParity(test, repo, entries, nil)
	mutations := map[string]map[string]any{
		"repository":      {"description": "updated", "default_branch": "trunk"},
		"revision":        {"graph": "99"},
		"snapshot":        {"message": "changed", "branches": []string{"topic", "main"}},
		"ref":             {"target": HashContent([]byte("replacement"))},
		"history":         {"created_at": "2026-10-19T00:00:00Z"},
		"pending":         {"activity_at": "2026-10-19T00:00:00Z", "dismissed": true},
		"unsync":          {"updated_at": "2026-10-19T00:00:00Z"},
		"session_archive": {"archived_by": "new-maintainer"},
		"reflog":          {"created_at": "2026-10-19T00:00:00Z"},
	}
	for index, entry := range entries {
		test.Run(fmt.Sprintf("%s/%d", entry.Kind, index), func(test *testing.T) {
			removed := append(slices.Clone(entries[:index]), entries[index+1:]...)
			next := repositoryDecoderParity(test, repo, removed, base)
			if next != nil {
				if len(next.rows) != len(entries)-1 {
					test.Fatal("removed row retained")
				}
				repositoryDecoderParity(test, repo, entries, next)
			}
			if fields, exists := mutations[entry.Kind]; exists {
				var value map[string]any
				if err := json.Unmarshal(entry.Value, &value); err != nil {
					test.Fatal(err)
				}
				for field, updated := range fields {
					value[field] = updated
				}
				if entry.Kind == "ref" && value["symbolic"] != nil && value["symbolic"] != "" {
					delete(value, "target")
					value["symbolic"] = "trunk"
				}
				changed := slices.Clone(entries)
				changed[index].Value = pendingProjectionTestJSON(test, value)
				if repositoryDecoderParity(test, repo, changed, base) == nil {
					test.Fatal("valid change was rejected")
				}
			}
		})
	}
	protocol := slices.Clone(entries)
	for index := range protocol {
		protocol[index].Value = bytes.Replace(protocol[index].Value, []byte(`"context_protocol":1`), []byte(`"context_protocol":0`), 1)
	}
	if repositoryDecoderParity(test, repo, protocol, base) == nil {
		test.Fatal("atomic protocol change was rejected")
	}
}

func TestRepositorySourceDecoderRejectsChangedInvalidInputs(test *testing.T) {
	repo, entries := repositoryDecoderFixture(test)
	base := repositoryDecoderParity(test, repo, entries, nil)
	for _, kind := range []string{"repository", "protocol", "revision", "snapshot", "ref", "history", "pending", "unsync", "session_archive", "reflog"} {
		for _, invalid := range []string{"unknown", "duplicate-json-field", "deleted", "sequence", "key", "duplicate-row"} {
			test.Run(kind+"/"+invalid, func(test *testing.T) {
				changed := slices.Clone(entries)
				for index := range changed {
					if changed[index].Kind != kind {
						continue
					}
					switch invalid {
					case "unknown":
						changed[index].Value = bytes.Replace(changed[index].Value, []byte("{"), []byte(`{"unknown_field":true,`), 1)
					case "duplicate-json-field":
						changed[index].Value = bytes.Replace(changed[index].Value, []byte("{"), []byte(`{"duplicate":1,"duplicate":2,`), 1)
					case "deleted":
						changed[index].Deleted = true
					case "sequence":
						changed[index].Sequence = -1
					case "key":
						changed[index].Key = "wrong"
					case "duplicate-row":
						changed = append(changed, changed[index])
					}
					break
				}
				if repositoryDecoderParity(test, repo, changed, base) != nil {
					test.Fatal("invalid change accepted")
				}
				repositoryDecoderParity(test, repo, entries, base)
			})
		}
	}
	aliases := slices.Clone(entries)
	for _, entry := range entries {
		if entry.Kind == "ref" {
			entry.Key = " [ " + entry.Key[1:len(entry.Key)-1] + " ] "
			aliases = append(aliases, entry)
			break
		}
	}
	if repositoryDecoderParity(test, repo, aliases, base) != nil {
		test.Fatal("canonical key collision accepted")
	}
	if _, _, next, err := DecodeRepositoryViewSourceReusing(context.Background(), HashContent([]byte("foreign")), entries, base); !errors.Is(err, ErrIntegrity) || next != nil {
		test.Fatal("cross-repository reuse accepted", err)
	}
}

func TestRepositorySourceDecoderOwnsInputsAndOutputs(test *testing.T) {
	repo, entries := repositoryDecoderFixture(test)
	ctx := context.Background()
	metadata, view, base, err := DecodeRepositoryViewSourceReusing(ctx, repo, entries, nil)
	if err != nil {
		test.Fatal(err)
	}
	metadata.Topics[0] = "mutated"
	for index := range view.Pending {
		*view.Pending[index].ActivityAt = view.Pending[index].ActivityAt.AddDate(1, 0, 0)
	}
	for index := range view.Snapshots {
		if len(view.Snapshots[index].Parents) > 0 {
			view.Snapshots[index].Parents[0] = HashContent([]byte("mutated parent"))
		}
	}
	for index := range view.ArchivedSessions {
		view.ArchivedSessions[index].SnapshotIDs[0] = HashContent([]byte("mutated archive"))
	}
	repositoryDecoderParity(test, repo, entries, base)
	for index := range entries {
		original := bytes.Clone(entries[index].Value)
		entries[index].Value[0] = '['
		if base.rows[[2]string{entries[index].Kind, entries[index].Key}].raw != string(original) {
			test.Fatal("caller bytes alias retained state")
		}
		entries[index].Value = original
	}
	var group sync.WaitGroup
	for range 12 {
		group.Go(func() {
			repositoryDecoderParity(test, repo, entries, base)
		})
	}
	group.Wait()
}

func TestRepositorySourceDecoderRetainsCancellationAndLimits(test *testing.T) {
	repo, entries := repositoryDecoderFixture(test)
	base := repositoryDecoderParity(test, repo, entries, nil)
	for _, remaining := range []int{0, 10, 50} {
		ctx := &pendingProjectionCancelContext{Context: context.Background(), remaining: remaining}
		if _, _, next, err := DecodeRepositoryViewSourceReusing(ctx, repo, entries, base); !errors.Is(err, context.Canceled) || next != nil {
			test.Fatal("cancellation retained partial state", remaining, err)
		}
	}
	for _, oversized := range [][]RepositoryViewSourceEntry{
		make([]RepositoryViewSourceEntry, MaxPendingProjectionInputRows+1),
		{{Kind: "repository", Key: string(repo), Value: bytes.Repeat([]byte(" "), MaxPendingProjectionInputBytes+1)}},
	} {
		if _, _, next, err := DecodeRepositoryViewSourceReusing(context.Background(), repo, oversized, base); !errors.Is(err, ErrRepositoryViewSourceLimit) || next != nil {
			test.Fatal("limit retained partial state", err)
		}
	}
	repositoryDecoderParity(test, repo, entries, base)
}

func TestRepositorySourceDecoderSnapshotIdentityValidation(test *testing.T) {
	repo, entries := repositoryDecoderFixture(test)
	base := repositoryDecoderParity(test, repo, entries, nil)
	for _, field := range []string{
		`"doc_identity":null,`,
		`"doc_identity":"unsupported",`,
		`"doc_identity":"","DOC_IDENTITY":"",`,
	} {
		changed := slices.Clone(entries)
		for index := range changed {
			if changed[index].Kind == "snapshot" {
				changed[index].Value = bytes.Replace(changed[index].Value, []byte("{"), []byte("{"+field), 1)
				break
			}
		}
		if repositoryDecoderParity(test, repo, changed, base) != nil {
			test.Fatal("invalid document identity accepted", field)
		}
	}
}

func TestRepositorySourceDecoderTimestampEncoding(test *testing.T) {
	repo, entries := repositoryDecoderFixture(test)
	base := repositoryDecoderParity(test, repo, entries, nil)
	fields := map[string][]string{
		"snapshot": {"created_at"}, "history": {"created_at"}, "pending": {"updated_at", "activity_at"},
		"unsync": {"updated_at"}, "session_archive": {"archived_at"}, "reflog": {"created_at"},
	}
	for kind, names := range fields {
		for _, name := range names {
			for _, offset := range []string{"Z", "+23:59", "-04:00", "+24:00", "-24:00"} {
				test.Run(kind+"/"+name+"/"+offset, func(test *testing.T) {
					changed := slices.Clone(entries)
					for index := range changed {
						if changed[index].Kind != kind {
							continue
						}
						var row map[string]any
						if err := json.Unmarshal(changed[index].Value, &row); err != nil {
							test.Fatal(err)
						}
						row[name] = "2026-10-19T00:00:00" + offset
						changed[index].Value = pendingProjectionTestJSON(test, row)
						break
					}
					next := repositoryDecoderParity(test, repo, changed, base)
					invalid := offset == "+24:00" || offset == "-24:00"
					if (next == nil) != invalid {
						test.Fatal("unexpected timestamp acceptance", offset)
					}
				})
			}
		}
	}
}
