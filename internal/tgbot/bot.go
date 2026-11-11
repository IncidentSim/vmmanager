// Package tgbot implements the private Telegram presentation/delivery layer.
// All VM ownership, authorization and orchestration remain in the HTTP API.
package tgbot

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/petruekhin/vibe-firecracker/controlclient"
	"github.com/petruekhin/vibe-firecracker/internal/identity"
	"github.com/petruekhin/vibe-firecracker/internal/jwtauth"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const helpText = "Команды:\n/create [имя] [image_id] [profile] [ttl_seconds] — создать VM\n/list — все VM с кнопками\n/status [имя] — состояние VM\n/terminal [имя] — ссылка на терминал в браузере\n/delete [имя] — удалить VM\n/stop [имя] — остановить VM\n/commit имя имя-образа — сохранить выключенный диск в новый образ\n/images — общий реестр образов (доступен всем)\n/deleteimage image_id — удалить образ, который создали вы\n/ssh [имя] — параметры SSH и ссылка для входа из браузера\n/capabilities — лимиты и профили\n/operation id — результат операции\n/token — личный токен для HTTP API и Swagger\n\nЕсли VM несколько, укажите имя или нажмите кнопку. Остановленные и сохранённые VM тоже занимают место."

var labelPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,18}[a-z0-9])?$`)

func validName(s string) bool { return labelPattern.MatchString(s) && !strings.HasPrefix(s, "bot-") }

type Bot struct {
	config                               Config
	tg                                   Telegram
	client                               *controlclient.Client
	allowed                              map[int64]bool
	Delivery                             *Delivery
	PollInterval, WaitBudget, RetryDelay time.Duration

	// Username access (empty without allowed_usernames/jwt).
	usernames map[string]bool
	bindings  *identity.Bindings
	signer    *jwtauth.Signer
	tokenTTL  time.Duration
	mu        sync.Mutex
	names     map[int64]string // verified bootstrap username of users admitted by username
}

// apiCallTTL is the lifetime of the short per-request tokens the bot uses for
// its own API calls; /token hands out the longer configured lifetime.
const apiCallTTL = 5 * time.Minute

// NewBot builds the bot. With a jwt config it loads the private signing key and
// signs a token per user for every API call; client may then be nil (a per-user
// client is created) and no service credential exists.
func NewBot(c Config, tg Telegram, client *controlclient.Client, delivery *Delivery) (*Bot, error) {
	if tg == nil || (client == nil && c.JWT == nil) || (len(c.AllowedUserIDs) == 0 && len(c.AllowedUsernames) == 0) || c.ImageID == "" {
		return nil, errors.New("invalid bot configuration")
	}
	if e := validateAdminConfig(c); e != nil {
		return nil, e
	}
	c.AdminUserIDs = append([]int64(nil), c.AdminUserIDs...)
	b := &Bot{config: c, tg: tg, client: client, Delivery: delivery, allowed: map[int64]bool{}, usernames: map[string]bool{}, names: map[int64]string{},
		PollInterval: time.Second, WaitBudget: 40 * time.Second, RetryDelay: 3 * time.Second}
	for _, u := range c.AllowedUserIDs {
		if u <= 0 || b.allowed[u] {
			return nil, errors.New("invalid user allowlist")
		}
		b.allowed[u] = true
	}
	canon, e := identity.CanonicalList(c.AllowedUsernames)
	if e != nil {
		return nil, errors.New("invalid username allowlist")
	}
	for _, n := range canon {
		b.usernames[n] = true
	}
	if c.JWT != nil {
		signer, e := jwtauth.LoadSigner(c.JWT.PrivateKeyFile, c.JWT.Issuer, c.JWT.Audience)
		if e != nil {
			return nil, errors.New("cannot load JWT signing key")
		}
		b.signer = signer
		b.tokenTTL = jwtauth.DefaultTTL
		if c.JWT.TTLSeconds > 0 {
			b.tokenTTL = time.Duration(c.JWT.TTLSeconds) * time.Second
		}
		if len(canon) > 0 {
			if c.DeliveryDir == "" {
				return nil, errors.New("delivery directory required for username bindings")
			}
			if b.bindings, e = identity.OpenBindings(filepath.Join(c.DeliveryDir, "username-bindings.json")); e != nil {
				return nil, errors.New("cannot open username bindings")
			}
		}
		if client == nil {
			if b.client, e = controlclient.NewPerUser(c.APIURL, b.apiToken, nil); e != nil {
				return nil, e
			}
		}
	}
	return b, nil
}

// apiToken signs a short-lived token for one verified user. Only users that
// passed allowedMessage in this process can obtain one.
func (b *Bot) apiToken(userID int64) (string, error) {
	b.mu.Lock()
	name, ok := b.names[userID]
	b.mu.Unlock()
	if !ok && !b.allowed[userID] && !b.isAdmin(userID) {
		return "", errors.New("user not verified")
	}
	tok, _, err := b.signer.Issue(userID, name, apiCallTTL, time.Now())
	return tok, err
}

// allowedMessage admits only a verified private-chat sender: Telegram's own
// sender ID equal to the chat ID, and either a configured numeric ID or a
// configured username (case-insensitive) whose slot is, or can now be, pinned to
// this exact ID. Display names, message text and callback data never count.
func (b *Bot) allowedMessage(from *User, chat Chat) bool {
	if from == nil || from.ID <= 0 || chat.Type != "private" || chat.ID != from.ID {
		return false
	}
	if b.allowed[from.ID] || b.isAdmin(from.ID) {
		b.mu.Lock()
		delete(b.names, from.ID)
		b.mu.Unlock()
		return true
	}
	if b.bindings == nil || from.Username == "" || strings.HasPrefix(from.Username, "@") {
		return false
	}
	name, e := identity.CanonicalUsername(from.Username)
	if e != nil || !b.usernames[name] || b.bindings.Claim(name, from.ID) != nil {
		return false
	}
	b.mu.Lock()
	b.names[from.ID] = name
	b.mu.Unlock()
	return true
}

var markdownV2Special = strings.NewReplacer("\\", "\\\\", "_", "\\_", "*", "\\*", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "~", "\\~", "`", "\\`",
	">", "\\>", "#", "\\#", "+", "\\+", "-", "\\-", "=", "\\=", "|", "\\|", "{", "\\{", "}", "\\}", ".", "\\.", "!", "\\!")

func escapeMarkdownV2(s string) string { return markdownV2Special.Replace(s) }

func lifetimeText(d time.Duration) string {
	if d%time.Hour == 0 {
		return strconv.Itoa(int(d/time.Hour)) + " ч"
	}
	if d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + " мин"
	}
	return strconv.Itoa(int(d/time.Second)) + " с"
}

// sendToken signs a user token and sends it as a protected code block without a
// link preview. The token is never logged and never stored.
func (b *Bot) sendToken(ctx context.Context, user int64) {
	if b.signer == nil {
		b.send(ctx, user, "Выпуск токенов не настроен.")
		return
	}
	b.mu.Lock()
	name := b.names[user]
	b.mu.Unlock()
	tok, _, err := b.signer.Issue(user, name, b.tokenTTL, time.Now())
	if err != nil {
		b.send(ctx, user, "Не удалось выпустить токен.")
		return
	}
	text := escapeMarkdownV2("Токен API, действует "+lifetimeText(b.tokenTTL)+". Вставьте его в Authorize в Swagger или в заголовок Authorization: Bearer. Обновить: снова /token.") + "\n```\n" + tok + "\n```"
	_ = b.tg.SendMessage(ctx, OutMessage{ChatID: user, Text: text, Secret: true, Markdown: true})
}
func (b *Bot) send(ctx context.Context, user int64, text string) {
	_ = b.tg.SendMessage(ctx, OutMessage{ChatID: user, Text: text})
}
func (b *Bot) fail(ctx context.Context, user int64, err error) {
	text := "Не удалось подтвердить результат операции. Проверьте /list и повторите позже: принятая сервером операция может продолжаться."
	var api *controlclient.APIError
	if errors.As(err, &api) {
		switch api.Code {
		case "not_found":
			text = "Такой VM или образа нет, либо кнопка устарела. Список: /list."
		case "capacity":
			text = "Достигнут лимит VM или ресурсов. Остановленные и сохранённые VM тоже занимают место. Список: /list."
		case "conflict":
			text = "Конфликт имени или состояния: проверьте /list и повторите позже."
		case "invalid_argument":
			text = "Недопустимые параметры команды. /help"
		case "forbidden", "unauthorized":
			text = "Доступ к API запрещён."
		}
	}
	b.send(ctx, user, text)
}
func parseCommand(text string) (string, []string) {
	text = strings.TrimSpace(text)
	switch text {
	case "Создать":
		text = "/create"
	case "Список":
		text = "/list"
	case "Статус":
		text = "/status"
	case "Терминал":
		text = "/terminal"
	case "Удалить":
		text = "/delete"
	}
	f := strings.Fields(text)
	if len(f) == 0 {
		return "", nil
	}
	cmd := strings.SplitN(f[0], "@", 2)[0]
	return cmd, f[1:]
}

// Handle derives identity solely from Telegram's sender and private chat. A
// callback is never an acting-user claim, and old generation buttons are inert.
func (b *Bot) Handle(ctx context.Context, u Update) {
	if u.UpdateID < 0 {
		return
	}
	if m := u.Message; m != nil {
		if !b.allowedMessage(m.From, m.Chat) {
			return
		}
		cmd, args := parseCommand(m.Text)
		b.command(ctx, m.From.ID, u.UpdateID, cmd, args)
		return
	}
	q := u.CallbackQuery
	if q == nil || q.Message == nil || !b.allowedMessage(&q.From, q.Message.Chat) {
		return
	}
	defer func() { _ = b.tg.AnswerCallback(ctx, q.ID, "") }()
	if strings.HasPrefix(q.Data, "da:") {
		if b.isAdmin(q.From.ID) && len(q.Data) == 46 {
			b.adminConfirm(ctx, q.From.ID, strings.TrimPrefix(q.Data, "da:"))
		}
		return
	}
	action, hash, ok := strings.Cut(q.Data, ":")
	if !ok || len(hash) != 43 || (action != "s" && action != "t" && action != "d") {
		b.send(ctx, q.From.ID, "Неизвестная или устаревшая кнопка. /list")
		return
	}
	all, e := b.client.ListVMs(ctx, q.From.ID)
	if e != nil {
		b.fail(ctx, q.From.ID, e)
		return
	}
	for _, v := range all {
		if buttonData(action, v.ID) == q.Data {
			cmd := map[string]string{"s": "status", "t": "terminal", "d": "delete"}[action]
			b.vmAction(ctx, q.From.ID, u.UpdateID, cmd, v)
			return
		}
	}
	b.send(ctx, q.From.ID, "Устарело: кнопка относится к другой VM. /list")
}
func buttonData(action, id string) string {
	h := sha256.Sum256([]byte(id))
	return action + ":" + base64.RawURLEncoding.EncodeToString(h[:])
}

// imageLine renders one common-registry entry. Every image is usable by every
// admitted user; /deleteimage is advertised only where the server says the
// caller may delete it (permission, not absence of references).
func imageLine(im controlclient.Image) string {
	kind := "личный"
	if im.Shared {
		kind = "общий"
	}
	line := fmt.Sprintf("%s (%s), %s", im.Name, im.ID, kind)
	if im.Phase == "deleting" {
		line += ", удаляется (недоступен для запуска)"
	}
	if im.CanDelete {
		line += ", можно удалить: /deleteimage " + im.ID
	}
	return line
}
func buttons(v controlclient.VM) [][]Button {
	return [][]Button{{{Text: "Статус " + v.Name, Data: buttonData("s", v.ID)}, {Text: "Терминал " + v.Name, Data: buttonData("t", v.ID)}, {Text: "Удалить " + v.Name, Data: buttonData("d", v.ID)}}}
}
func statusText(v controlclient.VM) string {
	return fmt.Sprintf("VM %s (%s)\nСостояние: %s; фаза: %s\nCPU: %d, RAM: %d MiB", v.Name, v.ID, v.State, v.Phase, v.Resources.VCPU, v.Resources.MemoryMiB)
}
func (b *Bot) list(ctx context.Context, u int64, header string) {
	all, e := b.client.ListVMs(ctx, u)
	if e != nil {
		b.fail(ctx, u, e)
		return
	}
	if len(all) == 0 {
		b.send(ctx, u, header+"VM нет. Создайте командой /create.")
		return
	}
	text := header
	var rows [][]Button
	flush := func() {
		if len(rows) > 0 {
			_ = b.tg.SendMessage(ctx, OutMessage{ChatID: u, Text: strings.TrimSpace(text), Inline: rows})
			text = ""
			rows = nil
		}
	}
	for _, v := range all {
		entry := statusText(v) + "\n\n"
		if len(rows) >= 8 || utf8.RuneCountInString(text+entry) > 3500 {
			flush()
		}
		text += entry
		rows = append(rows, buttons(v)[0])
	}
	flush()
}
func (b *Bot) resolve(ctx context.Context, u int64, sel string) (controlclient.VM, bool) {
	all, e := b.client.ListVMs(ctx, u)
	if e != nil {
		b.fail(ctx, u, e)
		return controlclient.VM{}, false
	}
	var matches []controlclient.VM
	for _, v := range all {
		if sel == "" || sel == v.ID || sel == v.Name {
			matches = append(matches, v)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	if len(matches) > 1 {
		b.list(ctx, u, "Несколько VM: укажите имя или нажмите кнопку. Ничего не выполнено.\n\n")
	} else {
		b.send(ctx, u, "Такой VM нет. Список: /list. Создать: /create.")
	}
	return controlclient.VM{}, false
}
func key(update int64, action string) string {
	return "tg:" + strconv.FormatInt(update, 10) + ":" + action
}
func (b *Bot) command(ctx context.Context, u, update int64, cmd string, args []string) {
	switch cmd {
	case "/delete_all":
		if b.isAdmin(u) && len(args) == 0 {
			b.adminPreview(ctx, u, update)
		}
		return
	case "/start", "/help":
		_ = b.tg.SendMessage(ctx, OutMessage{ChatID: u, Text: helpText, MainKeyboard: true})
		return
	case "/token":
		if len(args) != 0 {
			b.send(ctx, u, "Использование: /token")
			return
		}
		b.sendToken(ctx, u)
		return
	case "/list":
		if len(args) != 0 {
			b.send(ctx, u, "Использование: /list")
			return
		}
		b.list(ctx, u, "")
		return
	case "/create":
		if len(args) > 4 {
			b.send(ctx, u, "Использование: /create [имя] [image_id] [profile] [ttl_seconds]")
			return
		}
		r := controlclient.CreateRequest{ImageID: b.config.ImageID, Profile: b.config.Profile, TTLSeconds: b.config.VMTTLSeconds}
		if len(args) > 0 {
			if !validName(args[0]) {
				b.send(ctx, u, "Недопустимое имя: строчные латинские буквы, цифры и «-», до 20 символов, не «bot-…».")
				return
			}
			r.Name = args[0]
		}
		if len(args) > 1 {
			r.ImageID = args[1]
		}
		if len(args) > 2 {
			if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`).MatchString(args[2]) {
				b.send(ctx, u, "Недопустимый профиль.")
				return
			}
			r.Profile = args[2]
		}
		if len(args) == 4 {
			ttl, err := strconv.ParseInt(args[3], 10, 64)
			if err != nil || ttl < 1 {
				b.send(ctx, u, "ttl_seconds должно быть положительным целым числом.")
				return
			}
			r.TTLSeconds = ttl
		}
		b.send(ctx, u, "Создаю VM, это может занять некоторое время…")
		a, e := b.client.Create(ctx, u, key(update, "create"), r)
		if e != nil {
			b.fail(ctx, u, e)
			return
		}
		if b.finish(ctx, u, a, "VM создана.") {
			v, e := b.client.VM(ctx, u, a.VMID)
			if e != nil {
				b.fail(ctx, u, e)
				return
			}
			b.vmAction(ctx, u, update, "terminal", v)
		}
		return
	case "/operation":
		if len(args) != 1 {
			b.send(ctx, u, "Использование: /operation id")
			return
		}
		op, e := b.client.Operation(ctx, u, args[0])
		if e != nil {
			b.fail(ctx, u, e)
			return
		}
		b.showOperation(ctx, u, op)
		return
	case "/images":
		if len(args) != 0 {
			b.send(ctx, u, "Использование: /images")
			return
		}
		all, e := b.client.Images(ctx, u)
		if e != nil {
			b.fail(ctx, u, e)
			return
		}
		lines := []string{}
		for _, im := range all {
			lines = append(lines, imageLine(im))
		}
		b.lines(ctx, u, lines)
		return
	case "/deleteimage":
		if len(args) != 1 {
			b.send(ctx, u, "Использование: /deleteimage image_id")
			return
		}
		a, e := b.client.DeleteImage(ctx, u, args[0], key(update, "deleteimage"))
		if e != nil {
			b.fail(ctx, u, e)
			return
		}
		b.finish(ctx, u, a, "Образ удалён.")
		return
	case "/capabilities":
		if len(args) != 0 {
			b.send(ctx, u, "Использование: /capabilities")
			return
		}
		c, e := b.client.Capabilities(ctx, u)
		if e != nil {
			b.fail(ctx, u, e)
			return
		}
		lines := []string{fmt.Sprintf("Лимит VM: %d, на пользователя: %d", c.MaxVMs, c.MaxVMsPerUser)}
		for _, p := range c.Profiles {
			lines = append(lines, fmt.Sprintf("%s: CPU %d, RAM %d MiB", p.Name, p.VCPU, p.MemoryMiB))
		}
		b.lines(ctx, u, lines)
		return
	case "/commit":
		if len(args) != 2 || !validName(args[1]) {
			b.send(ctx, u, "Использование: /commit имя-VM имя-нового-образа")
			return
		}
		v, ok := b.resolve(ctx, u, args[0])
		if !ok {
			return
		}
		a, e := b.client.Commit(ctx, u, v.ID, key(update, "commit"), args[1])
		if e != nil {
			b.fail(ctx, u, e)
			return
		}
		b.finish(ctx, u, a, "Диск сохранён в новый образ. Исходная VM остановлена и занимает место до /delete.")
		return
	case "/status", "/terminal", "/delete", "/stop", "/ssh":
		if len(args) > 1 {
			b.send(ctx, u, "Использование: "+cmd+" [имя]")
			return
		}
		sel := ""
		if len(args) == 1 {
			sel = args[0]
		}
		v, ok := b.resolve(ctx, u, sel)
		if ok {
			b.vmAction(ctx, u, update, strings.TrimPrefix(cmd, "/"), v)
		}
		return
	default:
		_ = b.tg.SendMessage(ctx, OutMessage{ChatID: u, Text: "Неизвестная команда.\n\n" + helpText, MainKeyboard: true})
	}
}
func (b *Bot) vmAction(ctx context.Context, u, update int64, action string, v controlclient.VM) {
	switch action {
	case "status":
		fresh, e := b.client.VM(ctx, u, v.ID)
		if e != nil {
			b.fail(ctx, u, e)
			return
		}
		_ = b.tg.SendMessage(ctx, OutMessage{ChatID: u, Text: statusText(fresh), Inline: buttons(fresh)})
	case "ssh":
		ssh, e := b.client.SSH(ctx, u, v.ID)
		if e != nil {
			b.fail(ctx, u, e)
			return
		}
		text := fmt.Sprintf("SSH: %s@%s, port %d; ready=%t\nHost key: %s", ssh.User, ssh.Host, ssh.Port, ssh.Ready, ssh.HostKeyFingerprint)
		if line, ok := browserLine(ssh.BrowserURL, ssh.BrowserStatus); ok {
			text += "\n" + line
		}
		b.send(ctx, u, text)
	case "delete", "stop", "terminal":
		var a controlclient.Accepted
		var e error
		switch action {
		case "delete":
			a, e = b.client.Delete(ctx, u, v.ID, key(update, action))
		case "stop":
			a, e = b.client.Stop(ctx, u, v.ID, key(update, action))
		case "terminal":
			a, e = b.client.Terminal(ctx, u, v.ID, key(update, action))
		}
		if e != nil {
			b.fail(ctx, u, e)
			return
		}
		text := map[string]string{"delete": "VM удалена, её ссылка на терминал больше не действует.", "stop": "VM остановлена; терминал закрыт. Место занято до /delete.", "terminal": ""}[action]
		b.finish(ctx, u, a, text)
	}
}
func (b *Bot) finish(ctx context.Context, u int64, a controlclient.Accepted, success string) bool {
	if a.OperationID == "" {
		b.fail(ctx, u, errors.New("missing operation ID"))
		return false
	}
	budget := b.WaitBudget
	if budget <= 0 || budget > time.Minute {
		budget = 40 * time.Second
	}
	wait, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	op, e := b.client.Wait(wait, u, a.OperationID, b.PollInterval)
	if op.Status != "succeeded" && op.Status != "failed" {
		b.send(ctx, u, "Операция принята; результат пока не подтверждён. Проверить: /operation "+a.OperationID)
		return false
	}
	if e != nil {
		b.fail(ctx, u, e)
		return false
	}
	if success != "" {
		b.send(ctx, u, success)
	} else {
		b.showOperation(ctx, u, op)
	}
	return true
}
func (b *Bot) showOperation(ctx context.Context, u int64, op controlclient.Operation) {
	if op.Status == "failed" {
		if op.Error != nil {
			b.fail(ctx, u, op.Error)
		} else {
			b.fail(ctx, u, errors.New("operation failed"))
		}
		return
	}
	if op.Status != "succeeded" {
		b.send(ctx, u, "Операция ещё выполняется. /operation "+op.ID)
		return
	}
	var result struct {
		URL           string `json:"url"`
		BrowserStatus string `json:"browser_status"`
	}
	if json.Unmarshal(op.Result, &result) == nil && (result.URL != "" || result.BrowserStatus != "") {
		if result.BrowserStatus == "" { // Nexterm disabled: legacy ttyd capability link
			link, e := url.Parse(result.URL)
			if e != nil || link.Scheme != "https" || link.Host == "" || link.User != nil {
				b.fail(ctx, u, errors.New("invalid terminal URL"))
				return
			}
			_ = b.tg.SendMessage(ctx, OutMessage{ChatID: u, Secret: true, Text: "Ссылка на терминал. Это секрет: любой, у кого она есть, получает shell в VM. Действует до остановки, сохранения или удаления VM.\n" + result.URL})
			return
		}
		line, ok := browserLine(result.URL, result.BrowserStatus)
		if !ok {
			b.fail(ctx, u, errors.New("invalid terminal URL"))
			return
		}
		// Plain (copyable) message to the owner's own private chat only.
		b.send(ctx, u, line)
		return
	}
	text := "Операция завершена. /list"
	if op.VMID != "" {
		text += "\nVM: " + op.VMID
	}
	if op.ImageID != "" {
		text += "\nОбраз: " + op.ImageID
	}
	b.send(ctx, u, text)
}

// browserLine renders the unified terminal link message. ok=false means "nothing
// to show" for an absent status, or an invalid URL. The URL is a personal
// session secret and is never logged.
func browserLine(rawURL, status string) (string, bool) {
	if rawURL != "" {
		link, e := url.Parse(rawURL)
		if e != nil || link.Scheme != "https" || link.Host == "" || link.User != nil {
			return "", false
		}
		return "Терминал в браузере (личная ссылка, не пересылайте):\n" + rawURL, true
	}
	switch status {
	case "unsupported":
		return "Терминал в браузере недоступен: VM создана с вашим SSH-ключом. Используйте обычный SSH (/ssh).", true
	case "pending":
		return "Терминал в браузере ещё готовится. Повторите /terminal через минуту; обычный SSH доступен через /ssh.", true
	}
	return "", false
}
func (b *Bot) lines(ctx context.Context, u int64, lines []string) {
	if len(lines) == 0 {
		b.send(ctx, u, "Список пуст.")
		return
	}
	text := ""
	for _, l := range lines {
		if utf8.RuneCountInString(text+l) > 3500 {
			b.send(ctx, u, text)
			text = ""
		}
		text += l + "\n"
	}
	if text != "" {
		b.send(ctx, u, strings.TrimSpace(text))
	}
}

// Run preserves the legacy at-most-once acceptance policy: fsync the next
// offset BEFORE dispatch. A crash in that gap can lose a command; it cannot
// claim exactly-once Telegram delivery. API mutations remain idempotent.
func (b *Bot) Run(ctx context.Context) error {
	if b.Delivery == nil {
		return errors.New("delivery store required")
	}
	for ctx.Err() == nil {
		b.resumeAdmin(ctx)
		ups, e := b.tg.GetUpdates(ctx, b.Delivery.Offset(), 30)
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			timer := time.NewTimer(b.RetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
			continue
		}
		for _, u := range ups {
			if ctx.Err() != nil {
				return nil
			}
			if u.UpdateID < 0 || u.UpdateID == math.MaxInt64 || u.UpdateID < b.Delivery.Offset() {
				continue
			}
			if e = b.Delivery.Advance(u.UpdateID + 1); e != nil {
				return e
			}
			b.Handle(ctx, u)
		}
	}
	return nil
}
