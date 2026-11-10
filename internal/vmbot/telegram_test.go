package vmbot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const fakeToken = "123456:SECRET-TOKEN-VALUE"

func TestTelegramClientRequestsAndSecretMessageFlags(t *testing.T) {
	var paths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(raw, &m)
		bodies = append(bodies, m)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			io.WriteString(w, `{"ok":true,"result":[{"update_id":5,"message":{"message_id":1,"from":{"id":4242},"chat":{"id":4242,"type":"private"},"text":"/status"}}]}`)
			return
		}
		io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer srv.Close()
	c := NewTelegramClient(srv.URL, fakeToken, srv.Client())
	ups, err := c.GetUpdates(WithUser(context.Background(), testUser), 5, 1)
	if err != nil || len(ups) != 1 || ups[0].UpdateID != 5 || ups[0].Message.From.ID != 4242 || ups[0].Message.Chat.Type != "private" {
		t.Fatalf("updates: %+v %v", ups, err)
	}
	if bodies[0]["offset"].(float64) != 5 || bodies[0]["timeout"].(float64) != 1 {
		t.Fatalf("getUpdates body %v", bodies[0])
	}
	if err := c.SendMessage(WithUser(context.Background(), testUser), OutMessage{ChatID: 4242, Text: "https://x/t/abc/", Secret: true}); err != nil {
		t.Fatal(err)
	}
	b := bodies[1]
	lp, _ := b["link_preview_options"].(map[string]any)
	if lp["is_disabled"] != true || b["disable_web_page_preview"] != true || b["protect_content"] != true {
		t.Fatalf("secret message must disable previews and forwarding: %v", b)
	}
	if paths[1] != "/bot"+fakeToken+"/sendMessage" {
		t.Fatalf("path %q", paths[1])
	}
	if err := c.SendMessage(WithUser(context.Background(), testUser), OutMessage{ChatID: 1, Text: "hi", MainKeyboard: true,
		Inline: [][]Button{{{Text: "Удалить", Data: "d:1"}}, {{Text: "Терминал", Data: "t:1"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := bodies[2]["reply_markup"]; !ok {
		t.Fatal("keyboard missing")
	}
	if err := c.AnswerCallback(WithUser(context.Background(), testUser), "id1", "ok"); err != nil || !strings.HasSuffix(paths[3], "/answerCallbackQuery") {
		t.Fatalf("answer: %v %v", err, paths)
	}
}

func TestTelegramErrorsNeverContainToken(t *testing.T) {
	// 1. transport failure (closed server): net/http's error would embed the URL.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := NewTelegramClient(url, fakeToken, http.DefaultClient)
	_, err := c.GetUpdates(WithUser(context.Background(), testUser), 0, 0)
	if err == nil || strings.Contains(err.Error(), fakeToken) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("transport error leaks token: %v", err)
	}
	if !strings.Contains(err.Error(), "getUpdates") {
		t.Fatalf("error must name the stage: %v", err)
	}
	// 2. API-level failure whose description echoes the token.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized `+fakeToken+`"}`)
	}))
	defer srv2.Close()
	c = NewTelegramClient(srv2.URL, fakeToken, srv2.Client())
	err = c.SendMessage(WithUser(context.Background(), testUser), OutMessage{ChatID: 1, Text: "x"})
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "sendMessage") {
		t.Fatalf("api error: %v", err)
	}
}
