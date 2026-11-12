package vmbot

import (
	"errors"
	"fmt"
	"time"
)

// AdminBatch is an immutable inventory snapshot plus durable progress. The
// opaque token is a confirmation capability, but never substitutes for auth.
type AdminBatch struct {
	Token       string       `json:"token"`
	AdminUserID int64        `json:"admin_user_id"`
	ChatID      int64        `json:"chat_id"`
	Key         string       `json:"key"`
	ExpiresAt   time.Time    `json:"expires_at"`
	Confirmed   bool         `json:"confirmed"`
	VMs         []AdminVMRef `json:"vms"`
	Pending     int          `json:"pending"`
	Completed   int          `json:"completed"`
	Failed      int          `json:"failed"`
}
type AdminVMRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	OwnerUserID int64  `json:"owner_user_id"`
	OperationID string `json:"operation_id,omitempty"`
	Status      string `json:"status,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
}

func ValidateAdminIDs(ids []int64) error {
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return errors.New("admin_user_ids must contain unique positive numeric IDs")
		}
		seen[id] = true
	}
	return nil
}

// ConfigureAdmins must be called before serving; it copies rather than aliases config.
func (c *Controller) ConfigureAdmins(ids []int64) error {
	if e := ValidateAdminIDs(ids); e != nil {
		return e
	}
	c.admins = append([]int64(nil), ids...)
	return nil
}
func (c *Controller) AdminAllowed(id int64) bool {
	for _, a := range c.admins {
		if id == a && id > 0 {
			return true
		}
	}
	return false
}
func (c *Controller) AdminPreview(admin, chat int64, key string) (AdminBatch, error) {
	if !c.AdminAllowed(admin) || chat != admin {
		return AdminBatch{}, ErrControlForbidden
	}
	if len(key) < 1 || len(key) > 128 {
		return AdminBatch{}, ErrControlConflict
	}
	token, e := randToken()
	if e != nil {
		return AdminBatch{}, e
	}
	var out AdminBatch
	e = c.st.UpdateErr(func(s *State) error {
		for _, b := range s.AdminBatches {
			if b.AdminUserID == admin && b.Key == key {
				out = b
				return nil
			}
		}
		out = AdminBatch{Token: token, AdminUserID: admin, ChatID: chat, Key: key, ExpiresAt: c.now().Add(5 * time.Minute), VMs: []AdminVMRef{}}
		for _, v := range s.VMs {
			out.VMs = append(out.VMs, AdminVMRef{ID: v.ID, Name: v.Label, OwnerUserID: v.OwnerUserID})
		}
		out.Pending = len(out.VMs)
		s.AdminBatches = append(s.AdminBatches, out)
		return nil
	})
	return out, e
}

// AdminBatches returns only this admin's confirmed unfinished transactions for
// frontend recovery. It does not execute work, or re-snapshot inventory.
func (c *Controller) AdminBatches(admin int64) ([]AdminBatch, error) {
	if !c.AdminAllowed(admin) {
		return nil, ErrControlForbidden
	}
	out := []AdminBatch{}
	for _, b := range c.st.Snapshot().AdminBatches {
		if b.AdminUserID == admin && b.Confirmed {
			out = append(out, b)
		}
	}
	return out, nil
}

// AdminAdvance admits at most one existing owner delete at a time. No new
// lifecycle runner exists: the durable DBOS owner handles every VM effect.
// Re-delivery after any crash uses the same per-ID key, even across the gap
// between admission and saving OperationID. Unknown outcomes remain pending.
func (c *Controller) AdminAdvance(admin, chat int64, token string) (AdminBatch, error) {
	if !c.AdminAllowed(admin) || chat != admin {
		return AdminBatch{}, ErrControlForbidden
	}
	c.adminMu.Lock()
	defer c.adminMu.Unlock()
	var b AdminBatch
	found := false
	for _, x := range c.st.Snapshot().AdminBatches {
		if x.Token == token && x.AdminUserID == admin && x.ChatID == chat {
			b = x
			found = true
			break
		}
	}
	if !found {
		return b, ErrNoVM
	}
	if !b.Confirmed && !c.now().Before(b.ExpiresAt) {
		return AdminBatch{}, ErrControlConflict
	}
	if !b.Confirmed {
		b.Confirmed = true
		if e := c.saveAdminBatch(b); e != nil {
			return AdminBatch{}, e
		}
	}
	for i := range b.VMs {
		ref := &b.VMs[i]
		if ref.Status == "succeeded" || ref.Status == "failed" {
			continue
		}
		key := fmt.Sprintf("admin-delete:%s:%s", b.Token, ref.ID)
		// Reconcile a saved operation, or recover admission before a crash.
		var op ControlOperation
		exists := false
		for _, o := range c.st.Snapshot().Operations {
			if o.OwnerUserID == ref.OwnerUserID && o.Key == key {
				op = o
				exists = true
				break
			}
		}
		if !exists {
			current, ok := c.st.Snapshot().ByID(ref.ID)
			if !ok {
				ref.Status = "succeeded"
				continue
			} // already gone; never target a replacement
			if current.OwnerUserID != ref.OwnerUserID {
				ref.Status = "failed"
				ref.ErrorCode = "conflict"
				continue
			}
			var e error
			op, e = c.acceptControl(ref.OwnerUserID, key, ControlRequest{Kind: "delete", VMID: ref.ID}, admin)
			if e != nil {
				// Admission can persist intent then fail delivery: discover that durable
				// operation before deciding failure. Retained VM is never force-cleaned.
				for _, o := range c.st.Snapshot().Operations {
					if o.OwnerUserID == ref.OwnerUserID && o.Key == key {
						op = o
						exists = true
						break
					}
				}
				if !exists {
					ref.Status = "failed"
					ref.ErrorCode = "unavailable"
					continue
				}
			}
		}
		op = c.adminOperation(op)
		ref.OperationID = op.ID
		ref.Status = op.Status
		ref.ErrorCode = op.ErrorCode
		if ref.Status == "pending" || ref.Status == "running" {
			break
		}
	}
	b.Pending, b.Completed, b.Failed = 0, 0, 0
	for _, v := range b.VMs {
		switch v.Status {
		case "succeeded":
			b.Completed++
		case "failed":
			b.Failed++
		default:
			b.Pending++
		}
	}
	if e := c.saveAdminBatch(b); e != nil {
		return AdminBatch{}, e
	}
	return b, nil
}

// adminOperation reconciles a split observation of domain and owner state.
// A terminal owner has already projected its final operation result; refresh
// that result before turning an unfinished operation into a batch failure.
func (c *Controller) adminOperation(op ControlOperation) ControlOperation {
	if op.Status == "pending" || op.Status == "running" {
		if st, err := c.ownerState(op.VMID); err == nil && st != "" && !ownerAlive(st) {
			for _, current := range c.st.Snapshot().Operations {
				if current.ID == op.ID && current.OwnerUserID == op.OwnerUserID {
					op = current
					break
				}
			}
			if op.Status == "pending" || op.Status == "running" {
				op.Status, op.ErrorCode = "failed", "unavailable"
			}
		}
	}
	return op
}

func (c *Controller) saveAdminBatch(b AdminBatch) error {
	return c.st.UpdateErr(func(s *State) error {
		for i := range s.AdminBatches {
			if s.AdminBatches[i].Token == b.Token {
				s.AdminBatches[i] = b
				return nil
			}
		}
		return ErrNoVM
	})
}
