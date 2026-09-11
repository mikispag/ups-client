package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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

	client     *http.Client
	clientOnce sync.Once
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
	// The fallback summary is plain text and contains unescaped punctuation.
	if t.ParseMode != "" && t.Message != "" {
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
		// url.Parse failures embed the full URL (with bot token) in the
		// returned error — redact the same way as the Do() path below.
		return t.requestError(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.httpClient().Do(req)
	if err != nil {
		// http.Client wraps the request URL into *url.Error, which would
		// leak the bot token into logs/metrics. Redact it.
		return t.requestError(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return t.requestError(fmt.Errorf("read response: %w", err))
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("%s: response exceeds %d bytes", t.Name(), maxResponseBytes)
	}
	var response struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	decodeErr := json.Unmarshal(body, &response)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !response.OK {
		if response.Description != "" {
			return fmt.Errorf("%s: %d %s", t.Name(), response.ErrorCode, redactToken(response.Description, t.BotToken))
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return fmt.Errorf("%s: invalid or unsuccessful API response", t.Name())
		}
		return fmt.Errorf("%s: HTTP %d", t.Name(), resp.StatusCode)
	}
	if decodeErr != nil {
		return fmt.Errorf("%s: invalid API response", t.Name())
	}
	return nil
}

func (t *TelegramTarget) requestError(err error) error {
	// Preserve cancellation identity without exposing the credential-bearing
	// URL carried by http.Client errors.
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cause) {
			return fmt.Errorf("%s: %w", t.Name(), cause)
		}
	}
	return fmt.Errorf("%s: %s", t.Name(), redactToken(err.Error(), t.BotToken))
}

// redactToken replaces every occurrence of token in s with "***", in both
// the raw form and the strconv.Quote-escaped form (because *url.Error
// renders the URL via strconv.Quote, which escapes control bytes that
// would otherwise be present verbatim in the token).
func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	s = strings.ReplaceAll(s, token, "***")
	quoted := strings.TrimPrefix(strings.TrimSuffix(strconv.Quote(token), `"`), `"`)
	if quoted != token {
		s = strings.ReplaceAll(s, quoted, "***")
	}
	return s
}

func (t *TelegramTarget) httpClient() *http.Client {
	t.clientOnce.Do(func() {
		if t.client != nil {
			return
		}
		timeout := t.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		t.client = &http.Client{Timeout: timeout, CheckRedirect: rejectRedirect}
	})
	return t.client
}
