package tgbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type User struct {
	ID int64 `json:"id"`
}
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data"`
}
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}
type Button struct{ Text, Data string }
type OutMessage struct {
	ChatID               int64
	Text                 string
	Inline               [][]Button
	MainKeyboard, Secret bool
}
type Telegram interface {
	GetUpdates(context.Context, int64, int) ([]Update, error)
	SendMessage(context.Context, OutMessage) error
	AnswerCallback(context.Context, string, string) error
}
type TelegramClient struct {
	base, token string
	http        *http.Client
}

func NewTelegramClient(base, token string, hc *http.Client) *TelegramClient {
	var h http.Client
	if hc != nil {
		h = *hc
	}
	if h.Timeout <= 0 || h.Timeout > 45*time.Second {
		h.Timeout = 45 * time.Second
	}
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &TelegramClient{strings.TrimRight(base, "/"), token, &h}
}
func (c *TelegramClient) call(ctx context.Context, method string, in, out any) error {
	b, e := json.Marshal(in)
	if e != nil {
		return errors.New("telegram: encode failed")
	}
	r, e := http.NewRequestWithContext(ctx, "POST", c.base+"/bot"+c.token+"/"+method, bytes.NewReader(b))
	if e != nil {
		return errors.New("telegram: invalid request")
	}
	r.Header.Set("Content-Type", "application/json")
	resp, e := c.http.Do(r)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("telegram: transport failed")
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if e != nil || len(raw) > 4<<20 {
		return errors.New("telegram: response size invalid")
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(raw, &env) != nil || !env.OK {
		return fmt.Errorf("telegram: unsuccessful response (HTTP %d)", resp.StatusCode)
	}
	if out != nil && json.Unmarshal(env.Result, out) != nil {
		return errors.New("telegram: invalid result")
	}
	return nil
}
func (c *TelegramClient) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	var ups []Update
	e := c.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": timeout, "allowed_updates": []string{"message", "callback_query"}}, &ups)
	return ups, e
}
func (c *TelegramClient) SendMessage(ctx context.Context, m OutMessage) error {
	body := map[string]any{"chat_id": m.ChatID, "text": m.Text, "link_preview_options": map[string]bool{"is_disabled": true}, "disable_web_page_preview": true}
	if m.Secret {
		body["protect_content"] = true
	}
	if len(m.Inline) > 0 {
		rows := [][]map[string]string{}
		for _, row := range m.Inline {
			r := []map[string]string{}
			for _, b := range row {
				r = append(r, map[string]string{"text": b.Text, "callback_data": b.Data})
			}
			rows = append(rows, r)
		}
		body["reply_markup"] = map[string]any{"inline_keyboard": rows}
	} else if m.MainKeyboard {
		body["reply_markup"] = map[string]any{"keyboard": [][]string{{"Создать", "Список", "Статус"}, {"Терминал", "Удалить"}}, "resize_keyboard": true, "is_persistent": true}
	}
	return c.call(ctx, "sendMessage", body, nil)
}
func (c *TelegramClient) AnswerCallback(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]string{"callback_query_id": id, "text": text}, nil)
}
