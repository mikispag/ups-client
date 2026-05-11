// Package notifier delivers monitor.Event payloads to one or more sinks
// (shell command, generic webhook, SSH command, Telegram bot). Each sink
// runs concurrently, with its own per-target timeout and event filter.
package notifier

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/mikispag/ups-client/monitor"
)

// Notifier delivers a single monitor.Event somewhere. Implementations should
// honor ctx cancellation and respect their own timeout.
type Notifier interface {
	Name() string
	// Notify delivers e. Returning a non-nil error is logged but does not
	// halt other notifiers.
	Notify(ctx context.Context, e monitor.Event) error
	// Match reports whether this notifier wants to receive e (typically
	// based on event-kind filters).
	Match(e monitor.Event) bool
}

// Dispatcher implements monitor.Sink by fanning each event out to every
// matching notifier in parallel.
type Dispatcher struct {
	notifiers []Notifier
	log       *slog.Logger
}

// NewDispatcher builds a Dispatcher from the given notifiers. log may be nil.
func NewDispatcher(log *slog.Logger, notifiers ...Notifier) *Dispatcher {
	return &Dispatcher{notifiers: notifiers, log: log}
}

// Dispatch satisfies monitor.Sink.
func (d *Dispatcher) Dispatch(ctx context.Context, e monitor.Event) {
	var wg sync.WaitGroup
	for _, n := range d.notifiers {
		if !n.Match(e) {
			continue
		}
		wg.Add(1)
		go func(n Notifier) {
			defer wg.Done()
			if err := n.Notify(ctx, e); err != nil && d.log != nil {
				d.log.Error("notify",
					"notifier", n.Name(),
					"event", string(e.Kind),
					"err", err)
			}
		}(n)
	}
	wg.Wait()
}

// Filter is the per-notifier event filter shared by every target type. An
// empty Events list matches all events.
type Filter struct {
	Events []string
}

// Match reports whether kind is allowed through the filter (case-insensitive).
func (f Filter) Match(kind monitor.EventKind) bool {
	if len(f.Events) == 0 {
		return true
	}
	k := strings.ToUpper(string(kind))
	for _, ev := range f.Events {
		if strings.ToUpper(strings.TrimSpace(ev)) == k {
			return true
		}
	}
	return false
}

// TemplateData is the value passed to text/template-rendered fields. It also
// supplies env vars to shell targets via Env().
type TemplateData struct {
	Event           string
	Message         string
	UPS             string
	Status          string
	PreviousStatus  string
	Tokens          []string
	Time            time.Time
	BatteryCharge   string
	BatteryRuntime  string
	InputVoltage    string
	OutputVoltage   string
	UPSLoad         string
	DeviceModel     string
	DeviceSerial    string
	Alarm           string
	Vars            map[string]string
}

// NewTemplateData builds a TemplateData from a monitor.Event.
func NewTemplateData(e monitor.Event) TemplateData {
	td := TemplateData{
		Event:          string(e.Kind),
		Message:        e.Message,
		UPS:            e.Snapshot.UPS,
		Status:         e.Snapshot.Status,
		PreviousStatus: e.Previous.Status,
		Tokens:         e.Snapshot.Tokens,
		Time:           e.Snapshot.Time,
		Vars:           e.Snapshot.Vars,
	}
	if e.Snapshot.Vars != nil {
		td.BatteryCharge = e.Snapshot.Vars["battery.charge"]
		td.BatteryRuntime = e.Snapshot.Vars["battery.runtime"]
		td.InputVoltage = e.Snapshot.Vars["input.voltage"]
		td.OutputVoltage = e.Snapshot.Vars["output.voltage"]
		td.UPSLoad = e.Snapshot.Vars["ups.load"]
		td.DeviceModel = e.Snapshot.Vars["device.model"]
		td.DeviceSerial = e.Snapshot.Vars["device.serial"]
		td.Alarm = e.Snapshot.Vars["ups.alarm"]
	}
	return td
}

// Env returns environment variables suitable for inheritance by an exec'd
// process, with a UPS_ prefix on each key.
func (td TemplateData) Env() []string {
	env := []string{
		"UPS_EVENT=" + td.Event,
		"UPS_MESSAGE=" + td.Message,
		"UPS_NAME=" + td.UPS,
		"UPS_STATUS=" + td.Status,
		"UPS_PREVIOUS_STATUS=" + td.PreviousStatus,
		"UPS_TIMESTAMP=" + td.Time.Format(time.RFC3339),
	}
	add := func(k, v string) {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	add("UPS_BATTERY_CHARGE", td.BatteryCharge)
	add("UPS_BATTERY_RUNTIME", td.BatteryRuntime)
	add("UPS_INPUT_VOLTAGE", td.InputVoltage)
	add("UPS_OUTPUT_VOLTAGE", td.OutputVoltage)
	add("UPS_LOAD", td.UPSLoad)
	add("UPS_DEVICE_MODEL", td.DeviceModel)
	add("UPS_DEVICE_SERIAL", td.DeviceSerial)
	add("UPS_ALARM", td.Alarm)
	return env
}

// renderTemplate parses and executes tpl with td. An empty template yields "".
func renderTemplate(name, tpl string, td TemplateData) (string, error) {
	if tpl == "" {
		return "", nil
	}
	t, err := template.New(name).Option("missingkey=zero").Parse(tpl)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, td); err != nil {
		return "", fmt.Errorf("execute %s: %w", name, err)
	}
	return buf.String(), nil
}

// defaultMessage produces a fallback notification body when none is configured.
func defaultMessage(td TemplateData) string {
	parts := []string{fmt.Sprintf("[%s] UPS %s", td.Event, td.UPS)}
	if td.Status != "" {
		parts = append(parts, "status="+td.Status)
	}
	if td.BatteryCharge != "" {
		parts = append(parts, "charge="+td.BatteryCharge+"%")
	}
	if td.BatteryRuntime != "" {
		parts = append(parts, "runtime="+td.BatteryRuntime+"s")
	}
	return strings.Join(parts, " ")
}
