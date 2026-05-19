package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TelegramNotifier 通过 Telegram Bot API 发送通知。
type TelegramNotifier struct {
	botToken string
	chatID   string
	client   *http.Client
	baseURL  string
}

// NewTelegramNotifier 构造 Telegram 通知器。
func NewTelegramNotifier(cfg TelegramConfig) Notifier {
	if strings.TrimSpace(cfg.BotToken) == "" || strings.TrimSpace(cfg.ChatID) == "" {
		return NewNoop()
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.telegram.org"
	}
	client := cfg.Client
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	return &TelegramNotifier{
		botToken: strings.TrimSpace(cfg.BotToken),
		chatID:   strings.TrimSpace(cfg.ChatID),
		client:   client,
		baseURL:  baseURL,
	}
}

// Notify 发送 Telegram 消息。
func (n *TelegramNotifier) Notify(ctx context.Context, message Message) error {
	if n == nil {
		return nil
	}
	text := formatMessage(message, defaultTelegramLimit)
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", n.baseURL, n.botToken)
	form := url.Values{}
	form.Set("chat_id", n.chatID)
	form.Set("text", text)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("telegram notify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram notify sendMessage: %s", redactSensitive(err.Error(), n.botToken))
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("telegram notify http status %d", resp.StatusCode)
	}

	var payload telegramResponse
	if decodeErr := json.NewDecoder(resp.Body).Decode(&payload); decodeErr != nil {
		return fmt.Errorf("telegram notify decode response failed")
	}
	if !payload.OK {
		return payload.sanitizedError()
	}
	return nil
}

type telegramResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}

func (r telegramResponse) sanitizedError() error {
	desc := strings.TrimSpace(r.Description)
	if desc == "" {
		desc = "telegram request failed"
	}
	if r.ErrorCode > 0 {
		return fmt.Errorf("telegram api error %d: %s", r.ErrorCode, desc)
	}
	return fmt.Errorf("telegram api error: %s", desc)
}

func redactSensitive(text, token string) string {
	if token == "" || text == "" {
		return text
	}
	redacted := strings.ReplaceAll(text, token, "[redacted]")
	redacted = strings.ReplaceAll(redacted, "bot"+token, "bot[redacted]")
	return redacted
}
