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
		`notifications: { ssh: [{name: x, host: h}] }`,                      // missing user/cmd
		`notifications: { telegram: [{name: x, bot_token: T}] }`,            // missing chat_id
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

func TestExplicitZeroThresholds(t *testing.T) {
	c, err := Parse([]byte(`monitor: {nocomm_threshold: 0s, replbatt_debounce: 0s, alarm_debounce: 0s}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Monitor.NoCommThreshold != 0 || c.Monitor.ReplBattDebounce != 0 || c.Monitor.AlarmDebounce != 0 {
		t.Fatalf("explicit zero thresholds replaced by defaults: %+v", c.Monitor)
	}
	defaults, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Monitor.NoCommThreshold != time.Minute || defaults.Monitor.ReplBattDebounce != 10*time.Minute || defaults.Monitor.AlarmDebounce != time.Minute {
		t.Fatalf("omitted thresholds must retain defaults: %+v", defaults.Monitor)
	}
}

func TestParseRejectsUnusableConfig(t *testing.T) {
	cases := map[string]string{
		"extra document":          "{}\n---\nnotifications: {}",
		"empty extra document":    "{}\n---\n",
		"shell timeout":           `notifications: {shell: [{command: /bin/true, timeout: -1s}]}`,
		"webhook timeout":         `notifications: {webhook: [{url: https://example.com, timeout: -1s}]}`,
		"ssh timeout":             `notifications: {ssh: [{host: h, user: u, password: p, command: c, timeout: -1s}]}`,
		"telegram timeout":        `notifications: {telegram: [{bot_token: T, chat_id: C, timeout: -1s}]}`,
		"shell template":          `notifications: {shell: [{command: /bin/true, args: ["{{"]}]}`,
		"webhook body template":   `notifications: {webhook: [{url: https://example.com, body: "{{"}]}`,
		"webhook URL template":    `notifications: {webhook: [{url: "https://example.com/{{"}]}`,
		"webhook header template": `notifications: {webhook: [{url: https://example.com, headers: {Title: "{{"}}]}`,
		"ssh template":            `notifications: {ssh: [{host: h, user: u, password: p, command: "{{"}]}`,
		"telegram template":       `notifications: {telegram: [{bot_token: T, chat_id: C, message: "{{"}]}`,
		"webhook scheme":          `notifications: {webhook: [{url: ftp://example.com}]}`,
		"webhook relative URL":    `notifications: {webhook: [{url: /notify}]}`,
		"webhook method":          `notifications: {webhook: [{url: https://example.com, method: "GET POST"}]}`,
		"telegram scheme":         `notifications: {telegram: [{bot_token: T, chat_id: C, api_base: ftp://example.com}]}`,
		"telegram parse mode":     `notifications: {telegram: [{bot_token: T, chat_id: C, parse_mode: typo}]}`,
		"ssh negative port":       `notifications: {ssh: [{host: h, port: -1, user: u, password: p, command: c}]}`,
		"ssh large port":          `notifications: {ssh: [{host: h, port: 65536, user: u, password: p, command: c}]}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestExampleConfig(t *testing.T) {
	if _, err := Load("../ups-client.example.yaml"); err != nil {
		t.Fatal(err)
	}
}

func TestParseRejectsInvalidTransportSettings(t *testing.T) {
	cases := map[string]string{
		"duplicate HTTP header":     `notifications: {webhook: [{url: "https://example.com", headers: {Authorization: first, authorization: second}}]}`,
		"empty NUT address":         `nut: {address: ""}`,
		"missing NUT host":          `nut: {address: ":3493"}`,
		"invalid NUT port":          `nut: {address: "localhost:65536"}`,
		"invalid NUT host":          `nut: {address: "http://localhost:3493"}`,
		"empty UPS":                 `nut: {ups: ""}`,
		"NUT protocol delimiter":    `nut: {password: "secret\nLOGOUT"}`,
		"password without username": `nut: {password: secret}`,
		"shell command NUL":         `notifications: {shell: [{command: "true\0"}]}`,
		"shell argument NUL":        `notifications: {shell: [{command: /bin/true, args: ["a\0b"]}]}`,
		"shell environment name":    `notifications: {shell: [{command: /bin/true, env: {"A=B": value}}]}`,
		"shell environment NUL":     `notifications: {shell: [{command: /bin/true, env: {A: "a\0b"}}]}`,
		"webhook port":              `notifications: {webhook: [{url: "https://example.com:65536/path"}]}`,
		"webhook header name":       `notifications: {webhook: [{url: "https://example.com", headers: {"Bad Header": value}}]}`,
		"webhook header value":      `notifications: {webhook: [{url: "https://example.com", headers: {Authorization: "secret\r\nInjected: value"}}]}`,
		"SSH embedded port":         `notifications: {ssh: [{host: "example.com:22", user: u, password: p, command: c}]}`,
		"Telegram query":            `notifications: {telegram: [{bot_token: T, chat_id: C, api_base: "https://example.com?key=secret"}]}`,
		"Telegram fragment":         `notifications: {telegram: [{bot_token: T, chat_id: C, api_base: "https://example.com#fragment"}]}`,
		"Telegram token delimiter":  `notifications: {telegram: [{bot_token: "T/secret", chat_id: C}]}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(src))
			if err == nil {
				t.Fatal("expected invalid transport settings to fail validation")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("validation error leaked a credential: %v", err)
			}
		})
	}
}

func TestParseValidTransportSettings(t *testing.T) {
	for _, address := range []string{"localhost", "127.0.0.1:3493", "::1", "[::1]", "[::1]:3493", "[fe80::1%eth0]:3493"} {
		t.Run(address, func(t *testing.T) {
			if _, err := Parse([]byte("nut: {address: '" + address + "'}")); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := Parse([]byte(`notifications: {ssh: [{host: "::1", user: u, password: p, command: c}], webhook: [{url: "https://example.com:8443", headers: {Title: "{{if .Alarm}}Alert\n{{end}}"}}]}`)); err != nil {
		t.Fatal(err)
	}
}
