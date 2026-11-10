package remote_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// Backend policy must reach a facade client over BOTH real mTLS gRPC hops
// (backend trust domain -> Service -> facade trust domain) without any
// client-side catalog.
func TestFacadeCapabilitiesForwardsBackendPolicyOverMTLS(t *testing.T) {
	e := startBackend(t)
	want := vmlib.Capabilities{
		MaxVMs: 7,
		Profiles: []vmlib.Profile{
			{Name: "tiny", VCPU: 1, MemMiB: 256, DiskMiB: 1024},
			{Name: "wide", VCPU: 4, MemMiB: 3072, DiskMiB: 8192},
		},
		MaxTotalMemMiB: 6144,
		MaxTotalVCPU:   6,
		// Must NOT leak through the capabilities RPC: SSH metadata has its own call.
		Bastion: &vmlib.Bastion{Host: "secret-bastion.example.test", Port: 2222, User: "jump"},
		SSHUser: "root",
	}
	e.backend.SetCapabilities(want)
	svc, err := vmlib.NewService(connect(t, e))
	if err != nil {
		t.Fatal(err)
	}
	c := facadeClient(t, serveFacade(t, svc))
	ctx := context.Background()
	got, err := c.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	exp := vmlib.ResourceCapabilities{MaxVMs: 7, Profiles: want.Profiles, MaxTotalMemMiB: 6144, MaxTotalVCPU: 6}
	if !reflect.DeepEqual(got, exp) {
		t.Fatalf("got %+v want %+v", got, exp)
	}

	// Policy changes are live, not cached by either hop.
	want.MaxVMs, want.Profiles, want.MaxTotalMemMiB, want.MaxTotalVCPU = 1, []vmlib.Profile{{Name: "only", VCPU: 2, MemMiB: 640, DiskMiB: 512}}, 0, 0
	e.backend.SetCapabilities(want)
	got, err = c.Capabilities(ctx)
	if err != nil || got.MaxVMs != 1 || len(got.Profiles) != 1 || got.Profiles[0].Name != "only" || got.MaxTotalMemMiB != 0 || got.MaxTotalVCPU != 0 {
		t.Fatalf("live policy: %+v %v", got, err)
	}
}

// Service itself is the forwarding layer and is usable without gRPC.
func TestServiceCapabilitiesReflectsBackendWithoutCatalog(t *testing.T) {
	e := startBackend(t)
	e.backend.SetCapabilities(vmlib.Capabilities{MaxVMs: 3})
	svc, err := vmlib.NewService(connect(t, e))
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Capabilities(context.Background())
	if err != nil || got.MaxVMs != 3 || len(got.Profiles) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}
