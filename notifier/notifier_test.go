package notifier

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mikispag/ups-client/monitor"
)

type fakeNotifier struct {
	name    string
	filter  Filter
	calls   atomic.Int32
	err     error
	gotKind monitor.EventKind
	mu      sync.Mutex
}

func (f *fakeNotifier) Name() string                { return f.name }
func (f *fakeNotifier) Match(e monitor.Event) bool  { return f.filter.Match(e.Kind) }
func (f *fakeNotifier) Notify(_ context.Context, e monitor.Event) error {
	f.calls.Add(1)
	f.mu.Lock()
	f.gotKind = e.Kind
	f.mu.Unlock()
	return f.err
}

func sampleEvent(k monitor.EventKind) monitor.Event {
	return monitor.Event{
		Kind: k,
		Snapshot: monitor.Snapshot{
			UPS:    "ups",
			Status: "OL",
			Tokens: []string{"OL"},
			Time:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			Vars: map[string]string{
				"ups.status":      "OL",
				"battery.charge":  "98",
				"battery.runtime": "3600",
				"input.voltage":   "230",
				"output.voltage":  "230",
				"ups.load":        "12",
				"device.model":    "Back-UPS BX2200MI",
				"device.serial":   "SN12345",
			},
		},
		Previous: monitor.Snapshot{Status: "OB"},
		Message:  "test",
	}
}

func TestFilterMatch(t *testing.T) {
	f := Filter{}
	if !f.Match(monitor.EventOnline) {
		t.Error("empty filter must match all")
	}
	f = Filter{Events: []string{"online", " OnBatt "}}
	if !f.Match(monitor.EventOnline) {
		t.Error("case-insensitive match for ONLINE failed")
	}
	if !f.Match(monitor.EventOnBatt) {
		t.Error("trimmed match for ONBATT failed")
	}
	if f.Match(monitor.EventLowBatt) {
		t.Error("LOWBATT should not match")
	}
}

func TestDispatcherFanout(t *testing.T) {
	a := &fakeNotifier{name: "a"}
	b := &fakeNotifier{name: "b", filter: Filter{Events: []string{"ONBATT"}}}
	c := &fakeNotifier{name: "c", err: errors.New("boom")} // must not affect others
	d := NewDispatcher(nil, a, b, c)
	d.Dispatch(context.Background(), sampleEvent(monitor.EventOnline))
	if a.calls.Load() != 1 {
		t.Errorf("a calls = %d", a.calls.Load())
	}
	if b.calls.Load() != 0 {
		t.Errorf("b should be filtered out, calls = %d", b.calls.Load())
	}
	if c.calls.Load() != 1 {
		t.Errorf("c calls = %d", c.calls.Load())
	}
}

func TestTemplateDataAndEnv(t *testing.T) {
	td := NewTemplateData(sampleEvent(monitor.EventLowBatt))
	if td.Event != "LOWBATT" || td.UPS != "ups" || td.BatteryCharge != "98" {
		t.Errorf("TemplateData: %+v", td)
	}
	env := td.Env()
	join := strings.Join(env, "\n")
	for _, want := range []string{
		"UPS_EVENT=LOWBATT",
		"UPS_NAME=ups",
		"UPS_STATUS=OL",
		"UPS_PREVIOUS_STATUS=OB",
		"UPS_BATTERY_CHARGE=98",
		"UPS_BATTERY_RUNTIME=3600",
		"UPS_INPUT_VOLTAGE=230",
		"UPS_OUTPUT_VOLTAGE=230",
		"UPS_LOAD=12",
		"UPS_DEVICE_MODEL=Back-UPS BX2200MI",
		"UPS_DEVICE_SERIAL=SN12345",
		"UPS_TIMESTAMP=2026-01-02T03:04:05Z",
	} {
		if !strings.Contains(join, want) {
			t.Errorf("env missing %q in:\n%s", want, join)
		}
	}
}

func TestRenderTemplate(t *testing.T) {
	td := NewTemplateData(sampleEvent(monitor.EventOnBatt))
	out, err := renderTemplate("test", "{{.Event}} on {{.UPS}}: {{.Status}}", td)
	if err != nil {
		t.Fatal(err)
	}
	if out != "ONBATT on ups: OL" {
		t.Errorf("render = %q", out)
	}

	out, err = renderTemplate("empty", "", td)
	if err != nil || out != "" {
		t.Errorf("empty template: %q %v", out, err)
	}

	if _, err := renderTemplate("bad", "{{.Missing", td); err == nil {
		t.Error("expected parse error")
	}
}

func TestPerTargetMatch(t *testing.T) {
	// Each target type embeds Filter via its own Match method. Verify the
	// dispatch thunks delegate correctly.
	wantOnline := monitor.Event{Kind: monitor.EventOnline}
	wantOnBatt := monitor.Event{Kind: monitor.EventOnBatt}

	sh := &ShellTarget{Filter: Filter{Events: []string{"ONLINE"}}}
	if !sh.Match(wantOnline) || sh.Match(wantOnBatt) {
		t.Errorf("ShellTarget.Match wrong")
	}
	wh := &WebhookTarget{Filter: Filter{Events: []string{"ONLINE"}}}
	if !wh.Match(wantOnline) || wh.Match(wantOnBatt) {
		t.Errorf("WebhookTarget.Match wrong")
	}
	sn := &SSHTarget{Filter: Filter{Events: []string{"ONLINE"}}}
	if !sn.Match(wantOnline) || sn.Match(wantOnBatt) {
		t.Errorf("SSHTarget.Match wrong")
	}
	tg := &TelegramTarget{Filter: Filter{Events: []string{"ONLINE"}}}
	if !tg.Match(wantOnline) || tg.Match(wantOnBatt) {
		t.Errorf("TelegramTarget.Match wrong")
	}
}

func TestDefaultMessage(t *testing.T) {
	td := NewTemplateData(sampleEvent(monitor.EventOnBatt))
	msg := defaultMessage(td)
	if !strings.Contains(msg, "ONBATT") || !strings.Contains(msg, "ups") || !strings.Contains(msg, "98") {
		t.Errorf("defaultMessage = %q", msg)
	}
}
