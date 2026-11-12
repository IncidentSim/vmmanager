package hostinstall

import (
	"context"
	"os/user"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/vmnet"
)

type vmnetResult = vmnet.Result

type recorder func(args []string)

func (r recorder) Run(_ context.Context, _ string, name string, a ...string) (vmnet.Result, error) {
	r(append([]string{name}, a...))
	return vmnet.Result{}, nil
}

func currentUser(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	if u.Uid == "0" {
		t.Skip("setup refuses root as the vmapi user")
	}
	return u.Username
}
