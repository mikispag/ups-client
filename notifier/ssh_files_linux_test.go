//go:build linux

package notifier

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSSHRejectsCredentialPipes(t *testing.T) {
	for _, kind := range []string{"private key", "known hosts"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials")
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			// Keep a writer open so an accidental read blocks. Closing it
			// below releases the reader even when this regression fails.
			writer, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			done := make(chan error, 1)
			go func() {
				if kind == "private key" {
					_, err := (&SSHTarget{PrivateKeyFile: path}).authMethods()
					done <- err
				} else {
					_, err := (&SSHTarget{KnownHostsFile: path}).hostKeyCallback()
					done <- err
				}
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("credential pipe accepted")
				}
			case <-time.After(100 * time.Millisecond):
				t.Error("credential pipe read blocked before the SSH timeout")
				_ = writer.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("credential reader did not finish after FIFO cleanup")
				}
			}
		})
	}
}

func TestSSHCredentialFilesFollowSymlinks(t *testing.T) {
	key := writeTempKey(t)
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(knownHosts, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{key, knownHosts} {
		if err := os.Symlink(path, path+".link"); err != nil {
			t.Fatal(err)
		}
	}
	target := &SSHTarget{PrivateKeyFile: key + ".link", KnownHostsFile: knownHosts + ".link"}
	if _, err := target.authMethods(); err != nil {
		t.Fatalf("symlink to regular private key rejected: %v", err)
	}
	if _, err := target.hostKeyCallback(); err != nil {
		t.Fatalf("symlink to regular known_hosts rejected: %v", err)
	}
}
