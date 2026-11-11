package tgbot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
)

func validateAdminConfig(c Config) error {
	seen := map[int64]bool{}
	for _, id := range c.AdminUserIDs {
		if id <= 0 || seen[id] {
			return errors.New("admin_user_ids must contain unique positive numeric IDs")
		}
		seen[id] = true
	}
	if len(c.AdminUserIDs) > 0 && c.JWT == nil {
		return errors.New("admin commands require per-user JWT authentication; service delegation is not administrative")
	}
	return nil
}
func (b *Bot) isAdmin(id int64) bool {
	for _, a := range b.config.AdminUserIDs {
		if a == id && id > 0 {
			return true
		}
	}
	return false
}
func (b *Bot) adminPreview(ctx context.Context, u, update int64) {
	batch, e := b.client.AdminPreview(ctx, u, key(update, "delete-all-preview"))
	if e != nil {
		b.fail(ctx, u, e)
		return
	}
	lines := []string{fmt.Sprintf("Удалить ВСЕ VM всех владельцев: %d. Только этот снимок; новые VM не затрагиваются. Образы и base не удаляются. Подтверждение действует 5 минут.", len(batch.VMs))}
	for _, v := range batch.VMs {
		lines = append(lines, fmt.Sprintf("%s (%s), владелец %d", v.Name, v.ID, v.OwnerUserID))
	}
	b.lines(ctx, u, lines)
	_ = b.tg.SendMessage(ctx, OutMessage{ChatID: u, Text: "Подтвердить удаление этого снимка?", Inline: [][]Button{{{Text: "Удалить все VM снимка", Data: "da:" + batch.Token}}}})
}
func adminSummary(batch controlclient.AdminBatch) string {
	text := fmt.Sprintf("Удаление снимка: завершено %d; ошибок/сохранено %d; ожидается %d.", batch.Completed, batch.Failed, batch.Pending)
	for _, v := range batch.VMs {
		if v.Status == "failed" {
			text += fmt.Sprintf("\n%s: %s (удаление не подтверждено; принудительная очистка не выполнялась)", v.ID, v.ErrorCode)
		}
	}
	return text
}
func (b *Bot) reportAdmin(ctx context.Context, u int64, batch controlclient.AdminBatch) {
	if e := b.tg.SendMessage(ctx, OutMessage{ChatID: u, Text: adminSummary(batch)}); e == nil && batch.Pending == 0 && b.Delivery != nil {
		_ = b.Delivery.MarkAdminReported(batch.Token)
	}
}
func (b *Bot) adminConfirm(ctx context.Context, u int64, token string) {
	budget := b.WaitBudget
	if budget <= 0 || budget > time.Minute {
		budget = 40 * time.Second
	}
	wait, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var last controlclient.AdminBatch
	for {
		batch, e := b.client.AdminAdvance(wait, u, token)
		if e != nil {
			if last.Token != "" {
				b.reportAdmin(ctx, u, last)
			} else {
				b.fail(ctx, u, e)
			}
			return
		}
		last = batch
		if batch.Pending == 0 {
			b.reportAdmin(ctx, u, batch)
			return
		}
		interval := b.PollInterval
		if interval <= 0 {
			interval = time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-wait.Done():
			timer.Stop()
			b.reportAdmin(ctx, u, last)
			return
		case <-timer.C:
		}
	}
}

// resumeAdmin runs within the existing delivery poller, not a lifecycle worker.
// Confirmed API snapshots survive restart; local delivery receipts suppress
// already delivered final summaries. Sending then fsync may duplicate a summary
// after a crash, but can never broaden the snapshot or repeat VM effects.
func (b *Bot) resumeAdmin(ctx context.Context) {
	for _, u := range b.config.AdminUserIDs {
		batches, e := b.client.AdminBatches(ctx, u)
		if e != nil {
			continue
		}
		for _, batch := range batches {
			if b.Delivery.AdminReported(batch.Token) {
				continue
			}
			if batch.Pending > 0 {
				batch, e = b.client.AdminAdvance(ctx, u, batch.Token)
				if e != nil {
					continue
				}
			}
			if batch.Pending == 0 {
				b.reportAdmin(ctx, u, batch)
			}
		}
	}
}
func (s *Delivery) AdminReported(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.state.AdminReports {
		if v == token {
			return true
		}
	}
	return false
}
func (s *Delivery) MarkAdminReported(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(token) == "" {
		return errors.New("empty admin receipt")
	}
	for _, v := range s.state.AdminReports {
		if v == token {
			return nil
		}
	}
	next := s.state
	next.AdminReports = append(append([]string(nil), next.AdminReports...), token)
	return s.persist(next)
}
