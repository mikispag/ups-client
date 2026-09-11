package notifier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikispag/ups-client/monitor"
	"golang.org/x/crypto/ssh"
)

func TestSSHTimeoutBoundsSession(t *testing.T) {
	closed := make(chan struct{})
	target := &SSHTarget{
		Host: "h", User: "u", Password: "p", Command: "sleep 60",
		InsecureIgnoreHostKey: true, Timeout: 20 * time.Millisecond,
		dial: func(context.Context, string, string, *ssh.ClientConfig) (sshClient, error) {
			return &blockingSSHClient{closed: closed}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := target.Notify(ctx, sampleEvent(monitor.EventOnline)); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Notify error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Errorf("target timeout ignored: took %s", elapsed)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("timed-out SSH client was not closed")
	}
}

func TestShellTimeoutStopsDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell only")
	}
	marker := filepath.Join(t.TempDir(), "survived")
	target := &ShellTarget{Command: "/bin/sh", Args: []string{"-c", `sh -c 'sleep 0.3; echo alive > "$1"' sh "$1" & wait`, "sh", marker}, Timeout: 20 * time.Millisecond}
	start := time.Now()
	if err := target.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("descendant kept output pipes open for %s", elapsed)
	}
	time.Sleep(350 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("descendant survived cancellation: %v", err)
	}
}

func TestShellClearsMissingSnapshotEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell only")
	}
	t.Setenv("UPS_ALARM", "stale alarm")
	target := &ShellTarget{Command: "/bin/sh", Args: []string{"-c", `test -z "$UPS_ALARM"`}}
	if err := target.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err != nil {
		t.Fatalf("inherited stale alarm: %v", err)
	}
}

func TestWebhookRendersURL(t *testing.T) {
	path := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path <- r.URL.Path
	}))
	defer srv.Close()
	target := &WebhookTarget{URL: srv.URL + "/{{.UPS}}/{{.Event}}"}
	if err := target.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err != nil {
		t.Fatal(err)
	}
	if got := <-path; got != "/ups/ONLINE" {
		t.Errorf("request path = %q", got)
	}
}

func TestWebhookRejectsRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/redirected" {
			http.Redirect(w, r, "/redirected", http.StatusFound)
		}
	}))
	defer srv.Close()
	target := &WebhookTarget{URL: srv.URL}
	if err := target.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Fatal("redirected POST was reported as delivered")
	}
}

func TestWebhookErrorsHideCredentials(t *testing.T) {
	target := &WebhookTarget{URL: "http://user:password@127.0.0.1:1/secret-token?key=secret-query"}
	err := target.Notify(context.Background(), sampleEvent(monitor.EventOnline))
	if err == nil {
		t.Fatal("expected network failure")
	}
	for _, secret := range []string{"password", "secret-token", "secret-query"} {
		if strings.Contains(target.Name()+err.Error(), secret) {
			t.Errorf("webhook credentials leaked: %s %v", target.Name(), err)
		}
	}
}

func TestTelegramValidatesResponse(t *testing.T) {
	for _, body := range []string{`{"ok":false,"error_code":429,"description":"retry later"}`, `<html>login</html>`, `{"ok":true`, strings.Repeat("x", 70*1024)} {
		t.Run(fmt.Sprintf("length_%d", len(body)), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			target := &TelegramTarget{BotToken: "TOKEN", ChatID: "1", APIBase: srv.URL}
			if err := target.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
				t.Fatal("invalid Telegram response reported as success")
			}
		})
	}
}

func TestTelegramRedactsTokenInAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"invalid TOKEN-SECRET"}`)
	}))
	defer srv.Close()
	target := &TelegramTarget{BotToken: "TOKEN-SECRET", ChatID: "1", APIBase: srv.URL}
	err := target.Notify(context.Background(), sampleEvent(monitor.EventOnline))
	if err == nil || strings.Contains(err.Error(), "TOKEN-SECRET") {
		t.Fatalf("API error = %v", err)
	}
}

func TestHTTPClientInitializationConcurrent(t *testing.T) {
	webhook := &WebhookTarget{}
	telegram := &TelegramTarget{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			webhook.httpClient()
			telegram.httpClient()
		}()
	}
	close(start)
	wg.Wait()
}

func TestShellQuoteProtectsTemplateData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell only")
	}
	const command = `printf '%s' {{.Alarm | shellquote}} > "$1"`
	if err := ValidateTemplate("shellquote", command); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "battery's alarm", "line 1\nline 2", `'; printf injected; '`, "$(printf injected) `printf injected` $HOME"} {
		t.Run(value, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "output")
			event := sampleEvent(monitor.EventAlarm)
			event.Snapshot.Vars["ups.alarm"] = value
			target := &ShellTarget{Command: "/bin/sh", Args: []string{"-c", command, "sh", out}}
			if err := target.Notify(context.Background(), event); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(out)
			if err != nil || string(got) != value {
				t.Fatalf("shell changed template data: %q, %v", got, err)
			}
		})
	}
}

func TestShellOutputIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell only")
	}
	target := &ShellTarget{Command: "/bin/sh", Args: []string{"-c", "head -c 131072 /dev/zero; exit 1"}}
	err := target.Notify(context.Background(), sampleEvent(monitor.EventOnline))
	if err == nil || len(err.Error()) > maxResponseBytes+128 {
		t.Fatalf("command output was not capped (error size %d)", len(fmt.Sprint(err)))
	}
}

func TestShellExitedParentStillStopsDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell only")
	}
	marker := filepath.Join(t.TempDir(), "survived")
	target := &ShellTarget{Command: "/bin/sh", Args: []string{"-c", `sh -c 'sleep 0.3; echo alive > "$1"' sh "$1" &`, "sh", marker}}
	if err := target.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected output pipe cleanup error")
	}
	time.Sleep(350 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("descendant survived parent exit and pipe cleanup: %v", err)
	}
}

func TestTelegramDefaultMessageDoesNotUseParseMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if got := r.Form.Get("parse_mode"); got != "" {
			t.Errorf("unescaped default message sent with parse_mode=%q", got)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	target := &TelegramTarget{BotToken: "TOKEN", ChatID: "1", APIBase: srv.URL, ParseMode: "MarkdownV2"}
	if err := target.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err != nil {
		t.Fatal(err)
	}
}

func TestShellFailedParentStillStopsDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell only")
	}
	marker := filepath.Join(t.TempDir(), "survived")
	target := &ShellTarget{Command: "/bin/sh", Args: []string{"-c", `sh -c 'sleep 0.3; echo alive > "$1"' sh "$1" & exit 1`, "sh", marker}}
	if err := target.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Fatal("expected command failure")
	}
	time.Sleep(350 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("descendant survived failed parent and pipe cleanup: %v", err)
	}
}

func TestWebhookCustomHostHeader(t *testing.T) {
	host := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host <- r.Host
	}))
	defer srv.Close()
	target := &WebhookTarget{URL: srv.URL, Headers: map[string]string{"hOsT": "{{.UPS}}.example"}}
	if err := target.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err != nil {
		t.Fatal(err)
	}
	if got := <-host; got != "ups.example" {
		t.Errorf("configured Host header ignored: %q", got)
	}
}

func TestTelegramContextErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer srv.Close()
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := context.DeadlineExceeded
			if cancelled {
				cancel()
				want = context.Canceled
			}
			target := &TelegramTarget{BotToken: "SECRET", ChatID: "1", APIBase: srv.URL, Timeout: 20 * time.Millisecond}
			err := target.Notify(ctx, sampleEvent(monitor.EventOnline))
			if !errors.Is(err, want) {
				t.Errorf("lost context error identity: %v; want %v", err, want)
			}
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Errorf("context error leaked token: %v", err)
			}
		})
	}
}

func TestWebhookHeaderErrorsHideSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	for _, header := range []string{"Authorization", "Host"} {
		t.Run(header, func(t *testing.T) {
			event := sampleEvent(monitor.EventAlarm)
			event.Snapshot.Vars["ups.alarm"] = "SECRET-CREDENTIAL\n"
			target := &WebhookTarget{URL: srv.URL, Headers: map[string]string{header: "{{.Alarm}}"}}
			err := target.Notify(context.Background(), event)
			if err == nil {
				t.Fatal("expected invalid header error")
			}
			if strings.Contains(err.Error(), "SECRET-CREDENTIAL") {
				t.Errorf("invalid header error exposed secret: %v", err)
			}
		})
	}
}

func TestWebhookHostAuthorities(t *testing.T) {
	for _, tc := range []struct {
		host  string
		valid bool
	}{
		{":", false},
		{":8080", false},
		{"example.com,other", true},
		{"example.com|other", false},
		{"example.com", true},
		{"example.com:8080", true},
		{"[::1]", true},
		{"[::1]:8080", true},
	} {
		t.Run(tc.host, func(t *testing.T) {
			received := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Host
			}))
			defer srv.Close()
			target := &WebhookTarget{URL: srv.URL, Headers: map[string]string{"Host": tc.host}}
			err := target.Notify(context.Background(), sampleEvent(monitor.EventOnline))
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				if got := <-received; got != tc.host {
					t.Errorf("Host changed: %q", got)
				}
			} else {
				if err == nil {
					t.Error("invalid Host accepted")
				}
				select {
				case got := <-received:
					t.Errorf("invalid Host reached server as %q", got)
				default:
				}
			}
		})
	}
}
