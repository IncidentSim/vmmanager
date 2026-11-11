package controlclient

import (
	"context"
	"net/url"
	"time"
)

type AdminVMRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	OwnerUserID int64  `json:"owner_user_id"`
	OperationID string `json:"operation_id,omitempty"`
	Status      string `json:"status,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
}
type AdminBatch struct {
	Token       string       `json:"token"`
	AdminUserID int64        `json:"admin_user_id"`
	ChatID      int64        `json:"chat_id"`
	ExpiresAt   time.Time    `json:"expires_at"`
	Confirmed   bool         `json:"confirmed"`
	VMs         []AdminVMRef `json:"vms"`
	Pending     int          `json:"pending"`
	Completed   int          `json:"completed"`
	Failed      int          `json:"failed"`
}

func (c *Client) AdminPreview(ctx context.Context, admin int64, key string) (AdminBatch, error) {
	var b AdminBatch
	e := c.request(ctx, admin, "POST", "/v1/admin/delete-all", key, struct {
		ChatID int64 `json:"chat_id"`
	}{admin}, &b)
	return b, e
}
func (c *Client) AdminAdvance(ctx context.Context, admin int64, token string) (AdminBatch, error) {
	var b AdminBatch
	e := c.request(ctx, admin, "POST", "/v1/admin/delete-all/"+url.PathEscape(token), "admin-confirm:"+token, struct {
		ChatID int64 `json:"chat_id"`
	}{admin}, &b)
	return b, e
}
func (c *Client) AdminBatches(ctx context.Context, admin int64) ([]AdminBatch, error) {
	var env struct {
		Batches []AdminBatch `json:"batches"`
	}
	e := c.request(ctx, admin, "GET", "/v1/admin/delete-all", "", nil, &env)
	return env.Batches, e
}
