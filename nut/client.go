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
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultPort is the IANA-registered NUT TCP port.
const DefaultPort = 3493

// Client is a single multiplexed connection to a NUT upsd instance. It is not
// safe for concurrent use except Close; serialize commands or open multiple connections.
type Client struct {
	conn       net.Conn
	rd         *bufio.Reader
	writer     io.Writer
	timeout    time.Duration
	serverName string
	closeOnce  sync.Once
	closeErr   error
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
		return pe != nil && pe.Code == e.Code
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
		switch pe.Code {
		case "DATA-STALE", "DRIVER-NOT-CONNECTED":
			return true
		}
		return false
	}
	// All non-protocol errors (timeouts, EOFs, resets) — treat as transient
	// so the monitor can reconnect rather than crash.
	return true
}

// Dial opens a TCP connection to a NUT server. If addr lacks a port, 3493 is
// appended. timeout bounds dialing and each complete command/response exchange;
// pass 0 to disable deadlines.
func Dial(ctx context.Context, addr string, timeout time.Duration) (*Client, error) {
	host := addr
	if strings.HasPrefix(addr, "[") && strings.HasSuffix(addr, "]") {
		host = addr[1 : len(addr)-1]
	}
	if _, err := netip.ParseAddr(host); err == nil || !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(host, fmt.Sprint(DefaultPort))
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, rd: bufio.NewReader(conn), timeout: timeout, serverName: host}, nil
}

// Close tears down the TCP connection, interrupting any pending operation.
// It is safe to call concurrently with other methods and is idempotent.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		if c.conn != nil {
			c.closeErr = c.conn.Close()
		}
	})
	return c.closeErr
}

func (c *Client) setDeadlines() {
	if c.timeout > 0 && c.conn != nil {
		_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	}
}

func (c *Client) writeLine(line string) (int, error) {
	if err := validateArguments(line); err != nil {
		return 0, err
	}
	if c.conn == nil {
		return 0, net.ErrClosed
	}
	c.setDeadlines()
	writer := c.writer
	if writer == nil {
		writer = c.conn
	}
	n, err := writer.Write([]byte(line + "\n"))
	if err == nil && n != len(line)+1 {
		err = io.ErrShortWrite
	}
	if err != nil {
		_ = c.Close()
	}
	return n, err
}

func (c *Client) readLine() (line string, err error) {
	defer func() {
		var pe *ProtocolError
		if err != nil && !errors.As(err, &pe) {
			_ = c.Close()
		}
	}()
	var buf []byte
	for {
		part, err := c.rd.ReadSlice('\n')
		if len(buf)+len(part) > 64*1024 {
			return "", fmt.Errorf("nut: response line exceeds 64 KiB")
		}
		buf = append(buf, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return "", err
		}
		break
	}
	line = strings.TrimSuffix(strings.TrimSuffix(string(buf), "\n"), "\r")
	if err := validateArguments(line); err != nil {
		return "", err
	}
	fields := strings.Fields(line)
	if len(fields) > 0 && fields[0] == "ERR" {
		if len(fields) < 2 {
			return "", fmt.Errorf("nut: malformed error response")
		}
		for _, ch := range fields[1] {
			if ch != '-' && (ch < 'A' || ch > 'Z') {
				return "", fmt.Errorf("nut: malformed error code %q", fields[1])
			}
		}
		return line, &ProtocolError{Code: fields[1]}
	}
	return line, nil
}

func validateArguments(args ...string) error {
	for _, arg := range args {
		if strings.ContainsAny(arg, "\r\n\x00") {
			return fmt.Errorf("nut: argument contains a line break or NUL")
		}
	}
	return nil
}

func wireToken(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\v\f\"\\#=") {
		return Quote(s)
	}
	return s
}

func (c *Client) commandOK(cmd, want string) error {
	line, err := c.command(cmd)
	if err != nil {
		return err
	}
	if line != want {
		_ = c.Close()
		return fmt.Errorf("nut: unexpected response %q (want %q)", line, want)
	}
	return nil
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
	if err := validateArguments(username, password, ups); err != nil {
		return err
	}
	if username == "" && password == "" {
		return nil
	}
	if err := c.commandOK("USERNAME "+Quote(username), "OK"); err != nil {
		return err
	}
	if err := c.commandOK("PASSWORD "+Quote(password), "OK"); err != nil {
		return err
	}
	if ups != "" {
		if err := c.commandOK("LOGIN "+wireToken(ups), "OK"); err != nil {
			return err
		}
	}
	return nil
}

// StartTLS upgrades the connection to TLS. upsd must be built with TLS
// support (OpenSSL/NSS) and have CERTFILE configured; otherwise it returns
// ProtocolError{Code: "FEATURE-NOT-CONFIGURED"}.
func (c *Client) StartTLS(cfg *tls.Config) error {
	return c.StartTLSContext(context.Background(), cfg)
}

// StartTLSContext upgrades the connection and closes it if ctx is canceled
// during negotiation. An empty ServerName is inferred from the dial address.
// A nil cfg uses the system trust store and standard TLS defaults.
func (c *Client) StartTLSContext(ctx context.Context, cfg *tls.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if err := c.commandOK("STARTTLS", "OK STARTTLS"); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if cfg == nil {
		cfg = &tls.Config{}
	} else {
		cfg = cfg.Clone()
	}
	if cfg.ServerName == "" {
		cfg.ServerName = c.serverName
		if addr, err := netip.ParseAddr(cfg.ServerName); err == nil {
			cfg.ServerName = addr.WithZone("").String()
		}
	}
	if c.rd.Buffered() != 0 {
		_ = c.Close()
		return fmt.Errorf("nut: unexpected plaintext after STARTTLS acknowledgment")
	}
	tlsConn := tls.Client(c.conn, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = c.Close()
		return err
	}
	c.writer = tlsConn
	c.rd = bufio.NewReader(tlsConn)
	return nil
}

// GetVar issues `GET VAR <ups> <name>` and returns the unquoted value.
func (c *Client) GetVar(ups, name string) (string, error) {
	if _, err := c.writeLine(fmt.Sprintf("GET VAR %s %s", wireToken(ups), wireToken(name))); err != nil {
		return "", err
	}
	line, err := c.readLine()
	if err != nil {
		return "", err
	}
	v, err := parseVarLine(line)
	if err != nil {
		_ = c.Close()
		return "", err
	}
	if v.UPS != ups || v.Name != name {
		_ = c.Close()
		return "", fmt.Errorf("nut: response does not match requested variable: %q", line)
	}
	return v.Value, nil
}

// ListVars returns every variable exposed by the named UPS as a flat map
// (variable name → unquoted value).
func (c *Client) ListVars(ups string) (map[string]string, error) {
	out := make(map[string]string)
	token := wireToken(ups)
	err := c.list("LIST VAR "+token, "BEGIN LIST VAR "+token, "END LIST VAR "+token, func(line string) error {
		v, perr := parseVarLine(line)
		if perr != nil {
			return perr
		}
		if v.UPS != ups {
			return fmt.Errorf("nut: response does not match requested UPS: %q", line)
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
		fields, err := parseTokens(line)
		if err != nil || len(fields) != 3 || fields[0] != "UPS" || fields[1] == "" {
			return fmt.Errorf("nut: malformed UPS line: %q", line)
		}
		out[fields[1]] = fields[2]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) list(cmd, beginPrefix, endPrefix string, onLine func(string) error) (err error) {
	if _, err := c.writeLine(cmd); err != nil {
		return err
	}
	first, err := c.readLine()
	if err != nil {
		return err
	}
	if !sameTokens(first, beginPrefix) {
		_ = c.Close()
		return fmt.Errorf("nut: unexpected response %q (want %q)", first, beginPrefix)
	}
	// Once a LIST begins, any failure leaves its remaining rows unread.
	// Reusing that stream could mistake stale rows for a new command's reply.
	defer func() {
		if err != nil {
			_ = c.Close()
		}
	}()
	// Include a CRLF allowance for each line, even when the server uses LF.
	responseBytes := len(first) + 2
	for {
		line, err := c.readLine()
		if err != nil {
			return err
		}
		responseBytes += len(line) + 2
		if responseBytes > 1<<20 {
			return fmt.Errorf("nut: LIST response exceeds 1 MiB")
		}
		if sameTokens(line, endPrefix) {
			return nil
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
	fields, err := parseTokens(line)
	if err != nil || len(fields) != 4 || fields[0] != "VAR" || fields[1] == "" || fields[2] == "" {
		return Var{}, fmt.Errorf("nut: malformed VAR line: %q", line)
	}
	return Var{UPS: fields[1], Name: fields[2], Value: fields[3]}, nil
}

func sameTokens(a, b string) bool {
	x, err := parseTokens(a)
	if err != nil {
		return false
	}
	y, err := parseTokens(b)
	return err == nil && slices.Equal(x, y)
}

func parseTokens(line string) ([]string, error) {
	var fields []string
	for line = strings.Trim(line, " \t\v\f"); line != ""; line = strings.Trim(line, " \t\v\f") {
		i := 0
		if line[0] == '"' {
			for i = 1; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '\\' {
					i++
				}
			}
			if i >= len(line) {
				return nil, fmt.Errorf("nut: unterminated quoted token")
			}
			i++
			if i < len(line) && !strings.ContainsRune(" \t\v\f", rune(line[i])) {
				return nil, fmt.Errorf("nut: missing token separator")
			}
		} else {
			for i < len(line) && !strings.ContainsRune(" \t\v\f", rune(line[i])) {
				if line[i] == '"' || line[i] == '\\' {
					return nil, fmt.Errorf("nut: malformed bare token")
				}
				i++
			}
		}
		fields = append(fields, unquote(line[:i]))
		line = line[i:]
	}
	return fields, nil
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

// Quote wraps s in NUT-style double quotes, escaping `\`, `"`, and `#`.
// NUT's shared configuration/wire parser treats unescaped # as a comment,
// even inside a quoted token.
func Quote(s string) string {
	var sb strings.Builder
	sb.Grow(len(s) + 2)
	sb.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c == '"' || c == '#' {
			sb.WriteByte('\\')
		}
		sb.WriteByte(c)
	}
	sb.WriteByte('"')
	return sb.String()
}
