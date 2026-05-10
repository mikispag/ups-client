package notifier

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mikispag/ups-client/monitor"
)

func TestShellTargetRunsCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell only")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	tt := &ShellTarget{
		Label:   "echo",
		Command: "/bin/sh",
		Args:    []string{"-c", "printf '%s %s' \"$UPS_EVENT\" \"$1\" > " + out, "sh", "{{.UPS}}"},
		Timeout: 2 * time.Second,
	}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnBatt)); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "ONBATT") || !strings.Contains(got, "ups") {
		t.Errorf("output = %q", got)
	}
}

func TestShellTargetEnvOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell only")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	tt := &ShellTarget{
		Command: "/bin/sh",
		Args:    []string{"-c", "printf '%s' \"$MY_VAR\" > " + out},
		Env:     map[string]string{"MY_VAR": "hello"},
		Timeout: 2 * time.Second,
	}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if string(data) != "hello" {
		t.Errorf("custom env not propagated: %q", string(data))
	}
}

func TestShellTargetTemplateError(t *testing.T) {
	tt := &ShellTarget{Command: "/bin/true", Args: []string{"{{.Missing"}}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected template parse error")
	}
}

func TestShellTargetNonzero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell only")
	}
	tt := &ShellTarget{Command: "/bin/false"}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected non-zero exit error")
	}
}

func TestShellTargetEmptyCommand(t *testing.T) {
	tt := &ShellTarget{}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected error for empty command")
	}
}

func TestShellTargetName(t *testing.T) {
	tt := &ShellTarget{Command: "/bin/echo"}
	if !strings.HasPrefix(tt.Name(), "shell:") {
		t.Errorf("Name() = %q", tt.Name())
	}
	tt.Label = "x"
	if tt.Name() != "shell:x" {
		t.Errorf("labeled Name() = %q", tt.Name())
	}
}
