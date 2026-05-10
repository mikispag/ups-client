package notifier

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mikispag/ups-client/monitor"
)

func TestWebhookTargetPostsBody(t *testing.T) {
	var (
		gotBody    string
		gotMethod  string
		gotHeaders http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotHeaders = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tt := &WebhookTarget{
		URL:    srv.URL,
		Method: "POST",
		Headers: map[string]string{
			"X-UPS-Event": "{{.Event}}",
			"Title":       "UPS {{.UPS}}",
		},
		Body: "Event {{.Event}} on {{.UPS}}: {{.Status}}",
	}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnBatt)); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q", gotMethod)
	}
	if !strings.Contains(gotBody, "ONBATT on ups: OL") {
		t.Errorf("body = %q", gotBody)
	}
	if gotHeaders.Get("X-Ups-Event") != "ONBATT" {
		t.Errorf("X-UPS-Event header = %q", gotHeaders.Get("X-Ups-Event"))
	}
	if gotHeaders.Get("Title") != "UPS ups" {
		t.Errorf("Title header = %q", gotHeaders.Get("Title"))
	}
}

func TestWebhookTargetDefaultJSONBody(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tt := &WebhookTarget{URL: srv.URL}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got["Event"] != "ONLINE" {
		t.Errorf("decoded body = %#v", got)
	}
}

func TestWebhookTargetNtfyShape(t *testing.T) {
	// Verify the README's ntfy.sh recipe: POST plain text body with
	// Title/Priority/Tags headers — no special-casing needed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("ntfy expects POST, got %s", r.Method)
		}
		if got := r.Header.Get("Title"); !strings.Contains(got, "UPS ups") {
			t.Errorf("Title = %q", got)
		}
		if got := r.Header.Get("Priority"); got != "high" {
			t.Errorf("Priority = %q", got)
		}
		if got := r.Header.Get("Tags"); got != "warning,electric_plug" {
			t.Errorf("Tags = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "ONBATT") {
			t.Errorf("body = %q", string(body))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tt := &WebhookTarget{
		URL: srv.URL + "/ups-events",
		Headers: map[string]string{
			"Title":    "UPS {{.UPS}} – {{.Event}}",
			"Priority": "high",
			"Tags":     "warning,electric_plug",
		},
		Body: "{{.Event}} on {{.UPS}} (status: {{.Status}}, charge: {{.BatteryCharge}}%)",
	}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnBatt)); err != nil {
		t.Fatalf("Notify: %v", err)
	}
}

func TestWebhookTargetHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	tt := &WebhookTarget{URL: srv.URL, Body: "x"}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected error on 400")
	}
}

func TestWebhookTargetBadTemplate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	tt := &WebhookTarget{URL: srv.URL, Body: "{{.Missing"}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected template error")
	}
}

func TestWebhookEmptyURL(t *testing.T) {
	tt := &WebhookTarget{}
	if err := tt.Notify(context.Background(), sampleEvent(monitor.EventOnline)); err == nil {
		t.Error("expected error on empty URL")
	}
}

func TestWebhookName(t *testing.T) {
	tt := &WebhookTarget{URL: "https://example.com"}
	if !strings.HasPrefix(tt.Name(), "webhook:") {
		t.Errorf("Name = %q", tt.Name())
	}
	tt.Label = "lab"
	if tt.Name() != "webhook:lab" {
		t.Errorf("labeled name = %q", tt.Name())
	}
}
