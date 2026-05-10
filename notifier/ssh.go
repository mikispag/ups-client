package notifier

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/mikispag/ups-client/monitor"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSHTarget runs Command on a remote host over SSH. Authentication is by
// password or by private key file. Host key verification uses KnownHostsFile
// unless InsecureIgnoreHostKey is true (NOT recommended).
type SSHTarget struct {
	Label                 string
	Host                  string
	Port                  int
	User                  string
	Password              string
	PrivateKeyFile        string
	PrivateKeyPassphrase  string
	KnownHostsFile        string
	InsecureIgnoreHostKey bool
	Command               string
	Timeout               time.Duration
	Filter                Filter

	// dial is overridable in tests.
	dial func(ctx context.Context, network, addr string, cfg *ssh.ClientConfig) (sshClient, error)
}

// sshClient is the subset of *ssh.Client we use, so tests can fake it out.
type sshClient interface {
	NewSession() (sshSession, error)
	Close() error
}

type sshSession interface {
	CombinedOutput(cmd string) ([]byte, error)
	Close() error
}

type realSSHClient struct{ c *ssh.Client }

func (r realSSHClient) NewSession() (sshSession, error) {
	s, err := r.c.NewSession()
	if err != nil {
		return nil, err
	}
	return realSSHSession{s: s}, nil
}
func (r realSSHClient) Close() error { return r.c.Close() }

type realSSHSession struct{ s *ssh.Session }

func (r realSSHSession) CombinedOutput(cmd string) ([]byte, error) { return r.s.CombinedOutput(cmd) }
func (r realSSHSession) Close() error                              { return r.s.Close() }

// Name implements Notifier.
func (t *SSHTarget) Name() string {
	if t.Label != "" {
		return "ssh:" + t.Label
	}
	return fmt.Sprintf("ssh:%s@%s", t.User, t.Host)
}

// Match implements Notifier.
func (t *SSHTarget) Match(e monitor.Event) bool { return t.Filter.Match(e.Kind) }

// authMethods returns the configured ssh.AuthMethod set, or an error.
func (t *SSHTarget) authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if t.PrivateKeyFile != "" {
		key, err := os.ReadFile(t.PrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read key: %w", err)
		}
		var signer ssh.Signer
		if t.PrivateKeyPassphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(key, []byte(t.PrivateKeyPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(key)
		}
		if err != nil {
			return nil, fmt.Errorf("parse key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if t.Password != "" {
		methods = append(methods, ssh.Password(t.Password))
	}
	if len(methods) == 0 {
		return nil, errors.New("no SSH authentication configured (set password or private_key_file)")
	}
	return methods, nil
}

// hostKeyCallback returns a host-key verifier or an error.
func (t *SSHTarget) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if t.InsecureIgnoreHostKey {
		return ssh.InsecureIgnoreHostKey(), nil //#nosec G106 — opt-in only
	}
	path := t.KnownHostsFile
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate known_hosts: %w", err)
		}
		path = home + "/.ssh/known_hosts"
	}
	return knownhosts.New(path)
}

// Notify implements Notifier.
func (t *SSHTarget) Notify(ctx context.Context, e monitor.Event) error {
	if t.Host == "" {
		return fmt.Errorf("ssh %q: empty host", t.Label)
	}
	if t.User == "" {
		return fmt.Errorf("ssh %q: empty user", t.Label)
	}
	if t.Command == "" {
		return fmt.Errorf("ssh %q: empty command", t.Label)
	}
	td := NewTemplateData(e)
	cmd, err := renderTemplate(t.Name()+".command", t.Command, td)
	if err != nil {
		return err
	}

	auth, err := t.authMethods()
	if err != nil {
		return err
	}
	hk, err := t.hostKeyCallback()
	if err != nil {
		return err
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cfg := &ssh.ClientConfig{
		User:            t.User,
		Auth:            auth,
		HostKeyCallback: hk,
		Timeout:         timeout,
	}

	port := t.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(t.Host, strconv.Itoa(port))

	dial := t.dial
	if dial == nil {
		dial = func(ctx context.Context, network, addr string, cfg *ssh.ClientConfig) (sshClient, error) {
			d := &net.Dialer{Timeout: cfg.Timeout}
			nconn, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			// `cfg.Timeout` only bounds the TCP leg per the x/crypto/ssh
			// docs. Mirror what `ssh.Dial` does internally and bound the
			// handshake with a read deadline so a peer that accepts TCP
			// but stalls on the SSH banner cannot wedge us indefinitely.
			if cfg.Timeout > 0 {
				_ = nconn.SetReadDeadline(time.Now().Add(cfg.Timeout))
			}
			// Cancel the handshake on ctx cancel by closing the underlying
			// TCP socket — `ssh.NewClientConn` does not accept a context.
			stop := context.AfterFunc(ctx, func() { _ = nconn.Close() })
			cliConn, chans, reqs, err := ssh.NewClientConn(nconn, addr, cfg)
			stop()
			if err != nil {
				_ = nconn.Close()
				return nil, err
			}
			// Clear the read deadline so the session is not truncated
			// mid-command.
			_ = nconn.SetReadDeadline(time.Time{})
			return realSSHClient{c: ssh.NewClient(cliConn, chans, reqs)}, nil
		}
	}

	// Run dial+session in a goroutine. context.AfterFunc closes the client
	// handle on ctx cancel, which unblocks any in-flight session call so
	// the goroutine returns promptly instead of leaking.
	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		client, err := dial(ctx, "tcp", addr, cfg)
		if err != nil {
			ch <- result{nil, fmt.Errorf("dial %s: %w", addr, err)}
			return
		}
		// Close the client at most once, whether triggered by cancel or by
		// our own cleanup. sync.Once gives us the happens-before edge the
		// race detector wants between the AfterFunc and the explicit close.
		var closeOnce sync.Once
		closeClient := func() { closeOnce.Do(func() { _ = client.Close() }) }
		stop := context.AfterFunc(ctx, closeClient)

		sess, err := client.NewSession()
		if err != nil {
			stop()
			closeClient()
			ch <- result{nil, err}
			return
		}
		out, runErr := sess.CombinedOutput(cmd)
		_ = sess.Close()
		stop()
		closeClient()
		ch <- result{out, runErr}
	}()

	// Outer select belts-and-suspenders the in-goroutine ctx wiring: if a
	// fake dial or a buggy net stack ignores ctx, we still return
	// promptly. The goroutine eventually finishes on its own once the
	// OS-level timeout fires; the AfterFunc + read deadline above keep
	// that bounded.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("%s: %w (output: %s)", t.Name(), r.err, string(r.out))
		}
		return nil
	}
}
