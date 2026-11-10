package tgbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTelegramWireProtectionAndSanitizedErrors(t *testing.T) {
	var got map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		switch {
		case strings.HasSuffix(r.URL.Path, "sendMessage"):
			w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
		case strings.HasSuffix(r.URL.Path, "getUpdates"):
			w.Write([]byte(`{"ok":true,"result":[{"update_id":9,"message":{"from":{"id":42},"chat":{"id":42,"type":"private"},"text":"/list"}}]}`))
		default:
			w.WriteHeader(500)
			w.Write([]byte(`{"ok":false,"error_code":500,"description":"secret-token /private"}`))
		}
	}))
	defer s.Close()
	c := NewTelegramClient(s.URL, "secret-token", nil)
	if e := c.SendMessage(context.Background(), OutMessage{ChatID: 42, Text: "https://terminal/t/cap/", Secret: true, Inline: [][]Button{{{Text: "Delete", Data: "d:id"}}}}); e != nil {
		t.Fatal(e)
	}
	if got["protect_content"] != true || got["disable_web_page_preview"] != true || got["parse_mode"] != nil {
		t.Fatal(got)
	}
	ups, e := c.GetUpdates(context.Background(), 8, 30)
	if e != nil || len(ups) != 1 || ups[0].Message.From.ID != 42 {
		t.Fatalf("%+v %v", ups, e)
	}
	if e = c.AnswerCallback(context.Background(), "cb", ""); e == nil || strings.Contains(e.Error(), "secret-token") || strings.Contains(e.Error(), "/private") {
		t.Fatal(e)
	}
	s.Close()
	if e = c.SendMessage(context.Background(), OutMessage{}); e == nil || strings.Contains(e.Error(), "secret-token") {
		t.Fatal(e)
	}
}
