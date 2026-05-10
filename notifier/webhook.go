package notifier

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mikispag/ups-client/monitor"
)

// WebhookTarget posts the rendered body to URL on each matching event. It is
// generic enough to drive ntfy.sh, Slack incoming webhooks, Discord, Mattermost,
// Home Assistant webhooks, and similar services. Headers and body are
// text/template fragments evaluated against TemplateData.
type WebhookTarget struct {
	Label              string
	URL                string
	Method             string // defaults to POST
	Headers            map[string]string
	Body               string // empty → JSON-marshalled TemplateData
	Timeout            time.Duration
	InsecureSkipVerify bool
	Filter             Filter

	client *http.Client
}

// Name implements Notifier.
func (t *WebhookTarget) Name() string {
	if t.Label != "" {
		return "webhook:" + t.Label
	}
	return "webhook:" + t.URL
}

// Match implements Notifier.
func (t *WebhookTarget) Match(e monitor.Event) bool { return t.Filter.Match(e.Kind) }

// Notify implements Notifier.
func (t *WebhookTarget) Notify(ctx context.Context, e monitor.Event) error {
	if t.URL == "" {
		return fmt.Errorf("webhook %q: empty URL", t.Label)
	}
	td := NewTemplateData(e)

	method := strings.ToUpper(strings.TrimSpace(t.Method))
	if method == "" {
		method = http.MethodPost
	}

	var body io.Reader
	contentType := ""
	switch {
	case t.Body != "":
		rendered, err := renderTemplate(t.Name()+".body", t.Body, td)
		if err != nil {
			return err
		}
		body = strings.NewReader(rendered)
		contentType = "text/plain; charset=utf-8"
	default:
		// Default body: JSON-encoded TemplateData. Useful for HA / generic webhooks.
		buf, err := json.Marshal(td)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(buf))
		contentType = "application/json"
	}

	if t.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.Timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, method, t.URL, body)
	if err != nil {
		return err
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range t.Headers {
		rendered, rerr := renderTemplate(t.Name()+".header", v, td)
		if rerr != nil {
			return rerr
		}
		req.Header.Set(k, rendered)
	}

	client := t.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: HTTP %d", t.Name(), resp.StatusCode)
	}
	return nil
}

func (t *WebhookTarget) httpClient() *http.Client {
	if t.client != nil {
		return t.client
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if t.InsecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //#nosec G402 — opt-in only
	}
	t.client = &http.Client{Transport: tr, Timeout: timeout}
	return t.client
}
