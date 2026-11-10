package vmbot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

func say(b *Bot, id int64, text string) { b.Handle(bg, msg(id, testUser, testUser, "private", text)) }
func press(b *Bot, id int64, data string) {
	b.Handle(bg, cb(id, testUser, testUser, data))
}

func allButtons(msgs []OutMessage) []Button {
	var out []Button
	for _, m := range msgs {
		for _, r := range m.Inline {
			out = append(out, r...)
		}
	}
	return out
}

func hasData(bs []Button, data string) bool {
	for _, b := range bs {
		if b.Data == data {
			return true
		}
	}
	return false
}

func TestListShowsEveryVMWithPerVMTerminalAndDeleteButtons(t *testing.T) {
	e := newEnvMax(t, 2)
	b, api := newBot(t, e)
	mustCreate(t, e, "alpha")
	mustCreate(t, e, "beta")
	api.sent = nil
	say(b, 1, "/list")
	txt := api.texts()
	for _, want := range []string{"alpha", "beta", "Состояние"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("list lacks %q: %s", want, txt)
		}
	}
	bs := allButtons(api.sent)
	for _, d := range []string{"t:1", "d:1", "t:2", "d:2"} {
		if !hasData(bs, d) {
			t.Fatalf("missing generation-bound button %s in %+v", d, bs)
		}
	}
	for _, tok := range []string{e.store.Snapshot().VMs[0].Token, e.store.Snapshot().VMs[1].Token} {
		if strings.Contains(txt, tok) {
			t.Fatal("a list must never carry capabilities")
		}
	}
	// the reply-keyboard button maps to the list too
	api.sent = nil
	say(b, 2, "Список")
	if !strings.Contains(api.texts(), "beta") {
		t.Fatal("Список button")
	}
}

func TestAmbiguousCommandsShowTheListAndNeverAct(t *testing.T) {
	e := newEnvMax(t, 2)
	b, api := newBot(t, e)
	mustCreate(t, e, "alpha")
	mustCreate(t, e, "beta")
	for i, in := range []string{"/delete", "/terminal", "Удалить", "Терминал", "/delete@mybot"} {
		api.sent = nil
		say(b, int64(10+i), in)
		if e.be.Count() != 2 || len(e.store.Snapshot().VMs) != 2 {
			t.Fatalf("%q acted on an arbitrary VM", in)
		}
		if !strings.Contains(api.texts(), "alpha") || !strings.Contains(api.texts(), "beta") || !strings.Contains(api.texts(), "Ничего не выполнено") {
			t.Fatalf("%q must show the list and ask for a choice: %s", in, api.texts())
		}
		for _, m := range api.sent {
			if m.Secret {
				t.Fatalf("%q leaked a link", in)
			}
		}
	}
	// legacy generic buttons are ambiguous now: they show the list, never act
	api.sent, api.answers = nil, nil
	press(b, 30, "t")
	press(b, 31, "s")
	if !strings.Contains(api.texts(), "beta") || e.be.Count() != 2 || len(e.terms.revoked) != 0 {
		t.Fatal("legacy t/s must only list when ambiguous")
	}
	for _, m := range api.sent {
		if m.Secret {
			t.Fatal("legacy t must not hand out a link when ambiguous")
		}
	}
	// /status without selector with several VMs is the list
	api.sent = nil
	say(b, 32, "/status")
	if !strings.Contains(api.texts(), "alpha") || !strings.Contains(api.texts(), "beta") {
		t.Fatal("status of several VMs")
	}
}

func TestSelectorCommandsActOnExactlyTheNamedVM(t *testing.T) {
	e := newEnvMax(t, 2)
	b, api := newBot(t, e)
	a := mustCreate(t, e, "alpha")
	bb := mustCreate(t, e, "beta")
	tokA := e.store.Snapshot().VMs[0].Token
	tokB := e.store.Snapshot().VMs[1].Token
	api.sent = nil
	say(b, 1, "/terminal beta")
	last := api.sent[len(api.sent)-1]
	if !last.Secret || !strings.Contains(last.Text, "/t/"+tokB+"/") || strings.Contains(last.Text, tokA) {
		t.Fatalf("beta's link only: %+v", last)
	}
	say(b, 2, "/status "+a.VMID) // by exact ID
	if !strings.Contains(api.sent[len(api.sent)-1].Text, "alpha") {
		t.Fatal("status by id")
	}
	say(b, 3, "/delete alpha")
	if _, ok := e.store.Snapshot().ByID(a.VMID); ok {
		t.Fatal("alpha must be deleted")
	}
	if _, ok := e.store.Snapshot().ByID(bb.VMID); !ok || e.be.Count() != 1 {
		t.Fatal("beta must survive")
	}
	// one VM left: omitting the selector is convenient again
	api.sent = nil
	say(b, 4, "/terminal")
	if l := api.sent[len(api.sent)-1]; !l.Secret || !strings.Contains(l.Text, tokB) {
		t.Fatalf("%+v", l)
	}
	say(b, 5, "/delete")
	if e.be.Count() != 0 {
		t.Fatal("single-VM /delete stays convenient")
	}
	api.sent = nil
	say(b, 6, "/delete")
	if !strings.Contains(api.texts(), "нет") {
		t.Fatalf("%s", api.texts())
	}
}

func TestCreateWithNameSendsThatVMsSecretLinkAndAutoNameWorks(t *testing.T) {
	e := newEnvMax(t, 2)
	b, api := newBot(t, e)
	mustCreate(t, e, "alpha")
	api.sent = nil
	say(b, 1, "/create beta")
	n := len(api.sent)
	link := api.sent[n-1]
	tokB := e.store.Snapshot().VMs[1].Token
	if !link.Secret || !strings.Contains(link.Text, "/t/"+tokB+"/") || strings.Contains(link.Text, e.store.Snapshot().VMs[0].Token) {
		t.Fatalf("link for the new VM only: %+v", link)
	}
	for _, m := range api.sent[:n-1] {
		if m.Secret || strings.Contains(m.Text, tokB) {
			t.Fatalf("only the link message may carry the secret: %+v", m)
		}
	}
	api.sent = nil
	say(b, 2, "/create")
	if !strings.Contains(api.texts(), "лимит") {
		t.Fatalf("capacity message: %s", api.texts())
	}
	say(b, 5, "/delete beta") // room again, but the name "alpha" is taken
	api.sent = nil
	say(b, 3, "/create alpha")
	if !strings.Contains(api.texts(), "занято") {
		t.Fatalf("name-taken message: %s", api.texts())
	}
	say(b, 4, "/create a b")
	if !strings.Contains(api.sent[len(api.sent)-1].Text, "Использование") {
		t.Fatal("usage")
	}
}

func TestBackendFullIsReportedHonestly(t *testing.T) {
	e := newEnvMax(t, 8)
	b, api := newBot(t, e)
	for i := 0; i < 3; i++ { // foreign VMs fill the backend (capacity 3)
		if _, err := e.svc.Create(bg, vmlib.CreateRequest{ImageID: "base", Profile: "small"}); err != nil {
			t.Fatal(err)
		}
	}
	say(b, 1, "/create x")
	if !strings.Contains(api.texts(), "нет свободных ресурсов") {
		t.Fatalf("%s", api.texts())
	}
	if len(e.store.Snapshot().VMs) != 0 {
		t.Fatal("nothing owned after a definitive refusal")
	}
}

func TestOldGenerationCallbackSelectsTheMigratedOriginalNeverTheReplacement(t *testing.T) {
	e := newEnvMax(t, 2)
	const liveID = "bot-c5d5cd1e8a7894eb"
	if _, err := e.svc.Create(bg, vmlib.CreateRequest{ID: liveID, ImageID: "base", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	e.store.Close()
	writeState(t, e.dir, v1File(5, 77, &VMRecord{OwnerUserID: testUser, ID: liveID, Token: testTok(5), Phase: PhaseActive, Generation: 5}), 0o600)
	e.restart(t)
	b, api := newBot(t, e)
	// a second VM exists next to the original; the OLD message's button d:5
	mustCreate(t, e, "other")
	press(b, 1, "d:5")
	if _, ok := e.store.Snapshot().ByID(liveID); ok {
		t.Fatal("the old d:<generation> button must delete the migrated original")
	}
	if _, ok := e.store.Snapshot().BySelector("other"); !ok || e.be.Count() != 1 {
		t.Fatal("the other VM must be untouched")
	}
	// replacement created later; the same old button is now stale and inert
	mustCreate(t, e, "vm-5") // same label as the migrated original had: still a different generation
	api.answers = nil
	press(b, 2, "d:5")
	if len(api.answers) != 1 || !strings.Contains(api.answers[0], "Устарело") {
		t.Fatalf("answers %v", api.answers)
	}
	if _, ok := e.store.Snapshot().BySelector("vm-5"); !ok || e.be.Count() != 2 {
		t.Fatal("stale callback retargeted the replacement")
	}
}

func TestStaleCallbacksOfAnOriginalNeverTouchAReplacement(t *testing.T) {
	e := newEnvMax(t, 2)
	b, api := newBot(t, e)
	mustCreate(t, e, "alpha") // gen 1
	mustCreate(t, e, "beta")  // gen 2
	press(b, 1, "d:1")
	mustCreate(t, e, "alpha") // replacement, gen 3, same name
	started, revoked := len(e.terms.started), len(e.terms.revoked)
	api.answers, api.sent = nil, nil
	for i, d := range []string{"d:1", "t:1", "s:1"} {
		press(b, int64(10+i), d)
	}
	if len(api.answers) != 3 {
		t.Fatalf("%v", api.answers)
	}
	for _, a := range api.answers {
		if !strings.Contains(a, "Устарело") {
			t.Fatalf("answers %v", api.answers)
		}
	}
	if e.be.Count() != 2 || len(api.sent) != 0 || len(e.terms.started) != started || len(e.terms.revoked) != revoked {
		t.Fatal("stale buttons had effects")
	}
	// garbage and non-canonical data is refused, never parsed leniently
	api.answers = nil
	for i, d := range []string{"d:", "d:0", "d:01", "d:+3", "d:-1", "x:3", "d:3:4", "d:18446744073709551616", "t:3 ", "D:3"} {
		press(b, int64(30+i), d)
	}
	for _, a := range api.answers {
		if !strings.Contains(a, "Неизвестная") {
			t.Fatalf("answers %v", api.answers)
		}
	}
	if e.be.Count() != 2 {
		t.Fatal("malformed callback deleted something")
	}
	// current generation works
	press(b, 50, "d:3")
	if _, ok := e.store.Snapshot().ByGeneration(3); ok {
		t.Fatal("current generation must delete")
	}
}

func TestCallbackDataStaysWithinTelegramLimit(t *testing.T) {
	b := &Bot{}
	big := uint64(1<<64 - 1)
	var all []Button
	all = append(all, b.buttons(Status{Present: true, Generation: big})[0]...)
	for _, m := range b.listMessages(1, "", []Status{{Present: true, Generation: big, Name: "alpha", VMID: "bot-0123456789abcdef"}}) {
		all = append(all, allButtons([]OutMessage{m})...)
	}
	for _, bt := range all {
		if len(bt.Data) > 64 || len(bt.Data) == 0 {
			t.Fatalf("callback data %q is %d bytes", bt.Data, len(bt.Data))
		}
	}
}

func TestListSplitsOverMessagesWithoutLosingAnyVM(t *testing.T) {
	b := &Bot{}
	var all []Status
	for i := 1; i <= 40; i++ {
		all = append(all, Status{Present: true, Generation: uint64(i), VMID: fmt.Sprintf("bot-%016x", i), Name: fmt.Sprintf("vm-%d", i),
			State: "running", Phase: PhaseActive, Retained: i%2 == 0, Uncertain: i%3 == 0, Busy: i%5 == 0, Reason: strings.Repeat("я", 300)})
	}
	msgs := b.listMessages(7, "header", all)
	if len(msgs) < 2 {
		t.Fatalf("40 VMs must not fit one message: %d", len(msgs))
	}
	seen := map[string]int{}
	for _, m := range msgs {
		if n := utf8.RuneCountInString(m.Text); n > 4096 || n == 0 {
			t.Fatalf("message of %d runes violates the Telegram limit", n)
		}
		if len(m.Inline) > 100 {
			t.Fatal("too many buttons")
		}
		for _, r := range m.Inline {
			if len(r) > 8 {
				t.Fatal("too many buttons per row")
			}
		}
		for _, s := range all {
			if strings.Contains(m.Text, s.VMID) {
				seen[s.VMID]++
			}
		}
	}
	for _, s := range all {
		if seen[s.VMID] != 1 {
			t.Fatalf("%s appears %d times (silent truncation or duplication)", s.VMID, seen[s.VMID])
		}
	}
	// every VM has its delete button somewhere
	bs := allButtons(msgs)
	for _, s := range all {
		if !hasData(bs, "d:"+gstr(s.Generation)) {
			t.Fatalf("no delete button for %d", s.Generation)
		}
	}
}

func TestNameAndSelectorInputCannotInjectPathsOrCommands(t *testing.T) {
	e := newEnvMax(t, 2)
	b, api := newBot(t, e)
	marker := t.TempDir() + "/pwned"
	for i, in := range []string{
		"/create $(touch " + marker + ")", "/create a;touch", "/create ../../etc/passwd", "/create `id`", "/create A", "/create -rf",
		"/create bot-0123456789abcdef", "/create a\nb", "/create ‮abc", "/create " + strings.Repeat("a", 64),
	} {
		say(b, int64(i+1), in)
	}
	if len(e.store.Snapshot().VMs) > 1 || e.be.Count() > 1 { // "a\nb" splits into the command "/create a"; one valid VM at most
		t.Fatalf("hostile names created VMs: %v", labels(e))
	}
	for _, v := range e.store.Snapshot().VMs {
		if v.Label != "a" {
			t.Fatalf("unexpected label %q", v.Label)
		}
	}
	mustCreate(t, e, "real")
	n := e.be.Count()
	for i, in := range []string{"/delete ../real", "/delete real/../real", "/delete $(id)", "/terminal ;ls", "/status *", "/delete all", "/delete latest", "/delete -a"} {
		say(b, int64(100+i), in)
	}
	if e.be.Count() != n {
		t.Fatal("a hostile selector deleted a VM")
	}
	if _, err := e.ctl.Resolve(testUser, Ref{Sel: "all"}); err == nil {
		t.Fatal("'all' must not be a selector")
	}
	_ = api
	for _, m := range api.sent {
		if strings.Contains(m.Text, "$(") && m.Secret {
			t.Fatal("echoed hostile input in a secret message")
		}
	}
}

func TestCallbackIsAcknowledgedBeforeSlowWork(t *testing.T) {
	e := newEnvMax(t, 2)
	b, api := newBot(t, e)
	mustCreate(t, e, "alpha")
	release := make(chan struct{})
	e.lc.del = func(ctx context.Context, id string) (vmlib.DeleteResult, error) {
		<-release
		return e.svc.Delete(ctx, id)
	}
	done := make(chan struct{})
	go func() { press(b, 1, "d:1"); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		api.mu.Lock()
		n := len(api.answers)
		api.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the callback was not acknowledged while the delete is still running")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("work should still be blocked")
	default:
	}
	close(release)
	<-done
}

func TestStatusMessageNeverContainsCapabilityInMultiVMFlows(t *testing.T) {
	e := newEnvMax(t, 2)
	b, api := newBot(t, e)
	mustCreate(t, e, "alpha")
	mustCreate(t, e, "beta")
	api.sent = nil
	say(b, 1, "/list")
	say(b, 2, "/status alpha")
	say(b, 3, "/delete beta")
	for _, m := range api.sent {
		for _, v := range e.store.Snapshot().VMs {
			if strings.Contains(m.Text, v.Token) {
				t.Fatalf("capability in a normal message: %q", m.Text)
			}
		}
		if m.Secret {
			t.Fatal("no secret messages expected")
		}
	}
}
