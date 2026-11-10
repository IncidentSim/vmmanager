package vmbot

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

const testUser int64 = 4242

type fakeAPI struct {
	mu      sync.Mutex
	sent    []OutMessage
	answers []string
	batches [][]Update
	offsets []int64
	onPoll  func(offset int64)
}

func (f *fakeAPI) GetUpdates(ctx context.Context, offset int64, _ int) ([]Update, error) {
	f.mu.Lock()
	f.offsets = append(f.offsets, offset)
	var b []Update
	if len(f.batches) > 0 {
		b, f.batches = f.batches[0], f.batches[1:]
	}
	f.mu.Unlock()
	if f.onPoll != nil {
		f.onPoll(offset)
	}
	if b == nil {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b, nil
}
func (f *fakeAPI) SendMessage(_ context.Context, m OutMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}
func (f *fakeAPI) AnswerCallback(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, text)
	return nil
}
func (f *fakeAPI) texts() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, m := range f.sent {
		b.WriteString(m.Text + "\n")
	}
	return b.String()
}

func msg(id int64, from, chat int64, typ, text string) Update {
	return Update{UpdateID: id, Message: &Message{From: &User{ID: from}, Chat: Chat{ID: chat, Type: typ}, Text: text}}
}
func cb(id int64, from, chat int64, data string) Update {
	return Update{UpdateID: id, CallbackQuery: &CallbackQuery{ID: "cq", From: User{ID: from}, Data: data,
		Message: &Message{From: &User{ID: from}, Chat: Chat{ID: chat, Type: "private"}}}}
}

func newBot(t *testing.T, e *env) (*Bot, *fakeAPI) {
	api := &fakeAPI{}
	return NewBot(testUser, api, e.ctl, e.store, nil), api
}

func TestUnauthorizedSendersAndChatsAreIgnored(t *testing.T) {
	e := newEnv(t)
	b, api := newBot(t, e)
	ctx := context.Background()
	for _, u := range []Update{
		msg(1, 999, 999, "private", "/create"),             // wrong user
		msg(2, testUser, -100123, "supergroup", "/create"), // right user, group chat
		msg(3, testUser, -100123, "group", "/create"),
		msg(4, testUser, 777, "private", "/create"), // private chat id differs from allowed user
		cb(5, 999, 999, "d:1"),                      // wrong callback sender
		cb(6, testUser, -1, "c"),                    // callback from other chat
		{UpdateID: 7},                               // neither message nor callback
		{UpdateID: 8, Message: &Message{Text: "/create", Chat: Chat{ID: testUser, Type: "private"}}}, // no sender (channel post)
	} {
		b.Handle(ctx, u)
	}
	if e.be.Count() != 0 || len(api.sent) != 0 || len(api.answers) != 0 {
		t.Fatalf("unauthorized input had effects: vms=%d sent=%d answers=%d", e.be.Count(), len(api.sent), len(api.answers))
	}
}

func TestCommandsHappyPathAndRussianUI(t *testing.T) {
	e := newEnv(t)
	b, api := newBot(t, e)
	ctx := context.Background()
	b.Handle(ctx, msg(1, testUser, testUser, "private", "/start"))
	if !api.sent[0].MainKeyboard || !strings.Contains(api.sent[0].Text, "/create") {
		t.Fatalf("start: %+v", api.sent[0])
	}
	b.Handle(ctx, msg(2, testUser, testUser, "private", "/create"))
	if e.be.Count() != 1 {
		t.Fatal("create did not run")
	}
	b.Handle(ctx, msg(3, testUser, testUser, "private", "/status"))
	last := api.sent[len(api.sent)-1]
	if len(last.Inline) == 0 || !strings.HasPrefix(last.Inline[len(last.Inline)-1].Data, "d:1") {
		t.Fatalf("status must offer a generation-bound delete button: %+v", last)
	}
	b.Handle(ctx, msg(4, testUser, testUser, "private", "/terminal"))
	link := api.sent[len(api.sent)-1]
	tok := e.store.Snapshot().VM.Token
	if !strings.Contains(link.Text, "/t/"+tok+"/") || !link.Secret {
		t.Fatalf("link message must carry the secret and Secret=true: %+v", link)
	}
	for _, m := range api.sent[:len(api.sent)-1] {
		if strings.Contains(m.Text, tok) && !m.Secret {
			t.Fatal("token leaked into a non-secret message")
		}
	}
	b.Handle(ctx, msg(5, testUser, testUser, "private", "/delete"))
	if e.be.Count() != 0 {
		t.Fatal("delete did not run")
	}
	b.Handle(ctx, msg(6, testUser, testUser, "private", "Создать"))
	if e.be.Count() != 1 {
		t.Fatal("reply keyboard button must map to create")
	}
}

func TestStaleDeleteCallbackCannotDeleteReplacement(t *testing.T) {
	e := newEnv(t)
	b, api := newBot(t, e)
	ctx := context.Background()
	b.Handle(ctx, msg(1, testUser, testUser, "private", "/create"))
	b.Handle(ctx, msg(2, testUser, testUser, "private", "/delete"))
	b.Handle(ctx, msg(3, testUser, testUser, "private", "/create"))
	b.Handle(ctx, cb(4, testUser, testUser, "d:1"))
	if e.be.Count() != 1 || e.store.Snapshot().VM == nil {
		t.Fatal("stale callback deleted the replacement VM")
	}
	if len(api.answers) != 1 || !strings.Contains(api.answers[0], "Устарело") {
		t.Fatalf("answers %v", api.answers)
	}
	b.Handle(ctx, cb(5, testUser, testUser, "d:2"))
	if e.be.Count() != 0 {
		t.Fatal("current-generation callback must delete")
	}
	b.Handle(ctx, cb(6, testUser, testUser, "d:garbage"))
	b.Handle(ctx, cb(7, testUser, testUser, "zzz"))
}

func TestUserVisibleErrorsHideRPCInternals(t *testing.T) {
	e := newEnv(t)
	b, api := newBot(t, e)
	e.lc.list = func(context.Context) ([]vmlib.VM, error) {
		return nil, context.DeadlineExceeded
	}
	b.Handle(context.Background(), msg(1, testUser, testUser, "private", "/create"))
	txt := api.texts()
	if strings.Contains(txt, "deadline") || strings.Contains(txt, "rpc") {
		t.Fatalf("raw error leaked: %s", txt)
	}
	if !strings.Contains(txt, "Не удалось") {
		t.Fatalf("expected a Russian failure message: %s", txt)
	}
}

func TestRunPersistsOffsetBeforeHandlingAndNeverReplays(t *testing.T) {
	e := newEnv(t)
	b, api := newBot(t, e)
	var offsetDuringHandling int64
	e.lc.create = func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
		offsetDuringHandling = e.store.Snapshot().Offset
		return e.svc.Create(ctx, r)
	}
	api.batches = [][]Update{{msg(10, testUser, testUser, "private", "/create")}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for e.be.Count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if offsetDuringHandling != 11 {
		t.Fatalf("offset must be durable before the handler runs, got %d", offsetDuringHandling)
	}
	// "Restart": same state dir, new bot. The first poll must resume after 10.
	e.restart(t)
	api2 := &fakeAPI{}
	b2 := NewBot(testUser, api2, e.ctl, e.store, nil)
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { b2.Run(ctx2); close(done2) }()
	deadline = time.Now().Add(3 * time.Second)
	for {
		api2.mu.Lock()
		n := len(api2.offsets)
		api2.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel2()
	<-done2
	if len(api2.offsets) == 0 || api2.offsets[0] != 11 {
		t.Fatalf("restart must resume at offset 11: %v", api2.offsets)
	}
}

func TestCreateAutomaticallySendsSecretLinkAfterStatus(t *testing.T) {
	e := newEnv(t)
	b, api := newBot(t, e)
	b.Handle(context.Background(), msg(1, testUser, testUser, "private", "/create"))
	n := len(api.sent)
	if n < 3 {
		t.Fatalf("want progress, status and link messages, got %d", n)
	}
	status, link := api.sent[n-2], api.sent[n-1]
	tok := e.store.Snapshot().VM.Token
	if status.Secret || strings.Contains(status.Text, tok) || len(status.Inline) == 0 {
		t.Fatalf("status message must be concise, tokenless and keep buttons: %+v", status)
	}
	if !link.Secret || !strings.Contains(link.Text, "/t/"+tok+"/") {
		t.Fatalf("the secret link must follow create automatically: %+v", link)
	}
}

func TestFailedCreateSendsNoLink(t *testing.T) {
	e := newEnv(t)
	b, api := newBot(t, e)
	e.lc.list = func(context.Context) ([]vmlib.VM, error) { return nil, context.DeadlineExceeded }
	b.Handle(context.Background(), msg(1, testUser, testUser, "private", "/create"))
	for _, m := range api.sent {
		if m.Secret {
			t.Fatal("no link may be sent when create failed")
		}
	}
}
