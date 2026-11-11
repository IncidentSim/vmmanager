package controlclient

import "context"

// GeneratedKey contains sensitive material. Never log it or store it in workflow history.
type GeneratedKey struct {
	VMID       string `json:"vm_id"`
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
}

func (c *Client) SSHKey(ctx context.Context, u int64, id string) (GeneratedKey, error) {
	var k GeneratedKey
	err := c.request(ctx, u, "GET", vmPath(id)+"/ssh-key", "", nil, &k)
	return k, err
}
