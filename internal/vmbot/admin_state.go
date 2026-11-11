package vmbot

import (
	"errors"
	"fmt"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

func validateAdminBatches(batches []AdminBatch) error {
	tokens, keys := map[string]bool{}, map[string]bool{}
	for _, b := range batches {
		key := fmt.Sprintf("%d:%s", b.AdminUserID, b.Key)
		if !tokenPattern.MatchString(b.Token) || tokens[b.Token] || keys[key] || b.AdminUserID <= 0 || b.ChatID != b.AdminUserID || b.Key == "" || len(b.Key) > 128 || b.ExpiresAt.IsZero() || len(b.VMs) > MaxVMsLimit {
			return errors.New("invalid admin batch")
		}
		tokens[b.Token], keys[key] = true, true
		ids := map[string]bool{}
		pending, completed, failed := 0, 0, 0
		for _, v := range b.VMs {
			if vmlib.ValidateID("vm", v.ID) != nil || ids[v.ID] || v.OwnerUserID <= 0 {
				return errors.New("invalid admin snapshot VM")
			}
			ids[v.ID] = true
			if v.OperationID != "" && vmlib.ValidateID("operation", v.OperationID) != nil {
				return errors.New("invalid admin operation reference")
			}
			switch v.Status {
			case "succeeded":
				completed++
			case "failed":
				failed++
			case "", "pending", "running":
				pending++
			default:
				return errors.New("invalid admin VM status")
			}
		}
		if b.Pending != pending || b.Completed != completed || b.Failed != failed {
			return errors.New("invalid admin progress counts")
		}
	}
	return nil
}
func immutableAdminBatches(old, next []AdminBatch) error {
	byToken := map[string]AdminBatch{}
	for _, b := range next {
		byToken[b.Token] = b
	}
	for _, b := range old {
		n, ok := byToken[b.Token]
		if !ok || b.AdminUserID != n.AdminUserID || b.ChatID != n.ChatID || b.Key != n.Key || !b.ExpiresAt.Equal(n.ExpiresAt) || len(b.VMs) != len(n.VMs) || (b.Confirmed && !n.Confirmed) {
			return errors.New("admin snapshot is immutable")
		}
		for i, v := range b.VMs {
			cur := n.VMs[i]
			if v.ID != cur.ID || v.OwnerUserID != cur.OwnerUserID || v.Name != cur.Name || (v.OperationID != "" && v.OperationID != cur.OperationID) || ((v.Status == "failed" || v.Status == "succeeded") && v.Status != cur.Status) {
				return errors.New("admin snapshot VM is immutable")
			}
		}
	}
	return nil
}
