// Package monitor turns a stream of NUT ups.status snapshots into
// upsmon-style events (ONLINE, ONBATT, LOWBATT, FSD, REPLBATT, COMMBAD,
// COMMOK, NOCOMM, BYPASS, OVERLOAD, TRIM, BOOST, CAL, OFF, ALARM, plus the
// matching NOT* edges).
package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/mikispag/ups-client/nut"
)

// EventKind enumerates the events the monitor can emit. Names mirror upsmon's
// NOTIFYFLAG identifiers so existing operator playbooks are recognizable.
type EventKind string

const (
	EventStartup     EventKind = "STARTUP"
	EventOnline      EventKind = "ONLINE"
	EventOnBatt      EventKind = "ONBATT"
	EventLowBatt     EventKind = "LOWBATT"
	EventFSD         EventKind = "FSD"
	EventReplBatt    EventKind = "REPLBATT"
	EventCommBad     EventKind = "COMMBAD"
	EventCommOK      EventKind = "COMMOK"
	EventNoComm      EventKind = "NOCOMM"
	EventBypass      EventKind = "BYPASS"
	EventNotBypass   EventKind = "NOTBYPASS"
	EventOverload    EventKind = "OVERLOAD"
	EventNotOverload EventKind = "NOTOVERLOAD"
	EventTrim        EventKind = "TRIM"
	EventNotTrim     EventKind = "NOTTRIM"
	EventBoost       EventKind = "BOOST"
	EventNotBoost    EventKind = "NOTBOOST"
	EventCal         EventKind = "CAL"
	EventNotCal      EventKind = "NOTCAL"
	EventOff         EventKind = "OFF"
	EventNotOff      EventKind = "NOTOFF"
	EventAlarm       EventKind = "ALARM"
	EventNotAlarm    EventKind = "NOTALARM"
)

// AllEventKinds returns the canonical list of events emitted by the monitor.
// Useful for config validation.
func AllEventKinds() []EventKind {
	return []EventKind{
		EventStartup, EventOnline, EventOnBatt, EventLowBatt, EventFSD, EventReplBatt,
		EventCommBad, EventCommOK, EventNoComm,
		EventBypass, EventNotBypass, EventOverload, EventNotOverload,
		EventTrim, EventNotTrim, EventBoost, EventNotBoost,
		EventCal, EventNotCal, EventOff, EventNotOff, EventAlarm, EventNotAlarm,
	}
}

// Snapshot bundles everything we know about a UPS at one moment in time.
type Snapshot struct {
	UPS    string
	Status string
	Tokens []string
	Vars   map[string]string
	Time   time.Time
}

// Get returns the named NUT variable from the snapshot's bulk vars or "".
func (s Snapshot) Get(name string) string { return s.Vars[name] }

// Event is the value passed to notifiers when the monitor detects an
// upsmon-style state transition.
// FSD and LOWBATT are dispatched first when a snapshot contains multiple
// new conditions, ahead of communication recovery, startup, and other edges.
type Event struct {
	Kind     EventKind
	Snapshot Snapshot
	Previous Snapshot
	Message  string
}

// Conn is the subset of nut.Client behavior the monitor relies on. It is
// extracted so tests can swap in a fake without standing up a TCP server.
// Close must be safe concurrently with other methods and interrupt blocked IO.
type Conn interface {
	GetVar(ups, name string) (string, error)
	ListVars(ups string) (map[string]string, error)
	Close() error
}

// Dialer constructs a fresh Conn. The monitor calls it once at startup and
// again whenever it needs to reconnect after a comm-bad event.
type Dialer func(ctx context.Context) (Conn, error)

// Sink consumes events emitted by the monitor.
type Sink interface {
	Dispatch(ctx context.Context, e Event)
}

// SinkFunc adapts a plain function into a Sink.
type SinkFunc func(ctx context.Context, e Event)

func (f SinkFunc) Dispatch(ctx context.Context, e Event) { f(ctx, e) }

// Config controls poll cadence and event-detection behavior.
type Config struct {
	UPS              string
	StatusInterval   time.Duration // ups.status polling cadence
	SnapshotInterval time.Duration // bulk LIST VAR cadence (metadata refresh)
	NoCommThreshold  time.Duration // sustained comm-bad before NOCOMM fires
	ReplBattDebounce time.Duration // grace period to swallow APC BX RB flapping
	AlarmDebounce    time.Duration // grace period to swallow APC BX ALARM blips
	ReconnectBackoff time.Duration // initial reconnect backoff (caps at 30s)
}

// tokenEdge maps a status token to the events emitted when it enters or
// leaves the active set. An empty event name means "do not emit".
var tokenEdges = map[string]struct {
	enter, leave EventKind
}{
	"OL":     {EventOnline, ""},
	"OB":     {EventOnBatt, ""},
	"LB":     {EventLowBatt, ""},
	"FSD":    {EventFSD, ""},
	"BYPASS": {EventBypass, EventNotBypass},
	"OVER":   {EventOverload, EventNotOverload},
	"TRIM":   {EventTrim, EventNotTrim},
	"BOOST":  {EventBoost, EventNotBoost},
	"CAL":    {EventCal, EventNotCal},
	"OFF":    {EventOff, EventNotOff},
	"ALARM":  {EventAlarm, EventNotAlarm},
}

// Monitor maintains a long-lived connection to upsd, polls ups.status, diffs
// successive token sets, and emits Events. Reconnection is automatic.
type Monitor struct {
	cfg    Config
	dialer Dialer
	sink   Sink
	log    *slog.Logger

	conn      Conn
	prev      nut.Status
	last      Snapshot // most recent successful snapshot (for event payloads)
	started   bool
	stopClose func() bool

	commBad       bool
	commBadSince  time.Time
	noCommEmitted bool

	rbFirstSeen time.Time
	rbConfirmed bool

	alarmFirstSeen time.Time
	alarmConfirmed bool
}

// New constructs a Monitor with the given configuration, NUT dialer, and
// event sink. log may be nil to discard log output.
func New(cfg Config, dialer Dialer, sink Sink, log *slog.Logger) *Monitor {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.StatusInterval <= 0 {
		cfg.StatusInterval = 2 * time.Second
	}
	if cfg.SnapshotInterval <= 0 {
		cfg.SnapshotInterval = 30 * time.Second
	}
	if cfg.ReconnectBackoff <= 0 {
		cfg.ReconnectBackoff = time.Second
	}
	if cfg.ReconnectBackoff > 30*time.Second {
		cfg.ReconnectBackoff = 30 * time.Second
	}
	return &Monitor{cfg: cfg, dialer: dialer, sink: sink, log: log, last: Snapshot{UPS: cfg.UPS}}
}

// Run blocks, polling and emitting events until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) error {
	defer m.closeConn()
	statusTicker := time.NewTicker(m.cfg.StatusInterval)
	defer statusTicker.Stop()
	snapTicker := time.NewTicker(m.cfg.SnapshotInterval)
	defer snapTicker.Stop()

	backoff := m.cfg.ReconnectBackoff
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return nil
		}
		if m.conn == nil {
			if m.commBad {
				if !m.waitReconnect(ctx, backoff) {
					return nil
				}
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			if err := m.tryConnect(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				m.markCommBad(ctx, fmt.Sprintf("connect: %v", err))
				continue
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-statusTicker.C:
			if err := m.pollStatus(ctx); err != nil {
				m.handleConnErr(ctx, "status", err)
			} else {
				backoff = m.cfg.ReconnectBackoff
			}
		case <-snapTicker.C:
			if err := m.refreshVars(ctx); err != nil {
				m.handleConnErr(ctx, "snapshot", err)
			} else {
				backoff = m.cfg.ReconnectBackoff
			}
		}
	}
}

func (m *Monitor) waitReconnect(ctx context.Context, backoff time.Duration) bool {
	retry := time.NewTimer(backoff)
	defer retry.Stop()
	var noComm <-chan time.Time
	if !m.noCommEmitted && m.cfg.NoCommThreshold > 0 {
		timer := time.NewTimer(time.Until(m.commBadSince.Add(m.cfg.NoCommThreshold)))
		defer timer.Stop()
		noComm = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-retry.C:
			return true
		case <-noComm:
			m.markCommBad(ctx, "")
			noComm = nil
		}
	}
}

func (m *Monitor) closeConn() {
	if m.stopClose != nil {
		m.stopClose()
		m.stopClose = nil
	}
	if m.conn != nil {
		_ = m.conn.Close()
		m.conn = nil
	}
}

func (m *Monitor) tryConnect(ctx context.Context) error {
	c, err := m.dialer(ctx)
	if err != nil {
		return err
	}
	m.conn = c
	m.stopClose = context.AfterFunc(ctx, func() { _ = c.Close() })

	vars, err := m.conn.ListVars(m.cfg.UPS)
	if err != nil {
		m.closeConn()
		return err
	}
	rawStatus := vars["ups.status"]
	if rawStatus == "" {
		// Fall back to a focused GET if the bulk listing somehow omits it.
		s, gerr := m.conn.GetVar(m.cfg.UPS, "ups.status")
		if gerr != nil {
			m.closeConn()
			return gerr
		}
		rawStatus = s
		vars["ups.status"] = s
	}

	tokens := nut.ParseStatus(rawStatus)
	if len(tokens) == 0 {
		m.closeConn()
		return errors.New("empty ups.status")
	}
	snap := Snapshot{
		UPS:    m.cfg.UPS,
		Status: rawStatus,
		Tokens: tokens.Tokens(),
		Vars:   vars,
		Time:   time.Now(),
	}

	if m.commBad {
		// Restart pending debounce windows after an unobserved interval,
		// but preserve confirmed faults until we observe them clearing.
		m.rbFirstSeen = time.Time{}
		m.alarmFirstSeen = time.Time{}
	}

	// Include active conditions on first observation and any transitions
	// since the last successful observation after reconnecting.
	m.diffAndEmit(ctx, snap, tokens)
	m.last = snap
	m.prev = tokens
	m.commBad = false
	m.commBadSince = time.Time{}
	m.noCommEmitted = false
	return nil
}

func (m *Monitor) pollStatus(ctx context.Context) error {
	if m.conn == nil {
		return errors.New("not connected")
	}
	raw, err := m.conn.GetVar(m.cfg.UPS, "ups.status")
	if err != nil {
		return err
	}
	tokens := nut.ParseStatus(raw)
	if len(tokens) == 0 {
		return errors.New("empty ups.status")
	}
	snap := m.last
	snap.Status = raw
	snap.Tokens = tokens.Tokens()
	snap.Time = time.Now()
	if snap.Vars == nil {
		snap.Vars = map[string]string{}
	} else {
		// Avoid mutating the previous snapshot map.
		v := make(map[string]string, len(snap.Vars)+1)
		for k, val := range snap.Vars {
			v[k] = val
		}
		snap.Vars = v
	}
	snap.Vars["ups.status"] = raw
	delete(snap.Vars, "ups.alarm")

	// A valid shutdown condition must not wait for optional metadata or be
	// discarded if its follow-up read fails.
	m.emitCritical(ctx, snap, tokens)

	// Surface ups.alarm whenever ALARM is asserted so notifiers can render
	// the actual reason ("Replace battery", "Battery overheated", ...). The
	// fetch is best-effort: drivers don't always expose the variable. An
	// unsupported variable is harmless, but transport failures invalidate
	// the connection and cannot be ignored.
	var alarmErr error
	if tokens.Has("ALARM") {
		// Critical event consumers may retain the snapshot already dispatched.
		snap.Vars = maps.Clone(snap.Vars)
		if a, aerr := m.conn.GetVar(m.cfg.UPS, "ups.alarm"); aerr != nil && nut.IsTransient(aerr) {
			alarmErr = aerr
		} else if aerr == nil && a != "" {
			snap.Vars["ups.alarm"] = a
		}
	}

	m.emitOtherEdges(ctx, snap, tokens)
	m.last = snap
	m.prev = tokens
	return alarmErr
}

func (m *Monitor) refreshVars(ctx context.Context) error {
	if m.conn == nil {
		return errors.New("not connected")
	}
	vars, err := m.conn.ListVars(m.cfg.UPS)
	if err != nil {
		return err
	}
	raw, ok := vars["ups.status"]
	if !ok {
		raw, err = m.conn.GetVar(m.cfg.UPS, "ups.status")
		if err != nil {
			return err
		}
		vars["ups.status"] = raw
	}
	tokens := nut.ParseStatus(raw)
	if len(tokens) == 0 {
		return errors.New("empty ups.status")
	}
	snap := Snapshot{UPS: m.cfg.UPS, Status: raw, Tokens: tokens.Tokens(), Vars: vars, Time: time.Now()}
	m.diffAndEmit(ctx, snap, tokens)
	m.last = snap
	m.prev = tokens
	return nil
}

func (m *Monitor) diffAndEmit(ctx context.Context, snap Snapshot, cur nut.Status) {
	m.emitCritical(ctx, snap, cur)
	m.emitOtherEdges(ctx, snap, cur)
}

func (m *Monitor) emitCritical(ctx context.Context, snap Snapshot, cur nut.Status) {
	prev := m.prev

	// Dispatch shutdown conditions before any potentially slow informational
	// notifications, including initial startup and communication recovery.
	if cur.Has("FSD") && !prev.Has("FSD") {
		m.emit(ctx, Event{Kind: EventFSD, Snapshot: snap, Previous: m.last, Message: m.describe(EventFSD, snap)})
	}
	// LOWBATT requires both LB and OB. A bare LB on mains can be an APC
	// firmware self-test artifact and must not initiate a local shutdown.
	if cur.Has("LB") && cur.Has("OB") && !(prev.Has("LB") && prev.Has("OB")) {
		m.emit(ctx, Event{Kind: EventLowBatt, Snapshot: snap, Previous: m.last, Message: m.describe(EventLowBatt, snap)})
	}
}

func (m *Monitor) emitOtherEdges(ctx context.Context, snap Snapshot, cur nut.Status) {
	prev := m.prev
	if m.commBad {
		m.emit(ctx, Event{Kind: EventCommOK, Snapshot: snap, Previous: m.last, Message: "communication restored"})
	}
	if !m.started {
		m.started = true
		m.emit(ctx, Event{Kind: EventStartup, Snapshot: snap, Previous: snap, Message: "monitor started"})
	}

	// Stable iteration order so test assertions are deterministic.
	tokens := make([]string, 0, len(tokenEdges))
	for tok := range tokenEdges {
		tokens = append(tokens, tok)
	}
	sort.Strings(tokens)

	// Debounce measures observations; synchronous sinks may finish much later.
	now := snap.Time
	for _, tok := range tokens {
		edge := tokenEdges[tok]
		entered := cur.Has(tok) && !prev.Has(tok)
		left := !cur.Has(tok) && prev.Has(tok)

		if tok == "FSD" || tok == "RB" || tok == "LB" || tok == "ALARM" {
			// Shutdown conditions and debounced faults have dedicated handling.
			continue
		}
		if entered && edge.enter != "" {
			m.emit(ctx, Event{Kind: edge.enter, Snapshot: snap, Previous: m.last, Message: m.describe(edge.enter, snap)})
		}
		if left && edge.leave != "" {
			m.emit(ctx, Event{Kind: edge.leave, Snapshot: snap, Previous: m.last, Message: m.describe(edge.leave, snap)})
		}
	}

	// Replace-battery debounce — APC BX firmwares flap this token. Hold for
	// ReplBattDebounce before emitting; clear if it disappears.
	if cur.Has("RB") {
		if m.rbFirstSeen.IsZero() {
			m.rbFirstSeen = now
		}
		if !m.rbConfirmed && (m.cfg.ReplBattDebounce <= 0 || now.Sub(m.rbFirstSeen) >= m.cfg.ReplBattDebounce) {
			m.emit(ctx, Event{Kind: EventReplBatt, Snapshot: snap, Previous: m.last, Message: m.describe(EventReplBatt, snap)})
			m.rbConfirmed = true
		}
	} else {
		m.rbFirstSeen = time.Time{}
		m.rbConfirmed = false
	}

	// Alarm debounce — same shape as RB. APC BX firmwares briefly assert
	// ALARM during periodic background self-tests; those blips clear well
	// inside one minute and are never actionable. NOTALARM only fires if
	// we previously confirmed (and emitted) ALARM, otherwise a debounced
	// flap would surface as a bare NOTALARM with no matching ALARM.
	if cur.Has("ALARM") {
		if m.alarmFirstSeen.IsZero() {
			m.alarmFirstSeen = now
		}
		if !m.alarmConfirmed && (m.cfg.AlarmDebounce <= 0 || now.Sub(m.alarmFirstSeen) >= m.cfg.AlarmDebounce) {
			m.emit(ctx, Event{Kind: EventAlarm, Snapshot: snap, Previous: m.last, Message: m.describe(EventAlarm, snap)})
			m.alarmConfirmed = true
		}
	} else {
		if m.alarmConfirmed {
			m.emit(ctx, Event{Kind: EventNotAlarm, Snapshot: snap, Previous: m.last, Message: m.describe(EventNotAlarm, snap)})
		}
		m.alarmFirstSeen = time.Time{}
		m.alarmConfirmed = false
	}
}

// markCommBad transitions to a comm-bad state if not already there, and
// escalates to NOCOMM once the threshold has elapsed.
func (m *Monitor) markCommBad(ctx context.Context, reason string) {
	now := time.Now()
	if !m.commBad {
		m.commBad = true
		m.commBadSince = now
		m.log.Warn("comm bad", "reason", reason)
		snap := m.last
		snap.Time = now
		m.emit(ctx, Event{Kind: EventCommBad, Snapshot: snap, Previous: m.last, Message: "communication lost: " + reason})
	}
	if !m.noCommEmitted && m.cfg.NoCommThreshold > 0 && now.Sub(m.commBadSince) >= m.cfg.NoCommThreshold {
		snap := m.last
		snap.Time = now
		m.emit(ctx, Event{Kind: EventNoComm, Snapshot: snap, Previous: m.last, Message: "no communication for " + m.cfg.NoCommThreshold.String()})
		m.noCommEmitted = true
	}
}

// handleConnErr tears down a failed connection so the next loop reconnects.
func (m *Monitor) handleConnErr(ctx context.Context, op string, err error) {
	if ctx.Err() != nil {
		return
	}
	m.log.Warn("nut "+op, "err", err)
	// Any failed status or bulk read prevents us from monitoring the UPS,
	// including protocol errors such as revoked access or a removed UPS.
	m.closeConn()
	m.markCommBad(ctx, fmt.Sprintf("%s: %v", op, err))
}

func (m *Monitor) emit(ctx context.Context, e Event) {
	if m.sink == nil || ctx.Err() != nil {
		return
	}
	m.log.Info("event", "kind", string(e.Kind), "ups", e.Snapshot.UPS, "status", e.Snapshot.Status)
	m.sink.Dispatch(ctx, e)
}

// describe builds a short human-readable summary for the event.
func (m *Monitor) describe(kind EventKind, s Snapshot) string {
	parts := []string{string(kind)}
	if s.Status != "" {
		parts = append(parts, "status="+s.Status)
	}
	if c, ok := s.Vars["battery.charge"]; ok {
		parts = append(parts, "charge="+c+"%")
	}
	if r, ok := s.Vars["battery.runtime"]; ok {
		parts = append(parts, "runtime="+r+"s")
	}
	if l, ok := s.Vars["ups.load"]; ok {
		parts = append(parts, "load="+l+"%")
	}
	if kind == EventAlarm {
		if a, ok := s.Vars["ups.alarm"]; ok && a != "" {
			parts = append(parts, "alarm="+a)
		}
	}
	return strings.Join(parts, " ")
}
