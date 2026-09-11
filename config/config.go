// Package config loads ups-client's YAML configuration file and converts it
// into the typed Config consumed by main.
package config

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mikispag/ups-client/monitor"
	"github.com/mikispag/ups-client/notifier"
	"gopkg.in/yaml.v3"
)

// Config is the top-level YAML schema.
type Config struct {
	NUT           NUTConfig           `yaml:"nut"`
	Monitor       MonitorConfig       `yaml:"monitor"`
	Notifications NotificationsConfig `yaml:"notifications"`
}

// NUTConfig describes how to reach the upsd instance.
type NUTConfig struct {
	Address  string        `yaml:"address"`
	UPS      string        `yaml:"ups"`
	Username string        `yaml:"username"`
	Password string        `yaml:"password"`
	Timeout  time.Duration `yaml:"timeout"`
	TLS      *TLSConfig    `yaml:"tls,omitempty"`
}

// TLSConfig opts the connection into STARTTLS.
type TLSConfig struct {
	Enable             bool   `yaml:"enable"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	CAFile             string `yaml:"ca_file"`
	ServerName         string `yaml:"server_name"`
}

// MonitorConfig controls the monitor loop.
type MonitorConfig struct {
	StatusInterval   time.Duration `yaml:"status_interval"`
	SnapshotInterval time.Duration `yaml:"snapshot_interval"`
	NoCommThreshold  time.Duration `yaml:"nocomm_threshold"`
	ReplBattDebounce time.Duration `yaml:"replbatt_debounce"`
	AlarmDebounce    time.Duration `yaml:"alarm_debounce"`
	ReconnectBackoff time.Duration `yaml:"reconnect_backoff"`
}

// NotificationsConfig groups all notifier targets.
type NotificationsConfig struct {
	Shell    []ShellTarget    `yaml:"shell,omitempty"`
	Webhook  []WebhookTarget  `yaml:"webhook,omitempty"`
	SSH      []SSHTarget      `yaml:"ssh,omitempty"`
	Telegram []TelegramTarget `yaml:"telegram,omitempty"`
}

// EventFilter is the YAML embed of the per-target event allowlist.
type EventFilter struct {
	Events []string `yaml:"events,omitempty"`
}

// ShellTarget mirrors notifier.ShellTarget for YAML.
type ShellTarget struct {
	Name        string            `yaml:"name"`
	Command     string            `yaml:"command"`
	Args        []string          `yaml:"args"`
	Env         map[string]string `yaml:"env"`
	Timeout     time.Duration     `yaml:"timeout"`
	EventFilter `yaml:",inline"`
}

// WebhookTarget mirrors notifier.WebhookTarget for YAML.
type WebhookTarget struct {
	Name               string            `yaml:"name"`
	URL                string            `yaml:"url"`
	Method             string            `yaml:"method"`
	Headers            map[string]string `yaml:"headers"`
	Body               string            `yaml:"body"`
	Timeout            time.Duration     `yaml:"timeout"`
	InsecureSkipVerify bool              `yaml:"insecure_skip_verify"`
	EventFilter        `yaml:",inline"`
}

// SSHTarget mirrors notifier.SSHTarget for YAML.
type SSHTarget struct {
	Name                  string        `yaml:"name"`
	Host                  string        `yaml:"host"`
	Port                  int           `yaml:"port"`
	User                  string        `yaml:"user"`
	Password              string        `yaml:"password"`
	PrivateKeyFile        string        `yaml:"private_key_file"`
	PrivateKeyPassphrase  string        `yaml:"private_key_passphrase"`
	KnownHostsFile        string        `yaml:"known_hosts_file"`
	InsecureIgnoreHostKey bool          `yaml:"insecure_ignore_host_key"`
	Command               string        `yaml:"command"`
	Timeout               time.Duration `yaml:"timeout"`
	EventFilter           `yaml:",inline"`
}

// TelegramTarget mirrors notifier.TelegramTarget for YAML.
type TelegramTarget struct {
	Name        string        `yaml:"name"`
	BotToken    string        `yaml:"bot_token"`
	ChatID      string        `yaml:"chat_id"`
	Message     string        `yaml:"message"`
	ParseMode   string        `yaml:"parse_mode"`
	APIBase     string        `yaml:"api_base"`
	Timeout     time.Duration `yaml:"timeout"`
	EventFilter `yaml:",inline"`
}

// Load parses the YAML file at path, fills in defaults, and validates.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse loads a Config from raw YAML bytes (useful for tests).
func Parse(data []byte) (*Config, error) {
	var c Config
	c.applyDefaults()
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("parse config: expected exactly one YAML document")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.NUT.Address == "" {
		c.NUT.Address = "127.0.0.1:3493"
	}
	if c.NUT.UPS == "" {
		c.NUT.UPS = "ups"
	}
	if c.NUT.Timeout == 0 {
		c.NUT.Timeout = 5 * time.Second
	}
	if c.Monitor.StatusInterval == 0 {
		c.Monitor.StatusInterval = 2 * time.Second
	}
	if c.Monitor.SnapshotInterval == 0 {
		c.Monitor.SnapshotInterval = 30 * time.Second
	}
	if c.Monitor.NoCommThreshold == 0 {
		c.Monitor.NoCommThreshold = 60 * time.Second
	}
	if c.Monitor.ReplBattDebounce == 0 {
		c.Monitor.ReplBattDebounce = 600 * time.Second
	}
	if c.Monitor.AlarmDebounce == 0 {
		c.Monitor.AlarmDebounce = 60 * time.Second
	}
	if c.Monitor.ReconnectBackoff == 0 {
		c.Monitor.ReconnectBackoff = time.Second
	}
}

func (c *Config) validate() error {
	address := c.NUT.Address
	host := address
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		host = address[1 : len(address)-1]
	}
	if _, err := netip.ParseAddr(host); err == nil || !strings.Contains(address, ":") {
		address = net.JoinHostPort(host, "3493")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || !validHost(host) || !validPort(port) {
		return fmt.Errorf("nut.address: expected a host with an optional valid TCP port")
	}
	if c.NUT.UPS == "" {
		return fmt.Errorf("nut.ups must not be empty")
	}
	for _, value := range []string{c.NUT.UPS, c.NUT.Username, c.NUT.Password} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("nut: UPS and credentials must not contain line breaks or NUL")
		}
	}
	if c.NUT.Password != "" && c.NUT.Username == "" {
		return fmt.Errorf("nut.username is required when password is set")
	}
	// Zero disables NOCOMM or debounce; polling and I/O need positive durations.
	for _, v := range []struct {
		name string
		d    time.Duration
		min  time.Duration
		max  time.Duration
	}{
		{"monitor.status_interval", c.Monitor.StatusInterval, 500 * time.Millisecond, 5 * time.Minute},
		{"monitor.snapshot_interval", c.Monitor.SnapshotInterval, time.Second, 30 * time.Minute},
		{"monitor.nocomm_threshold", c.Monitor.NoCommThreshold, 0, time.Hour},
		{"monitor.replbatt_debounce", c.Monitor.ReplBattDebounce, 0, 24 * time.Hour},
		{"monitor.alarm_debounce", c.Monitor.AlarmDebounce, 0, time.Hour},
		{"monitor.reconnect_backoff", c.Monitor.ReconnectBackoff, 100 * time.Millisecond, time.Minute},
		{"nut.timeout", c.NUT.Timeout, 100 * time.Millisecond, time.Minute},
	} {
		if v.d < v.min {
			return fmt.Errorf("%s must be >= %s, got %s", v.name, v.min, v.d)
		}
		if v.d > v.max {
			return fmt.Errorf("%s must be <= %s, got %s", v.name, v.max, v.d)
		}
	}
	known := allEventNames()
	check := func(target string, timeout time.Duration, ev []string, templates ...string) error {
		if timeout < 0 {
			return fmt.Errorf("%s: timeout must be >= 0", target)
		}
		for _, raw := range templates {
			if err := notifier.ValidateTemplate(target, raw); err != nil {
				return err
			}
		}
		for _, e := range ev {
			up := strings.ToUpper(strings.TrimSpace(e))
			if _, ok := known[up]; !ok {
				return fmt.Errorf("%s: unknown event %q (known: %s)", target, e, strings.Join(sortedKeys(known), ", "))
			}
		}
		return nil
	}
	for i, t := range c.Notifications.Shell {
		if t.Command == "" {
			return fmt.Errorf("shell[%d]: command is required", i)
		}
		for _, arg := range append([]string{t.Command}, t.Args...) {
			if strings.ContainsRune(arg, '\x00') {
				return fmt.Errorf("shell[%d]: command and args must not contain NUL", i)
			}
		}
		for key, value := range t.Env {
			if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
				return fmt.Errorf("shell[%d]: invalid environment entry", i)
			}
		}
		if err := check(fmt.Sprintf("shell[%d]", i), t.Timeout, t.Events, t.Args...); err != nil {
			return err
		}
	}
	for i, t := range c.Notifications.Webhook {
		if t.URL == "" {
			return fmt.Errorf("webhook[%d]: url is required", i)
		}
		target := fmt.Sprintf("webhook[%d]", i)
		templates := []string{t.URL, t.Body}
		headers := make(map[string]bool, len(t.Headers))
		for key, header := range t.Headers {
			if !validHeaderName(key) {
				return fmt.Errorf("%s: invalid HTTP header name", target)
			}
			canonical := http.CanonicalHeaderKey(key)
			if headers[canonical] {
				return fmt.Errorf("%s: duplicate HTTP header name (case insensitive)", target)
			}
			headers[canonical] = true
			if !strings.Contains(header, "{{") && strings.IndexFunc(header, func(r rune) bool {
				return (r < 32 && r != '\t') || r == 127
			}) >= 0 {
				return fmt.Errorf("%s: invalid HTTP header value", target)
			}
			templates = append(templates, header)
		}
		if err := check(target, t.Timeout, t.Events, templates...); err != nil {
			return err
		}
		if !strings.Contains(t.URL, "{{") {
			if err := validateHTTPURL(target+".url", t.URL); err != nil {
				return err
			}
		}
		if _, err := http.NewRequest(strings.ToUpper(strings.TrimSpace(t.Method)), "https://example.invalid", nil); err != nil {
			return fmt.Errorf("%s: invalid HTTP method", target)
		}
	}
	for i, t := range c.Notifications.SSH {
		if t.Host == "" || t.User == "" || t.Command == "" {
			return fmt.Errorf("ssh[%d]: host, user and command are required", i)
		}
		if !validHost(t.Host) {
			return fmt.Errorf("ssh[%d]: host must be a hostname or unbracketed IP address; use port separately", i)
		}
		if t.Password == "" && t.PrivateKeyFile == "" {
			return fmt.Errorf("ssh[%d]: set either password or private_key_file", i)
		}
		if t.Port < 0 || t.Port > 65535 {
			return fmt.Errorf("ssh[%d]: port must be 0 (default) or between 1 and 65535", i)
		}
		if err := check(fmt.Sprintf("ssh[%d]", i), t.Timeout, t.Events, t.Command); err != nil {
			return err
		}
	}
	for i, t := range c.Notifications.Telegram {
		if t.BotToken == "" || t.ChatID == "" {
			return fmt.Errorf("telegram[%d]: bot_token and chat_id are required", i)
		}
		target := fmt.Sprintf("telegram[%d]", i)
		if strings.ContainsAny(t.BotToken, "/?# \t\r\n\x00") {
			return fmt.Errorf("%s: bot_token contains an invalid URL path character", target)
		}
		if t.APIBase != "" {
			if err := validateHTTPURL(target+".api_base", t.APIBase); err != nil {
				return err
			}
			u, _ := url.Parse(t.APIBase) // validateHTTPURL already parsed it.
			if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
				return fmt.Errorf("%s: api_base must not contain a query or fragment", target)
			}
		}
		switch t.ParseMode {
		case "", "Markdown", "MarkdownV2", "HTML":
		default:
			return fmt.Errorf("%s: parse_mode must be Markdown, MarkdownV2, HTML or empty", target)
		}
		if err := check(target, t.Timeout, t.Events, t.Message); err != nil {
			return err
		}
	}
	return nil
}

func validateHTTPURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		// URLs may contain credentials; report the field without its value.
		return fmt.Errorf("%s: expected an absolute HTTP(S) URL", field)
	}
	if !validHost(u.Hostname()) || (u.Port() != "" && !validPort(u.Port())) {
		return fmt.Errorf("%s: invalid HTTP(S) host or port", field)
	}
	return nil
}

func validHost(host string) bool {
	if host == "" || strings.IndexFunc(host, func(r rune) bool {
		return r <= 32 || r == 127 || strings.ContainsRune("/\\@[]", r)
	}) >= 0 {
		return false
	}
	if strings.Contains(host, ":") {
		_, err := netip.ParseAddr(host)
		return err == nil
	}
	return true
}

func validPort(port string) bool {
	n, err := net.LookupPort("tcp", port)
	return err == nil && n > 0
}

func validHeaderName(name string) bool {
	return name != "" && strings.IndexFunc(name, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", r))
	}) < 0
}

func allEventNames() map[string]struct{} {
	out := make(map[string]struct{})
	for _, k := range monitor.AllEventKinds() {
		out[strings.ToUpper(string(k))] = struct{}{}
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// BuildNotifiers materializes the configured targets into notifier.Notifier
// instances ready to be passed to a Dispatcher.
func (c *Config) BuildNotifiers() []notifier.Notifier {
	var ns []notifier.Notifier
	for _, t := range c.Notifications.Shell {
		ns = append(ns, &notifier.ShellTarget{
			Label:   t.Name,
			Command: t.Command,
			Args:    t.Args,
			Env:     t.Env,
			Timeout: t.Timeout,
			Filter:  notifier.Filter{Events: t.Events},
		})
	}
	for _, t := range c.Notifications.Webhook {
		ns = append(ns, &notifier.WebhookTarget{
			Label:              t.Name,
			URL:                t.URL,
			Method:             t.Method,
			Headers:            t.Headers,
			Body:               t.Body,
			Timeout:            t.Timeout,
			InsecureSkipVerify: t.InsecureSkipVerify,
			Filter:             notifier.Filter{Events: t.Events},
		})
	}
	for _, t := range c.Notifications.SSH {
		ns = append(ns, &notifier.SSHTarget{
			Label:                 t.Name,
			Host:                  t.Host,
			Port:                  t.Port,
			User:                  t.User,
			Password:              t.Password,
			PrivateKeyFile:        t.PrivateKeyFile,
			PrivateKeyPassphrase:  t.PrivateKeyPassphrase,
			KnownHostsFile:        t.KnownHostsFile,
			InsecureIgnoreHostKey: t.InsecureIgnoreHostKey,
			Command:               t.Command,
			Timeout:               t.Timeout,
			Filter:                notifier.Filter{Events: t.Events},
		})
	}
	for _, t := range c.Notifications.Telegram {
		ns = append(ns, &notifier.TelegramTarget{
			Label:     t.Name,
			BotToken:  t.BotToken,
			ChatID:    t.ChatID,
			Message:   t.Message,
			ParseMode: t.ParseMode,
			APIBase:   t.APIBase,
			Timeout:   t.Timeout,
			Filter:    notifier.Filter{Events: t.Events},
		})
	}
	return ns
}

// MonitorConfig converts the YAML monitor block into the runtime monitor.Config.
func (c *Config) MonitorRuntimeConfig() monitor.Config {
	return monitor.Config{
		UPS:              c.NUT.UPS,
		StatusInterval:   c.Monitor.StatusInterval,
		SnapshotInterval: c.Monitor.SnapshotInterval,
		NoCommThreshold:  c.Monitor.NoCommThreshold,
		ReplBattDebounce: c.Monitor.ReplBattDebounce,
		AlarmDebounce:    c.Monitor.AlarmDebounce,
		ReconnectBackoff: c.Monitor.ReconnectBackoff,
	}
}
