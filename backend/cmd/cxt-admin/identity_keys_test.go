//go:build postgres

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestIdentityKeyCommandRejectsSecretsWithoutEchoingConfiguration(t *testing.T) {
	marker := "private-operator-key-must-never-be-printed"
	t.Setenv("CXT_IDENTITY_ENCRYPTION_KEY", marker)
	t.Setenv("CXT_IDENTITY_ENCRYPTION_KEYRING", "")
	var out, stderr bytes.Buffer
	err := runIdentityKeys(nil, &out, &stderr)
	if err == nil || strings.Contains(err.Error()+out.String()+stderr.String(), marker) || out.Len() != 0 {
		t.Fatal("credential exposed or invalid key accepted")
	}
}
