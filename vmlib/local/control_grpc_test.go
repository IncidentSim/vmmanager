//go:build linux

package local_test

import (
	"crypto/tls"
	"errors"
	"net"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/remote"
	"github.com/petruekhin/vibe-firecracker/vmlib/remote/pkitest"
	"google.golang.org/grpc"
)

// Uses real generated transports and real local disk/catalog/metadata logic.
// Only the launcher and Firecracker API are fakes; no host VM is started.
func controlGRPCListener(t *testing.T, domain string, register func(*grpc.Server)) (string, *tls.Config) {
	t.Helper()
	ca := pkitest.NewCA(t, domain)
	server := ca.Server(t, "backend.test")
	client := ca.Client(t, "operator")
	sc, err := remote.ServerTLSFromPEM(ca.CertPEM, server.CertPEM, server.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cc, err := remote.ClientTLSFromPEM(ca.CertPEM, client.CertPEM, client.KeyPEM, "backend.test")
	if err != nil {
		t.Fatal(err)
	}
	s := remote.NewGRPCServer(sc)
	register(s)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	t.Cleanup(s.Stop)
	return l.Addr().String(), cc
}

func TestLocalBackendDynamicResourcesThroughBothGRPCListeners(t *testing.T) {
	r := newRig(t, 2)
	b := r.backend()
	addr, tlsConfig := controlGRPCListener(t, "backend", func(s *grpc.Server) { remote.RegisterBackend(s, b) })
	bc, err := remote.DialBackend(addr, tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	svc, err := vmlib.NewService(bc)
	if err != nil {
		t.Fatal(err)
	}
	addr, tlsConfig = controlGRPCListener(t, "facade", func(s *grpc.Server) { remote.RegisterFacade(s, svc) })
	fc, err := remote.DialFacade(addr, tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()
	want := vmlib.VMResources{VCPU: 1, MemMiB: 193}
	v, err := fc.Create(ctx, vmlib.CreateRequest{ID: "custom", ImageID: "base", Resources: &want})
	if err != nil || v.Resources != want {
		t.Fatalf("create: %+v %v", v, err)
	}
	h, err := bc.Inspect(ctx, "custom")
	if err != nil || h.Resources != want {
		t.Fatalf("inspect: %+v %v", h, err)
	}
	if got := r.vmm.specs[0]; got.VCPU != want.VCPU || got.MemMiB != want.MemMiB {
		t.Fatalf("machine spec: %+v", got)
	}
	r.receipt(0, inv, "success", "exited", "0")
	if _, err := fc.Commit(ctx, "custom", "derived"); err != nil {
		t.Fatal(err)
	}
	if _, err := fc.Create(ctx, vmlib.CreateRequest{ID: "legacy", ImageID: "derived", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	if v, err := fc.Get(ctx, "legacy"); err != nil || v.Resources.MemMiB != 256 {
		t.Fatalf("legacy effective resources: %+v %v", v, err)
	}
	if err := fc.DeleteImage(ctx, "derived"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("reference guard: %v", err)
	}
	r.receipt(1, inv, "success", "exited", "0")
	if _, err := fc.Delete(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := fc.DeleteImage(ctx, "derived"); err != nil {
		t.Fatal(err)
	}
	if err := fc.DeleteImage(ctx, "derived"); err != nil {
		t.Fatal(err)
	}
	if _, err := bc.GetImage(ctx, "derived"); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatalf("readback: %v", err)
	}
	if err := fc.DeleteImage(ctx, "base"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("base: %v", err)
	}
}
