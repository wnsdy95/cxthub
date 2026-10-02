package nativecodex

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestThreadSettingsBindExplicitChoices(t *testing.T) {
	const valid = `{"model":"m","modelProvider":"p","cwd":"/work","approvalPolicy":"untrusted","approvalsReviewer":"user","sandbox":{"type":"readOnly"},"thread":{"id":"fresh","cwd":"/work","turns":[]}}`
	thread := Thread{Model: "m", ModelProvider: "p", Cwd: "/work"}
	opts := ThreadOptions{Model: "m", ModelProvider: "p", Sandbox: "read-only", ApprovalPolicy: "untrusted"}
	hash, ok := threadSettings([]byte(valid), thread, opts)
	if !ok || !strings.HasPrefix(hash, "sha256:") {
		t.Fatal("valid native settings rejected")
	}
	for _, tc := range []struct{ from, to string }{
		{`"cwd":"/work"`, `"cwd":"/other"`},
		{`"approvalPolicy":"untrusted"`, `"approvalPolicy":"never"`},
		{`"approvalPolicy":"untrusted"`, `"approvalPolicy":null`},
		{`"approvalPolicy":"untrusted"`, `"ApprovalPolicy":"never","approvalPolicy":"untrusted"`},
		{`"type":"readOnly"`, `"type":"dangerFullAccess"`},
		{`"type":"readOnly"`, `"Type":"dangerFullAccess","type":"readOnly"`},
		{`"type":"readOnly"`, `"type":"readOnly","networkAccess":"yes"`},
		{`"type":"readOnly"`, `"type":"readOnly","NetworkAccess":true`},
		{`"type":"readOnly"`, `"type":"readOnly","networkAccess":null`},
		{`"approvalsReviewer":"user"`, `"approvalsReviewer":null`},
	} {
		if _, ok := threadSettings([]byte(strings.Replace(valid, tc.from, tc.to, 1)), thread, opts); ok {
			t.Fatal("changed or ambiguous runtime accepted")
		}
	}
	thread.Model = "other"
	if _, ok := threadSettings([]byte(valid), thread, opts); ok {
		t.Fatal("explicit model replaced")
	}
	thread.Model = "m"
	changed := strings.Replace(valid, `"type":"readOnly"`, `"type":"readOnly","networkAccess":true`, 1)
	changedHash, ok := threadSettings([]byte(changed), thread, opts)
	if !ok || changedHash == hash {
		t.Fatal("effective permission details not included in observation")
	}
	reordered := strings.Replace(changed, `"type":"readOnly","networkAccess":true`, `"networkAccess":true,"type":"readOnly"`, 1)
	if h, ok := threadSettings([]byte(reordered), thread, opts); !ok || h != changedHash {
		t.Fatal("native JSON ordering changed settings identity")
	}
	opts.ApprovalPolicy = ""
	for _, granular := range []string{`null`, `{}`, `{"mcp_elicitations":true,"rules":true,"sandbox_approval":null}`, `{"mcp_elicitations":true,"rules":true,"sandbox_approval":false,"Rules":false}`} {
		candidate := strings.Replace(valid, `"approvalPolicy":"untrusted"`, `"approvalPolicy":{"granular":`+granular+`}`, 1)
		if _, ok := threadSettings([]byte(candidate), thread, opts); ok {
			t.Fatal("invalid granular approval accepted")
		}
	}
	granular := strings.Replace(valid, `"approvalPolicy":"untrusted"`, `"approvalPolicy":{"granular":{"mcp_elicitations":true,"rules":true,"sandbox_approval":false}}`, 1)
	if _, ok := threadSettings([]byte(granular), thread, opts); !ok {
		t.Fatal("valid native granular approval rejected")
	}
}

func TestNativeSandboxSchema(t *testing.T) {
	root, _ := json.Marshal(t.TempDir())
	for _, raw := range []string{
		`{"type":"workspaceWrite","writableRoots":[` + string(root) + `],"networkAccess":false,"excludeSlashTmp":true}`,
		`{"type":"externalSandbox","networkAccess":"restricted"}`,
		`{"type":"dangerFullAccess"}`,
	} {
		fields, ok := identityObject([]byte(raw), "type")
		var kind string
		_ = json.Unmarshal(fields["type"], &kind)
		if !ok || !validSandboxFields(kind, fields) {
			t.Fatal("valid native sandbox rejected")
		}
	}
	for _, raw := range []string{
		`{"type":"workspaceWrite","writableRoots":null}`,
		`{"type":"workspaceWrite","writableRoots":["relative"]}`,
		`{"type":"workspaceWrite","writableRoots":[1]}`,
		`{"type":"workspaceWrite","excludeSlashTmp":"yes"}`,
		`{"type":"externalSandbox","networkAccess":true}`,
		`{"type":"dangerFullAccess","networkAccess":false}`,
	} {
		fields, _ := identityObject([]byte(raw), "type")
		var kind string
		_ = json.Unmarshal(fields["type"], &kind)
		if validSandboxFields(kind, fields) {
			t.Fatal("malformed native sandbox accepted")
		}
	}
}
