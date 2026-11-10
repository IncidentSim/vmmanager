package vmlib_test

import (
	"context"
	"errors"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
	"testing"
)

type publicationBusyBackend struct {
	vmlib.Backend
	busy bool
}

func (b *publicationBusyBackend) Prepare(ctx context.Context, r vmlib.PrepareRequest) (vmlib.HostVM, error) {
	if b.busy {
		return vmlib.HostVM{}, vmlib.ErrResourceExhausted
	}
	return b.Backend.Prepare(ctx, r)
}
func TestDefinitiveImagePublicationRefusalDoesNotRetainCreate(t *testing.T) {
	b := &publicationBusyBackend{Backend: vmlibtest.New(1, "base"), busy: true}
	s, _ := vmlib.NewService(b)
	req := vmlib.CreateRequest{ID: "retry", ImageID: "base", Profile: "small"}
	if _, err := s.Create(context.Background(), req); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatal(err)
	}
	b.busy = false
	if _, err := s.Create(context.Background(), req); err != nil {
		t.Fatalf("definitively refused creation retained identity: %v", err)
	}
}
