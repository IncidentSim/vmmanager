package controlapi

import (
	"context"
	"errors"
	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
)

// AccessKeys fences the encrypted credential store with the same owner check as
// VM projection, including when called outside the HTTP route.
type AccessKeys struct {
	Controller  *vmbot.Controller
	Credentials sshaccess.Credentials
}

func (a AccessKeys) GeneratedKey(ctx context.Context, owner int64, id string) (GeneratedKey, error) {
	if err := ctx.Err(); err != nil {
		return GeneratedKey{}, err
	}
	if _, err := a.Controller.ControlVM(owner, id); err != nil {
		return GeneratedKey{}, err
	}
	pub, err := a.Credentials.Public(id)
	if err != nil {
		return GeneratedKey{}, err
	}
	if !pub.OwnerGenerated {
		return GeneratedKey{}, ErrKeyNotGenerated
	}
	key, err := a.Credentials.OwnerPrivateKey(id)
	if errors.Is(err, sshaccess.ErrKeyNotGenerated) {
		return GeneratedKey{}, ErrKeyNotGenerated
	}
	if err != nil {
		return GeneratedKey{}, err
	}
	return GeneratedKey{PrivateKeyPEM: key, PublicKey: pub.OwnerPublic}, nil
}
