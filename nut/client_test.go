package nut

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
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
		if got := unquote(c.in); got != c.want {
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
		// Login quotes username and password so spaces and quotes don't
		// frame-shift the upsd parser.
		`USERNAME "admin"`:  "OK",
		`PASSWORD "secret"`: "OK",
		`LOGIN ups`:         "OK",
		`USERNAME "bad"`:    "OK",
		`PASSWORD "bad"`:    "ERR INVALID-PASSWORD",
		`USERNAME "u s er"`: "OK",
		`PASSWORD "p\"\\w"`: "OK",
		`LOGOUT`:            "OK Goodbye",
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

	// Credentials with spaces and quote characters must round-trip.
	c4, err := Dial(context.Background(), fs.addr, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c4.Close()
	if err := c4.Login("u s er", `p"\w`, ""); err != nil {
		t.Errorf("quoted Login: %v", err)
	}
}

func TestDialDefaultPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, addr := range []string{"127.0.0.1", "::1", "[::1]", "fe80::1%lo"} {
		_, err := Dial(ctx, addr, time.Second)
		if err == nil || !strings.Contains(err.Error(), ":3493") {
			t.Errorf("Dial(%q) did not append default port: %v", addr, err)
		}
	}
}

func TestDialPreservesScopedIPv6ExplicitPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	const address = "[fe80::1%eth0]:12345"
	_, err := Dial(ctx, address, time.Second)
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Addr == nil || opErr.Addr.String() != address {
		t.Fatalf("Dial changed scoped IPv6 address or explicit port: %v", err)
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

func TestRejectMalformedResponses(t *testing.T) {
	tests := []struct {
		name, command, response string
		call                    func(*Client) error
	}{
		{"username acknowledgment", `USERNAME "user"`, "NOT OK", func(c *Client) error { return c.Login("user", "", "") }},
		{"get wrong UPS", "GET VAR ups ups.status", `VAR other ups.status "OL"`, func(c *Client) error { _, err := c.GetVar("ups", "ups.status"); return err }},
		{"get wrong variable", "GET VAR ups ups.status", `VAR ups battery.charge "OL"`, func(c *Client) error { _, err := c.GetVar("ups", "ups.status"); return err }},
		{"get unterminated string", "GET VAR ups ups.status", `VAR ups ups.status "OL`, func(c *Client) error { _, err := c.GetVar("ups", "ups.status"); return err }},
		{"get extra value", "GET VAR ups ups.status", `VAR ups ups.status "OL" junk`, func(c *Client) error { _, err := c.GetVar("ups", "ups.status"); return err }},
		{"list header suffix", "LIST VAR ups", "BEGIN LIST VAR ups2\nEND LIST VAR ups", func(c *Client) error { _, err := c.ListVars("ups"); return err }},
		{"list footer suffix", "LIST VAR ups", "BEGIN LIST VAR ups\nEND LIST VAR ups2", func(c *Client) error { _, err := c.ListVars("ups"); return err }},
		{"list wrong UPS", "LIST VAR ups", "BEGIN LIST VAR ups\nVAR other ups.status \"OB\"\nEND LIST VAR ups", func(c *Client) error { _, err := c.ListVars("ups"); return err }},
		{"list invalid row", "LIST VAR ups", "BEGIN LIST VAR ups\nGARBAGE\nEND LIST VAR ups", func(c *Client) error { _, err := c.ListVars("ups"); return err }},
		{"list UPS invalid row", "LIST UPS", "BEGIN LIST UPS\nGARBAGE\nEND LIST UPS", func(c *Client) error { _, err := c.ListUPS(); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeServer(t, map[string]string{tt.command: tt.response, `PASSWORD ""`: "OK"})
			c, err := Dial(context.Background(), fs.addr, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err := tt.call(c); err == nil {
				t.Fatal("accepted malformed response")
			}
		})
	}
}

type recordingConn struct {
	net.Conn
	input  *strings.Reader
	output strings.Builder
	closed bool
}

func (c *recordingConn) Read(p []byte) (int, error) { return c.input.Read(p) }
func (c *recordingConn) Write(p []byte) (int, error) {
	if c.closed {
		return 0, net.ErrClosed
	}
	return c.output.Write(p)
}
func (c *recordingConn) Close() error { c.closed = true; return nil }

func TestRejectCommandInjection(t *testing.T) {
	for _, call := range []func(*Client) error{
		func(c *Client) error { return c.Login("user\nFSD ups", "password", "ups") },
		func(c *Client) error { return c.Login("user", "secret\r\nFSD ups", "ups") },
		func(c *Client) error { _, err := c.GetVar("ups\nFSD ups", "ups.status"); return err },
		func(c *Client) error { _, err := c.GetVar("ups", "ups.status\x00"); return err },
		func(c *Client) error { _, err := c.ListVars("ups\r\nFSD ups"); return err },
	} {
		conn := &recordingConn{input: strings.NewReader("OK\nOK\nOK\n")}
		c := &Client{conn: conn, rd: bufio.NewReader(conn)}
		if err := call(c); err == nil {
			t.Error("accepted command injection")
		}
		if conn.output.Len() != 0 {
			t.Errorf("wrote invalid command: %q", conn.output.String())
		}
	}
}

func TestCloseInterruptsBlockedRead(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := &Client{conn: client, rd: bufio.NewReader(client)}
	readDone := make(chan error, 1)
	go func() { _, err := c.GetVar("ups", "ups.status"); readDone <- err }()
	if _, err := bufio.NewReader(server).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		server.Close()
		t.Fatal("Close blocked writing LOGOUT instead of interrupting read")
	}
	if err := <-readDone; err == nil {
		t.Fatal("blocked command succeeded after close")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
	if _, err := c.GetVar("ups", "ups.status"); err == nil {
		t.Fatal("command after close succeeded")
	}
}

func TestTLSInfersServerName(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
			serverDone <- err
			return
		}
		if _, err := fmt.Fprint(conn, "OK STARTTLS\n"); err != nil {
			serverDone <- err
			return
		}
		secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}})
		if err := secure.Handshake(); err != nil {
			serverDone <- err
			return
		}
		if _, err := bufio.NewReader(secure).ReadString('\n'); err != nil {
			serverDone <- err
			return
		}
		_, err = fmt.Fprint(secure, "VAR ups ups.status \"OL\"\n")
		serverDone <- err
	}()
	c, err := Dial(context.Background(), listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cfg := &tls.Config{RootCAs: roots}
	if err := c.StartTLS(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "" {
		t.Fatal("StartTLS mutated caller configuration")
	}
	if got, err := c.GetVar("ups", "ups.status"); err != nil || got != "OL" {
		t.Fatalf("TLS GET = %q, %v", got, err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestResponseLineLimit(t *testing.T) {
	conn := &recordingConn{input: strings.NewReader(strings.Repeat("x", 1<<20) + "\n")}
	c := &Client{conn: conn, rd: bufio.NewReader(conn)}
	if _, err := c.readLine(); err == nil {
		t.Fatal("accepted oversized response")
	}
}

func TestStartTLSContextCancellation(t *testing.T) {
	for _, acknowledge := range []bool{false, true} {
		t.Run(fmt.Sprint(acknowledge), func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			c := &Client{conn: client, rd: bufio.NewReader(client)}
			defer c.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- c.StartTLSContext(ctx, &tls.Config{ServerName: "localhost"}) }()
			if _, err := bufio.NewReader(server).ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			if acknowledge {
				if _, err := fmt.Fprint(server, "OK STARTTLS\n"); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation returned %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("TLS negotiation did not cancel")
			}
		})
	}
}

func TestStartTLSRejectsUnexpectedAcknowledgment(t *testing.T) {
	conn := &recordingConn{input: strings.NewReader("OK\n")}
	c := &Client{conn: conn, rd: bufio.NewReader(conn)}
	if err := c.StartTLS(nil); err == nil {
		t.Fatal("accepted incorrect STARTTLS acknowledgment")
	}
	if conn.output.String() != "STARTTLS\n" {
		t.Fatalf("sent TLS data before acknowledgment: %q", conn.output.String())
	}
}

func TestQuotedIdentifiers(t *testing.T) {
	fs := newFakeServer(t, map[string]string{
		`GET VAR "server ups" device.model`: `VAR "server ups" device.model "a \"quoted\" model"`,
		`LIST VAR "server ups"`:             "BEGIN LIST VAR \"server ups\"\nVAR \"server ups\" ups.status \"OL\"\nEND LIST VAR \"server ups\"",
	})
	c, err := Dial(context.Background(), fs.addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if value, err := c.GetVar("server ups", "device.model"); err != nil || value != `a "quoted" model` {
		t.Fatalf("GetVar = %q, %v", value, err)
	}
	if vars, err := c.ListVars("server ups"); err != nil || vars["ups.status"] != "OL" {
		t.Fatalf("ListVars = %v, %v", vars, err)
	}
}

func TestListResponseSizeLimit(t *testing.T) {
	var response strings.Builder
	response.WriteString("BEGIN LIST VAR ups\n")
	for i := 0; i < 40000; i++ {
		fmt.Fprintf(&response, "VAR ups battery.variable.%d \"100\"\n", i)
	}
	response.WriteString("END LIST VAR ups\n")
	fs := newFakeServer(t, map[string]string{"LIST VAR ups": response.String()})
	c, err := Dial(context.Background(), fs.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ListVars("ups"); err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("oversized LIST response returned %v", err)
	}
}

func TestListTimeoutCoversWholeResponse(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := &Client{conn: client, rd: bufio.NewReader(client), timeout: 100 * time.Millisecond}
	defer c.Close()
	done := make(chan error, 1)
	go func() { _, err := c.ListVars("ups"); done <- err }()
	if _, err := bufio.NewReader(server).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(server, "BEGIN LIST VAR ups\n"); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	limit := time.NewTimer(400 * time.Millisecond)
	defer limit.Stop()
	for {
		select {
		case err := <-done:
			var netErr net.Error
			if !errors.As(err, &netErr) || !netErr.Timeout() {
				t.Fatalf("LIST returned %v, want timeout", err)
			}
			return
		case <-ticker.C:
			server.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
			_, _ = fmt.Fprint(server, "VAR ups ups.status \"OL\"\n")
		case <-limit.C:
			t.Fatal("LIST rows kept extending the command deadline")
		}
	}
}

func TestIncompleteListCannotContaminateNextCommand(t *testing.T) {
	for _, badLine := range []string{"GARBAGE", "ERR VAR-NOT-SUPPORTED"} {
		t.Run(badLine, func(t *testing.T) {
			conn := &recordingConn{input: strings.NewReader("BEGIN LIST VAR ups\n" + badLine + "\nVAR ups ups.status \"OL\"\nEND LIST VAR ups\n")}
			c := &Client{conn: conn, rd: bufio.NewReader(conn)}
			if _, err := c.ListVars("ups"); err == nil {
				t.Fatal("expected invalid LIST response error")
			}
			if value, err := c.GetVar("ups", "ups.status"); err == nil {
				t.Fatalf("reused abandoned LIST data as new GET response: %q", value)
			}
			if !conn.closed {
				t.Fatal("connection remained open after incomplete LIST")
			}
		})
	}
}

func TestMalformedFrameClosesConnection(t *testing.T) {
	for _, response := range []string{
		"VAR other ups.status \"OL\"\n", "VAR ups ups.status \"OL\n",
		"ERR\n", "ERR \"\"\n", "ERR invalid-code\n", "ERR \"DATA-STALE\"\n",
		"VAR ups ups.status \"OL\"\x00\n", strings.Repeat("x", 65536) + "\n",
	} {
		conn := &recordingConn{input: strings.NewReader(response)}
		c := &Client{conn: conn, rd: bufio.NewReader(conn)}
		if _, err := c.GetVar("ups", "ups.status"); err == nil {
			t.Fatal("accepted malformed response")
		}
		if !conn.closed {
			t.Errorf("connection remained open after malformed frame (%d bytes)", len(response))
		}
	}
}

func TestCommandErrorKeepsConnectionUsable(t *testing.T) {
	conn := &recordingConn{input: strings.NewReader("ERR VAR-NOT-SUPPORTED optional detail\nVAR ups ups.status \"OB\"\n")}
	c := &Client{conn: conn, rd: bufio.NewReader(conn)}
	if _, err := c.ListVars("ups"); !errors.Is(err, &ProtocolError{Code: "VAR-NOT-SUPPORTED"}) {
		t.Fatalf("LIST error = %v", err)
	}
	if value, err := c.GetVar("ups", "ups.status"); err != nil || value != "OB" {
		t.Fatalf("valid command-level ERR prevented fallback GET: %q, %v", value, err)
	}
}

func TestNUTParserSpecialCharacters(t *testing.T) {
	fs := newFakeServer(t, map[string]string{
		`USERNAME "user\#one"`: "OK", `PASSWORD "pass\#word"`: "OK",
		`GET VAR "ups=one" ups.status`:    `VAR "ups=one" ups.status "OL"`,
		`GET VAR "ups\#one" ups.status`:   `VAR "ups\#one" ups.status "OL"`,
		"GET VAR \"ups\vone\" ups.status": "VAR \"ups\vone\" ups.status \"OL\"",
	})
	c, err := Dial(context.Background(), fs.addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login("user#one", "pass#word", ""); err != nil {
		t.Errorf("NUT hash-escaped credentials rejected: %v", err)
	}
	for _, ups := range []string{"ups=one", "ups#one", "ups\vone"} {
		if value, err := c.GetVar(ups, "ups.status"); err != nil || value != "OL" {
			t.Errorf("GetVar(%q) = %q, %v", ups, value, err)
		}
	}
}

func TestParseTokensPreservesNonASCIIWhitespace(t *testing.T) {
	got, err := parseTokens("VAR ups device.model model\u00a0")
	if err != nil || len(got) != 4 || got[3] != "model\u00a0" {
		t.Fatalf("non-ASCII byte data was changed: %q, %v", got, err)
	}
}
