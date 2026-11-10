package tgbot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"github.com/petruekhin/vibe-firecracker/internal/controlapi"
	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// Only the remote host and Telegram are fake. The controller, durable DBOS
// SQLite execution, API authentication/authorization and SDK HTTP are real.
// These imports are confined to tests: the frontend executable stays HTTP-only.
type testTerminals struct{}

func (*testTerminals) Start(context.Context, vmbot.TerminalTarget) error { return nil }
func (*testTerminals) Revoke(string)                                     {}
func (*testTerminals) Close()                                            {}
func TestBotAgainstActualControlAPI(t *testing.T) {
	st, e := vmbot.OpenControlStore(privateDir(t)+"/api-state", 0)
	if e != nil {
		t.Fatal(e)
	}
	backend := vmlibtest.New(4, "ubuntu")
	svc, e := vmlib.NewService(backend)
	if e != nil {
		t.Fatal(e)
	}
	ctl := vmbot.NewController(svc, st, &testTerminals{}, vmbot.ControllerConfig{Profile: "small", PublicOrigin: "https://terminal.test", OpTimeout: time.Second})
	ctl.ConfigureControl(vmbot.ControlPolicy{AllowedUsers: []int64{42, 43}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 1})
	if e = ctl.Start(context.Background()); e != nil {
		st.Close()
		t.Fatal(e)
	}
	sum := sha256.Sum256([]byte("test-only-service-credential"))
	handler, e := controlapi.New(ctl, []controlapi.Credential{{SHA256: hex.EncodeToString(sum[:]), Service: true}})
	if e != nil {
		ctl.Close()
		st.Close()
		t.Fatal(e)
	}
	server := httptest.NewServer(handler)
	defer func() { server.Close(); ctl.Close(); st.Close() }()
	client, e := controlclient.New(server.URL, "test-only-service-credential", nil)
	if e != nil {
		t.Fatal(e)
	}
	tg := &fakeTelegram{}
	bot, e := NewBot(Config{AllowedUserIDs: []int64{42, 43}, ImageID: "ubuntu"}, tg, client, nil)
	if e != nil {
		t.Fatal(e)
	}
	bot.WaitBudget = 5 * time.Second
	bot.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bot.Handle(ctx, message(100, 42, "/create lab ubuntu 1 512"))
	all, e := client.ListVMs(ctx, 42)
	if e != nil || len(all) != 1 {
		t.Fatalf("create/list: %+v %v; messages %+v", all, e, tg.messages)
	}
	vm := all[0]
	if vm.Name != "lab" || vm.Resources.VCPU != 1 || vm.Resources.MemoryMiB != 512 {
		t.Fatalf("resource contract %+v", vm)
	}
	if len(tg.messages) < 3 || !tg.messages[len(tg.messages)-1].Secret || !strings.Contains(tg.messages[len(tg.messages)-1].Text, "https://terminal.test/t/") {
		t.Fatalf("actual terminal result absent: %+v", tg.messages)
	}
	a, e := client.Create(ctx, 42, "tg:100:create", controlclient.CreateRequest{Name: "lab", ImageID: "ubuntu", Resources: &controlclient.Resources{VCPU: 1, MemoryMiB: 512}})
	if e != nil || a.VMID != vm.ID {
		t.Fatalf("idempotent HTTP replay %+v %v", a, e)
	}
	_, e = client.Create(ctx, 42, "tg:100:create", controlclient.CreateRequest{Name: "different", ImageID: "ubuntu"})
	var api *controlclient.APIError
	if !errors.As(e, &api) || api.Status != 409 {
		t.Fatalf("idempotency conflict %v", e)
	}
	for _, foreign := range []func() error{func() error { _, e := client.VM(ctx, 43, vm.ID); return e }, func() error { _, e := client.SSH(ctx, 43, vm.ID); return e }, func() error { _, e := client.Operation(ctx, 43, a.OperationID); return e }, func() error { _, e := client.Delete(ctx, 43, vm.ID, "foreign-delete"); return e }} {
		api = nil
		e = foreign()
		if !errors.As(e, &api) || api.Status != 404 {
			t.Fatalf("foreign HTTP response: %v", e)
		}
	}
	images, e := client.Images(ctx, 42)
	if e != nil || len(images) != 1 || !images[0].Shared {
		t.Fatalf("images %+v %v", images, e)
	}
	if _, e = client.Image(ctx, 42, "ubuntu"); e != nil {
		t.Fatal(e)
	}
	if _, e = client.Capabilities(ctx, 42); e != nil {
		t.Fatal(e)
	}
	if ssh, e := client.SSH(ctx, 42, vm.ID); e != nil || ssh.Host == "" {
		t.Fatalf("SSH %+v %v", ssh, e)
	}
	bot.Handle(ctx, message(101, 43, "/create lab"))
	other, e := client.ListVMs(ctx, 43)
	if e != nil || len(other) != 1 || other[0].ID == vm.ID {
		t.Fatalf("per-owner names %+v %v", other, e)
	}
	before := len(tg.messages)
	bot.Handle(ctx, Update{UpdateID: 102, CallbackQuery: &CallbackQuery{ID: "foreign", From: User{ID: 43}, Message: &Message{Chat: Chat{ID: 43, Type: "private"}}, Data: buttonData("d", vm.ID)}})
	if len(tg.messages) != before+1 || !strings.Contains(tg.messages[before].Text, "Устарело") {
		t.Fatal("foreign callback not rejected")
	}
	bot.Handle(ctx, message(103, 42, "/stop lab"))
	v, e := client.VM(ctx, 42, vm.ID)
	if e != nil || v.State != "stopped" {
		t.Fatalf("stop %+v %v; messages %+v", v, e, tg.messages)
	}
	bot.Handle(ctx, message(104, 42, "/commit lab postgres"))
	owned, e := client.Images(ctx, 42)
	if e != nil || len(owned) != 2 {
		t.Fatalf("commit %+v %v; messages %+v", owned, e, tg.messages)
	}
	imageID := ""
	for _, i := range owned {
		if !i.Shared {
			imageID = i.ID
		}
	}
	if imageID == "" {
		t.Fatal("missing private image")
	}
	foreignImages, e := client.Images(ctx, 43)
	if e != nil || len(foreignImages) != 1 {
		t.Fatalf("image privacy %+v %v", foreignImages, e)
	}
	bot.Handle(ctx, message(105, 42, "/delete lab"))
	all, e = client.ListVMs(ctx, 42)
	if e != nil || len(all) != 0 {
		t.Fatalf("delete %+v %v", all, e)
	}
	a, e = client.DeleteImage(ctx, 42, imageID, "tg:106:deleteimage")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = client.Wait(ctx, 42, a.OperationID, 5*time.Millisecond); e != nil {
		t.Fatal(e)
	}
	owned, e = client.Images(ctx, 42)
	if e != nil || len(owned) != 1 {
		t.Fatalf("image delete %+v %v", owned, e)
	}
	if _, e = client.Operation(ctx, 42, a.OperationID); e != nil {
		t.Fatal("operation unavailable after image deletion", e)
	}
}
