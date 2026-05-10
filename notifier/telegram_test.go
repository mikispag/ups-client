package notifier

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mikispag/ups-client/monitor"
)

func TestTelegramTargetSendsMessage(t *testing.T) {
	var (
		gotPath string
		gotForm url.Values
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	tt := &TelegramTarget{
		BotToken:  "TESTTOKEN",
		ChatID:    "12345",
		Message:   "{{.Event}} on {{.UPS}}",
		ParseMode: "MarkdownV2",
		APIBase:   srv.URL,
	}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnBatt)); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotPath != "/botTESTTOKEN/sendMessage" {
		t.Errorf("path = %q", gotPath)
	}
	if gotForm.Get("chat_id") != "12345" {
		t.Errorf("chat_id = %q", gotForm.Get("chat_id"))
	}
	if !strings.Contains(gotForm.Get("text"), "ONBATT on ups") {
		t.Errorf("text = %q", gotForm.Get("text"))
	}
	if gotForm.Get("parse_mode") != "MarkdownV2" {
		t.Errorf("parse_mode = %q", gotForm.Get("parse_mode"))
	}
}

func TestTelegramTargetDefaultMessage(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tt := &TelegramTarget{BotToken: "TOK", ChatID: "1", APIBase: srv.URL}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotForm.Get("text"), "ONLINE") {
		t.Errorf("default text = %q", gotForm.Get("text"))
	}
}

func TestTelegramTargetAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"chat not found"}`))
	}))
	defer srv.Close()
	tt := &TelegramTarget{BotToken: "T", ChatID: "X", APIBase: srv.URL}
	err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline))
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("got err = %v", err)
	}
}

func TestTelegramTargetHTTPErrorWithoutDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	tt := &TelegramTarget{BotToken: "T", ChatID: "X", APIBase: srv.URL}
	err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline))
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("got err = %v", err)
	}
}

func TestTelegramRequiresFields(t *testing.T) {
	tt := &TelegramTarget{}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected bot_token error")
	}
	tt.BotToken = "x"
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected chat_id error")
	}
}

func TestTelegramName(t *testing.T) {
	tt := &TelegramTarget{ChatID: "@chan"}
	if tt.Name() != "telegram:@chan" {
		t.Errorf("Name = %q", tt.Name())
	}
	tt.Label = "ops"
	if tt.Name() != "telegram:ops" {
		t.Errorf("labeled Name = %q", tt.Name())
	}
}

func TestTelegramRedactsTokenInRequestParseError(t *testing.T) {
	// A bot token containing a URL-invalid byte (e.g. \n from a YAML block
	// scalar copy-paste) makes http.NewRequestWithContext fail with a
	// *url.Error embedding the full URL. The error must not surface the
	// raw token.
	tt := &TelegramTarget{
		BotToken: "TOKEN-WITH-\nNEWLINE",
		ChatID:   "1",
		APIBase:  "https://api.telegram.org",
		Timeout:  100 * time.Millisecond,
	}
	err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline))
	if err == nil {
		t.Fatal("expected url.Parse error")
	}
	if strings.Contains(err.Error(), "TOKEN-WITH-") {
		t.Errorf("bot token leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("expected redaction marker in %v", err)
	}
}

func TestTelegramRedactsTokenInNetworkError(t *testing.T) {
	// Point at a closed listener so http.Client.Do returns a *url.Error
	// whose URL embeds the bot token.
	tt := &TelegramTarget{
		BotToken: "SECRET-TOKEN-12345",
		ChatID:   "1",
		APIBase:  "http://127.0.0.1:1", // refused
		Timeout:  100 * time.Millisecond,
	}
	err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline))
	if err == nil {
		t.Fatal("expected network error")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN-12345") {
		t.Errorf("bot token leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("expected redaction marker in %v", err)
	}
}

func TestTelegramBadTemplate(t *testing.T) {
	tt := &TelegramTarget{BotToken: "T", ChatID: "X", Message: "{{.Missing"}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected template error")
	}
}
