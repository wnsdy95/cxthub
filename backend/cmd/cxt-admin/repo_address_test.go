//go:build postgres

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestParseRepoAddressOptions(test *testing.T) {
	for name, args := range map[string][]string{
		"a list":                  {"--list"},
		"a move to look at":       {"--from", "http://localhost:5173/a/b", "--to", "https://cxthub.example/a/b"},
		"a move":                  {"--from", "http://localhost:5173/a/b", "--to", "https://cxthub.example/a/b", "--apply"},
		"a move with a directory": {"--dir", "/migrations", "--from", "x", "--to", "y"},
	} {
		options, err := parseRepoAddressOptions(args, "")
		if err != nil {
			test.Fatalf("%s: %v", name, err)
		}
		if options.apply != (name == "a move") || options.list != (name == "a list") {
			test.Fatalf("%s: %+v", name, options)
		}
		if want := map[bool]string{true: "/migrations", false: "schemas/db/migrations"}[name == "a move with a directory"]; options.directory != want {
			test.Fatalf("%s: the directory is %q", name, options.directory)
		}
	}
	if options, err := parseRepoAddressOptions([]string{"--list"}, " /bundle "); err != nil || options.directory != "/bundle" {
		test.Fatalf("the directory from the environment: %+v %v", options, err)
	}
	for name, args := range map[string][]string{
		"nothing":                 {},
		"only where from":         {"--from", "x"},
		"only where to":           {"--to", "y"},
		"an empty address":        {"--from", " ", "--to", "y"},
		"a list with a move":      {"--list", "--from", "x", "--to", "y"},
		"a list applied":          {"--list", "--apply"},
		"an argument left over":   {"--list", "extra"},
		"an unknown option":       {"--list", "--force"},
		"an empty directory":      {"--dir", " ", "--list"},
		"an option without value": {"--from"},
	} {
		if _, err := parseRepoAddressOptions(args, ""); err == nil {
			test.Fatalf("%s was taken", name)
		}
	}
	if _, err := parseRepoAddressOptions([]string{"--help"}, ""); !errors.Is(err, flag.ErrHelp) {
		test.Fatalf("help: %v", err)
	}
}

func TestRepoAddressNeedsTheDatabaseBeforeOpeningIt(test *testing.T) {
	test.Setenv("CXT_POSTGRES_DSN", "")
	var stdout, stderr bytes.Buffer
	if err := runRepoAddress(context.Background(), []string{"--list"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "CXT_POSTGRES_DSN is required") || stdout.Len() != 0 {
		test.Fatalf("without a database: %v %q", err, stdout.String())
	}
	test.Setenv("CXT_POSTGRES_DSN", "postgres://user:synthetic-secret@[::1:bad")
	if err := runRepoAddress(context.Background(), []string{"--list"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "must be a valid PostgreSQL connection string") || strings.Contains(err.Error(), "synthetic-secret") {
		test.Fatalf("an invalid connection string: %v", err)
	}
	stderr.Reset()
	if err := runRepoAddress(context.Background(), []string{"--help"}, &stdout, &stderr); err != nil || !strings.Contains(stderr.String(), "cxt-admin repo-address") {
		test.Fatalf("help: %v %q", err, stderr.String())
	}
}
