//go:build darwin || linux

package nativeclaude

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestSupervisedResumePreservesTerminalGroupAndOneShot(t *testing.T) {
	opts, _ := idleUnitOptions(t, "exit")
	_, plan := idleUnitPlan(t, opts)
	copy := *plan
	cmd, err := plan.StartSupervised(context.Background(), strings.NewReader(""), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr != nil {
		t.Fatal("isolated process group was applied to controlling terminal")
	}
	if err := waitSupervised(t, cmd); err != nil {
		t.Fatal(err)
	}
	if _, err := copy.StartSupervised(context.Background(), strings.NewReader(""), io.Discard, io.Discard); !errors.Is(err, ErrState) {
		t.Fatal("copied plan reused", err)
	}
}

func TestSupervisedResumeRejectsArchiveDriftBeforeProcessCreation(t *testing.T) {
	opts, log := idleUnitOptions(t, "exit")
	s, plan := idleUnitPlan(t, opts)
	f, err := os.OpenFile(unitArchive(s.s), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if cmd, err := plan.StartSupervised(context.Background(), strings.NewReader(""), io.Discard, io.Discard); err == nil || cmd != nil {
		t.Fatal("changed archive started")
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("resume process was created", err)
	}
}
