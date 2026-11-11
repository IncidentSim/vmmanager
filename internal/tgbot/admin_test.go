package tgbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeleteAllPrivateAdminConfirmation(t *testing.T) {
	priv, _ := writeKeys(t, t.TempDir())
	token := strings.Repeat("x", 43)
	calls, previews, advances := 0, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Acting-User-ID") != "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			t.Error("must use signed subject, not delegation")
		}
		if r.URL.Path == "/v1/admin/delete-all" && r.Method == "POST" {
			previews++
		} else if r.URL.Path == "/v1/admin/delete-all/"+token && r.Method == "POST" {
			advances++
		} else {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]any{"token": token, "chat_id": 42, "admin_user_id": 42, "pending": map[bool]int{true: 2, false: 0}[advances == 0], "completed": map[bool]int{true: 0, false: 2}[advances == 0], "vms": []map[string]any{{"id": "vm-a", "owner_user_id": 42}, {"id": "vm-b", "owner_user_id": 43}}})
	}))
	defer srv.Close()
	tg := &fakeTelegram{}
	b, e := NewBot(Config{AllowedUserIDs: []int64{42, 43}, AdminUserIDs: []int64{42}, ImageID: "ubuntu", APIURL: srv.URL, JWT: &JWTConfig{PrivateKeyFile: priv, Issuer: jIss, Audience: jAud}}, tg, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	b.WaitBudget = time.Millisecond * 20
	b.PollInterval = time.Millisecond
	ctx := context.Background()
	bad := []Update{message(1, 43, "/delete_all"), {Message: &Message{From: &User{ID: 42}, Chat: Chat{ID: 42, Type: "group"}, Text: "/delete_all"}}, {Message: &Message{From: &User{ID: 42}, Chat: Chat{ID: 43, Type: "private"}, Text: "/delete_all"}}}
	for _, u := range bad {
		b.Handle(ctx, u)
	}
	if calls != 0 {
		t.Fatal("nonadmin accessed inventory")
	}
	b.Handle(ctx, message(5, 42, "/delete_all"))
	if previews != 1 || advances != 0 {
		t.Fatalf("preview executed cleanup: %d %d", previews, advances)
	}
	text := ""
	data := ""
	for _, m := range tg.messages {
		text += m.Text
		for _, row := range m.Inline {
			for _, btn := range row {
				data = btn.Data
			}
		}
	}
	if !strings.Contains(text, "vm-a") || !strings.Contains(text, "vm-b") || !strings.Contains(text, "2") || data != "da:"+token {
		t.Fatalf("missing snapshot confirmation: %s %q", text, data)
	}
	foreign := Update{UpdateID: 6, CallbackQuery: &CallbackQuery{From: User{ID: 43}, Message: &Message{Chat: Chat{ID: 43, Type: "private"}}, Data: data}}
	b.Handle(ctx, foreign)
	if advances != 0 {
		t.Fatal("foreign sender confirmed")
	}
	good := Update{UpdateID: 7, CallbackQuery: &CallbackQuery{From: User{ID: 42}, Message: &Message{Chat: Chat{ID: 42, Type: "private"}}, Data: data}}
	b.Handle(ctx, good)
	b.Handle(ctx, good)
	if previews != 1 || advances != 2 {
		t.Fatalf("callback broadened inventory: %d %d", previews, advances)
	}
	if !strings.Contains(tg.messages[len(tg.messages)-1].Text, "2") {
		t.Fatal("missing completion counts")
	}
}

func TestAdminConfigRejectsInvalidIDsAndDelegation(t *testing.T) {
	b, _, _ := testBot(t)
	for _, ids := range [][]int64{{0}, {-1}, {42, 42}, {42}} {
		c := b.config
		c.AdminUserIDs = ids
		if _, e := NewBot(c, b.tg, b.client, nil); e == nil {
			t.Fatalf("accepted invalid/admin delegation config: %v", ids)
		}
	}
}
