package vmbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Minimal Bot API types (only what the bot reads).
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

type User struct {
	ID int64 `json:"id"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data"`
}

// Button is one inline-keyboard button.
type Button struct{ Text, Data string }

// OutMessage is a message to send. Secret messages carry a capability link:
// previews are disabled and forwarding/saving is blocked.
type OutMessage struct {
	ChatID       int64
	Text         string
	Inline       []Button // one row; takes precedence over MainKeyboard
	MainKeyboard bool     // attach the persistent reply keyboard
	Secret       bool
}

// TGAPI is the seam between the bot and the Bot API (real client or fake).
type TGAPI interface {
	GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error)
	SendMessage(ctx context.Context, m OutMessage) error
	AnswerCallback(ctx context.Context, id, text string) error
}

// TelegramClient talks to the Bot API over HTTPS using only the standard library.
// The token appears only in the request path; no error it returns contains the
// URL, the token or the server's free-text description.
type TelegramClient struct {
	base  string
	token string
	hc    *http.Client
}

// NewTelegramClient builds a client. base is normally https://api.telegram.org;
// tests pass an httptest server URL (the production config has no knob for it).
func NewTelegramClient(base, token string, hc *http.Client) *TelegramClient {
	if hc == nil {
		hc = &http.Client{}
	}
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &TelegramClient{base: strings.TrimRight(base, "/"), token: token, hc: &c}
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
}

func (c *TelegramClient) call(ctx context.Context, method string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("telegram %s: encode request", method)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("telegram %s: build request", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("telegram %s: %w", method, ctx.Err())
		}
		return fmt.Errorf("telegram %s: request failed", method) // url.Error would embed the token
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: read response failed", method)
	}
	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return fmt.Errorf("telegram %s: unexpected response (HTTP %d)", method, resp.StatusCode)
	}
	if !ar.OK {
		return fmt.Errorf("telegram %s: api error %d", method, ar.ErrorCode)
	}
	if out != nil {
		if err := json.Unmarshal(ar.Result, out); err != nil {
			return fmt.Errorf("telegram %s: unexpected result", method)
		}
	}
	return nil
}

func (c *TelegramClient) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	var ups []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeoutSec, "allowed_updates": []string{"message", "callback_query"},
	}, &ups)
	return ups, err
}

var mainKeyboard = map[string]any{
	"keyboard": [][]map[string]string{
		{{"text": "Создать"}, {"text": "Статус"}},
		{{"text": "Терминал"}, {"text": "Удалить"}},
	},
	"resize_keyboard": true,
	"is_persistent":   true,
}

func (c *TelegramClient) SendMessage(ctx context.Context, m OutMessage) error {
	body := map[string]any{
		"chat_id": m.ChatID, "text": m.Text,
		"link_preview_options":     map[string]any{"is_disabled": true},
		"disable_web_page_preview": true, // pre-7.0 clients of the Bot API
	}
	if m.Secret {
		body["protect_content"] = true
	}
	switch {
	case len(m.Inline) > 0:
		row := make([]map[string]string, 0, len(m.Inline))
		for _, b := range m.Inline {
			row = append(row, map[string]string{"text": b.Text, "callback_data": b.Data})
		}
		body["reply_markup"] = map[string]any{"inline_keyboard": [][]map[string]string{row}}
	case m.MainKeyboard:
		body["reply_markup"] = mainKeyboard
	}
	return c.call(ctx, "sendMessage", body, nil)
}

func (c *TelegramClient) AnswerCallback(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}
