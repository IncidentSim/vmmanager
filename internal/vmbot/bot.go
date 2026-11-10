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
)

const helpText = "Команды:\n" +
	"/create — создать VM\n" +
	"/status — состояние VM\n" +
	"/terminal — ссылка на веб-терминал\n" +
	"/delete — удалить VM\n\n" +
	"Одновременно существует не более одной VM (в том числе остановленной или сохранённой)."

// Bot is the Telegram front end. It processes only the configured user in a
// private chat; everything else is dropped without any reply.
type Bot struct {
	userID     int64
	api        TGAPI
	ctl        *Controller
	st         *Store
	log        *log.Logger
	RetryDelay time.Duration
}

func NewBot(userID int64, api TGAPI, ctl *Controller, st *Store, lg *log.Logger) *Bot {
	return &Bot{userID: userID, api: api, ctl: ctl, st: st, log: lg, RetryDelay: 3 * time.Second}
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
	return from != nil && from.ID == b.userID && chat.Type == "private" && chat.ID == b.userID
}

// Handle authorizes and dispatches one update.
func (b *Bot) Handle(ctx context.Context, u Update) {
	switch {
	case u.Message != nil:
		m := u.Message
		if !b.allowed(m.From, m.Chat) {
			return
		}
		b.command(ctx, m.Chat.ID, normalizeCommand(m.Text))
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		if q.Message == nil || !b.allowed(&q.From, q.Message.Chat) {
			return
		}
		b.callback(ctx, q)
	}
}

func normalizeCommand(text string) string {
	text = strings.TrimSpace(text)
	switch text {
	case "Создать":
		return "/create"
	case "Статус":
		return "/status"
	case "Терминал":
		return "/terminal"
	case "Удалить":
		return "/delete"
	}
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	cmd := strings.Fields(text)[0]
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	return cmd
}

func (b *Bot) send(ctx context.Context, m OutMessage) {
	if err := b.api.SendMessage(ctx, m); err != nil {
		b.logf("send: %v", err)
	}
}

func (b *Bot) command(ctx context.Context, chat int64, cmd string) {
	switch cmd {
	case "/start", "/help":
		b.send(ctx, OutMessage{ChatID: chat, Text: helpText, MainKeyboard: true})
	case "/create":
		b.doCreate(ctx, chat)
	case "/status":
		b.doStatus(ctx, chat)
	case "/terminal":
		b.doTerminal(ctx, chat)
	case "/delete":
		b.doDelete(ctx, chat, 0)
	default:
		b.send(ctx, OutMessage{ChatID: chat, Text: "Неизвестная команда.\n\n" + helpText, MainKeyboard: true})
	}
}

func (b *Bot) callback(ctx context.Context, q *CallbackQuery) {
	chat := q.Message.Chat.ID
	answer := func(text string) {
		if err := b.api.AnswerCallback(ctx, q.ID, text); err != nil {
			b.logf("answer callback: %v", err)
		}
	}
	switch {
	case q.Data == "t":
		answer("")
		b.doTerminal(ctx, chat)
	case q.Data == "s":
		answer("")
		b.doStatus(ctx, chat)
	case strings.HasPrefix(q.Data, "d:"):
		gen, err := strconv.ParseUint(q.Data[2:], 10, 64)
		if err != nil || gen == 0 {
			answer("Неизвестная кнопка.")
			return
		}
		cur := b.st.Snapshot().VM
		if cur == nil || cur.Generation != gen {
			answer("Устарело: кнопка относится к другой VM.")
			return
		}
		answer("Удаляю…")
		b.doDelete(ctx, chat, gen)
	default:
		answer("Неизвестная кнопка.")
	}
}

func (b *Bot) buttons(s Status) []Button {
	if !s.Present {
		return nil
	}
	return []Button{
		{Text: "Статус", Data: "s"},
		{Text: "Терминал", Data: "t"},
		{Text: "Удалить", Data: "d:" + strconv.FormatUint(s.Generation, 10)},
	}
}

func statusText(s Status) string {
	if !s.Present {
		return "VM нет. Создайте командой /create."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "VM %s (поколение %d)\n", s.VMID, s.Generation)
	state := s.State
	if state == "" {
		state = "неизвестно"
	}
	fmt.Fprintf(&sb, "Состояние: %s\n", state)
	switch s.Phase {
	case PhaseCreating:
		sb.WriteString("Создание не подтверждено.\n")
	case PhaseDeleting:
		sb.WriteString("Удаление начато и не подтверждено; терминал закрыт. Повторите /delete.\n")
	}
	if s.Retained {
		sb.WriteString("Диск сохранён: остановка VM не подтверждена.\n")
	}
	if s.Uncertain {
		sb.WriteString("Не удалось подтвердить состояние на сервере; VM считается существующей.\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// userError converts internal errors to a fixed Russian text; raw error text
// (RPC internals, paths) is never shown.
func userError(err error, s Status) string {
	switch {
	case errors.Is(err, ErrOccupied):
		return "Не удалось создать: уже есть VM (в том числе остановленная или сохранённая). Удалите её командой /delete."
	case errors.Is(err, ErrUncertain):
		return "Не удалось подтвердить состояние VM на сервере. Существующая VM не брошена, новая не создаётся. Повторите позже."
	case errors.Is(err, ErrBusy):
		return "Выполняется другая операция. Повторите чуть позже."
	case errors.Is(err, ErrNoVM):
		return "VM нет. Создайте командой /create."
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
	b.send(ctx, OutMessage{ChatID: chat, Text: userError(err, s), Inline: b.buttons(s)})
}

func (b *Bot) doCreate(ctx context.Context, chat int64) {
	b.send(ctx, OutMessage{ChatID: chat, Text: "Создаю VM, это может занять некоторое время…"})
	s, err := b.ctl.Create(ctx)
	if err != nil {
		b.fail(ctx, chat, err, s)
		return
	}
	b.send(ctx, OutMessage{ChatID: chat, Text: "VM создана.\n" + statusText(s), Inline: b.buttons(s)})
	b.doTerminal(ctx, chat) // the link follows automatically; no extra tap needed
}

func (b *Bot) doStatus(ctx context.Context, chat int64) {
	s, err := b.ctl.Status(ctx)
	if err != nil && !errors.Is(err, ErrUncertain) {
		b.fail(ctx, chat, err, s)
		return
	}
	b.send(ctx, OutMessage{ChatID: chat, Text: statusText(s), Inline: b.buttons(s)})
}

func (b *Bot) doTerminal(ctx context.Context, chat int64) {
	link, err := b.ctl.Terminal(ctx)
	if err != nil {
		b.fail(ctx, chat, err, Status{})
		return
	}
	b.send(ctx, OutMessage{ChatID: chat, Secret: true,
		Text: "Ссылка на терминал. Это секрет: любой, у кого она есть, получает shell в VM. " +
			"Действует до удаления VM.\n" + link})
}

func (b *Bot) doDelete(ctx context.Context, chat int64, gen uint64) {
	s, err := b.ctl.Delete(ctx, gen)
	if err != nil {
		b.fail(ctx, chat, err, s)
		return
	}
	b.send(ctx, OutMessage{ChatID: chat, Text: "VM удалена, ссылка на терминал больше не действует."})
}
