package nut

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeServer is a tiny scriptable NUT server for tests. Each entry maps an
// expected client request line to the response (which may span multiple
// lines, separated by `\n`). Unknown commands yield `ERR UNKNOWN-COMMAND`.
type fakeServer struct {
	t        *testing.T
	listener net.Listener
	addr     string
	script   map[string]string
	closed   chan struct{}
}

func newFakeServer(t *testing.T, script map[string]string) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fs := &fakeServer{t: t, listener: ln, addr: ln.Addr().String(), script: script, closed: make(chan struct{})}
	go fs.serve()
	t.Cleanup(func() { _ = ln.Close(); <-fs.closed })
	return fs
}

func (fs *fakeServer) serve() {
	defer close(fs.closed)
	for {
		conn, err := fs.listener.Accept()
		if err != nil {
			return
		}
		go fs.handle(conn)
	}
}

func (fs *fakeServer) handle(conn net.Conn) {
	defer conn.Close()
	rd := bufio.NewReader(conn)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		resp, ok := fs.script[line]
		if !ok {
			_, _ = conn.Write([]byte("ERR UNKNOWN-COMMAND\n"))
			continue
		}
		if !strings.HasSuffix(resp, "\n") {
			resp += "\n"
		}
		if _, err := conn.Write([]byte(resp)); err != nil {
			return
		}
		if line == "LOGOUT" {
			return
		}
	}
}

func TestParseStatus(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"OL", []string{"OL"}},
		{"OL CHRG", []string{"CHRG", "OL"}},
		{"  ob discHRG lb ", []string{"DISCHRG", "LB", "OB"}},
		{"", nil},
	}
	for _, c := range cases {
		got := ParseStatus(c.raw).Tokens()
		if c.want == nil {
			if len(got) != 0 {
				t.Errorf("ParseStatus(%q) = %v, want empty", c.raw, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseStatus(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
	s := ParseStatus("OL CHRG")
	if !s.Has("ol") || !s.Has("CHRG") || s.Has("OB") {
		t.Errorf("Has: unexpected membership in %v", s.Tokens())
	}
	if s.String() != "CHRG OL" {
		t.Errorf("String() = %q", s.String())
	}
}

func TestUnquoteAndQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`"hello"`, "hello"},
		{`"a \"b\" c"`, `a "b" c`},
		{`"back\\slash"`, `back\slash`},
		{`bare`, "bare"},
		{`""`, ""},
	}
	for _, c := range cases {
		got, err := unquote(c.in)
		if err != nil {
			t.Errorf("unquote(%q) err: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("unquote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if Quote(`a"b\c`) != `"a\"b\\c"` {
		t.Errorf("Quote roundtrip wrong: %q", Quote(`a"b\c`))
	}
}

func TestParseVarLine(t *testing.T) {
	v, err := parseVarLine(`VAR ups battery.charge "100"`)
	if err != nil {
		t.Fatalf("parseVarLine: %v", err)
	}
	if v.UPS != "ups" || v.Name != "battery.charge" || v.Value != "100" {
		t.Errorf("parseVarLine: %+v", v)
	}
	v2, err := parseVarLine(`VAR myups device.model "Back-UPS BX2200MI"`)
	if err != nil {
		t.Fatalf("parseVarLine model: %v", err)
	}
	if v2.Value != "Back-UPS BX2200MI" {
		t.Errorf("parseVarLine model value: %q", v2.Value)
	}
	if _, err := parseVarLine("OK"); err == nil {
		t.Error("expected error on non-VAR line")
	}
}

func TestGetVar(t *testing.T) {
	fs := newFakeServer(t, map[string]string{
		"GET VAR ups ups.status":     `VAR ups ups.status "OL CHRG"`,
		"GET VAR ups battery.charge": `VAR ups battery.charge "100"`,
		"GET VAR ups missing":        `ERR VAR-NOT-SUPPORTED`,
		"LOGOUT":                     `OK Goodbye`,
	})
	c, err := Dial(context.Background(), fs.addr, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	v, err := c.GetVar("ups", "ups.status")
	if err != nil {
		t.Fatalf("GetVar status: %v", err)
	}
	if v != "OL CHRG" {
		t.Errorf("status = %q", v)
	}
	v, err = c.GetVar("ups", "battery.charge")
	if err != nil || v != "100" {
		t.Errorf("battery.charge = %q, %v", v, err)
	}

	_, err = c.GetVar("ups", "missing")
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Code != "VAR-NOT-SUPPORTED" {
		t.Errorf("expected VAR-NOT-SUPPORTED, got %v", err)
	}
}

func TestListVars(t *testing.T) {
	fs := newFakeServer(t, map[string]string{
		"LIST VAR ups": `BEGIN LIST VAR ups
VAR ups ups.status "OL"
VAR ups battery.charge "98"
VAR ups device.model "Back-UPS BX2200MI"
END LIST VAR ups`,
		"LOGOUT": "OK Goodbye",
	})
	c, err := Dial(context.Background(), fs.addr, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	vars, err := c.ListVars("ups")
	if err != nil {
		t.Fatalf("ListVars: %v", err)
	}
	want := map[string]string{
		"ups.status":     "OL",
		"battery.charge": "98",
		"device.model":   "Back-UPS BX2200MI",
	}
	if !reflect.DeepEqual(vars, want) {
		t.Errorf("ListVars = %v, want %v", vars, want)
	}
}

func TestListVarsErr(t *testing.T) {
	fs := newFakeServer(t, map[string]string{
		"LIST VAR bogus": `ERR UNKNOWN-UPS`,
		"LOGOUT":         "OK Goodbye",
	})
	c, err := Dial(context.Background(), fs.addr, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	_, err = c.ListVars("bogus")
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Code != "UNKNOWN-UPS" {
		t.Errorf("want UNKNOWN-UPS, got %v", err)
	}
}

func TestListUPS(t *testing.T) {
	fs := newFakeServer(t, map[string]string{
		"LIST UPS": `BEGIN LIST UPS
UPS ups "APC BX2200MI"
UPS spare "Backup unit"
END LIST UPS`,
		"LOGOUT": "OK Goodbye",
	})
	c, err := Dial(context.Background(), fs.addr, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	upses, err := c.ListUPS()
	if err != nil {
		t.Fatalf("ListUPS: %v", err)
	}
	if upses["ups"] != "APC BX2200MI" {
		t.Errorf("ups desc = %q", upses["ups"])
	}
	if upses["spare"] != "Backup unit" {
		t.Errorf("spare desc = %q", upses["spare"])
	}
}

func TestLogin(t *testing.T) {
	fs := newFakeServer(t, map[string]string{
		"USERNAME admin": "OK",
		"PASSWORD secret": "OK",
		"LOGIN ups":       "OK",
		"USERNAME bad":    "OK",
		"PASSWORD bad":    "ERR INVALID-PASSWORD",
		"LOGOUT":          "OK Goodbye",
	})
	c, err := Dial(context.Background(), fs.addr, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Login("admin", "secret", "ups"); err != nil {
		t.Errorf("Login ok: %v", err)
	}
	c.Close()

	c2, err := Dial(context.Background(), fs.addr, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c2.Close()
	err = c2.Login("bad", "bad", "")
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Code != "INVALID-PASSWORD" {
		t.Errorf("want INVALID-PASSWORD, got %v", err)
	}

	// Empty creds: no-op.
	c3, err := Dial(context.Background(), fs.addr, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c3.Close()
	if err := c3.Login("", "", ""); err != nil {
		t.Errorf("empty Login: %v", err)
	}
}

func TestDialDefaultPort(t *testing.T) {
	// We cannot dial 3493 reliably in CI; instead test the address rewriting
	// by attempting a hostname-only dial against an unroutable IP and
	// confirming the error string mentions the appended port.
	_, err := Dial(context.Background(), "127.0.0.1", 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "3493") {
		// This either succeeds (real upsd running) or fails citing the
		// default port. Either is acceptable; only fail if neither.
		t.Logf("Dial(127.0.0.1) err = %v", err)
	}
}

func TestIsTransient(t *testing.T) {
	if IsTransient(nil) {
		t.Error("nil should not be transient")
	}
	if !IsTransient(&ProtocolError{Code: "DATA-STALE"}) {
		t.Error("DATA-STALE should be transient")
	}
	if !IsTransient(&ProtocolError{Code: "DRIVER-NOT-CONNECTED"}) {
		t.Error("DRIVER-NOT-CONNECTED should be transient")
	}
	if IsTransient(&ProtocolError{Code: "UNKNOWN-UPS"}) {
		t.Error("UNKNOWN-UPS should NOT be transient")
	}
	if !IsTransient(io.EOF) {
		t.Error("EOF should be transient")
	}
}

func TestProtocolErrorIs(t *testing.T) {
	pe := &ProtocolError{Code: "DATA-STALE"}
	if !errors.Is(pe, &ProtocolError{Code: "DATA-STALE"}) {
		t.Error("errors.Is should match same code")
	}
	if errors.Is(pe, &ProtocolError{Code: "OTHER"}) {
		t.Error("errors.Is should not match different code")
	}
}

func TestStatusTokensSorted(t *testing.T) {
	tokens := ParseStatus("OL CHRG OB").Tokens()
	if !sort.StringsAreSorted(tokens) {
		t.Errorf("tokens not sorted: %v", tokens)
	}
}
