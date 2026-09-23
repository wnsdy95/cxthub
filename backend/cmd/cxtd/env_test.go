package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
)

func TestServerDotEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	t.Setenv("CXT_ENV_FILE", p)
	t.Setenv("CXT_ENV_TEST_EXISTING", "process")
	for _, k := range []string{"CXT_ENV_TEST_SINGLE", "CXT_ENV_TEST_DOUBLE", "CXT_ENV_TEST_LITERAL"} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	if err := os.WriteFile(p, []byte("# test\nexport CXT_ENV_TEST_EXISTING=file\nCXT_ENV_TEST_SINGLE='hello world' # comment\nCXT_ENV_TEST_DOUBLE=\"a\\nvalue\"\nCXT_ENV_TEST_LITERAL=$(do-not-execute) $SECRET # ignored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadServerEnv(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CXT_ENV_TEST_EXISTING") != "process" || os.Getenv("CXT_ENV_TEST_SINGLE") != "hello world" || os.Getenv("CXT_ENV_TEST_DOUBLE") != "a\nvalue" || os.Getenv("CXT_ENV_TEST_LITERAL") != "$(do-not-execute) $SECRET" {
		t.Fatal("precedence/literal parsing failed")
	}
	if err := os.WriteFile(p, []byte("bad-synthetic-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	err := loadServerEnv()
	if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal("parse error leaked source", err)
	}
}
func TestServerDotEnvOptionalAndExplicitMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CXT_ENV_FILE", "")
	if err := loadServerEnv(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CXT_ENV_FILE", "missing.env")
	if loadServerEnv() == nil {
		t.Fatal("explicit file must exist")
	}
}
func TestResendKeyOnlyConfiguration(t *testing.T) {
	s := app.NewIdentityService(nil, store.NewFSStore(t.TempDir()))
	t.Setenv("RESEND_API_KEY", "synthetic-key")
	t.Setenv("RESEND_FROM", "")
	t.Setenv("CXT_WEB_URL", "")
	if err := configureInvitationEmail(s, "127.0.0.1:8907", "http://127.0.0.1:8907"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESEND_FROM", "invalid")
	if configureInvitationEmail(s, ":8907", "https://example.test") == nil {
		t.Fatal("invalid sender accepted")
	}
	t.Setenv("RESEND_API_KEY", "")
	if err := configureInvitationEmail(s, ":8907", ""); err != nil {
		t.Fatal("disabled email blocked inbox", err)
	}
}
