package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
nut:
  address: 127.0.0.1:3493
  ups: ups
notifications:
  shell:
    - name: log
      command: /usr/bin/logger
      args: ["-t", "ups", "{{.Event}}"]
      events: [ONBATT, ONLINE]
  webhook:
    - name: ntfy
      url: https://ntfy.sh/my-ups
      headers:
        Title: "UPS {{.UPS}} {{.Event}}"
      body: "Status: {{.Status}}"
  ssh:
    - name: ha
      host: ha.local
      user: root
      private_key_file: /etc/ups-client/id
      command: "service nas {{.Event}}"
  telegram:
    - name: ops
      bot_token: TOKEN
      chat_id: "12345"
      message: "{{.Event}} {{.UPS}}"
`

func TestParseMinimal(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.NUT.UPS != "ups" || c.NUT.Address != "127.0.0.1:3493" {
		t.Errorf("nut: %+v", c.NUT)
	}
	if c.Monitor.StatusInterval != 2*time.Second {
		t.Errorf("default StatusInterval = %s", c.Monitor.StatusInterval)
	}
	if len(c.Notifications.Shell) != 1 || c.Notifications.Shell[0].Command != "/usr/bin/logger" {
		t.Errorf("shell: %+v", c.Notifications.Shell)
	}
	if len(c.Notifications.Webhook) != 1 || c.Notifications.Webhook[0].URL != "https://ntfy.sh/my-ups" {
		t.Errorf("webhook: %+v", c.Notifications.Webhook)
	}
	if len(c.Notifications.SSH) != 1 || c.Notifications.SSH[0].Host != "ha.local" {
		t.Errorf("ssh: %+v", c.Notifications.SSH)
	}
	if len(c.Notifications.Telegram) != 1 {
		t.Errorf("telegram: %+v", c.Notifications.Telegram)
	}
}

func TestBuildNotifiers(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	ns := c.BuildNotifiers()
	if len(ns) != 4 {
		t.Errorf("expected 4 notifiers, got %d", len(ns))
	}
	names := []string{}
	for _, n := range ns {
		names = append(names, n.Name())
	}
	want := []string{"shell:log", "webhook:ntfy", "ssh:ha", "telegram:ops"}
	for _, w := range want {
		found := false
		for _, n := range names {
			if n == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing notifier %q in %v", w, names)
		}
	}
}

func TestValidateUnknownEvent(t *testing.T) {
	src := `notifications:
  shell:
    - name: x
      command: /bin/true
      events: [BOGUS]`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "BOGUS") {
		t.Errorf("expected unknown event error, got %v", err)
	}
}

func TestValidateMissingFields(t *testing.T) {
	cases := []string{
		`notifications: { shell: [{name: x}] }`,
		`notifications: { webhook: [{name: x}] }`,
		`notifications: { ssh: [{name: x, host: h, user: u, command: c}] }`, // missing auth
		`notifications: { ssh: [{name: x, host: h}] }`,                       // missing user/cmd
		`notifications: { telegram: [{name: x, bot_token: T}] }`,             // missing chat_id
	}
	for i, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	src := `nut: { unknown_field: 42 }`
	if _, err := Parse([]byte(src)); err == nil {
		t.Error("expected unknown-field error")
	}
}

func TestStatusIntervalTooSmall(t *testing.T) {
	src := `monitor: { status_interval: 100ms }`
	if _, err := Parse([]byte(src)); err == nil {
		t.Error("expected too-small interval error")
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, []byte(minimal), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.NUT.UPS != "ups" {
		t.Errorf("UPS = %q", c.NUT.UPS)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/ups-client.yaml"); err == nil {
		t.Error("expected error")
	}
}

func TestMonitorRuntimeConfig(t *testing.T) {
	c, _ := Parse([]byte(minimal))
	rc := c.MonitorRuntimeConfig()
	if rc.UPS != "ups" || rc.StatusInterval != 2*time.Second {
		t.Errorf("rt config: %+v", rc)
	}
}
