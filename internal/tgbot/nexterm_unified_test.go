package tgbot

import (
	"context"
	"strings"
	"testing"
)

const nxLink = "https://nexterm.example/servers?token=SESSION&connectId=7&connectFtpId=7"

func lastText(tg *fakeTelegram) OutMessage { return tg.messages[len(tg.messages)-1] }

// Create sends exactly one terminal link (the unified Nexterm URL carrying both
// IDs), as a copyable message, to the owner's private chat; no login/password
// message and no separate provisioning notification.
func TestCreateSendsSingleUnifiedCopyableLink(t *testing.T) {
	b, tg, f := testBot(t)
	f.result = map[string]string{"url": nxLink, "browser_status": "ready"}
	b.Handle(context.Background(), message(90, 42, "/create new"))
	links := 0
	for _, m := range tg.messages {
		if strings.Contains(m.Text, "connectId=7&connectFtpId=7") {
			links++
			if m.Secret || m.ChatID != 42 {
				t.Fatalf("link must be copyable and owner-only: %+v", m)
			}
		}
		if strings.Contains(strings.ToLower(m.Text), "парол") || strings.Contains(m.Text, "ttyd") || strings.Contains(m.Text, "/t/") {
			t.Fatalf("unexpected credential/ttyd text: %q", m.Text)
		}
	}
	if links != 1 {
		t.Fatalf("want exactly one unified link, got %d: %+v", links, tg.messages)
	}
	if f.mutations != 2 || f.keys[1] != "tg:90:terminal" {
		t.Fatalf("create must be followed by one terminal request: %v", f.keys)
	}
}

// /terminal, the session-menu Terminal button and /ssh all give the same URL.
func TestTerminalAliasMenuButtonAndSSHUnifiedLink(t *testing.T) {
	b, tg, f := testBot(t)
	f.result = map[string]string{"url": nxLink, "browser_status": "ready"}
	f.ssh = map[string]any{"host": "h", "port": 2222, "user": "u", "host_key_fingerprint": "SHA256:x", "ready": true, "browser_url": nxLink, "browser_status": "ready"}
	b.Handle(context.Background(), message(1, 42, "/terminal old-immutable"))
	if m := lastText(tg); !strings.Contains(m.Text, nxLink) || m.Secret {
		t.Fatalf("/terminal: %+v", m)
	}
	b.Handle(context.Background(), message(2, 42, "/list"))
	var data string
	for _, row := range tg.messages[len(tg.messages)-1].Inline {
		for _, bt := range row {
			if strings.HasPrefix(bt.Data, "t:") {
				data = bt.Data
			}
		}
	}
	if data == "" {
		t.Fatal("no Terminal button in session menu")
	}
	n := len(tg.messages)
	b.Handle(context.Background(), Update{UpdateID: 3, CallbackQuery: &CallbackQuery{ID: "c", From: User{ID: 42}, Message: &Message{Chat: Chat{ID: 42, Type: "private"}}, Data: data}})
	if len(tg.messages) <= n || !strings.Contains(lastText(tg).Text, nxLink) {
		t.Fatalf("menu button: %+v", tg.messages[n:])
	}
	b.Handle(context.Background(), message(4, 42, "/ssh old-immutable"))
	m := lastText(tg)
	if !strings.Contains(m.Text, "u@h, port 2222") || !strings.Contains(m.Text, nxLink) {
		t.Fatalf("/ssh must keep raw SSH and add the link: %q", m.Text)
	}
}

func TestSSHUserSuppliedKeyReportsBrowserUnsupportedKeepsRawSSH(t *testing.T) {
	b, tg, f := testBot(t)
	f.ssh = map[string]any{"host": "h", "port": 22, "user": "u", "host_key_fingerprint": "SHA256:x", "ready": true, "browser_status": "unsupported"}
	b.Handle(context.Background(), message(1, 42, "/ssh old-immutable"))
	m := lastText(tg).Text
	if !strings.Contains(m, "u@h, port 22") || !strings.Contains(m, "недоступен") || strings.Contains(m, "https://") {
		t.Fatalf("%q", m)
	}
	f.result = map[string]string{"browser_status": "unsupported"}
	b.Handle(context.Background(), message(2, 42, "/terminal old-immutable"))
	if !strings.Contains(lastText(tg).Text, "недоступен") {
		t.Fatalf("%q", lastText(tg).Text)
	}
}

// Nexterm is no longer a bot command and the bot never reads Nexterm state.
func TestNextermCommandRemoved(t *testing.T) {
	b, tg, f := testBot(t)
	calls := f.calls
	b.Handle(context.Background(), message(1, 42, "/nexterm"))
	if f.calls != calls || strings.Contains(strings.ToLower(helpText), "nexterm") {
		t.Fatal("/nexterm must not exist")
	}
	_ = tg
}

func TestNonHTTPSBrowserURLRejected(t *testing.T) {
	b, tg, f := testBot(t)
	f.result = map[string]string{"url": "http://evil.example/x", "browser_status": "ready"}
	b.Handle(context.Background(), message(1, 42, "/terminal old-immutable"))
	if strings.Contains(lastText(tg).Text, "evil.example") {
		t.Fatal("non-https link delivered")
	}
}
