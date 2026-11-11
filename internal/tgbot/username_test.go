package tgbot

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"github.com/petruekhin/vibe-firecracker/internal/controlapi"
	"github.com/petruekhin/vibe-firecracker/internal/identity"
	"github.com/petruekhin/vibe-firecracker/internal/jwtauth"
	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

const (
	jIss = "test-bot"
	jAud = "test-api"
)

// syncTelegram is a goroutine-safe fake Telegram.
type syncTelegram struct {
	mu       sync.Mutex
	messages []OutMessage
}

func (f *syncTelegram) GetUpdates(ctx context.Context, _ int64, _ int) ([]Update, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (f *syncTelegram) SendMessage(_ context.Context, m OutMessage) error {
	f.mu.Lock()
	f.messages = append(f.messages, m)
	f.mu.Unlock()
	return nil
}
func (f *syncTelegram) AnswerCallback(context.Context, string, string) error { return nil }
func (f *syncTelegram) all() []OutMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]OutMessage(nil), f.messages...)
}

type jfix struct {
	tg       *syncTelegram
	bot      *Bot
	cfg      Config
	api      *httptest.Server
	apiCalls atomic.Int32
	hdrs     []http.Header
	hmu      sync.Mutex
	verifier *jwtauth.Verifier
	delivery string
}

func writeKeys(t *testing.T, dir string) (priv, pub string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	priv = filepath.Join(dir, "jwt-private.pem")
	if err = os.WriteFile(priv, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	pk, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	pub = filepath.Join(dir, "jwt-public.pem")
	if err = os.WriteFile(pub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pk}), 0o644); err != nil {
		t.Fatal(err)
	}
	return
}

// newJFix: REAL controller + DBOS + API + JWT verification; the VM backend and
// Telegram are fakes. The bot has no service credential at all.
func newJFix(t *testing.T, usernames []string, legacy []int64, mutate func(*Config)) *jfix {
	t.Helper()
	dir := privateDir(t)
	priv, pub := writeKeys(t, dir)
	ver, err := jwtauth.LoadVerifier(pub, jIss, jAud)
	if err != nil {
		t.Fatal(err)
	}
	st, err := vmbot.OpenControlStore(filepath.Join(dir, "api-state"), 0)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := vmlib.NewService(vmlibtest.New(6, "ubuntu"))
	if err != nil {
		t.Fatal(err)
	}
	ctl := vmbot.NewController(svc, st, &testTerminals{}, vmbot.ControllerConfig{Profile: "small", PublicOrigin: "https://terminal.test", OpTimeout: time.Second})
	ctl.ConfigureControl(vmbot.ControlPolicy{AllowedUsers: legacy, SharedImages: []string{"ubuntu"}, MaxVMs: 4, MaxVMsPerUser: 1})
	if err = ctl.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := identity.OpenBindings(filepath.Join(dir, "api-state", "username-bindings.json"))
	if err != nil {
		t.Fatal(err)
	}
	canon, err := identity.CanonicalList(usernames)
	if err != nil {
		t.Fatal(err)
	}
	h, err := controlapi.NewWithJWT(ctl, nil, &controlapi.JWTAuth{Verifier: ver, Bindings: b, Usernames: canon})
	if err != nil {
		t.Fatal(err)
	}
	f := &jfix{tg: &syncTelegram{}, verifier: ver, delivery: filepath.Join(dir, "delivery")}
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls.Add(1)
		f.hmu.Lock()
		f.hdrs = append(f.hdrs, r.Header.Clone())
		f.hmu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { f.api.Close(); ctl.Close(); st.Close() })
	if err = os.MkdirAll(f.delivery, 0o700); err != nil {
		t.Fatal(err)
	}
	f.cfg = Config{Version: 1, APIURL: f.api.URL, AllowedUserIDs: legacy, AllowedUsernames: usernames, DeliveryDir: f.delivery, ImageID: "ubuntu",
		JWT: &JWTConfig{PrivateKeyFile: priv, Issuer: jIss, Audience: jAud}}
	if mutate != nil {
		mutate(&f.cfg)
	}
	f.bot = f.newBot(t)
	return f
}

func (f *jfix) newBot(t *testing.T) *Bot {
	t.Helper()
	bot, err := NewBot(f.cfg, f.tg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bot.WaitBudget = 5 * time.Second
	bot.PollInterval = 5 * time.Millisecond
	return bot
}

func from(id int64, username string, text string) Update {
	return Update{UpdateID: id, Message: &Message{From: &User{ID: id, Username: username}, Chat: Chat{ID: id, Type: "private"}, Text: text}}
}

func TestUsernameAccessIsCaseInsensitiveAndFailsClosed(t *testing.T) {
	f := newJFix(t, []string{"@Alice_Example"}, nil, nil)
	ctx := context.Background()
	deny := []Update{
		from(2001, "", "/help"),                // missing username
		from(2002, "mallory_example", "/help"), // unlisted
		from(2003, "@alice_example", "/help"),  // Telegram never sends '@'; refuse
		from(2004, "alice_example ", "/help"),  // not canonical
		from(2005, "alice_exampl", "/help"),    // prefix of allowed
		from(2006, "alice_example2", "/help"),  // suffix
		from(2007, "alice-example", "/help"),   // other punctuation
		from(2008, "аlice_example", "/help"),   // Cyrillic homoglyph
	}
	// Non-private chat / group chat with matching username, and sender != chat.
	g := from(2009, "alice_example", "/help")
	g.Message.Chat.Type = "group"
	deny = append(deny, g)
	m := from(2010, "alice_example", "/help")
	m.Message.Chat.ID = 9999
	deny = append(deny, m)
	n := from(2011, "alice_example", "/help")
	n.Message.From = nil
	deny = append(deny, n)
	z := from(0, "alice_example", "/help")
	z.Message.From.ID = 0
	deny = append(deny, z)
	for _, u := range deny {
		f.bot.Handle(ctx, u)
	}
	// callback from an unlisted user must also be inert
	f.bot.Handle(ctx, Update{UpdateID: 2020, CallbackQuery: &CallbackQuery{ID: "c", From: User{ID: 2021, Username: "mallory_example"}, Data: "s:" + strings.Repeat("A", 43), Message: &Message{Chat: Chat{ID: 2021, Type: "private"}}}})
	if len(f.tg.all()) != 0 || f.apiCalls.Load() != 0 {
		t.Fatalf("denied senders produced work: %d messages, %d API calls", len(f.tg.all()), f.apiCalls.Load())
	}
	if _, ok := identityOwner(t, f, "alice_example"); ok {
		t.Fatal("a denied sender bound the slot")
	}
	// Allowed regardless of case, and bound on first contact.
	f.bot.Handle(ctx, from(1001, "ALICE_example", "/help"))
	if len(f.tg.all()) != 1 {
		t.Fatalf("allowed sender got %d messages", len(f.tg.all()))
	}
	if id, ok := identityOwner(t, f, "alice_example"); !ok || id != 1001 {
		t.Fatalf("binding %d %v", id, ok)
	}
}

func identityOwner(t *testing.T, f *jfix, name string) (int64, bool) {
	t.Helper()
	b, err := identity.OpenBindings(filepath.Join(f.delivery, "username-bindings.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b.Owner(name)
}

func TestRecycledHandleDoesNotRebindAcrossRestart(t *testing.T) {
	f := newJFix(t, []string{"alice_example"}, nil, nil)
	ctx := context.Background()
	f.bot.Handle(ctx, from(1001, "alice_example", "/help"))
	before := len(f.tg.all())
	f.bot.Handle(ctx, from(1002, "alice_example", "/token")) // handle recycled by a different ID
	if len(f.tg.all()) != before {
		t.Fatal("recycled handle received a reply (a token!)")
	}
	// restart: a brand new Bot instance still honours the durable pin
	bot2 := f.newBot(t)
	bot2.Handle(ctx, from(1002, "alice_example", "/token"))
	if len(f.tg.all()) != before {
		t.Fatal("pin lost on restart")
	}
	bot2.Handle(ctx, from(1001, "alice_example", "/help"))
	if len(f.tg.all()) != before+1 {
		t.Fatal("rightful owner locked out after restart")
	}
	// the bound ID that renamed itself to an unlisted handle fails closed
	bot2.Handle(ctx, from(1001, "renamed_example", "/help"))
	if len(f.tg.all()) != before+1 {
		t.Fatal("missing/unlisted handle must fail closed even for a bound ID")
	}
}

func TestConcurrentFirstContactHasOneWinner(t *testing.T) {
	f := newJFix(t, []string{"race_example"}, nil, nil)
	var wg sync.WaitGroup
	var wins atomic.Int32
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		id := int64(3000 + i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if f.bot.allowedMessage(&User{ID: id, Username: "RACE_example"}, Chat{ID: id, Type: "private"}) {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d winners", wins.Load())
	}
}

func lastSecret(t *testing.T, f *jfix) OutMessage {
	t.Helper()
	m := f.tg.all()
	if len(m) == 0 {
		t.Fatal("no message")
	}
	return m[len(m)-1]
}

func extractToken(t *testing.T, text string) string {
	t.Helper()
	_, rest, ok := strings.Cut(text, "```\n")
	if !ok {
		t.Fatalf("no code block in %q", text)
	}
	tok, _, ok := strings.Cut(rest, "\n```")
	if !ok || strings.ContainsAny(tok, " \n`") {
		t.Fatalf("malformed code block in %q", text)
	}
	return tok
}

func TestTokenCommandOnlyForAllowedUserAndIsProtected(t *testing.T) {
	f := newJFix(t, []string{"alice_example"}, []int64{11}, nil)
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	ctx := context.Background()

	f.bot.Handle(ctx, from(4001, "mallory_example", "/token"))
	f.bot.Handle(ctx, from(4002, "", "/token"))
	if len(f.tg.all()) != 0 {
		t.Fatal("/token answered a disallowed user")
	}
	f.bot.Handle(ctx, from(1001, "Alice_Example", "/token"))
	m := lastSecret(t, f)
	if !m.Secret || !m.Markdown || m.ChatID != 1001 || len(m.Inline) != 0 {
		t.Fatalf("token reply must be protected markdown to the requester: %+v", m)
	}
	tok := extractToken(t, m.Text)
	now := time.Now()
	c, err := f.verifier.Verify(tok, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != 1001 || c.Username != "alice_example" || c.ExpiresAt.Sub(c.IssuedAt) != 28800*time.Second {
		t.Fatalf("claims %+v", c)
	}
	if !strings.Contains(m.Text, "8 ч") {
		t.Fatalf("reply must state the lifetime: %q", m.Text)
	}
	// The token works from curl/Swagger without any bot header.
	req, _ := http.NewRequest("GET", f.api.URL+"/v1/vms", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("token against API: %v %v", err, resp)
	}
	resp.Body.Close()
	// ...but cannot pick another acting user.
	req.Header.Set("X-Acting-User-ID", "11")
	if resp, err = http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("acting header with user token: %v %v", err, resp)
	}
	resp.Body.Close()
	// Renewal: asking again gives another valid token.
	f.bot.Handle(ctx, from(1001, "alice_example", "/token"))
	tok2 := extractToken(t, lastSecret(t, f).Text)
	if _, err = f.verifier.Verify(tok2, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Legacy numeric user: token has no username claim.
	f.bot.Handle(ctx, from(11, "whatever_name", "/token"))
	c, err = f.verifier.Verify(extractToken(t, lastSecret(t, f).Text), time.Now())
	if err != nil || c.Subject != 11 || c.Username != "" {
		t.Fatalf("legacy claims %+v %v", c, err)
	}
	// Usage error never signs anything.
	n := len(f.tg.all())
	f.bot.Handle(ctx, from(1001, "alice_example", "/token now"))
	if got := f.tg.all(); len(got) != n+1 || strings.Contains(got[n].Text, "```") || got[n].Secret && strings.Contains(got[n].Text, "eyJ") {
		t.Fatalf("bad usage: %+v", got[n:])
	}
	if strings.Contains(logs.String(), tok) || strings.Contains(logs.String(), "eyJ") {
		t.Fatal("token was logged")
	}
}

func TestBotNormalRequestsUsePerUserJWTNeverDelegation(t *testing.T) {
	f := newJFix(t, []string{"alice_example"}, []int64{11}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f.bot.Handle(ctx, from(1001, "alice_example", "/create lab ubuntu small"))
	msgs := f.tg.all()
	if len(msgs) < 2 || !strings.Contains(msgs[len(msgs)-1].Text, "https://terminal.test/t/") {
		t.Fatalf("create flow through per-user JWT failed: %+v", msgs)
	}
	f.hmu.Lock()
	hdrs := append([]http.Header(nil), f.hdrs...)
	f.hmu.Unlock()
	if len(hdrs) == 0 {
		t.Fatal("no API traffic")
	}
	for _, h := range hdrs {
		a := h.Get("Authorization")
		tok := strings.TrimPrefix(a, "Bearer ")
		if tok == a || strings.Count(tok, ".") != 2 || len(h.Values("X-Acting-User-ID")) != 0 {
			t.Fatalf("every bot request must carry a per-user JWT and no delegation header: %v", h)
		}
		c, err := f.verifier.Verify(tok, time.Now())
		if err != nil || c.Subject != 1001 || c.Username != "alice_example" {
			t.Fatalf("request token claims %+v %v", c, err)
		}
	}
	// The VM is owned by the verified numeric ID (the API derived it from sub).
	own, _ := controlclient.NewPerUser(f.api.URL, func(id int64) (string, error) {
		s, err := jwtauth.LoadSigner(f.cfg.JWT.PrivateKeyFile, jIss, jAud)
		if err != nil {
			return "", err
		}
		tok, _, err := s.Issue(id, "", time.Hour, time.Now())
		return tok, err
	}, nil)
	if all, err := own.ListVMs(ctx, 1001); err != nil || len(all) != 1 {
		t.Fatalf("owner list %v %v", all, err)
	}
	if all, err := own.ListVMs(ctx, 11); err != nil || len(all) != 0 {
		t.Fatalf("other user must not see it: %v %v", all, err)
	}
	// A legacy numeric user works through the same JWT path.
	before := len(f.tg.all())
	f.bot.Handle(ctx, from(11, "", "/list"))
	if len(f.tg.all()) != before+1 {
		t.Fatal("legacy numeric ID user got no reply")
	}
}

func TestTelegramClientSendsTokenMessageProtectedWithoutPreview(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer srv.Close()
	c := NewTelegramClient(srv.URL, "123:test", nil)
	if err := c.SendMessage(context.Background(), OutMessage{ChatID: 5, Secret: true, Markdown: true, Text: "x\n```\nabc\n```"}); err != nil {
		t.Fatal(err)
	}
	lp, _ := body["link_preview_options"].(map[string]any)
	if body["protect_content"] != true || body["parse_mode"] != "MarkdownV2" || lp["is_disabled"] != true || body["disable_web_page_preview"] != true {
		t.Fatalf("%v", body)
	}
	body = nil
	if err := c.SendMessage(context.Background(), OutMessage{ChatID: 5, Text: "plain"}); err != nil {
		t.Fatal(err)
	}
	if _, has := body["parse_mode"]; has {
		t.Fatal("plain messages must not use a parse mode")
	}
}

func TestEscapeMarkdownV2(t *testing.T) {
	got := escapeMarkdownV2(`a_b*c[d]e(f)g~h>i#j+k-l=m|n{o}p.q!r\s`)
	want := `a\_b\*c\[d\]e\(f\)g\~h\>i\#j\+k\-l\=m\|n\{o\}p\.q\!r\\s`
	if got != want {
		t.Fatalf("%q", got)
	}
}

func TestConfigUsernamesAndJWT(t *testing.T) {
	d := privateDir(t)
	priv, _ := writeKeys(t, d)
	tok := filepath.Join(d, "telegram-token")
	if err := os.WriteFile(tok, []byte("telegram-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "config.json")
	jwtBlock := `"jwt":{"private_key_file":"` + priv + `","issuer":"incident-sim-bot","audience":"incident-sim-api","ttl_seconds":28800}`
	base := func(extra string) string {
		return `{"version":1,"telegram_token_file":"` + tok + `","api_url":"http://127.0.0.1:8090","delivery_dir":"` + filepath.Join(d, "delivery") + `","image_id":"ubuntu",` + extra + `}`
	}
	load := func(body string) (Config, error) {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(p)
	}
	c, err := load(base(`"allowed_usernames":["@Alice_Example"],` + jwtBlock))
	if err != nil {
		t.Fatalf("JWT-only config (no api_token_file, no IDs): %v", err)
	}
	if c.JWT.TTLSeconds != 28800 || len(c.AllowedUsernames) != 1 {
		t.Fatalf("%+v", c)
	}
	if c, err = load(base(`"allowed_usernames":["alice_example"],"jwt":{"private_key_file":"` + priv + `","issuer":"a","audience":"b"}`)); err != nil || c.JWT.TTLSeconds != 28800 {
		t.Fatalf("ttl default must be 8h: %v %+v", err, c.JWT)
	}
	// secrets: only the Telegram token is read in JWT mode
	if tg, api, err := c.Secrets(); err != nil || tg != "telegram-secret" || api != "" {
		t.Fatalf("secrets %q %q %v", tg, api, err)
	}
	for name, body := range map[string]string{
		"dup usernames":     base(`"allowed_usernames":["alice_example","@ALICE_EXAMPLE"],` + jwtBlock),
		"invalid username":  base(`"allowed_usernames":["no way"],` + jwtBlock),
		"usernames no jwt":  base(`"allowed_usernames":["alice_example"],"api_token_file":"` + tok + `"`),
		"no identity":       base(jwtBlock),
		"ttl too long":      base(`"allowed_usernames":["alice_example"],"jwt":{"private_key_file":"` + priv + `","issuer":"a","audience":"b","ttl_seconds":99999999}`),
		"ttl negative":      base(`"allowed_usernames":["alice_example"],"jwt":{"private_key_file":"` + priv + `","issuer":"a","audience":"b","ttl_seconds":-1}`),
		"relative key":      base(`"allowed_usernames":["alice_example"],"jwt":{"private_key_file":"k.pem","issuer":"a","audience":"b"}`),
		"missing issuer":    base(`"allowed_usernames":["alice_example"],"jwt":{"private_key_file":"` + priv + `","audience":"b"}`),
		"unknown jwt field": base(`"allowed_usernames":["alice_example"],"jwt":{"private_key_file":"` + priv + `","issuer":"a","audience":"b","secret":"x"}`),
		"neither token":     base(`"allowed_user_ids":[42]`),
	} {
		if _, err := load(body); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	// A readable private key is refused when the bot starts (never logged).
	bad := filepath.Join(d, "loose.pem")
	raw, _ := os.ReadFile(priv)
	if err := os.WriteFile(bad, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := load(base(`"allowed_usernames":["alice_example"],"jwt":{"private_key_file":"` + bad + `","issuer":"a","audience":"b"}`))
	if _, err := NewBot(cfg, &syncTelegram{}, nil, nil); err == nil {
		t.Fatal("group/world readable private key accepted")
	}
	// legacy static config: unchanged behaviour
	cred := filepath.Join(d, "api-token")
	_ = os.WriteFile(cred, []byte("service-secret\n"), 0o600)
	if _, err := load(base(`"api_token_file":"` + cred + `","allowed_user_ids":[42]`)); err != nil {
		t.Fatalf("legacy config: %v", err)
	}
}
