package notifier

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mikispag/ups-client/monitor"
	"golang.org/x/crypto/ssh"
)

type fakeSSHClient struct {
	closed bool
	sess   *fakeSSHSession
	err    error
}

func (f *fakeSSHClient) NewSession() (sshSession, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sess, nil
}
func (f *fakeSSHClient) Close() error { f.closed = true; return nil }

type fakeSSHSession struct {
	cmd  string
	out  []byte
	err  error
	done bool
}

func (f *fakeSSHSession) CombinedOutput(cmd string) ([]byte, error) {
	f.cmd = cmd
	f.done = true
	return f.out, f.err
}
func (f *fakeSSHSession) Close() error { return nil }

func writeTempKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "ups-client test key")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSSHTargetRunsCommand(t *testing.T) {
	keyPath := writeTempKey(t)
	sess := &fakeSSHSession{out: []byte("ok")}
	client := &fakeSSHClient{sess: sess}

	tt := &SSHTarget{
		Label:                 "remote",
		Host:                  "host.example",
		Port:                  22,
		User:                  "ops",
		PrivateKeyFile:        keyPath,
		InsecureIgnoreHostKey: true,
		Command:               "logger -t ups '{{.Event}} on {{.UPS}}'",
		Timeout:               2 * time.Second,
		dial: func(network, addr string, cfg *ssh.ClientConfig) (sshClient, error) {
			if network != "tcp" {
				t.Errorf("network = %q", network)
			}
			if addr != "host.example:22" {
				t.Errorf("addr = %q", addr)
			}
			if cfg.User != "ops" {
				t.Errorf("user = %q", cfg.User)
			}
			return client, nil
		},
	}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnBatt)); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !sess.done {
		t.Error("session not executed")
	}
	if !strings.Contains(sess.cmd, "ONBATT on ups") {
		t.Errorf("cmd = %q", sess.cmd)
	}
	if !client.closed {
		t.Error("client not closed")
	}
}

func TestSSHTargetDialError(t *testing.T) {
	keyPath := writeTempKey(t)
	tt := &SSHTarget{
		Host: "h", User: "u", PrivateKeyFile: keyPath, InsecureIgnoreHostKey: true,
		Command: "true",
		dial: func(network, addr string, cfg *ssh.ClientConfig) (sshClient, error) {
			return nil, errors.New("connection refused")
		},
	}
	err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("got err = %v", err)
	}
}

func TestSSHTargetSessionError(t *testing.T) {
	keyPath := writeTempKey(t)
	tt := &SSHTarget{
		Host: "h", User: "u", PrivateKeyFile: keyPath, InsecureIgnoreHostKey: true,
		Command: "true",
		dial: func(network, addr string, cfg *ssh.ClientConfig) (sshClient, error) {
			return &fakeSSHClient{err: errors.New("session denied")}, nil
		},
	}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected session error")
	}
}

func TestSSHTargetMissingFields(t *testing.T) {
	tt := &SSHTarget{}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected host error")
	}
	tt.Host = "h"
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected user error")
	}
	tt.User = "u"
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected command error")
	}
}

func TestSSHTargetNoAuth(t *testing.T) {
	tt := &SSHTarget{Host: "h", User: "u", Command: "x", InsecureIgnoreHostKey: true}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected no-auth error")
	}
}

func TestSSHTargetPasswordAuth(t *testing.T) {
	tt := &SSHTarget{
		Host: "h", User: "u", Command: "true",
		Password: "secret", InsecureIgnoreHostKey: true,
		dial: func(network, addr string, cfg *ssh.ClientConfig) (sshClient, error) {
			if len(cfg.Auth) == 0 {
				t.Error("expected at least one auth method")
			}
			return &fakeSSHClient{sess: &fakeSSHSession{}}, nil
		},
	}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err != nil {
		t.Errorf("Notify: %v", err)
	}
}

func TestSSHTargetBadKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	_ = os.WriteFile(path, []byte("not a key"), 0600)
	tt := &SSHTarget{
		Host: "h", User: "u", Command: "x",
		PrivateKeyFile: path, InsecureIgnoreHostKey: true,
	}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected parse-key error")
	}
}

func TestSSHHostKeyCallbackKnownHostsFile(t *testing.T) {
	dir := t.TempDir()
	khPath := filepath.Join(dir, "known_hosts")
	// A known_hosts file with one valid pinned host. This is a *real*
	// ed25519 public key (paired key isn't needed; we never actually use
	// it to connect — knownhosts.New just parses the file).
	const kh = "host.example ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIBJyx2cuM4l3OfVBu/0e9NrPQ4cUQozRu7yRmdmDIxd\n"
	if err := os.WriteFile(khPath, []byte(kh), 0600); err != nil {
		t.Fatal(err)
	}
	tt := &SSHTarget{KnownHostsFile: khPath}
	cb, err := tt.hostKeyCallback()
	if err != nil {
		t.Fatalf("hostKeyCallback: %v", err)
	}
	if cb == nil {
		t.Error("nil callback")
	}
}

func TestSSHHostKeyCallbackInsecure(t *testing.T) {
	tt := &SSHTarget{InsecureIgnoreHostKey: true}
	cb, err := tt.hostKeyCallback()
	if err != nil || cb == nil {
		t.Errorf("insecure cb: %v %v", cb, err)
	}
}

func TestSSHTargetName(t *testing.T) {
	tt := &SSHTarget{Host: "h", User: "u"}
	if !strings.Contains(tt.Name(), "u@h") {
		t.Errorf("Name = %q", tt.Name())
	}
	tt.Label = "x"
	if tt.Name() != "ssh:x" {
		t.Errorf("labeled Name = %q", tt.Name())
	}
}
