package vmbot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const helpText = "Команды:\n" +
	"/create [имя] — создать VM (имя: a-z, 0-9, «-», до 20 символов; без имени будет создано автоматически)\n" +
	"/list — все VM с кнопками\n" +
	"/status [имя] — состояние VM\n" +
	"/terminal [имя] — ссылка на веб-терминал\n" +
	"/delete [имя] — удалить VM\n\n" +
	"Если VM несколько, имя нужно указать (или нажать кнопку): бот никогда не выбирает VM сам. " +
	"Остановленные и сохранённые VM тоже занимают место."

// Telegram limits: 4096 characters per message; we stay well below.
const (
	maxMessageRunes = 3500
	maxListRows     = 8 // VM entries per list message (<= 100 buttons allowed)
)

// Bot is the Telegram front end. It processes only the configured user in a
// private chat; everything else is dropped without any reply.
type Bot struct {
	users      map[int64]bool
	api        TGAPI
	ctl        *Controller
	st         *Store
	log        *log.Logger
	RetryDelay time.Duration
}

func NewBot(userID int64, api TGAPI, ctl *Controller, st *Store, lg *log.Logger) *Bot {
	return NewBotUsers([]int64{userID}, api, ctl, st, lg)
}

func NewBotUsers(ids []int64, api TGAPI, ctl *Controller, st *Store, lg *log.Logger) *Bot {
	users := map[int64]bool{}
	for _, id := range ids {
		if id > 0 {
			users[id] = true
		}
	}
	return &Bot{users: users, api: api, ctl: ctl, st: st, log: lg, RetryDelay: 3 * time.Second}
}

func (b *Bot) logf(format string, a ...any) {
	if b.log != nil {
		b.log.Printf(format, a...)
	}
}

// Run long-polls until ctx ends. The offset is persisted BEFORE an update is
// handled (at-most-once), so a restart can never replay a create or delete.
func (b *Bot) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	sem := make(chan struct{}, 4)
	for ctx.Err() == nil {
		offset := b.st.Snapshot().Offset
		ups, err := b.api.GetUpdates(ctx, offset, 30)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			b.logf("poll: %v", err)
			b.sleep(ctx)
			continue
		}
		for _, u := range ups {
			next := u.UpdateID + 1
			if next <= b.st.Snapshot().Offset {
				continue // already accepted
			}
			if err := b.st.Update(func(s *State) { s.Offset = next }); err != nil {
				b.logf("cannot persist poll offset; not handling update: %v", err)
				b.sleep(ctx)
				break
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(u Update) {
				defer wg.Done()
				defer func() { <-sem }()
				b.Handle(ctx, u)
			}(u)
		}
	}
	return nil
}

func (b *Bot) sleep(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(b.RetryDelay):
	}
}

func (b *Bot) allowed(from *User, chat Chat) bool {
	return from != nil && b.users[from.ID] && chat.Type == "private" && chat.ID == from.ID
}

// Handle authorizes and dispatches one update.
func (b *Bot) Handle(ctx context.Context, u Update) {
	switch {
	case u.Message != nil:
		m := u.Message
		if !b.allowed(m.From, m.Chat) {
			return
		}
		cmd, args := parseCommand(m.Text)
		b.command(WithUser(ctx, m.From.ID), m.Chat.ID, cmd, args)
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		if q.Message == nil || !b.allowed(&q.From, q.Message.Chat) {
			return
		}
		b.callback(WithUser(ctx, q.From.ID), q)
	}
}

// parseCommand returns the command ("" when the text is not one) and its
// whitespace-separated arguments. Arguments are data: they are only ever
// compared with stored names/IDs or validated, never interpreted.
func parseCommand(text string) (string, []string) {
	text = strings.TrimSpace(text)
	switch text {
	case "Создать":
		return "/create", nil
	case "Список":
		return "/list", nil
	case "Статус":
		return "/status", nil
	case "Терминал":
		return "/terminal", nil
	case "Удалить":
		return "/delete", nil
	}
	if !strings.HasPrefix(text, "/") {
		return "", nil
	}
	f := strings.Fields(text)
	cmd := f[0]
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	return cmd, f[1:]
}

func (b *Bot) send(ctx context.Context, m OutMessage) {
	if err := b.api.SendMessage(ctx, m); err != nil {
		b.logf("send: %v", err)
	}
}

func (b *Bot) usage(ctx context.Context, chat int64, text string) {
	b.send(ctx, OutMessage{ChatID: chat, Text: text})
}

func (b *Bot) command(ctx context.Context, chat int64, cmd string, args []string) {
	var sel string
	switch cmd {
	case "/start", "/help":
		b.send(ctx, OutMessage{ChatID: chat, Text: helpText, MainKeyboard: true})
		return
	case "/list":
		if len(args) != 0 {
			b.usage(ctx, chat, "Использование: /list")
			return
		}
		b.doList(ctx, chat, "")
		return
	case "/create", "/status", "/terminal", "/delete":
		if len(args) > 1 {
			b.usage(ctx, chat, "Использование: "+cmd+" [имя] — не больше одного аргумента.")
			return
		}
		if len(args) == 1 {
			sel = args[0]
		}
	default:
		b.send(ctx, OutMessage{ChatID: chat, Text: "Неизвестная команда.\n\n" + helpText, MainKeyboard: true})
		return
	}
	switch cmd {
	case "/create":
		b.doCreate(ctx, chat, sel)
	case "/status":
		b.doStatus(ctx, chat, Ref{Sel: sel})
	case "/terminal":
		b.doTerminal(ctx, chat, Ref{Sel: sel})
	case "/delete":
		b.doDelete(ctx, chat, Ref{Sel: sel})
	}
}

// parseGen parses a canonical positive decimal generation.
func parseGen(s string) (uint64, bool) {
	g, err := strconv.ParseUint(s, 10, 64)
	if err != nil || g == 0 || strconv.FormatUint(g, 10) != s {
		return 0, false
	}
	return g, true
}

func (b *Bot) callback(ctx context.Context, q *CallbackQuery) {
	chat := q.Message.Chat.ID
	answered := false
	answer := func(text string) {
		answered = true
		if err := b.api.AnswerCallback(ctx, q.ID, text); err != nil {
			b.logf("answer callback: %v", err)
		}
	}
	defer func() {
		if !answered {
			answer("")
		}
	}()
	switch q.Data {
	case "t": // legacy generic button: acts only when unambiguous
		answer("")
		b.doTerminal(ctx, chat, Ref{})
		return
	case "s":
		answer("")
		b.doStatus(ctx, chat, Ref{})
		return
	}
	kind, genStr, ok := strings.Cut(q.Data, ":")
	gen, gok := parseGen(genStr)
	if !ok || !gok || (kind != "s" && kind != "t" && kind != "d") {
		answer("Неизвестная кнопка.")
		return
	}
	// Buttons name a generation, which is unique forever: a replacement VM can
	// never be reached through a button made for its predecessor.
	if _, err := b.ctl.Resolve(caller(ctx), Ref{Gen: gen}); err != nil {
		answer("Устарело: кнопка относится к другой VM.")
		return
	}
	ref := Ref{Gen: gen}
	switch kind {
	case "s":
		answer("")
		b.doStatus(ctx, chat, ref)
	case "t":
		answer("")
		b.doTerminal(ctx, chat, ref)
	case "d":
		answer("Удаляю…")
		b.doDelete(ctx, chat, ref)
	}
}

func gstr(g uint64) string { return strconv.FormatUint(g, 10) }

// buttons is the one-VM keyboard.
func (b *Bot) buttons(s Status) [][]Button {
	if !s.Present {
		return nil
	}
	return [][]Button{{
		{Text: "Статус", Data: "s:" + gstr(s.Generation)},
		{Text: "Терминал", Data: "t:" + gstr(s.Generation)},
		{Text: "Удалить", Data: "d:" + gstr(s.Generation)},
	}}
}

func displayName(s Status) string {
	if s.Name != "" {
		return s.Name
	}
	return s.VMID
}

func statusText(s Status) string {
	if !s.Present {
		return "VM нет. Создайте командой /create."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "VM %s (%s, поколение %d)\n", displayName(s), s.VMID, s.Generation)
	state := s.State
	if state == "" {
		state = "неизвестно"
	}
	fmt.Fprintf(&sb, "Состояние: %s\n", state)
	switch s.Phase {
	case PhaseCreating:
		sb.WriteString("Создание не подтверждено.\n")
	case PhaseDeleting:
		sb.WriteString("Удаление начато и не подтверждено; терминал закрыт. Повторите /delete " + displayName(s) + ".\n")
	}
	if s.Retained {
		sb.WriteString("Диск сохранён: остановка VM не подтверждена.\n")
	}
	if s.Uncertain {
		sb.WriteString("Не удалось подтвердить состояние на сервере; VM считается существующей.\n")
	}
	if s.Busy {
		sb.WriteString("Сейчас выполняется операция над этой VM.\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// userError converts internal errors to a fixed Russian text; raw error text
// (RPC internals, paths) is never shown.
func userError(err error, s Status) string {
	switch {
	case errors.Is(err, ErrCapacity):
		return "Не удалось создать: достигнут лимит VM этого бота (остановленные и сохранённые тоже считаются). Удалите ненужную: /list."
	case errors.Is(err, ErrBackendFull):
		return "Не удалось создать: на сервере нет свободных ресурсов (их могут занимать и чужие VM). Новая VM не создана."
	case errors.Is(err, ErrNameTaken):
		return "Такое имя уже занято. Выберите другое или создайте без имени."
	case errors.Is(err, ErrBadName):
		return "Недопустимое имя. Разрешены строчные латинские буквы, цифры и «-», начинается с буквы, до 20 символов, не «bot-…»."
	case errors.Is(err, ErrUncertain):
		return "Не удалось подтвердить состояние VM на сервере. Существующая VM не брошена, место остаётся занятым. Повторите позже."
	case errors.Is(err, ErrBusy):
		return "Выполняется другая операция над этой VM. Повторите чуть позже."
	case errors.Is(err, ErrNoVM):
		return "Такой VM нет. Список: /list. Создать: /create."
	case errors.Is(err, ErrStale):
		return "Устарело: команда относится к другой VM."
	case errors.Is(err, ErrNotRunning):
		return "VM не запущена, либо терминал закрыт (после /delete он не восстанавливается)."
	case errors.Is(err, ErrFailed):
		if s.Retained {
			return "Не удалось удалить: VM сохранена, её остановка не подтверждена. Повторите /delete позже."
		}
		if s.Present {
			return "Не удалось завершить операцию. VM остаётся занятой; см. /status."
		}
		return "Не удалось выполнить операцию. Подробности в журнале сервиса."
	}
	return "Не удалось выполнить операцию. Подробности в журнале сервиса."
}

func (b *Bot) fail(ctx context.Context, chat int64, err error, s Status) {
	b.logf("operation failed: %v", err)
	if errors.Is(err, ErrNeedSelector) {
		b.doList(ctx, chat, "Несколько VM: укажите имя (например, /terminal имя) или нажмите кнопку. Ничего не выполнено.")
		return
	}
	b.send(ctx, OutMessage{ChatID: chat, Text: userError(err, s), Inline: b.buttons(s)})
}

func (b *Bot) doCreate(ctx context.Context, chat int64, name string) {
	if name != "" && ValidateLabel(name) != nil {
		b.fail(ctx, chat, ErrBadName, Status{})
		return
	}
	b.send(ctx, OutMessage{ChatID: chat, Text: "Создаю VM, это может занять некоторое время…"})
	s, err := b.ctl.Create(ctx, name)
	if err != nil {
		b.fail(ctx, chat, err, s)
		return
	}
	b.send(ctx, OutMessage{ChatID: chat, Text: "VM создана.\n" + statusText(s), Inline: b.buttons(s)})
	b.doTerminal(ctx, chat, Ref{Gen: s.Generation}) // the link follows automatically; no extra tap needed
}

func (b *Bot) doStatus(ctx context.Context, chat int64, ref Ref) {
	s, err := b.ctl.Status(ctx, ref)
	if err != nil && !errors.Is(err, ErrUncertain) {
		b.fail(ctx, chat, err, s)
		return
	}
	b.send(ctx, OutMessage{ChatID: chat, Text: statusText(s), Inline: b.buttons(s)})
}

func (b *Bot) doTerminal(ctx context.Context, chat int64, ref Ref) {
	link, err := b.ctl.Terminal(ctx, ref)
	if err != nil {
		b.fail(ctx, chat, err, Status{})
		return
	}
	name := ""
	if r, rerr := b.ctl.Resolve(caller(ctx), ref); rerr == nil {
		name = " для " + displayLabel(r)
	}
	b.send(ctx, OutMessage{ChatID: chat, Secret: true,
		Text: "Ссылка на терминал" + name + ". Это секрет: любой, у кого она есть, получает shell в VM. " +
			"Действует до удаления VM.\n" + link})
}

func displayLabel(r VMRecord) string {
	if r.Label != "" {
		return r.Label
	}
	return r.ID
}

func (b *Bot) doDelete(ctx context.Context, chat int64, ref Ref) {
	s, err := b.ctl.Delete(ctx, ref)
	if err != nil {
		b.fail(ctx, chat, err, s)
		return
	}
	b.send(ctx, OutMessage{ChatID: chat, Text: "VM " + displayName(s) + " удалена, её ссылка на терминал больше не действует."})
}

// listEntry is one VM in a list message.
func listEntry(s Status) string {
	return statusText(s)
}

// doList sends every owned VM, split over as many messages as the Telegram
// limits require: no VM is ever dropped or truncated.
func (b *Bot) doList(ctx context.Context, chat int64, header string) {
	all, err := b.ctl.List(ctx)
	if err != nil {
		b.fail(ctx, chat, err, Status{})
		return
	}
	if len(all) == 0 {
		text := "VM нет. Создайте командой /create."
		if header != "" {
			text = header + "\n\n" + text
		}
		b.send(ctx, OutMessage{ChatID: chat, Text: text})
		return
	}
	for _, m := range b.listMessages(chat, header, all) {
		b.send(ctx, m)
	}
}

func (b *Bot) listMessages(chat int64, header string, all []Status) []OutMessage {
	var out []OutMessage
	var text strings.Builder
	var rows [][]Button
	n := 0
	flush := func() {
		if n == 0 {
			return
		}
		out = append(out, OutMessage{ChatID: chat, Text: strings.TrimRight(text.String(), "\n"), Inline: rows})
		text.Reset()
		rows, n = nil, 0
	}
	if header != "" {
		text.WriteString(header + "\n\n")
	}
	for _, s := range all {
		e := listEntry(s) + "\n\n"
		if n > 0 && (n >= maxListRows || utf8.RuneCountInString(text.String())+utf8.RuneCountInString(e) > maxMessageRunes) {
			flush()
		}
		text.WriteString(e)
		row := []Button{}
		if s.Phase != PhaseDeleting {
			row = append(row, Button{Text: "Терминал " + displayName(s), Data: "t:" + gstr(s.Generation)})
		}
		row = append(row, Button{Text: "Удалить " + displayName(s), Data: "d:" + gstr(s.Generation)})
		rows = append(rows, row)
		n++
	}
	flush()
	return out
}
