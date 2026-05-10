// Package config loads ups-client's YAML configuration file and converts it
// into the typed Config consumed by main.
package config

import (
	"fmt"
	"os"
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
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
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
	if c.Monitor.ReconnectBackoff == 0 {
		c.Monitor.ReconnectBackoff = time.Second
	}
}

func (c *Config) validate() error {
	if c.Monitor.StatusInterval < 500*time.Millisecond {
		return fmt.Errorf("monitor.status_interval must be >= 500ms, got %s", c.Monitor.StatusInterval)
	}
	known := allEventNames()
	check := func(target string, ev []string) error {
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
		if err := check(fmt.Sprintf("shell[%d]", i), t.Events); err != nil {
			return err
		}
	}
	for i, t := range c.Notifications.Webhook {
		if t.URL == "" {
			return fmt.Errorf("webhook[%d]: url is required", i)
		}
		if err := check(fmt.Sprintf("webhook[%d]", i), t.Events); err != nil {
			return err
		}
	}
	for i, t := range c.Notifications.SSH {
		if t.Host == "" || t.User == "" || t.Command == "" {
			return fmt.Errorf("ssh[%d]: host, user and command are required", i)
		}
		if t.Password == "" && t.PrivateKeyFile == "" {
			return fmt.Errorf("ssh[%d]: set either password or private_key_file", i)
		}
		if err := check(fmt.Sprintf("ssh[%d]", i), t.Events); err != nil {
			return err
		}
	}
	for i, t := range c.Notifications.Telegram {
		if t.BotToken == "" || t.ChatID == "" {
			return fmt.Errorf("telegram[%d]: bot_token and chat_id are required", i)
		}
		if err := check(fmt.Sprintf("telegram[%d]", i), t.Events); err != nil {
			return err
		}
	}
	return nil
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
	// not strictly necessary, but stable error messages help users debug
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
		ReconnectBackoff: c.Monitor.ReconnectBackoff,
	}
}
