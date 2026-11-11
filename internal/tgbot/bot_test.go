package tgbot

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/petruekhin/vibe-firecracker/controlclient"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeTelegram struct {
	messages []OutMessage
	answers  []string
	updates  []Update
	onPoll   func(int64)
}

func (f *fakeTelegram) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	if f.onPoll != nil {
		f.onPoll(offset)
	}
	if len(f.updates) > 0 {
		u := f.updates
		f.updates = nil
		return u, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (f *fakeTelegram) SendMessage(ctx context.Context, m OutMessage) error {
	f.messages = append(f.messages, m)
	return nil
}
func (f *fakeTelegram) AnswerCallback(ctx context.Context, id, text string) error {
	f.answers = append(f.answers, text)
	return nil
}
func message(id, user int64, text string) Update {
	return Update{UpdateID: id, Message: &Message{From: &User{ID: user}, Chat: Chat{ID: user, Type: "private"}, Text: text}}
}

// This is a real HTTP wire-contract peer, not a client adapter: it decodes
// requests and verifies the pinned 202/operation/result schema end to end.
type apiFixture struct {
	t                *testing.T
	vms              map[int64][]controlclient.VM
	calls, mutations int
	keys             []string
	pending          bool
	actor            int64
	opVM             string
	result           map[string]string // overrides the terminal operation result
	ssh              map[string]any    // overrides GET /ssh body
}

func (f *apiFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls++
	if r.Header.Get("Authorization") != "Bearer service-secret" {
		f.t.Fatal("missing service credential")
	}
	var u int64
	if _, e := fmt.Sscan(r.Header.Get("X-Acting-User-ID"), &u); e != nil || u <= 0 {
		f.t.Fatal("missing service actor")
	}
	f.actor = u
	w.Header().Set("Content-Type", "application/json")
	all := f.vms[u]
	p := r.URL.Path
	if r.Method == "GET" && p == "/v1/vms" {
		json.NewEncoder(w).Encode(map[string]any{"vms": all})
		return
	}
	if r.Method == "GET" && strings.HasSuffix(p, "/ssh") && f.ssh != nil {
		json.NewEncoder(w).Encode(f.ssh)
		return
	}
	if r.Method == "GET" && strings.HasPrefix(p, "/v1/operations/") {
		if f.result != nil {
			json.NewEncoder(w).Encode(map[string]any{"id": "op1", "status": "succeeded", "kind": "terminal", "vm_id": f.opVM, "result": f.result})
			return
		}
		status := "succeeded"
		if f.pending {
			status = "running"
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "op1", "status": status, "kind": "terminal", "vm_id": f.opVM, "result": map[string]string{"url": "https://terminal.example/t/capability/"}})
		return
	}
	if r.Method == "POST" && p == "/v1/vms" {
		var req controlclient.CreateRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.ImageID == "" {
			f.t.Error("bad create payload")
		}
		f.opVM = "new-immutable"
		f.vms[u] = append(all, controlclient.VM{ID: f.opVM, Name: req.Name, State: "running", Phase: "active"})
	} else {
		found := false
		for _, vm := range all {
			if p == "/v1/vms/"+vm.ID {
				found = true
				f.opVM = vm.ID
				if r.Method == "GET" {
					json.NewEncoder(w).Encode(vm)
					return
				}
			}
			if p == "/v1/vms/"+vm.ID+"/terminal" || p == "/v1/vms/"+vm.ID+"/stop" || p == "/v1/vms/"+vm.ID+"/commit" {
				found = true
				f.opVM = vm.ID
			}
		}
		if !found {
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"code":"not_found","message":"secret foreign object"}}`))
			return
		}
	}
	f.mutations++
	f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
	if f.keys[len(f.keys)-1] == "" {
		f.t.Error("missing mutation key")
	}
	w.WriteHeader(202)
	json.NewEncoder(w).Encode(map[string]string{"operation_id": "op1", "vm_id": f.opVM})
}
func testBot(t *testing.T) (*Bot, *fakeTelegram, *apiFixture) {
	t.Helper()
	f := &apiFixture{t: t, vms: map[int64][]controlclient.VM{42: {{ID: "old-immutable", Name: "lab", State: "running", Phase: "active"}}, 43: {{ID: "foreign", Name: "lab", State: "running"}}}}
	s := httptest.NewServer(f)
	t.Cleanup(s.Close)
	c, e := controlclient.New(s.URL, "service-secret", nil)
	if e != nil {
		t.Fatal(e)
	}
	tg := &fakeTelegram{}
	b, e := NewBot(Config{AllowedUserIDs: []int64{42, 43}, ImageID: "ubuntu"}, tg, c, nil)
	if e != nil {
		t.Fatal(e)
	}
	b.PollInterval = time.Millisecond
	b.WaitBudget = 50 * time.Millisecond
	return b, tg, f
}
func TestPrivateVerifiedIdentity(t *testing.T) {
	b, tg, f := testBot(t)
	bad := []Update{message(1, 44, "/create"), message(2, 0, "/list"), {Message: &Message{From: &User{ID: 42}, Chat: Chat{ID: 43, Type: "private"}, Text: "/create"}}, {Message: &Message{From: &User{ID: 42}, Chat: Chat{ID: 42, Type: "group"}, Text: "/list"}}, {CallbackQuery: &CallbackQuery{From: User{ID: 42}, Message: &Message{Chat: Chat{ID: 43, Type: "private"}}, Data: "d:foreign"}}}
	for _, u := range bad {
		b.Handle(context.Background(), u)
	}
	if f.calls != 0 || len(tg.messages) != 0 || len(tg.answers) != 0 {
		t.Fatal("unauthenticated side effect")
	}
	b.Handle(context.Background(), message(3, 43, "/status"))
	if f.actor != 43 || len(tg.messages) != 1 || !strings.Contains(tg.messages[0].Text, "foreign") {
		t.Fatal("identity not taken from sender")
	}
}
func TestCreateHTTPAsyncAndProtectedLink(t *testing.T) {
	b, tg, f := testBot(t)
	b.Handle(context.Background(), message(90, 42, "/create new"))
	if f.mutations != 2 || f.keys[0] != "tg:90:create" || f.keys[1] != "tg:90:terminal" {
		t.Fatalf("%d %v", f.mutations, f.keys)
	}
	m := tg.messages[len(tg.messages)-1]
	if !m.Secret || !strings.Contains(m.Text, "https://terminal.example/t/capability/") {
		t.Fatalf("missing protected terminal: %+v", m)
	}
	b.Handle(context.Background(), message(90, 42, "/create new"))
	if f.keys[2] != f.keys[0] || f.keys[3] != f.keys[1] {
		t.Fatal("retry uses different keys")
	}
}
func TestSelectionAndStaleForeignCallbacks(t *testing.T) {
	b, tg, f := testBot(t)
	b.Handle(context.Background(), message(1, 42, "/list"))
	data := tg.messages[0].Inline[0][2].Data
	f.vms[42] = []controlclient.VM{{ID: "replacement", Name: "lab"}, {ID: "another", Name: "other"}}
	b.Handle(context.Background(), message(2, 42, "/delete"))
	if f.mutations != 0 {
		t.Fatal("ambiguous selection mutated")
	}
	for _, d := range []string{data, buttonData("d", "foreign"), "d:1", "d:", "d:../../foreign"} {
		b.Handle(context.Background(), Update{UpdateID: 3, CallbackQuery: &CallbackQuery{ID: "cb", From: User{ID: 42}, Message: &Message{Chat: Chat{ID: 42, Type: "private"}}, Data: d}})
	}
	if f.mutations != 0 {
		t.Fatal("stale/foreign button reached mutation")
	}
	b.Handle(context.Background(), Update{UpdateID: 4, CallbackQuery: &CallbackQuery{ID: "cb", From: User{ID: 42}, Message: &Message{Chat: Chat{ID: 42, Type: "private"}}, Data: buttonData("d", "replacement")}})
	if f.mutations != 1 || f.keys[0] != "tg:4:delete" {
		t.Fatalf("%d %v", f.mutations, f.keys)
	}
}
func TestListPaginationAndInvalidNames(t *testing.T) {
	b, tg, f := testBot(t)
	f.vms[42] = nil
	for i := 0; i < 25; i++ {
		f.vms[42] = append(f.vms[42], controlclient.VM{ID: fmt.Sprintf("id%d", i), Name: fmt.Sprintf("vm-%d", i), State: "running"})
	}
	b.Handle(context.Background(), message(1, 42, "Список"))
	rows := 0
	for _, m := range tg.messages {
		rows += len(m.Inline)
		if len([]rune(m.Text)) > 3500 || len(m.Inline) > 8 {
			t.Fatal("Telegram limits exceeded")
		}
		for _, row := range m.Inline {
			for _, bt := range row {
				if len(bt.Data) > 64 {
					t.Fatal("oversize callback")
				}
			}
		}
	}
	if rows != 25 || len(tg.messages) != 4 {
		t.Fatalf("rows %d messages %d", rows, len(tg.messages))
	}
	for _, name := range []string{"../bad", "UPPER", "bot-x", "a b C", "x;ls"} {
		b.Handle(context.Background(), message(2, 42, "/create "+name))
	}
	if f.mutations != 0 {
		t.Fatal("invalid input reached create")
	}
}
func TestPendingDoesNotClaimFailureOrSuccess(t *testing.T) {
	b, tg, f := testBot(t)
	f.pending = true
	b.WaitBudget = 5 * time.Millisecond
	b.Handle(context.Background(), message(1, 42, "/delete lab"))
	m := tg.messages[len(tg.messages)-1]
	if !strings.Contains(m.Text, "/operation op1") || strings.Contains(m.Text, "удалена") {
		t.Fatalf("lied about result: %s", m.Text)
	}
}
func TestOffsetBeforeHandlingNoReplay(t *testing.T) {
	b, tg, f := testBot(t)
	d, e := OpenDelivery(privateDir(t))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	b.Delivery = d
	ctx, cancel := context.WithCancel(context.Background())
	tg.updates = []Update{message(20, 42, "/status"), message(20, 42, "/create")}
	tg.onPoll = func(offset int64) {
		if offset == 21 {
			cancel()
		}
	}
	b.Run(ctx)
	if d.Offset() != 21 || f.mutations != 0 || len(tg.messages) != 1 {
		t.Fatalf("offset %d mutation %d sends %d", d.Offset(), f.mutations, len(tg.messages))
	}
}
