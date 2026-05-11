package monitor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mikispag/ups-client/nut"
)

// fakeConn is a scriptable Conn. statusSeq is consumed one entry per
// GetVar(ups, "ups.status") call; once exhausted, the last entry repeats.
// listVars is returned for every ListVars call. If failGet is non-nil, it is
// returned instead of the next status.
type fakeConn struct {
	mu        sync.Mutex
	statusSeq []string
	idx       int
	listVars  map[string]string
	failGet   error
	failList  error
	closed    bool
}

func (f *fakeConn) GetVar(ups, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet != nil {
		err := f.failGet
		return "", err
	}
	if name != "ups.status" {
		return f.listVars[name], nil
	}
	if len(f.statusSeq) == 0 {
		return "OL", nil
	}
	if f.idx >= len(f.statusSeq) {
		return f.statusSeq[len(f.statusSeq)-1], nil
	}
	s := f.statusSeq[f.idx]
	f.idx++
	return s, nil
}

func (f *fakeConn) ListVars(ups string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failList != nil {
		return nil, f.failList
	}
	out := map[string]string{"ups.status": "OL"}
	for k, v := range f.listVars {
		out[k] = v
	}
	if len(f.statusSeq) > 0 {
		out["ups.status"] = f.statusSeq[0]
	}
	return out, nil
}

func (f *fakeConn) Close() error { f.closed = true; return nil }

// recordingSink stores every event for later inspection.
type recordingSink struct {
	mu     sync.Mutex
	events []Event
}

func (r *recordingSink) Dispatch(_ context.Context, e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}
func (r *recordingSink) Kinds() []EventKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]EventKind, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func newMonitorWithStatus(t *testing.T, statuses ...string) (*Monitor, *recordingSink, *fakeConn) {
	t.Helper()
	fc := &fakeConn{statusSeq: statuses, listVars: map[string]string{"battery.charge": "100"}}
	rs := &recordingSink{}
	cfg := Config{
		UPS:              "ups",
		StatusInterval:   10 * time.Millisecond,
		SnapshotInterval: time.Hour, // disable in tests
		ReconnectBackoff: 5 * time.Millisecond,
	}
	m := New(cfg, func(ctx context.Context) (Conn, error) { return fc, nil }, rs, nil)
	return m, rs, fc
}

// runFor steps the monitor through one connect + n status polls and returns.
func runFor(t *testing.T, m *Monitor, polls int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	// Connect + len(statusSeq) ticks; give a generous wall-clock budget.
	timeout := time.After(time.Duration(polls+5) * 50 * time.Millisecond)
	deadline := time.NewTimer(time.Duration(polls+2) * m.cfg.StatusInterval)
	<-deadline.C
	cancel()
	select {
	case <-done:
	case <-timeout:
		t.Fatal("monitor did not stop in time")
	}
}

func TestMonitorEmitsStartup(t *testing.T) {
	m, rs, _ := newMonitorWithStatus(t, "OL")
	runFor(t, m, 1)
	if len(rs.events) == 0 || rs.events[0].Kind != EventStartup {
		t.Fatalf("first event = %v", rs.Kinds())
	}
}

func TestMonitorOnBattLowBattOnline(t *testing.T) {
	m, rs, _ := newMonitorWithStatus(t,
		"OL",                // initial connect
		"OL",                // first poll: no change
		"OB DISCHRG",        // power loss
		"OB DISCHRG LB",     // low battery
		"OL CHRG",           // power restored, charging
	)
	runFor(t, m, 5)

	kinds := rs.Kinds()
	want := []EventKind{EventStartup, EventOnBatt, EventLowBatt, EventOnline}
	if !containsInOrder(kinds, want) {
		t.Errorf("events = %v; want sequence containing %v", kinds, want)
	}
}

func TestMonitorBareLBOnOLIsSuppressed(t *testing.T) {
	// APC BX-series firmware spuriously asserts LB (and RB) during periodic
	// battery self-tests at full charge while on mains. The status string
	// looks like "OL LB RB". LOWBATT must NOT fire because there is no
	// shutdown coming — we are still online — and the alert would be
	// noise. ONBATT is also absent, so we know there's nothing real here.
	m, rs, _ := newMonitorWithStatus(t,
		"OL",
		"OL LB RB", // firmware glitch
		"OL",       // glitch clears
	)
	runFor(t, m, 3)
	for _, e := range rs.events {
		if e.Kind == EventLowBatt {
			t.Errorf("LOWBATT must not fire for bare LB on OL; events: %v", rs.Kinds())
		}
	}
}

func TestMonitorReplBattDebounce(t *testing.T) {
	// RB appears, then disappears within debounce window — must NOT emit.
	fc := &fakeConn{statusSeq: []string{"OL", "OL RB", "OL"}, listVars: map[string]string{}}
	rs := &recordingSink{}
	cfg := Config{
		UPS:              "ups",
		StatusInterval:   10 * time.Millisecond,
		SnapshotInterval: time.Hour,
		ReconnectBackoff: 5 * time.Millisecond,
		ReplBattDebounce: 500 * time.Millisecond,
	}
	m := New(cfg, func(ctx context.Context) (Conn, error) { return fc, nil }, rs, nil)
	runFor(t, m, 3)
	for _, e := range rs.events {
		if e.Kind == EventReplBatt {
			t.Errorf("REPLBATT should have been debounced; got %v", rs.Kinds())
		}
	}
}

func TestMonitorReplBattConfirmed(t *testing.T) {
	// RB persists past the debounce — should emit exactly once.
	fc := &fakeConn{statusSeq: []string{"OL RB"}, listVars: map[string]string{}}
	rs := &recordingSink{}
	cfg := Config{
		UPS:              "ups",
		StatusInterval:   10 * time.Millisecond,
		SnapshotInterval: time.Hour,
		ReconnectBackoff: 5 * time.Millisecond,
		ReplBattDebounce: 20 * time.Millisecond,
	}
	m := New(cfg, func(ctx context.Context) (Conn, error) { return fc, nil }, rs, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = m.Run(ctx)
	count := 0
	for _, e := range rs.events {
		if e.Kind == EventReplBatt {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 REPLBATT, got %d (%v)", count, rs.Kinds())
	}
}

func TestMonitorAlarmDebounce(t *testing.T) {
	// ALARM rises then clears within the debounce window — APC BX self-test
	// pattern. Neither ALARM nor NOTALARM must fire (NOTALARM without a
	// matching ALARM would be a confusing notification).
	fc := &fakeConn{statusSeq: []string{"OL", "OL ALARM", "OL"}, listVars: map[string]string{}}
	rs := &recordingSink{}
	cfg := Config{
		UPS:              "ups",
		StatusInterval:   10 * time.Millisecond,
		SnapshotInterval: time.Hour,
		ReconnectBackoff: 5 * time.Millisecond,
		AlarmDebounce:    500 * time.Millisecond,
	}
	m := New(cfg, func(ctx context.Context) (Conn, error) { return fc, nil }, rs, nil)
	runFor(t, m, 3)
	for _, e := range rs.events {
		if e.Kind == EventAlarm || e.Kind == EventNotAlarm {
			t.Errorf("ALARM/NOTALARM should have been debounced; got %v", rs.Kinds())
		}
	}
}

func TestMonitorAlarmConfirmedSurfacesReason(t *testing.T) {
	// ALARM persists past the debounce — should emit exactly once, and the
	// snapshot must carry ups.alarm so notifier templates can render the
	// actual reason instead of a bare "alarm" string.
	fc := &fakeConn{
		statusSeq: []string{"OL ALARM"},
		listVars:  map[string]string{"ups.alarm": "Replace battery"},
	}
	rs := &recordingSink{}
	cfg := Config{
		UPS:              "ups",
		StatusInterval:   10 * time.Millisecond,
		SnapshotInterval: time.Hour,
		ReconnectBackoff: 5 * time.Millisecond,
		AlarmDebounce:    20 * time.Millisecond,
	}
	m := New(cfg, func(ctx context.Context) (Conn, error) { return fc, nil }, rs, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = m.Run(ctx)
	count := 0
	var alarmEvent Event
	for _, e := range rs.events {
		if e.Kind == EventAlarm {
			count++
			alarmEvent = e
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 ALARM, got %d (%v)", count, rs.Kinds())
	}
	if got := alarmEvent.Snapshot.Vars["ups.alarm"]; got != "Replace battery" {
		t.Errorf("ALARM snapshot missing ups.alarm reason: got %q", got)
	}
}

func TestMonitorBypassEnterLeave(t *testing.T) {
	m, rs, _ := newMonitorWithStatus(t, "OL", "OL BYPASS", "OL")
	runFor(t, m, 3)
	kinds := rs.Kinds()
	if !containsInOrder(kinds, []EventKind{EventBypass, EventNotBypass}) {
		t.Errorf("expected BYPASS then NOTBYPASS in %v", kinds)
	}
}

func TestMonitorOverloadEdges(t *testing.T) {
	m, rs, _ := newMonitorWithStatus(t, "OL", "OL OVER", "OL")
	runFor(t, m, 3)
	if !containsInOrder(rs.Kinds(), []EventKind{EventOverload, EventNotOverload}) {
		t.Errorf("missing overload edges: %v", rs.Kinds())
	}
}

func TestMonitorFSDFires(t *testing.T) {
	m, rs, _ := newMonitorWithStatus(t, "OL", "OB DISCHRG LB FSD")
	runFor(t, m, 2)
	saw := false
	for _, e := range rs.events {
		if e.Kind == EventFSD {
			saw = true
		}
	}
	if !saw {
		t.Errorf("FSD not emitted: %v", rs.Kinds())
	}
}

func TestMonitorCommBadAndOK(t *testing.T) {
	calls := 0
	dialErr := errors.New("dial fail")
	fc := &fakeConn{statusSeq: []string{"OL"}}
	rs := &recordingSink{}
	cfg := Config{
		UPS:              "ups",
		StatusInterval:   10 * time.Millisecond,
		SnapshotInterval: time.Hour,
		ReconnectBackoff: 5 * time.Millisecond,
		NoCommThreshold:  0, // disable NOCOMM escalation in this test
	}
	dialer := func(ctx context.Context) (Conn, error) {
		calls++
		if calls == 1 {
			return nil, dialErr
		}
		return fc, nil
	}
	m := New(cfg, dialer, rs, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = m.Run(ctx)
	saw := map[EventKind]bool{}
	for _, e := range rs.events {
		saw[e.Kind] = true
	}
	if !saw[EventCommBad] {
		t.Errorf("expected COMMBAD: %v", rs.Kinds())
	}
	// On reconnect we emit COMMOK then STARTUP-like resync.
	if !saw[EventCommOK] {
		t.Errorf("expected COMMOK after reconnect: %v", rs.Kinds())
	}
}

func TestMonitorNoCommEscalation(t *testing.T) {
	dialErr := errors.New("dial fail")
	rs := &recordingSink{}
	cfg := Config{
		UPS:              "ups",
		StatusInterval:   10 * time.Millisecond,
		SnapshotInterval: time.Hour,
		ReconnectBackoff: 5 * time.Millisecond,
		NoCommThreshold:  30 * time.Millisecond,
	}
	m := New(cfg, func(ctx context.Context) (Conn, error) { return nil, dialErr }, rs, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = m.Run(ctx)
	count := 0
	for _, e := range rs.events {
		if e.Kind == EventNoComm {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 NOCOMM, got %d (%v)", count, rs.Kinds())
	}
}

func TestStaleDataIsTransient(t *testing.T) {
	if !nut.IsTransient(&nut.ProtocolError{Code: "DATA-STALE"}) {
		t.Error("DATA-STALE should be transient (sanity check from monitor pkg)")
	}
}

func TestAllEventKinds(t *testing.T) {
	if len(AllEventKinds()) < 20 {
		t.Errorf("AllEventKinds returned only %d", len(AllEventKinds()))
	}
}

func TestSinkFunc(t *testing.T) {
	called := false
	var s Sink = SinkFunc(func(_ context.Context, _ Event) { called = true })
	s.Dispatch(context.Background(), Event{Kind: EventOnline})
	if !called {
		t.Error("SinkFunc not invoked")
	}
}

func TestSnapshotGet(t *testing.T) {
	s := Snapshot{Vars: map[string]string{"ups.load": "42"}}
	if s.Get("ups.load") != "42" {
		t.Errorf("Get failed: %q", s.Get("ups.load"))
	}
	if s.Get("missing") != "" {
		t.Errorf("Get missing should be empty")
	}
}

func containsInOrder(haystack, needle []EventKind) bool {
	i := 0
	for _, h := range haystack {
		if i < len(needle) && h == needle[i] {
			i++
		}
	}
	return i == len(needle)
}
