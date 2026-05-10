// Package nut implements a minimal client for the Network UPS Tools (NUT) TCP
// protocol as spoken by upsd on port 3493.
//
// The client speaks the wire protocol directly — it does not shell out to
// upsc/upsmon. It supports the read-side commands needed by a monitoring
// client (LIST UPS, LIST VAR, GET VAR), authentication (USERNAME / PASSWORD /
// LOGIN), and STARTTLS for credentialed or remote setups.
//
// References:
//   - NUT Developer Guide § Network protocol
//   - RFC 9271
package nut

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"
)

// DefaultPort is the IANA-registered NUT TCP port.
const DefaultPort = 3493

// Client is a single multiplexed connection to a NUT upsd instance. It is not
// safe for concurrent use; serialize calls or open multiple connections.
type Client struct {
	conn    net.Conn
	rd      *bufio.Reader
	timeout time.Duration
}

// Var is a single (ups, name, value) tuple as returned by GET VAR / LIST VAR.
type Var struct {
	UPS   string
	Name  string
	Value string
}

// ProtocolError represents an `ERR <code>` reply from upsd.
type ProtocolError struct {
	Code string
}

func (e *ProtocolError) Error() string { return "NUT error: " + e.Code }

// Is reports whether target is a *ProtocolError with the same code.
func (e *ProtocolError) Is(target error) bool {
	var pe *ProtocolError
	if errors.As(target, &pe) {
		return pe.Code == e.Code
	}
	return false
}

// IsTransient reports whether err represents a recoverable backend condition
// (driver disconnect, stale data) as opposed to a hard protocol violation.
// Network/IO errors are also considered transient.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var pe *ProtocolError
	if errors.As(err, &pe) {
		switch {
		case strings.HasPrefix(pe.Code, "DATA-STALE"),
			strings.HasPrefix(pe.Code, "DRIVER-NOT-CONNECTED"):
			return true
		}
		return false
	}
	// All non-protocol errors (timeouts, EOFs, resets) — treat as transient
	// so the monitor can reconnect rather than crash.
	return true
}

// Dial opens a TCP connection to a NUT server. If addr lacks a port, 3493 is
// appended. timeout applies to each individual read/write deadline; pass 0
// to disable deadlines.
func Dial(ctx context.Context, addr string, timeout time.Duration) (*Client, error) {
	if !strings.Contains(addr, ":") || strings.HasSuffix(addr, "]") {
		addr = fmt.Sprintf("%s:%d", addr, DefaultPort)
	}
	d := &net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, rd: bufio.NewReader(conn), timeout: timeout}, nil
}

// Close politely sends LOGOUT and tears down the TCP connection. The first
// non-nil error encountered is returned.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	_, _ = c.writeLine("LOGOUT")
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *Client) setDeadlines() {
	if c.timeout > 0 && c.conn != nil {
		_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	}
}

func (c *Client) writeLine(line string) (int, error) {
	c.setDeadlines()
	return c.conn.Write([]byte(line + "\n"))
}

func (c *Client) readLine() (string, error) {
	c.setDeadlines()
	line, err := c.rd.ReadString('\n')
	if err != nil {
		// Surface a clean EOF rather than a partial-line truncation.
		if errors.Is(err, io.EOF) && line == "" {
			return "", io.EOF
		}
		return strings.TrimRight(line, "\r\n"), err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// command sends one line and reads exactly one OK/ERR-shaped reply.
func (c *Client) command(cmd string) (string, error) {
	if _, err := c.writeLine(cmd); err != nil {
		return "", err
	}
	line, err := c.readLine()
	if err != nil {
		return line, err
	}
	if strings.HasPrefix(line, "ERR ") {
		fields := strings.Fields(strings.TrimPrefix(line, "ERR "))
		code := ""
		if len(fields) > 0 {
			code = fields[0]
		}
		return line, &ProtocolError{Code: code}
	}
	return line, nil
}

// Login authenticates the connection. Pass empty username and password to
// skip; read-only LIST/GET on a default upsd does not require auth. ups may
// be empty to skip the LOGIN step (only relevant for upsmon-style primary
// claims and SET/INSTCMD operations).
//
// Username and password are NUT-quoted on the wire so spaces or quote
// characters in either are passed through as a single token rather than
// frame-shifting the parser.
func (c *Client) Login(username, password, ups string) error {
	if username == "" && password == "" {
		return nil
	}
	if _, err := c.command("USERNAME " + Quote(username)); err != nil {
		return err
	}
	if _, err := c.command("PASSWORD " + Quote(password)); err != nil {
		return err
	}
	if ups != "" {
		if _, err := c.command("LOGIN " + ups); err != nil {
			return err
		}
	}
	return nil
}

// StartTLS upgrades the connection to TLS. upsd must be built with TLS
// support (OpenSSL/NSS) and have CERTFILE configured; otherwise it returns
// ProtocolError{Code: "FEATURE-NOT-CONFIGURED"}.
func (c *Client) StartTLS(cfg *tls.Config) error {
	if _, err := c.command("STARTTLS"); err != nil {
		return err
	}
	tlsConn := tls.Client(c.conn, cfg)
	if err := tlsConn.HandshakeContext(context.Background()); err != nil {
		return err
	}
	c.conn = tlsConn
	c.rd = bufio.NewReader(tlsConn)
	return nil
}

// GetVar issues `GET VAR <ups> <name>` and returns the unquoted value.
func (c *Client) GetVar(ups, name string) (string, error) {
	if _, err := c.writeLine(fmt.Sprintf("GET VAR %s %s", ups, name)); err != nil {
		return "", err
	}
	line, err := c.readLine()
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(line, "ERR ") {
		fields := strings.Fields(strings.TrimPrefix(line, "ERR "))
		code := ""
		if len(fields) > 0 {
			code = fields[0]
		}
		return "", &ProtocolError{Code: code}
	}
	v, err := parseVarLine(line)
	if err != nil {
		return "", err
	}
	return v.Value, nil
}

// ListVars returns every variable exposed by the named UPS as a flat map
// (variable name → unquoted value).
func (c *Client) ListVars(ups string) (map[string]string, error) {
	out := make(map[string]string)
	err := c.list("LIST VAR "+ups, "BEGIN LIST VAR "+ups, "END LIST VAR "+ups, func(line string) error {
		v, perr := parseVarLine(line)
		if perr != nil {
			return nil // skip stray non-VAR lines rather than aborting
		}
		out[v.Name] = v.Value
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListUPS returns every UPS upsd is aware of as (name → description).
func (c *Client) ListUPS() (map[string]string, error) {
	out := make(map[string]string)
	err := c.list("LIST UPS", "BEGIN LIST UPS", "END LIST UPS", func(line string) error {
		if !strings.HasPrefix(line, "UPS ") {
			return nil
		}
		rest := strings.TrimPrefix(line, "UPS ")
		sp := strings.IndexByte(rest, ' ')
		if sp < 0 {
			out[rest] = ""
			return nil
		}
		name := rest[:sp]
		out[name] = unquote(strings.TrimSpace(rest[sp+1:]))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) list(cmd, beginPrefix, endPrefix string, onLine func(string) error) error {
	if _, err := c.writeLine(cmd); err != nil {
		return err
	}
	first, err := c.readLine()
	if err != nil {
		return err
	}
	if strings.HasPrefix(first, "ERR ") {
		fields := strings.Fields(strings.TrimPrefix(first, "ERR "))
		code := ""
		if len(fields) > 0 {
			code = fields[0]
		}
		return &ProtocolError{Code: code}
	}
	if !strings.HasPrefix(first, beginPrefix) {
		return fmt.Errorf("nut: unexpected response %q (want %q)", first, beginPrefix)
	}
	for {
		line, err := c.readLine()
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, endPrefix) {
			return nil
		}
		if strings.HasPrefix(line, "ERR ") {
			fields := strings.Fields(strings.TrimPrefix(line, "ERR "))
			code := ""
			if len(fields) > 0 {
				code = fields[0]
			}
			return &ProtocolError{Code: code}
		}
		if err := onLine(line); err != nil {
			return err
		}
	}
}

// Status is a set of upper-case ups.status tokens.
type Status map[string]struct{}

// ParseStatus splits a NUT ups.status string into a set of canonical tokens.
func ParseStatus(raw string) Status {
	out := make(Status)
	for _, t := range strings.Fields(raw) {
		out[strings.ToUpper(t)] = struct{}{}
	}
	return out
}

// Has reports whether the status set contains token (case-insensitive).
func (s Status) Has(token string) bool {
	_, ok := s[strings.ToUpper(token)]
	return ok
}

// Tokens returns the status tokens sorted lexicographically.
func (s Status) Tokens() []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// String returns the tokens joined with single spaces, sorted.
func (s Status) String() string { return strings.Join(s.Tokens(), " ") }

// parseVarLine parses `VAR <ups> <name> "<value>"`.
func parseVarLine(line string) (Var, error) {
	if !strings.HasPrefix(line, "VAR ") {
		return Var{}, fmt.Errorf("nut: not a VAR line: %q", line)
	}
	rest := strings.TrimPrefix(line, "VAR ")
	sp1 := strings.IndexByte(rest, ' ')
	if sp1 < 0 {
		return Var{}, fmt.Errorf("nut: malformed VAR line: %q", line)
	}
	ups := rest[:sp1]
	rest = rest[sp1+1:]
	sp2 := strings.IndexByte(rest, ' ')
	if sp2 < 0 {
		return Var{}, fmt.Errorf("nut: malformed VAR line: %q", line)
	}
	name := rest[:sp2]
	return Var{UPS: ups, Name: name, Value: unquote(strings.TrimSpace(rest[sp2+1:]))}, nil
}

// unquote parses a NUT-quoted token: surrounding `"` and `\\`/`\"` escapes.
// An unquoted bareword is returned verbatim. A trailing unescaped `\` inside
// a quoted string is treated as a literal `\` (matching upsd's lenient
// behavior) — the function deliberately never returns an error so the
// caller doesn't have to thread one through, but malformed inputs always
// produce *some* string and never panic.
func unquote(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	s = s[1 : len(s)-1]
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			sb.WriteByte(s[i+1])
			i++
			continue
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

// Quote wraps s in NUT-style double quotes, escaping `\` and `"`.
func Quote(s string) string {
	var sb strings.Builder
	sb.Grow(len(s) + 2)
	sb.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c == '"' {
			sb.WriteByte('\\')
		}
		sb.WriteByte(c)
	}
	sb.WriteByte('"')
	return sb.String()
}
