package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mikispag/ups-client/monitor"
)

// TelegramTarget posts a message via the Telegram Bot HTTP API. BotToken is
// the value from @BotFather. ChatID may be a numeric chat id or an
// `@channelusername`. Message is a text/template string evaluated against
// TemplateData; if empty, a default summary is used.
type TelegramTarget struct {
	Label     string
	BotToken  string
	ChatID    string
	Message   string
	ParseMode string // "", "Markdown", "MarkdownV2", "HTML"
	APIBase   string // override for tests; defaults to https://api.telegram.org
	Timeout   time.Duration
	Filter    Filter

	client *http.Client
}

// Name implements Notifier.
func (t *TelegramTarget) Name() string {
	if t.Label != "" {
		return "telegram:" + t.Label
	}
	return "telegram:" + t.ChatID
}

// Match implements Notifier.
func (t *TelegramTarget) Match(e monitor.Event) bool { return t.Filter.Match(e.Kind) }

// Notify implements Notifier.
func (t *TelegramTarget) Notify(ctx context.Context, e monitor.Event) error {
	if t.BotToken == "" {
		return fmt.Errorf("telegram %q: empty bot_token", t.Label)
	}
	if t.ChatID == "" {
		return fmt.Errorf("telegram %q: empty chat_id", t.Label)
	}

	td := NewTemplateData(e)
	text := t.Message
	if text == "" {
		text = defaultMessage(td)
	} else {
		rendered, err := renderTemplate(t.Name()+".message", text, td)
		if err != nil {
			return err
		}
		text = rendered
	}

	apiBase := t.APIBase
	if apiBase == "" {
		apiBase = "https://api.telegram.org"
	}

	form := url.Values{}
	form.Set("chat_id", t.ChatID)
	form.Set("text", text)
	if t.ParseMode != "" {
		form.Set("parse_mode", t.ParseMode)
	}

	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(apiBase, "/"), t.BotToken)

	if t.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.Timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		// Telegram API returns JSON like {"ok":false,"error_code":400,"description":"..."}
		var apiErr struct {
			OK          bool   `json:"ok"`
			ErrorCode   int    `json:"error_code"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Description != "" {
			return fmt.Errorf("%s: %d %s", t.Name(), apiErr.ErrorCode, apiErr.Description)
		}
		return fmt.Errorf("%s: HTTP %d", t.Name(), resp.StatusCode)
	}
	return nil
}

func (t *TelegramTarget) httpClient() *http.Client {
	if t.client != nil {
		return t.client
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	t.client = &http.Client{Timeout: timeout}
	return t.client
}
